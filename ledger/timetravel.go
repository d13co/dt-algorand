// Copyright (C) 2019-2026 Algorand Foundation Ltd.
// This file is part of go-algorand
//
// go-algorand is free software: you can redistribute it and/or modify
// it under the terms of the GNU Affero General Public License as
// published by the Free Software Foundation, either version 3 of the
// License, or (at your option) any later version.
//
// go-algorand is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
// GNU Affero General Public License for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with go-algorand.  If not, see <https://www.gnu.org/licenses/>.

package ledger

import (
	"bytes"
	"cmp"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"time"

	"github.com/algorand/go-deadlock"
	"github.com/algorand/msgp/msgp"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/eval"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/metrics"
)

// historyTracker feeds the time travel store with the object versions and
// header of every round. The ledger serves lookups for rounds older than the
// trackers' window from it. The store lives as long as the ledger, so readers
// need no tracker lock. Rounds are written in prepareCommit, before (and
// outside) the tracker DB transaction, so the store always reaches at least
// the tracker DB round; replays after a crash are skipped by the store.
type historyTracker struct {
	store *timetravel.Store
	path  string

	mu      deadlock.Mutex
	pending []historyRound

	// online holds the accounts online as of the last round given to
	// newBlock, for each round's expired stake. Only loadFromDisk and
	// newBlock, which the tracker registry serializes, touch it.
	online onlineSet

	// reeval bounds concurrent re-evaluations of old state deltas.
	// ponytail: fixed at 2, a config knob if delta-heavy clients need more
	reeval chan struct{}
}

type historyRound struct {
	rnd     basics.Round
	changes []timetravel.Change
	idx     map[string]int // changes by key
	hdr     []byte
	expired *uint64 // expired online stake, for rounds with ExcludeExpiredCirculation
}

// A time travel ledger replays whole archives (tools/ttimport), where each
// flush's fixed costs dominate rounds that change little: onlineAccounts reads
// every online account to prune their history. Past
// initializeCachesRoundFlushInterval rounds, its replay therefore flushes only
// once the rounds since the last flush made more than
// timeTravelReplayFlushChanges changes (replayChanges) or number
// timeTravelReplayMaxFlushRounds. Variables for tests.
var (
	timeTravelReplayMaxFlushRounds = basics.Round(10_000)
	timeTravelReplayFlushChanges   = 100_000
)

// replayChanges counts what a round keeps in the trackers' memory until it is
// flushed: changed accounts, resources and boxes, and transactions.
func replayChanges(delta *ledgercore.StateDelta) int {
	return delta.Accts.Len() + len(delta.Accts.AppResources) + len(delta.Accts.AssetResources) + len(delta.KvMods) + len(delta.Txids)
}

func openHistoryTracker(dirs DirsAndPrefix, cfg config.Local, log logging.Logger) (*historyTracker, error) {
	// hand-built DirsAndPrefix may lack the time travel dir: keep the store next to the blocks then
	path := filepath.Join(cmp.Or(dirs.TimeTravelGenesisDir, dirs.BlockGenesisDir), dirs.DBFilePrefix) + ".timetravel"
	s, err := timetravel.Open(path, timetravel.Options{CheckpointInterval: cfg.TimeTravelCheckpointInterval, Logger: pebbleLog{log}})
	if err != nil {
		return nil, fmt.Errorf("opening time travel store %s: %w", path, err)
	}
	return &historyTracker{store: s, path: path, reeval: make(chan struct{}, 2)}, nil
}

// checkAccountsNotAhead refuses an account DB ahead of the block DB, which
// trackerDBInitialize would reset: a resync from genesis, weeks on mainnet.
// tools/ttimport leaves the data directory that way until a run completes.
func (l *Ledger) checkAccountsNotAhead() error {
	ar, err := l.trackerDBs.MakeAccountsReader()
	if err != nil {
		return err
	}
	rnd, err := ar.AccountsRound()
	if err != nil {
		return nil //nolint:nilerr // a new account DB has no round until its migrations run
	}
	if latest := l.Latest(); rnd > latest {
		return fmt.Errorf("the account DB is at round %d but the block DB ends at round %d: rerun tools/ttimport if it was interrupted, or delete the account DB to resync from genesis", rnd, latest)
	}
	return nil
}

