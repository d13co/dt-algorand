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
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// A time travel node can catch up from a catchpoint when its history store
// was installed beforehand (a snapshot of another node's store, see
// tools/ttsnap): the catchpoint brings the state at its balances round B,
// verified against its label as usual, and the history must reach B. Before
// the catchup swaps the tables, CompleteCatchup checks the history against
// what the catchpoint verified: its header of round B (hash-chained to the
// labelled block) and its totals. It then deletes the history's records of
// the rounds after B (truncate): the ledger replays those rounds from blocks
// and writes them from its own evaluation, as a node that synced them would,
// so nothing after B is the snapshot producer's. Rounds up to B are, which
// attesters cover (tools/ttsnap).

// ErrHistoryUntrusted is returned for lookups of a history that is not
// served: it differs from the chain (the store has to be replaced, or the
// mark cleared), or a replay found what a reload clears (see
// verifyReplayed).
var ErrHistoryUntrusted = errors.New("the time travel history is not served")

// ErrHistoryTruncationInterrupted is returned by PrepareCatchpointHistory when
// its context ends while it truncates the history. The store finishes the
// truncation when it is next opened, and the catchup can resume.
var ErrHistoryTruncationInterrupted = errors.New("the truncation of the time travel history was interrupted")

// ErrHistoryTooShort is returned for a catchpoint whose balances round the
// installed history does not reach.
var ErrHistoryTooShort = errors.New("the time travel history does not reach the catchpoint")

// checkStart checks that the store starts with this network's genesis block.
func (ht *historyTracker) checkStart() error {
	if ht.genesisHdr == nil {
		return nil
	}
	h0, ok, err := ht.store.Header(0)
	if err != nil {
		return err
	}
	if !ok {
		first, found, ferr := ht.store.First()
		if ferr != nil {
			return ferr
		}
		if !found {
			return fmt.Errorf("time travel store %s holds no headers", ht.path)
		}
		return fmt.Errorf("time travel store %s starts at round %d, not at genesis", ht.path, first)
	}
	if !bytes.Equal(h0, ht.genesisHdr) {
		return fmt.Errorf("time travel store %s holds another network's genesis block", ht.path)
	}
	return nil
}

// checkHeader compares the store's header of round rnd with the block DB's,
// if the block DB still has it. The ledger's BlockHdr would fall back to the
// history itself, so this reads the block DB directly.
func (ht *historyTracker) checkHeader(l ledgerForTracker, rnd basics.Round) error {
	var hdr bookkeeping.BlockHeader
	rdb := l.blockDB().Rdb
	err := rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		hdr, err = blockdb.BlockGetHdr(tx, rnd)
		return err
	})
	var noEntry ledgercore.ErrNoEntry
	if errors.As(err, &noEntry) {
		return nil
	}
	if err != nil {
		return err
	}
	return ht.sameHeader(rnd, hdr)
}

func (ht *historyTracker) sameHeader(rnd basics.Round, hdr bookkeeping.BlockHeader) error {
	stored, ok, err := ht.store.Header(uint64(rnd))
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(stored, protocol.Encode(&hdr)) {
		return fmt.Errorf("time travel store %s has another header for round %d than the blocks: it is from another chain, or corrupt", ht.path, rnd)
	}
	return nil
}

// loadDiverged marks the history untrusted if the store says it diverged.
func (ht *historyTracker) loadDiverged() error {
	_, err := ht.loadDivergedMark()
	return err
}

// loadDivergedMark is loadDiverged, and says whether the store is marked.
func (ht *historyTracker) loadDivergedMark() (bool, error) {
	reason, found, err := ht.store.Diverged()
	if err != nil || !found {
		return false, err
	}
	ht.mu.Lock()
	ht.untrusted = fmt.Errorf("%w: %s", ErrHistoryUntrusted, reason)
	ht.mu.Unlock()
	ht.log.Errorf("time travel store %s is marked as differing from the chain (%s): the history is not served until the store is replaced", ht.path, reason)
	return true, nil
}