// pebbleLog demotes the engine's informational messages to debug.
type pebbleLog struct{ logging.Logger }

func (p pebbleLog) Infof(format string, args ...interface{}) { p.Debugf(format, args...) }

func (ht *historyTracker) loadFromDisk(l ledgerForTracker, dbRound basics.Round) error {
	ht.mu.Lock()
	ht.pending = nil
	ht.mu.Unlock()
	if err := ht.loadOnline(l); err != nil {
		return err
	}

	last, ok := ht.store.Last()
	if ok {
		if basics.Round(last) < dbRound {
			return fmt.Errorf("time travel store %s ends at round %d but the ledger is at round %d", ht.path, last, dbRound)
		}
		return nil
	}
	if dbRound != 0 {
		return fmt.Errorf("time travel store %s is empty but the ledger is at round %d: if the store was moved, remove this empty one and point TimeTravelDBDir at it; otherwise the node must sync from genesis (no fast catchup)", ht.path, dbRound)
	}
	hdr, err := l.BlockHdr(0)
	if err != nil {
		return err
	}
	var ot basics.OverflowTracker
	var totals ledgercore.AccountTotals
	for _, ad := range l.GenesisAccounts() {
		totals.AddAccount(l.GenesisProto().RewardUnit, ledgercore.ToAccountData(ad), &ot)
	}
	if err := ht.store.AddRound(0, timetravel.GenesisChanges(l.GenesisAccounts(), totals), protocol.Encode(&hdr), nil); err != nil {
		return err
	}
	return ht.store.Sync()
}

// loadOnline loads the online accounts at the tracker DB round from its
// onlineaccounts table; newBlock then applies the rounds replayed after it.
func (ht *historyTracker) loadOnline(l ledgerForTracker) error {
	accts := map[basics.Address]onlineEntry{}
	var rows []trackerdb.PersistedOnlineAccountData
	err := l.trackerDB().Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
		ar, err := tx.MakeAccountsReader()
		if err != nil {
			return err
		}
		rows, err = ar.OnlineAccountsAll(0)
		return err
	})
	if err != nil {
		return fmt.Errorf("loading online accounts for the time travel history: %w", err)
	}
	// rows are by address, then round: an address ends at its latest row,
	// which is empty if it went offline
	for _, r := range rows {
		if r.AccountData.IsVotingEmpty() {
			delete(accts, r.Addr)
		} else {
			accts[r.Addr] = onlineEntry{microAlgos: r.AccountData.MicroAlgos.Raw, rewardsBase: r.AccountData.RewardsBase, voteLastValid: r.AccountData.VoteLastValid}
		}
	}
	// no reward unit yet: the first round's advance builds the sums
	ht.online = onlineSet{accts: accts}
	return nil
}

// expiredStake applies the round's accounts to the online set and returns
// what onlineAccounts.expiredOnlineCirculation(rnd, voteRnd) will return once
// rnd is the balance round of vote round voteRnd: the stake, with rewards at
// rnd, of the accounts online at rnd whose keys expire before voteRnd. Nil
// for rounds whose online stake does not exclude it.
func (ht *historyTracker) expiredStake(blk bookkeeping.Block, delta *ledgercore.StateDelta) *uint64 {
	for i := 0; i < delta.Accts.Len(); i++ {
		addr, ad := delta.Accts.GetByIdx(i)
		if ad.Status == basics.Online {
			ht.online.set(addr, onlineEntry{microAlgos: ad.MicroAlgos.Raw, rewardsBase: ad.RewardsBase, voteLastValid: ad.VoteLastValid})
		} else {
			ht.online.drop(addr)
		}
	}
	proto := config.Consensus[blk.CurrentProtocol]
	ht.online.advance(blk.Round()+agreement.BalanceLookback(proto), proto.RewardUnit)
	if blk.Round() == 0 || !proto.ExcludeExpiredCirculation {
		return nil
	}
	expired := ht.online.stake(delta.Totals.RewardsLevel)
	return &expired
}

func (ht *historyTracker) newBlock(blk bookkeeping.Block, delta ledgercore.StateDelta) {
	r := historyRound{rnd: blk.Round(), changes: timetravel.Changes(&delta), hdr: protocol.Encode(&blk.BlockHeader)}
	r.idx = make(map[string]int, len(r.changes))
	for i, ch := range r.changes {
		r.idx[string(ch.Key)] = i
	}
	r.expired = ht.expiredStake(blk, &delta)
	ht.mu.Lock()
	ht.pending = append(ht.pending, r)
	ht.mu.Unlock()
}

func (ht *historyTracker) committedUpTo(rnd basics.Round) (basics.Round, basics.Round) {
	return rnd, 0
}

func (ht *historyTracker) produceCommittingTask(committedRound basics.Round, dbRound basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	return dcr
}

func (ht *historyTracker) prepareCommit(dcc *deferredCommitContext) error {
	newBase := dcc.newBase()
	ht.mu.Lock()
	n := 0
	for n < len(ht.pending) && ht.pending[n].rnd <= newBase {
		n++
	}
	rounds := ht.pending[:n]
	ht.mu.Unlock()
	if n == 0 {
		return nil
	}
	defer ledgerTimeTravelWriteMicros.AddMicrosecondsSince(time.Now(), nil)
	for _, r := range rounds {
		if err := ht.store.AddRound(uint64(r.rnd), r.changes, r.hdr, r.expired); err != nil {
			return err
		}
	}
	return ht.store.Sync()
}

// ledgerTimeTravelWriteMicros is the time commits spend writing the history,
// outside the account DB transaction that ledger_commitround_micros times.
var ledgerTimeTravelWriteMicros = metrics.NewCounter("ledger_timetravel_write_micros", "µs spent writing rounds to the time travel store")

func (ht *historyTracker) commitRound(context.Context, trackerdb.TransactionScope, *deferredCommitContext) error {
	return nil
}

func (ht *historyTracker) postCommit(ctx context.Context, dcc *deferredCommitContext) {
	ht.mu.Lock()
	n := 0
	for n < len(ht.pending) && ht.pending[n].rnd <= dcc.newBase() {
		n++
	}
	ht.pending = append([]historyRound(nil), ht.pending[n:]...)
	ht.mu.Unlock()
}

// close is a no-op: the store outlives tracker reloads and is closed by Ledger.Close.
func (ht *historyTracker) close() {}

// historyAt returns a view of the history if it reaches round rnd.
func (l *Ledger) historyAt(rnd basics.Round) *histView {
	if l.history == nil {
		return nil
	}
	ht := l.history
	ht.mu.Lock()
	v := &histView{store: ht.store, pending: ht.pending}
	ht.mu.Unlock()
	// Read last after taking pending: rounds leave pending only once they are
	// in the store, so pending holds every round in (last, latest].
	last, ok := ht.store.Last()
	if !ok {
		return nil // loadFromDisk writes round 0 before any round is pending
	}
	v.last = basics.Round(last)
	if n := len(v.pending); rnd <= v.last || (n > 0 && rnd <= v.pending[n-1].rnd) {
		return v
	}
	return nil
}

// historyFor returns the history view when err says the trackers no longer
// hold round rnd. Checking err first keeps lookups the trackers answered free.
func (l *Ledger) historyFor(rnd basics.Round, err error) *histView {
	if !tooOld(err) {
		return nil
	}
	return l.historyAt(rnd)
}

// historyNow returns the history view of round rnd, taken under trackerMu so
// that every round up to Latest() is in it.
func (l *Ledger) historyNow(rnd basics.Round) *histView {
	l.trackerMu.RLock()
	defer l.trackerMu.RUnlock()
	return l.historyAt(rnd)
}