// verifyReplayed checks a round the store already holds, which the ledger
// replays, against its evaluation. That is a round the node wrote itself
// before a restart (the store is synced before the tracker DB commits), or a
// round of an installed snapshot that the ledger syncs without a catchpoint
// catchup (foreignThrough tells them apart). A difference never fails the
// round: the trackers keep committing.
//
// A difference in an installed round marks the store diverged, durably: the
// snapshot is not the chain's, and its history is not served. In a round the
// node wrote, it can't be an attack, but a bug or a change of build: the
// node's current evaluation is what the tracker DB commits, so the history
// follows it. verifyReplayed deletes the history from that round on
// (repair), and the ledger writes it and the later rounds again as it
// replays them. A round that still can't be read after a retry, or a repair
// that fails, holds the history back until the ledger loads again (a
// restart), which does not check that round again: the tracker DB commits
// past it.
func (ht *historyTracker) verifyReplayed(r historyRound) {
	err := verifyStored(ht.store, r)
	if err != nil && !errors.Is(err, timetravel.ErrDiverged) && !errors.Is(err, timetravel.ErrClosed) {
		time.Sleep(verifyRetryDelay)
		err = verifyStored(ht.store, r)
	}
	if err == nil || errors.Is(err, timetravel.ErrClosed) {
		return
	}
	installed := r.rnd <= ht.foreignThrough
	if errors.Is(err, timetravel.ErrDiverged) && !installed && !ht.repairFailed {
		rerr := ht.repair(r.rnd, err)
		if rerr == nil {
			return
		}
		// one failed repair per load: a later one would likely fail the
		// same way, after another scan of the whole history
		ht.repairFailed = true
		if errors.Is(rerr, timetravel.ErrCorrupt) {
			// the truncation marked the store diverged, if it got that far
			marked, lerr := ht.loadDivergedMark()
			if lerr != nil {
				ht.log.Errorf("time travel: reading store %s's diverged mark: %v", ht.path, lerr)
			}
			if marked {
				return
			}
		}
		err = fmt.Errorf("%w; deleting the history from that round failed: %v", err, rerr)
	}
	ht.mu.Lock()
	first := !ht.replayFailed
	ht.replayFailed = true
	switch {
	case !first:
	case !errors.Is(err, timetravel.ErrDiverged):
		ht.untrusted = fmt.Errorf("%w: round %d could not be verified: %v", ErrHistoryUntrusted, r.rnd, err)
	case installed:
		ht.untrusted = fmt.Errorf("%w: %v", ErrHistoryUntrusted, err)
	default:
		ht.untrusted = fmt.Errorf("%w: the node's own round %d differs from its evaluation: %v", ErrHistoryUntrusted, r.rnd, err)
	}
	ht.mu.Unlock()
	if !first {
		return
	}
	switch {
	case !errors.Is(err, timetravel.ErrDiverged):
		ht.log.Errorf("time travel: could not verify stored round %d, not serving the history until the ledger loads again (a restart), which won't check this round again: %v", r.rnd, err)
	case !installed:
		ht.log.Errorf("time travel store %s: %v: not serving the history until the ledger loads again (a restart), which won't check this round again, so the history may then answer otherwise than the ledger for it", ht.path, err)
	default:
		ht.log.Errorf("time travel store %s differs from the chain (%v): the history is not served until the store is replaced", ht.path, err)
		if merr := ht.store.MarkDiverged(err.Error()); merr != nil {
			ht.log.Errorf("time travel: could not mark store %s as diverged, not serving the history until algod restarts: %v", ht.path, merr)
			ht.mu.Lock()
			ht.sticky = ht.untrusted
			ht.mu.Unlock()
		}
	}
}

// repair deletes the history from round rnd on, which the node wrote before
// a restart and now evaluates otherwise (cause), so that the ledger writes it
// and the later rounds again from its evaluation. It reads the whole history,
// minutes on mainnet, while the ledger replays at startup.
func (ht *historyTracker) repair(rnd basics.Round, cause error) error {
	ht.log.Errorf("time travel store %s holds round %d, which this node wrote before a restart, other than the node evaluates it now (%v): a bug, or a change of build since; deleting the history from that round on, which the ledger writes again as it replays", ht.path, rnd, cause)
	start := time.Now()
	var mu deadlock.Mutex
	logged := start
	deleted, err := ht.store.TruncateAfter(context.Background(), uint64(rnd-1), timetravel.TruncateOptions{
		Progress: func(scanned, deleted uint64) {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(logged) >= 30*time.Second {
				logged = time.Now()
				ht.log.Infof("time travel: deleting the history from round %d: %d records scanned, %d deleted", rnd, scanned, deleted)
			}
		},
	})
	if err != nil {
		ht.log.Errorf("time travel: deleting store %s's history from round %d: %v", ht.path, rnd, err)
		return err
	}
	ht.log.Infof("time travel: deleted %d records of the history from round %d in %s", deleted, rnd, time.Since(start).Round(time.Second))
	return nil
}