// histView reads the history: the rounds the tracker has not written to the
// store yet (in memory) first, then the store.
type histView struct {
	store   *timetravel.Store
	pending []historyRound // oldest first; never modified in place
	last    basics.Round   // the store holds every round up to last
}

// Get returns the value of obj at rnd.
func (v *histView) Get(obj []byte, rnd uint64) ([]byte, bool, error) {
	for i := len(v.pending) - 1; i >= 0 && v.pending[i].rnd > v.last; i-- {
		p := &v.pending[i]
		if uint64(p.rnd) > rnd {
			continue
		}
		if j, ok := p.idx[string(obj)]; ok {
			return p.changes[j].Value, !p.changes[j].Deleted, nil
		}
	}
	return v.store.Get(obj, min(rnd, uint64(v.last)))
}

// Digest returns the ChangesDigest of round rnd.
func (v *histView) Digest(rnd uint64) ([]byte, bool, error) {
	for i := range v.pending {
		if uint64(v.pending[i].rnd) == rnd {
			return timetravel.ChangesDigest(v.pending[i].changes), true, nil
		}
	}
	return v.store.Digest(rnd)
}

// Header returns the encoded header of round rnd.
func (v *histView) Header(rnd uint64) ([]byte, bool, error) {
	for i := range v.pending {
		if uint64(v.pending[i].rnd) == rnd {
			return v.pending[i].hdr, true, nil
		}
	}
	return v.store.Header(rnd)
}

// ExpiredStake returns the expired online stake recorded for round rnd.
func (v *histView) ExpiredStake(rnd uint64) (uint64, bool, error) {
	for i := range v.pending {
		if uint64(v.pending[i].rnd) == rnd {
			if e := v.pending[i].expired; e != nil {
				return *e, true, nil
			}
			return 0, false, nil
		}
	}
	return v.store.ExpiredStake(rnd)
}

// Scan is timetravel.Store.Scan with the in-memory rounds merged in.
func (v *histView) Scan(prefix, after []byte, rnd uint64, fn func(obj, val []byte) bool) error {
	over := map[string]timetravel.Change{}
	for i := len(v.pending) - 1; i >= 0 && v.pending[i].rnd > v.last; i-- {
		if uint64(v.pending[i].rnd) > rnd {
			continue
		}
		for _, ch := range v.pending[i].changes {
			if _, seen := over[string(ch.Key)]; !seen && bytes.HasPrefix(ch.Key, prefix) && bytes.Compare(ch.Key, after) > 0 {
				over[string(ch.Key)] = ch
			}
		}
	}
	keys := slices.Sorted(maps.Keys(over))
	stopped := false
	// emit sends the in-memory objects before obj ("" for all of them).
	emit := func(obj string) bool {
		for len(keys) > 0 && (obj == "" || keys[0] < obj) {
			ch := over[keys[0]]
			keys = keys[1:]
			if !ch.Deleted && !fn(ch.Key, ch.Value) {
				stopped = true
				return false
			}
		}
		return true
	}
	err := v.store.Scan(prefix, after, min(rnd, uint64(v.last)), func(obj, val []byte) bool {
		if !emit(string(obj)) {
			return false
		}
		if len(keys) > 0 && keys[0] == string(obj) {
			ch := over[keys[0]]
			keys = keys[1:]
			if ch.Deleted {
				return true
			}
			val = ch.Value
		}
		stopped = !fn(obj, val)
		return !stopped
	})
	if err != nil || stopped {
		return err
	}
	emit("")
	return nil
}

// tooOld reports whether err is a tracker's "round is older than my window" error.
func tooOld(err error) bool {
	var roe *RoundOffsetError
	return errors.As(err, &roe)
}

var errHistoryUnavailable = errors.New("round is not available in the time travel history yet")

func histGet(h *histView, obj []byte, rnd basics.Round, v msgp.Unmarshaler) (bool, error) {
	b, ok, err := h.Get(obj, uint64(rnd))
	if err != nil || !ok {
		return false, err
	}
	return true, protocol.Decode(b, v)
}