// verifyStored checks a replayed round against the store; tests make it fail.
var verifyStored = func(st *timetravel.Store, r historyRound) error {
	return st.Verify(uint64(r.rnd), r.changes, r.hdr, r.expired)
}

// verifyRetryDelay is the wait before verifyReplayed reads a round again
// after a read error.
var verifyRetryDelay = 100 * time.Millisecond

// diverged returns an ErrHistoryUntrusted error if the store is marked
// diverged. Unlike untrusted, it ignores what a reload clears.
func (ht *historyTracker) diverged() error {
	reason, found, err := ht.store.Diverged()
	if err != nil || !found {
		return err
	}
	return fmt.Errorf("%w: %s", ErrHistoryUntrusted, reason)
}

// covers checks that the store reaches round b from genesis.
func (ht *historyTracker) covers(b basics.Round) error {
	last, ok := ht.store.Last()
	if !ok {
		return fmt.Errorf("%w: the time travel store %s is empty; install a history snapshot first (tools/ttsnap)", ErrHistoryTooShort, ht.path)
	}
	if basics.Round(last) < b {
		return fmt.Errorf("%w: the history ends at round %d and the catchpoint needs round %d; install a newer history snapshot or use an older catchpoint", ErrHistoryTooShort, last, b)
	}
	return ht.checkStart()
}

// HistoryUntrusted returns an ErrHistoryUntrusted error if the time travel
// history was found to differ from the chain, nil otherwise.
func (l *Ledger) HistoryUntrusted() error {
	if l.history == nil {
		return nil
	}
	l.history.mu.Lock()
	defer l.history.mu.Unlock()
	return l.history.untrusted
}

// CheckHistoryForCatchpoint is a quick check, before a catchpoint catchup
// starts, that the time travel history reaches the catchpoint's balances
// round. It takes the lookback of the latest block's protocol; the catchup
// checks the exact balances round, and more, before it completes.
func (l *Ledger) CheckHistoryForCatchpoint(catchpointRound basics.Round) error {
	if l.history == nil {
		return nil
	}
	if err := l.history.diverged(); err != nil {
		return err
	}
	hdr, err := l.BlockHdr(l.Latest())
	if err != nil {
		return err
	}
	b := catchpointRound.SubSaturate(catchpointLookback(hdr.CurrentProtocol))
	// the catchup would delete the history after b, which the ledger serves
	if l.Latest() > b {
		return fmt.Errorf("the ledger is at round %d, past the catchpoint's balances round %d: the catchup would delete history it serves", l.Latest(), b)
	}
	return l.history.covers(b)
}

func catchpointLookback(proto protocol.ConsensusVersion) basics.Round {
	return ledgercore.CatchpointLookback(config.Consensus[proto])
}

// checkCatchpoint checks the history against a catchpoint about to be
// applied: it must reach the balances round b from genesis, hold the
// catchpoint's header of b, which the catchup authenticated through the hash
// chain from the labelled block, and the catchpoint's totals at b.
func (ht *historyTracker) checkCatchpoint(b basics.Round, hdr bookkeeping.BlockHeader, totals ledgercore.AccountTotals) error {
	// a failure that a reload clears does not count: the catchup reloads
	if err := ht.diverged(); err != nil {
		return err
	}
	if err := ht.covers(b); err != nil {
		return err
	}
	if err := ht.sameHeader(b, hdr); err != nil {
		return err
	}
	stored, ok, err := ht.store.Get(timetravel.TotalsKey, uint64(b))
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(stored, protocol.Encode(&totals)) {
		return fmt.Errorf("time travel store %s has other totals at round %d than the catchpoint", ht.path, b)
	}
	return nil
}