func histAccount(h *histView, rnd basics.Round, addr basics.Address) (ledgercore.AccountData, error) {
	var ba trackerdb.BaseAccountData
	if _, err := histGet(h, timetravel.AccountKey(addr), rnd, &ba); err != nil {
		return ledgercore.AccountData{}, err
	}
	return ba.GetLedgerCoreAccountData(), nil
}

func histTotals(h *histView, rnd basics.Round) (ledgercore.AccountTotals, error) {
	var t ledgercore.AccountTotals
	ok, err := histGet(h, timetravel.TotalsKey, rnd, &t)
	if err == nil && !ok {
		err = fmt.Errorf("no totals for round %d", rnd)
	}
	return t, err
}

// ErrOnlineStakeUnavailable is returned for an online stake the history did
// not record: a round written before it recorded expired stake, or a vote
// round that agreement does not pair with the balance round.
var ErrOnlineStakeUnavailable = errors.New("the time travel history does not record this online stake")

// histOnlineCirculation is onlineAccounts.onlineCirculation from the history.
func histOnlineCirculation(h *histView, rnd, voteRnd basics.Round) (basics.MicroAlgos, error) {
	t, err := histTotals(h, rnd)
	if err != nil {
		return basics.MicroAlgos{}, err
	}
	hdr, err := histHeader(h, rnd)
	if err != nil {
		return basics.MicroAlgos{}, err
	}
	proto := config.Consensus[hdr.CurrentProtocol]
	if rnd == 0 || !proto.ExcludeExpiredCirculation {
		return t.Online.Money, nil
	}
	expired, ok, err := h.ExpiredStake(uint64(rnd))
	if err != nil {
		return basics.MicroAlgos{}, err
	}
	if !ok || voteRnd != rnd+agreement.BalanceLookback(proto) {
		return basics.MicroAlgos{}, fmt.Errorf("%w: balance round %d, vote round %d", ErrOnlineStakeUnavailable, rnd, voteRnd)
	}
	var ot basics.OverflowTracker
	c := ot.SubA(t.Online.Money, basics.MicroAlgos{Raw: expired})
	if ot.Overflowed {
		return basics.MicroAlgos{}, fmt.Errorf("round %d: expired stake %d exceeds the online money %d", rnd, expired, t.Online.Money.Raw)
	}
	return c, nil
}

func histHeader(h *histView, rnd basics.Round) (bookkeeping.BlockHeader, error) {
	var hdr bookkeeping.BlockHeader
	b, ok, err := h.Header(uint64(rnd))
	if err != nil {
		return hdr, err
	}
	if !ok {
		return hdr, ledgercore.ErrNoEntry{Round: rnd}
	}
	return hdr, protocol.Decode(b, &hdr)
}

// histParams returns the creator and params of an asset (p *basics.AssetParams) or app (*basics.AppParams).
func histParams(h *histView, rnd basics.Round, key []byte, p msgp.Unmarshaler) (basics.Address, bool, error) {
	var creator basics.Address
	b, ok, err := h.Get(key, uint64(rnd))
	if err != nil || !ok {
		return creator, false, err
	}
	if len(b) < len(creator) {
		return creator, false, fmt.Errorf("short params record %x", key)
	}
	copy(creator[:], b)
	if p != nil {
		err = protocol.Decode(b[len(creator):], p)
	}
	return creator, true, err
}

func histCreator(h *histView, rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	key := timetravel.AppParamsKey(basics.AppIndex(cidx))
	if ctype == basics.AssetCreatable {
		key = timetravel.AssetParamsKey(basics.AssetIndex(cidx))
	}
	return histParams(h, rnd, key, nil)
}

func histResource(h *histView, rnd basics.Round, addr basics.Address, cidx basics.CreatableIndex, ctype basics.CreatableType) (ledgercore.AccountResource, error) {
	var res ledgercore.AccountResource
	if ctype == basics.AssetCreatable {
		var hold basics.AssetHolding
		if ok, err := histGet(h, timetravel.HoldingKey(addr, basics.AssetIndex(cidx)), rnd, &hold); err != nil {
			return res, err
		} else if ok {
			res.AssetHolding = &hold
		}
		var p basics.AssetParams
		creator, ok, err := histParams(h, rnd, timetravel.AssetParamsKey(basics.AssetIndex(cidx)), &p)
		if ok && creator == addr {
			res.AssetParams = &p
		}
		return res, err
	}
	var ls basics.AppLocalState
	if ok, err := histGet(h, timetravel.LocalKey(addr, basics.AppIndex(cidx)), rnd, &ls); err != nil {
		return res, err
	} else if ok {
		res.AppLocalState = &ls
	}
	var p basics.AppParams
	creator, ok, err := histParams(h, rnd, timetravel.AppParamsKey(basics.AppIndex(cidx)), &p)
	if ok && creator == addr {
		res.AppParams = &p
	}
	return res, err
}

// histKvPairs lists the kv pairs with keyPrefix after cursor, up to limit
// pairs or maxBytes (0: unlimited); more reports a truncation.
func histKvPairs(h *histView, rnd basics.Round, keyPrefix, cursor string, limit, maxBytes uint64, includeValues bool) (res []ledgercore.KvPairResult, more bool, err error) {
	if limit == 0 {
		return nil, false, nil
	}
	var after []byte
	if cursor != "" {
		after = timetravel.KvKey(cursor)
	}
	var size uint64
	err = h.Scan(timetravel.KvPrefix(keyPrefix), after, uint64(rnd), func(obj, val []byte) bool {
		kv := ledgercore.KvPairResult{Key: timetravel.KvKeyString(obj)}
		if includeValues {
			kv.Value = val
		}
		if uint64(len(res)) == limit || (maxBytes > 0 && len(res) > 0 && size+kv.ByteSize() > maxBytes) {
			more = true
			return false
		}
		size += kv.ByteSize()
		res = append(res, kv)
		return true
	})
	return res, more, err
}

// idValue is an object that scanIDs found: its id (the trailing 8 key bytes)
// and its value.
type idValue struct {
	id  uint64
	val []byte
}

// scanIDs returns the objects under prefix that exist at rnd and whose ids
// are greater than gt, in id order, up to limit.
func scanIDs(h *histView, rnd basics.Round, prefix []byte, gt uint64, limit uint64) ([]idValue, error) {
	if limit == 0 {
		return nil, nil
	}
	var out []idValue
	err := h.Scan(prefix, binary.BigEndian.AppendUint64(append([]byte(nil), prefix...), gt), uint64(rnd), func(obj, val []byte) bool {
		out = append(out, idValue{id: binary.BigEndian.Uint64(obj[len(obj)-8:]), val: val})
		return uint64(len(out)) < limit
	})
	return out, err
}

// LookupAssetsAt is LookupAssets at round rnd.
func (l *Ledger) LookupAssetsAt(rnd basics.Round, addr basics.Address, assetIDGT basics.AssetIndex, limit uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	h := l.historyNow(rnd)
	if h == nil {
		return nil, 0, errHistoryUnavailable
	}
	holdings, err := scanIDs(h, rnd, timetravel.HoldingKey(addr, 0)[:1+len(addr)], uint64(assetIDGT), limit)
	if err != nil {
		return nil, 0, err
	}
	out := make([]ledgercore.AssetResourceWithIDs, 0, len(holdings))
	for _, hv := range holdings {
		r := ledgercore.AssetResourceWithIDs{AssetID: basics.AssetIndex(hv.id)}
		var hold basics.AssetHolding
		if err := protocol.Decode(hv.val, &hold); err != nil {
			return nil, 0, err
		}
		r.AssetHolding = &hold
		var p basics.AssetParams
		creator, ok, err := histParams(h, rnd, timetravel.AssetParamsKey(r.AssetID), &p)
		if err != nil {
			return nil, 0, err
		}
		if ok {
			r.Creator, r.AssetParams = creator, &p
		}
		out = append(out, r)
	}
	return out, rnd, nil
}