// PrepareCatchpointHistory checks the time travel history against the staged
// catchpoint (checkCatchpoint), then deletes its rounds after the balances
// round, which takes minutes on mainnet. CompleteCatchup does it too, at no
// cost once done; the catchpoint service runs it first, with a context that a
// shutdown ends but an abort does not, since a truncation cut short leaves the
// store refusing rounds until it is opened again. When ctx ends during the
// truncation, it returns an ErrHistoryTruncationInterrupted error.
func (c *catchpointCatchupAccessorImpl) PrepareCatchpointHistory(ctx context.Context) error {
	ht := c.ledger.history
	if ht == nil {
		return nil
	}
	err := c.prepareHistory(ctx, ht)
	if err != nil && ctx.Err() != nil && !errors.Is(err, timetravel.ErrCorrupt) {
		// whatever failed, the catchup can resume
		err = fmt.Errorf("%w: %w", ErrHistoryTruncationInterrupted, err)
	}
	return err
}

func (c *catchpointCatchupAccessorImpl) prepareHistory(ctx context.Context, ht *historyTracker) error {
	rnd, err := c.catchpointStore.ReadCatchpointStateUint64(ctx, trackerdb.CatchpointStateCatchupBalancesRound)
	if err != nil {
		return err
	}
	b := basics.Round(rnd)
	// truncating after b would cut the history of rounds the ledger serves,
	// and of a ledger the catchup leaves as it is if it aborts
	if latest := c.ledger.Latest(); latest > b {
		return fmt.Errorf("the ledger is at round %d, past the catchpoint's balances round %d", latest, b)
	}
	var totals ledgercore.AccountTotals
	err = c.ledger.trackerDB().Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
		ar, rerr := tx.MakeAccountsReader()
		if rerr != nil {
			return rerr
		}
		totals, rerr = ar.AccountsTotals(ctx, true)
		return rerr
	})
	if err != nil {
		return err
	}
	var hdr bookkeeping.BlockHeader
	rdb := c.ledger.blockDB().Rdb
	err = rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		hdr, err = blockdb.BlockGetStagingHdr(tx, b)
		return err
	})
	if err != nil {
		return fmt.Errorf("reading the catchpoint's header of round %d: %w", b, err)
	}
	if err := ht.checkCatchpoint(b, hdr, totals); err != nil {
		return err
	}
	if beforeHistoryTruncate != nil {
		beforeHistoryTruncate()
	}
	return ht.truncate(ctx, b)
}

// beforeHistoryTruncate is called before prepareHistory truncates the
// history; tests set it.
var beforeHistoryTruncate func()

// truncate deletes the history's records of the rounds after the
// catchpoint's balances round b, which the ledger then writes from its own
// evaluation as it replays them. Replaying and comparing them instead would
// miss records of objects a round did not change, which a store from
// elsewhere can hold. It reads the whole history, minutes on mainnet, unless
// it truncated it after b already. A history that no node writes is marked
// diverged.
func (ht *historyTracker) truncate(ctx context.Context, b basics.Round) error {
	if after, ok := ht.store.TruncatedAfter(); ok && after == uint64(b) {
		return nil // by this process: an earlier step, or Open finishing it
	}
	start := time.Now()
	ht.log.Infof("time travel: deleting the history's rounds after %d, which the ledger writes again as it replays them", b)
	var mu deadlock.Mutex
	logged := start
	deleted, err := ht.store.TruncateAfter(ctx, uint64(b), timetravel.TruncateOptions{
		Progress: func(scanned, deleted uint64) {
			mu.Lock()
			defer mu.Unlock()
			if time.Since(logged) >= 30*time.Second {
				logged = time.Now()
				ht.log.Infof("time travel: truncating the history after round %d: %d records scanned, %d deleted", b, scanned, deleted)
			}
		},
	})
	if err != nil {
		if errors.Is(err, timetravel.ErrCorrupt) {
			err = errors.Join(err, ht.loadDiverged())
		}
		return fmt.Errorf("deleting the time travel history after round %d: %w", b, err)
	}
	ht.log.Infof("time travel: deleted %d records of the history after round %d in %s", deleted, b, time.Since(start).Round(time.Second))
	return nil
}