// LookupApplicationsAt is LookupApplications at round rnd.
func (l *Ledger) LookupApplicationsAt(rnd basics.Round, addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	h := l.historyNow(rnd)
	if h == nil {
		return nil, 0, errHistoryUnavailable
	}
	return histApplications(h, rnd, addr, appIDGT, limit, includeParams)
}

func histApplications(h *histView, rnd basics.Round, addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	local, err := scanIDs(h, rnd, timetravel.LocalKey(addr, 0)[:1+len(addr)], uint64(appIDGT), limit)
	if err != nil {
		return nil, 0, err
	}
	created, err := scanIDs(h, rnd, timetravel.CreatedPrefix(addr, basics.AppCreatable), uint64(appIDGT), limit)
	if err != nil {
		return nil, 0, err
	}
	// An app found only in created has no local state: had local stopped
	// before its local state, out would fill with local's ids first.
	out := []ledgercore.AppResourceWithIDs{}
	for len(local)+len(created) > 0 && uint64(len(out)) < limit {
		var lv idValue // the local state
		inLocal := len(local) > 0 && (len(created) == 0 || local[0].id <= created[0].id)
		if inLocal {
			lv, local = local[0], local[1:]
		}
		id := lv.id
		if len(created) > 0 && (!inLocal || created[0].id == id) {
			id, created = created[0].id, created[1:]
		}
		r := ledgercore.AppResourceWithIDs{AppID: basics.AppIndex(id)}
		if inLocal {
			r.AppLocalState = new(basics.AppLocalState)
			if err := protocol.Decode(lv.val, r.AppLocalState); err != nil {
				return nil, 0, err
			}
		}
		var p basics.AppParams
		creator, ok, err := histParams(h, rnd, timetravel.AppParamsKey(r.AppID), &p)
		if err != nil {
			return nil, 0, err
		}
		if ok && creator == addr {
			r.Creator = addr
			if includeParams {
				r.AppParams = &p
			}
		}
		out = append(out, r)
	}
	return out, rnd, nil
}

// LookupLatestAt is LookupLatest at round rnd: the account with all its
// resources and pending rewards applied.
func (l *Ledger) LookupLatestAt(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	h := l.historyNow(rnd)
	if h == nil {
		return basics.AccountData{}, 0, basics.MicroAlgos{}, errHistoryUnavailable
	}
	ad, withoutRewards, err := l.histAccountWithRewards(h, rnd, addr)
	if err != nil {
		return basics.AccountData{}, 0, basics.MicroAlgos{}, err
	}
	var out basics.AccountData
	ledgercore.AssignAccountData(&out, ad)

	holdings, err := scanIDs(h, rnd, timetravel.HoldingKey(addr, 0)[:1+len(addr)], 0, ^uint64(0))
	if err != nil {
		return out, 0, withoutRewards, err
	}
	for _, hv := range holdings {
		var hold basics.AssetHolding
		if err = protocol.Decode(hv.val, &hold); err != nil {
			return out, 0, withoutRewards, err
		}
		ledgercore.AssignAccountResourceToAccountData(basics.CreatableIndex(hv.id), ledgercore.AccountResource{AssetHolding: &hold}, &out)
	}
	createdAssets, err := scanIDs(h, rnd, timetravel.CreatedPrefix(addr, basics.AssetCreatable), 0, ^uint64(0))
	if err != nil {
		return out, 0, withoutRewards, err
	}
	for _, cv := range createdAssets {
		var p basics.AssetParams
		creator, ok, err := histParams(h, rnd, timetravel.AssetParamsKey(basics.AssetIndex(cv.id)), &p)
		if err != nil {
			return out, 0, withoutRewards, err
		}
		if ok && creator == addr {
			ledgercore.AssignAccountResourceToAccountData(basics.CreatableIndex(cv.id), ledgercore.AccountResource{AssetParams: &p}, &out)
		}
	}
	apps, _, err := histApplications(h, rnd, addr, 0, ^uint64(0), true)
	if err != nil {
		return out, 0, withoutRewards, err
	}
	for _, app := range apps {
		ledgercore.AssignAccountResourceToAccountData(basics.CreatableIndex(app.AppID), ledgercore.AccountResource{AppLocalState: app.AppLocalState, AppParams: app.AppParams}, &out)
	}
	return out, rnd, withoutRewards, nil
}

// histAccountWithRewards returns the account at rnd with pending rewards
// applied, and its balance before them.
func (l *Ledger) histAccountWithRewards(h *histView, rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.MicroAlgos, error) {
	ad, err := histAccount(h, rnd, addr)
	if err != nil {
		return ad, basics.MicroAlgos{}, err
	}
	hdr, err := histHeader(h, rnd)
	if err != nil {
		return ad, basics.MicroAlgos{}, err
	}
	withoutRewards := ad.MicroAlgos
	return ad.WithUpdatedRewards(config.Consensus[hdr.CurrentProtocol].RewardUnit, hdr.RewardsLevel), withoutRewards, nil
}

// pastLedger is the ledger as of round rnd, for re-evaluating block rnd+1.
type pastLedger struct {
	*Ledger
	rnd basics.Round
}

func (p pastLedger) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	t, err := p.Totals(p.rnd)
	return p.rnd, t, err
}

func (p pastLedger) FlushCaches() {}

// OnlineCirculation falls back to the online money, an upper bound, where the
// history did not record the online stake: verifyDelta rejects a delta that
// it changed.
func (p pastLedger) OnlineCirculation(rnd, voteRnd basics.Round) (basics.MicroAlgos, error) {
	c, err := p.Ledger.OnlineCirculation(rnd, voteRnd)
	if errors.Is(err, ErrOnlineStakeUnavailable) {
		t, terr := p.Totals(rnd)
		return t.Online.Money, terr
	}
	return c, err
}

// ErrDeltaReevaluation wraps the failures to rebuild an old round's state
// delta other than its block being gone.
var ErrDeltaReevaluation = errors.New("re-evaluating the round's block failed")

// reevalDelta rebuilds the state delta of round rnd by evaluating its block
// against the history at rnd-1 the way AddBlock did (no validation: the block
// is certified), then checks the result against the round's stored changes.
// Where the history did not record the online stake (online_stake opcode),
// the evaluation uses an upper bound and can diverge; the check turns that
// into an error.
func (l *Ledger) reevalDelta(rnd basics.Round) (ledgercore.StateDelta, error) {
	blk, err := l.Block(rnd)
	if err != nil {
		return ledgercore.StateDelta{}, fmt.Errorf("round %d is older than the in-memory deltas and its block is not stored: %w", rnd, err)
	}
	l.history.reeval <- struct{}{}
	defer func() { <-l.history.reeval }()
	sd, err := eval.Eval(context.Background(), pastLedger{l, rnd - 1}, blk, false, nil, nil, nil)
	if err != nil {
		return ledgercore.StateDelta{}, fmt.Errorf("%w: round %d: %w", ErrDeltaReevaluation, rnd, err)
	}
	if err := verifyDelta(l.historyAt(rnd), rnd, &sd); err != nil {
		return ledgercore.StateDelta{}, err
	}
	return sd, nil
}

// verifyDelta checks that sd changes exactly what the history recorded for rnd.
func verifyDelta(h *histView, rnd basics.Round, sd *ledgercore.StateDelta) error {
	want, ok, err := h.Digest(uint64(rnd))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: round %d: no stored digest", ErrDeltaReevaluation, rnd)
	}
	if !bytes.Equal(timetravel.ChangesDigest(timetravel.Changes(sd)), want) {
		return fmt.Errorf("%w: round %d: the result differs from the stored changes", ErrDeltaReevaluation, rnd)
	}
	return nil
}
