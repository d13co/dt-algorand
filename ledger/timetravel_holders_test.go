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
	"encoding/binary"
	"errors"
	"math/rand"
	"slices"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// holdersHistory builds random holdings of three assets: opt-ins (zero
// balances), transfers, freezes, opt-outs, opt-outs and opt-ins again within a
// round, and the second asset's destruction at round destroyAt. The rounds
// after commitAt are in memory only.
func holdersHistory(t *testing.T, dl *DoubleLedger, addrs []basics.Address, seed int64, rounds int) (assets []basics.AssetIndex, destroyedAt basics.Round) {
	l := dl.validator
	creators := addrs[:3]
	assets = []basics.AssetIndex{
		dl.txn(&txntest.Txn{Type: "acfg", Sender: creators[0], AssetParams: basics.AssetParams{Total: 1e9, UnitName: "a", Manager: creators[0], Freeze: creators[0]}}).ApplyData.ConfigAsset,
		dl.txn(&txntest.Txn{Type: "acfg", Sender: creators[1], AssetParams: basics.AssetParams{Total: 1000, UnitName: "b", Manager: creators[1]}}).ApplyData.ConfigAsset,
		dl.txn(&txntest.Txn{Type: "acfg", Sender: creators[2], AssetParams: basics.AssetParams{Total: 1e6, UnitName: "c", Manager: creators[2]}}).ApplyData.ConfigAsset,
	}
	holders := addrs[3:]
	type key struct {
		addr  basics.Address
		asset int
	}
	opted, frozen := map[key]bool{}, map[key]bool{}
	rng := rand.New(rand.NewSource(seed))
	destroyed := false
	for i := 0; i < rounds; i++ {
		var txs []*txntest.Txn
		touched := map[key]bool{}
		for range 4 {
			k := key{holders[rng.Intn(len(holders))], rng.Intn(len(assets))}
			if touched[k] || (destroyed && k.asset == 1) {
				continue
			}
			touched[k] = true
			aidx, creator := assets[k.asset], creators[k.asset]
			optIn := &txntest.Txn{Type: "axfer", Sender: k.addr, AssetReceiver: k.addr, XferAsset: aidx}
			closeOut := &txntest.Txn{Type: "axfer", Sender: k.addr, AssetReceiver: creator, AssetCloseTo: creator, XferAsset: aidx}
			switch r := rng.Intn(10); {
			case !opted[k]:
				txs = append(txs, optIn)
				opted[k] = true
			case r < 2: // closing out to the creator works even when frozen
				txs = append(txs, closeOut)
				opted[k], frozen[k] = false, false
			case r < 4: // out and back in within the round: no gap
				txs = append(txs, closeOut, optIn)
				frozen[k] = false
			case frozen[k] && r < 6:
				txs = append(txs, &txntest.Txn{Type: "afrz", Sender: creator, FreezeAccount: k.addr, FreezeAsset: aidx, AssetFrozen: false})
				frozen[k] = false
			case r < 6 && k.asset == 0:
				txs = append(txs, &txntest.Txn{Type: "afrz", Sender: creator, FreezeAccount: k.addr, FreezeAsset: aidx, AssetFrozen: true})
				frozen[k] = true
			case k.asset != 1 && !frozen[k]: // the second asset stays with its creator, so that it can be destroyed
				txs = append(txs, &txntest.Txn{Type: "axfer", Sender: creator, AssetReceiver: k.addr, XferAsset: aidx, AssetAmount: uint64(1 + rng.Intn(100))})
			}
		}
		if i == rounds*2/3 { // the holders of the second asset keep their holdings
			txs = append(txs, &txntest.Txn{Type: "acfg", Sender: creators[1], ConfigAsset: assets[1]})
			destroyed, destroyedAt = true, l.Latest()+1
		}
		for j := range txs { // identical transactions recur
			tx := *txs[j]
			tx.Note = []byte{byte(i), byte(j)}
			txs[j] = &tx
		}
		dl.fullBlock(txs...)
		if i == rounds/2 {
			commitRoundLookback(4, l)
		}
	}
	return assets, destroyedAt
}

// ttHolders is the oracle: the holders of aidx at rnd by looking up every
// account's holding, ok false if the asset does not exist.
func ttHolders(t *testing.T, l *Ledger, addrs []basics.Address, rnd basics.Round, aidx basics.AssetIndex) ([]AssetHolder, bool) {
	_, ok, err := l.GetCreatorForRound(rnd, basics.CreatableIndex(aidx), basics.AssetCreatable)
	require.NoError(t, err)
	if !ok {
		return nil, false
	}
	var out []AssetHolder
	for _, addr := range addrs {
		res, err := l.LookupAsset(rnd, addr, aidx)
		require.NoError(t, err)
		if res.AssetHolding != nil {
			out = append(out, AssetHolder{Address: addr, Holding: *res.AssetHolding})
		}
	}
	slices.SortFunc(out, func(a, b AssetHolder) int { return bytes.Compare(a.Address[:], b.Address[:]) })
	return out, true
}

// holdersPages concatenates the pages of a listing.
func holdersPages(t *testing.T, lookup func(after *basics.Address, limit uint64) (AssetHoldersPage, error), limit uint64) []AssetHolder {
	var all []AssetHolder
	var after *basics.Address
	for pages := 0; ; pages++ {
		require.Less(t, pages, 1000, "pages make progress")
		p, err := lookup(after, limit)
		require.NoError(t, err)
		require.LessOrEqual(t, uint64(len(p.Holders)), limit)
		all = append(all, p.Holders...)
		if !p.More {
			return all
		}
		if after != nil {
			require.Positive(t, bytes.Compare(p.Next[:], after[:]))
		}
		if len(p.Holders) > 0 {
			last := p.Holders[len(p.Holders)-1].Address
			require.GreaterOrEqual(t, bytes.Compare(p.Next[:], last[:]), 0, "Next is at or after the last holder")
		}
		next := p.Next
		after = &next
	}
}

// collectHolders is holdersPages for goroutines: it returns errors instead of
// failing the test.
func collectHolders(lookup func(after *basics.Address, limit uint64) (AssetHoldersPage, error), limit uint64) ([]AssetHolder, error) {
	var all []AssetHolder
	var after *basics.Address
	for range 1000 {
		p, err := lookup(after, limit)
		if err != nil {
			return nil, err
		}
		all = append(all, p.Holders...)
		if !p.More {
			return all, nil
		}
		next := p.Next
		after = &next
	}
	return nil, errors.New("the pages make no progress")
}

func TestTimeTravelAssetHolders(t *testing.T) {
	partitiontest.PartitionTest(t)
	// not parallel: it sets holdersExamineCap and holdersBatch

	defer func(cap, batch int) { holdersExamineCap, holdersBatch = cap, batch }(holdersExamineCap, holdersBatch)
	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival())
	defer dl.Close()
	l := dl.validator
	_, err := l.history.store.IndexHolders(context.Background(), nil)
	require.NoError(t, err)

	assets, destroyedAt := holdersHistory(t, &dl, addrs, 1, 40)
	check := func() map[basics.Round]map[basics.AssetIndex][]AssetHolder {
		all := map[basics.Round]map[basics.AssetIndex][]AssetHolder{}
		for rnd := basics.Round(0); rnd <= l.Latest(); rnd++ {
			all[rnd] = map[basics.AssetIndex][]AssetHolder{}
			for _, aidx := range append(assets, 999_999) {
				want, exists := ttHolders(t, l, addrs, rnd, aidx)
				lookup := func(after *basics.Address, limit uint64) (AssetHoldersPage, error) {
					return l.AssetHoldersAt(rnd, aidx, after, limit)
				}
				if !exists {
					_, err := lookup(nil, 10)
					require.ErrorIs(t, err, ErrAssetNotFound, "round %d asset %d", rnd, aidx)
					if aidx == assets[1] && rnd >= destroyedAt {
						require.NotEmpty(t, func() []AssetHolder {
							var held []AssetHolder
							for _, addr := range addrs[3:] {
								if res, _ := l.LookupAsset(rnd, addr, aidx); res.AssetHolding != nil {
									held = append(held, AssetHolder{Address: addr})
								}
							}
							return held
						}(), "holdings of a destroyed asset remain")
					}
					continue
				}
				all[rnd][aidx] = want
				for _, cap := range []int{1, 2, 5, 100_000} {
					for _, batch := range []int{1, 3, 1024} {
						holdersExamineCap, holdersBatch = cap, batch
						for _, limit := range []uint64{1, 2, 3, 1000} {
							require.Equal(t, want, holdersPages(t, lookup, limit), "round %d asset %d cap %d batch %d limit %d", rnd, aidx, cap, batch, limit)
						}
					}
				}
				holdersExamineCap, holdersBatch = 100_000, 1024
				if len(want) > 0 {
					// a page that ends exactly at the last holder says there is no more
					p, err := lookup(nil, uint64(len(want)))
					require.NoError(t, err)
					require.False(t, p.More)
					require.Equal(t, want, p.Holders)
					if len(want) > 1 {
						// one short: the next page starts at the last holder
						p, err = lookup(nil, uint64(len(want)-1))
						require.NoError(t, err)
						require.True(t, p.More)
						require.GreaterOrEqual(t, bytes.Compare(p.Next[:], want[len(want)-2].Address[:]), 0)
						require.Negative(t, bytes.Compare(p.Next[:], want[len(want)-1].Address[:]))
					}
				}
			}
		}
		return all
	}

	last, _ := l.history.store.Last()
	require.Less(t, basics.Round(last), l.Latest()-10, "the last rounds are in memory only")
	want := check()
	var zero, frozen, gone bool
	for _, byAsset := range want {
		for _, hs := range byAsset {
			for _, h := range hs {
				zero = zero || h.Holding.Amount == 0
				frozen = frozen || h.Holding.Frozen
			}
		}
	}
	for rnd := basics.Round(1); rnd <= l.Latest(); rnd++ {
		for _, aidx := range []basics.AssetIndex{assets[0], assets[2]} {
			for _, h := range want[rnd-1][aidx] {
				gone = gone || !slices.ContainsFunc(want[rnd][aidx], func(x AssetHolder) bool { return x.Address == h.Address })
			}
		}
	}
	require.True(t, zero && frozen && gone, "the history has zero balances, frozen holdings and opt-outs")
	_, err = l.AssetHoldersAt(l.Latest()+1, assets[0], nil, 10)
	require.ErrorIs(t, err, errHistoryUnavailable)
	_, err = l.AssetHoldersAt(l.Latest(), assets[0], nil, 0)
	require.Error(t, err)

	for range 8 {
		dl.fullBlock()
	}
	commitRoundLookback(4, l)
	last, _ = l.history.store.Last()
	require.Greater(t, basics.Round(last), l.Latest()-10, "the rounds reached the store")
	flushed := check()
	for rnd, byAsset := range want {
		require.Equal(t, byAsset, flushed[rnd], "round %d answers the same once flushed", rnd)
	}

	// pages read while rounds are added and committed answer as before: small
	// pages of one-entry reads and a cap of 3, so that a listing spans many
	// reads of the index while the store changes
	latest := l.Latest()
	holdersExamineCap, holdersBatch = 3, 1
	done := make(chan struct{})
	var wg sync.WaitGroup
	// stop also runs if the test fails meanwhile, before the variables are restored
	var once sync.Once
	stop := func() { once.Do(func() { close(done); wg.Wait() }) }
	defer stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		rng := rand.New(rand.NewSource(2))
		for i := 0; ; i++ {
			select {
			case <-done:
				return
			default:
			}
			rnd := basics.Round(rng.Intn(int(latest) + 1))
			aidx := assets[rng.Intn(len(assets))]
			w, exists := flushed[rnd][aidx]
			got, err := collectHolders(func(after *basics.Address, limit uint64) (AssetHoldersPage, error) {
				return l.AssetHoldersAt(rnd, aidx, after, limit)
			}, uint64(1+rng.Intn(3)))
			if !exists {
				if !errors.Is(err, ErrAssetNotFound) {
					t.Errorf("round %d asset %d: %v", rnd, aidx, err)
					return
				}
				continue
			}
			if err != nil || !slices.Equal(w, got) {
				t.Errorf("round %d asset %d: %v %v, want %v", rnd, aidx, err, got, w)
				return
			}
		}
	}()
	more, _ := holdersHistory(t, &dl, addrs, 3, 20)
	stop()
	holdersExamineCap, holdersBatch = 100_000, 1024
	commitRoundLookback(0, l)
	for _, aidx := range more {
		w, exists := ttHolders(t, l, addrs, l.Latest(), aidx)
		if !exists { // destroyed
			continue
		}
		got := holdersPages(t, func(after *basics.Address, limit uint64) (AssetHoldersPage, error) {
			return l.AssetHoldersAt(l.Latest(), aidx, after, limit)
		}, 2)
		require.Equal(t, w, got)
	}

	// a reader over the store of the closed ledger answers the same
	path := l.history.path
	dl.Close()
	s, err := timetravel.Open(path, timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer s.Close()
	require.Equal(t, timetravel.HoldersComplete, s.HoldersIndexState())
	r, err := NewHistoryReader(s)
	require.NoError(t, err)
	for rnd, byAsset := range flushed {
		for aidx, w := range byAsset {
			p, err := r.AssetHoldersAt(rnd, aidx, nil, 1000)
			require.NoError(t, err)
			require.Equal(t, w, p.Holders, "reader round %d asset %d", rnd, aidx)
		}
	}
	_, err = r.AssetHoldersAt(r.Last()+1, assets[0], nil, 10)
	require.ErrorIs(t, err, ErrRoundOutOfRange)
}

// TestTimeTravelInitHoldersIndex checks what the node's launch does to the
// index: with the option off it deletes it, on it warns until it is usable.
func TestTimeTravelInitHoldersIndex(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	// the ledgers' goroutines log too: a locked buffer, from the start
	var logged logBuffer
	log := logging.NewLogger()
	log.SetOutput(&logged)
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival(), simpleLedgerLogger(log))
	defer dl.Close()
	l := dl.validator
	s := l.history.store

	require.NoError(t, l.InitHoldersIndex(true))
	require.Contains(t, logged.String(), "the asset holders index is missing")
	require.Contains(t, logged.String(), "ttimport -holders-index")
	_, err := s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	logged.Reset()
	require.NoError(t, l.InitHoldersIndex(true))
	require.NotContains(t, logged.String(), "holders", "nothing to say about a complete index")

	// a ledger loaded without the call, as ttimport loads it, keeps a
	// complete index up to date whatever its options
	assets, _ := holdersHistory(t, &dl, addrs, 4, 12)
	commitRoundLookback(0, l)
	require.Equal(t, timetravel.HoldersComplete, l.HoldersIndexState())
	for _, aidx := range assets[:1] {
		w, _ := ttHolders(t, l, addrs, l.Latest(), aidx)
		require.NotEmpty(t, w)
		p, err := l.AssetHoldersAt(l.Latest(), aidx, nil, 1000)
		require.NoError(t, err)
		require.Equal(t, w, p.Holders)
	}

	require.NoError(t, l.InitHoldersIndex(false))
	require.Contains(t, logged.String(), "deleted the asset holders index: TimeTravelHoldersIndex is off")
	st := l.HoldersIndexState()
	require.Equal(t, timetravel.HoldersNone, st)
	_, err = l.AssetHoldersAt(l.Latest(), assets[0], nil, 10)
	require.ErrorIs(t, err, ErrNoHoldersIndex)
	require.ErrorContains(t, err, "the asset holders index is missing")
	logged.Reset()
	require.NoError(t, l.InitHoldersIndex(false))
	require.NotContains(t, logged.String(), "holders", "nothing left to delete")

	t.Run("without time travel", func(t *testing.T) {
		// nothing to do
		dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, config.GetDefaultLocal())
		defer dl.Close()
		require.NoError(t, dl.validator.InitHoldersIndex(false))
		require.NoError(t, dl.validator.InitHoldersIndex(true))
		require.Equal(t, timetravel.HoldersNone, dl.validator.HoldersIndexState())
	})
}

// logBuffer collects a ledger's log, which its goroutines write too.
type logBuffer struct {
	mu deadlock.Mutex
	b  bytes.Buffer
}

func (b *logBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *logBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func (b *logBuffer) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.b.Reset()
}

// TestTimeTravelHoldersIndexReopen checks the index across ledger reopens. A
// ledger loaded without InitHoldersIndex, as ttimport loads it, keeps a
// complete index complete and up to date. A stale one gets the warning, the
// rounds added leave it alone, and the option off deletes it.
func TestTimeTravelHoldersIndexReopen(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerOnDisk(), simpleLedgerNotArchival())
	defer func() { dl.Close() }() // Close has a value receiver: close the validator reopen left
	_, err := dl.validator.history.store.IndexHolders(context.Background(), nil)
	require.NoError(t, err)

	// reopen closes the validator once the store has its rounds, runs between
	// on the closed ledger's store, and opens the validator again
	var logged logBuffer
	reopen := func(between func(s *timetravel.Store)) *Ledger {
		l := dl.validator
		commitRoundLookback(0, l)
		dir, path, genHash := l.dirsAndPrefix.HotGenesisDir, l.history.path, l.GenesisHash()
		l.Close()
		if between != nil {
			s, err := timetravel.Open(path, timetravel.Options{})
			require.NoError(t, err)
			between(s)
			require.NoError(t, s.Close())
		}
		genBlock, err := bookkeeping.MakeGenesisBlock(protocol.ConsensusFuture, genBalances, "test", genHash)
		require.NoError(t, err)
		log := logging.NewLogger()
		log.SetOutput(&logged)
		c := cfg
		c.Archival = false
		l, err = OpenLedger(log, dir, false, ledgercore.InitState{Block: genBlock, Accounts: genBalances.Balances, GenesisHash: genHash}, c)
		require.NoError(t, err)
		dl.validator = l
		return l
	}
	// check compares the holders of the assets at every round with the oracle
	check := func(l *Ledger, assets []basics.AssetIndex) {
		for rnd := basics.Round(1); rnd <= l.Latest(); rnd++ {
			for _, aidx := range assets {
				w, exists := ttHolders(t, l, addrs, rnd, aidx)
				if !exists {
					continue
				}
				p, err := l.AssetHoldersAt(rnd, aidx, nil, 1000)
				require.NoError(t, err)
				require.Equal(t, w, p.Holders, "round %d asset %d", rnd, aidx)
			}
		}
	}

	assets, _ := holdersHistory(t, &dl, addrs, 6, 12)
	l := reopen(nil)
	st := l.HoldersIndexState()
	require.Equal(t, timetravel.HoldersComplete, st, "loading a ledger leaves a complete index alone")
	check(l, assets)
	more, _ := holdersHistory(t, &dl, addrs, 7, 12)
	commitRoundLookback(0, l)
	st = l.HoldersIndexState()
	require.Equal(t, timetravel.HoldersComplete, st)
	check(l, append(assets, more...))

	// as a build without the index leaves it once it adds rounds
	meta := binary.BigEndian.AppendUint64(nil, 1)
	l = reopen(func(s *timetravel.Store) {
		require.Equal(t, timetravel.HoldersComplete, s.HoldersIndexState())
		require.NoError(t, s.DB().Set([]byte("mh"), meta, pebble.Sync))
	})
	st = l.HoldersIndexState()
	require.Equal(t, timetravel.HoldersStale, st)
	logged.Reset()
	require.NoError(t, l.InitHoldersIndex(true))
	require.Contains(t, logged.String(), "the asset holders index is stale")
	require.Contains(t, logged.String(), "ttimport -holders-index")
	_, err = l.AssetHoldersAt(l.Latest(), assets[0], nil, 10)
	require.ErrorIs(t, err, ErrNoHoldersIndex)
	require.ErrorContains(t, err, "the asset holders index is stale")
	entries := holdersEntries(t, l.history.store)
	require.NotEmpty(t, entries)
	holdersHistory(t, &dl, addrs, 8, 6)
	commitRoundLookback(0, l)
	require.Equal(t, entries, holdersEntries(t, l.history.store), "the rounds added leave a stale index alone")
	v, closer, err := l.history.store.DB().Get([]byte("mh"))
	require.NoError(t, err)
	require.Equal(t, meta, v)
	require.NoError(t, closer.Close())

	logged.Reset()
	require.NoError(t, l.InitHoldersIndex(false))
	require.Contains(t, logged.String(), "deleted the asset holders index: TimeTravelHoldersIndex is off")
	st = l.HoldersIndexState()
	require.Equal(t, timetravel.HoldersNone, st)
	require.Empty(t, holdersEntries(t, l.history.store))
	_, _, err = l.history.store.DB().Get([]byte("mh"))
	require.ErrorIs(t, err, pebble.ErrNotFound)
}

// holdersEntries returns the keys and values of the store's asset holders
// index entries (the 'a' keyspace).
func holdersEntries(t *testing.T, s *timetravel.Store) [][2][]byte {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{'a'}, UpperBound: []byte{'b'}})
	require.NoError(t, err)
	var out [][2][]byte
	for it.First(); it.Valid(); it.Next() {
		out = append(out, [2][]byte{slices.Clone(it.Key()), slices.Clone(it.Value())})
	}
	require.NoError(t, it.Close())
	return out
}

// TestTimeTravelAssetHoldersFirstRound checks the holders on a store that
// starts after genesis: before its first round nothing exists, and a
// HistoryReader refuses the round.
func TestTimeTravelAssetHoldersFirstRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := timetravel.Open(t.TempDir(), timetravel.Options{})
	require.NoError(t, err)
	defer s.Close()
	const aidx = basics.AssetIndex(7)
	a, b := basics.Address{1}, basics.Address{2}
	holding := func(addr basics.Address, amount uint64) timetravel.Change {
		return timetravel.Change{Key: timetravel.HoldingKey(addr, aidx), Value: protocol.Encode(&basics.AssetHolding{Amount: amount})}
	}
	params := timetravel.Change{Key: timetravel.AssetParamsKey(aidx), Value: append(a[:], protocol.Encode(&basics.AssetParams{Total: 10})...)}
	rounds := map[uint64][]timetravel.Change{
		5: {params, holding(a, 10), holding(b, 0)},
		6: {holding(a, 7), holding(b, 3)},
		7: {{Key: timetravel.HoldingKey(b, aidx), Deleted: true}},
		8: {holding(b, 0)},
	}
	// built on the empty store, the index is maintained from its first round
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	for r := uint64(5); r <= 8; r++ {
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r), UpgradeState: bookkeeping.UpgradeState{CurrentProtocol: protocol.ConsensusFuture}})
		require.NoError(t, s.AddRound(r, rounds[r], hdr, nil))
	}
	mh, closer, err := s.DB().Get([]byte("mh"))
	require.NoError(t, err)
	require.Equal(t, binary.BigEndian.AppendUint64(nil, 9), mh, "the index covers the rounds added")
	require.NoError(t, closer.Close())
	entries := holdersEntries(t, s)
	require.Len(t, entries, 2)
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, entries, holdersEntries(t, s), "maintained as rebuilt")

	// the view has no record before the store's first round; only a
	// HistoryReader knows the round is out of range
	h := &histView{store: s, last: 8}
	_, err = histAssetHolders(h, 4, aidx, nil, 10)
	require.ErrorIs(t, err, ErrAssetNotFound)
	r, err := NewHistoryReader(s)
	require.NoError(t, err)
	require.EqualValues(t, 5, r.First())
	_, err = r.AssetHoldersAt(4, aidx, nil, 10)
	require.ErrorIs(t, err, ErrRoundOutOfRange)

	want := map[basics.Round][]AssetHolder{
		5: {{a, basics.AssetHolding{Amount: 10}}, {b, basics.AssetHolding{}}},
		6: {{a, basics.AssetHolding{Amount: 7}}, {b, basics.AssetHolding{Amount: 3}}},
		7: {{a, basics.AssetHolding{Amount: 7}}},
		8: {{a, basics.AssetHolding{Amount: 7}}, {b, basics.AssetHolding{}}},
	}
	for rnd, w := range want {
		p, err := histAssetHolders(h, rnd, aidx, nil, 10)
		require.NoError(t, err)
		require.Equal(t, w, p.Holders, "round %d", rnd)
		p, err = r.AssetHoldersAt(rnd, aidx, nil, 10)
		require.NoError(t, err)
		require.Equal(t, w, p.Holders, "reader round %d", rnd)
	}
}

// TestTimeTravelHoldersIndexFromGenesis checks that a ledger starting its
// store from genesis with TimeTravelHoldersIndex, as a node syncing from
// genesis does, has the index from the genesis round (its holdings included)
// and keeps it up to date, with no ttimport -holders-index; and that it has
// none with the option off.
func TestTimeTravelHoldersIndexFromGenesis(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	// an asset that exists at genesis, held by its creator and another account
	const genAsset = basics.AssetIndex(1 << 40)
	creator := genBalances.Balances[addrs[0]]
	creator.AssetParams = map[basics.AssetIndex]basics.AssetParams{genAsset: {Total: 100, UnitName: "g"}}
	creator.Assets = map[basics.AssetIndex]basics.AssetHolding{genAsset: {Amount: 60}}
	genBalances.Balances[addrs[0]] = creator
	holder := genBalances.Balances[addrs[9]]
	holder.Assets = map[basics.AssetIndex]basics.AssetHolding{genAsset: {Amount: 40, Frozen: true}}
	genBalances.Balances[addrs[9]] = holder

	newCfg := func(on bool) config.Local {
		cfg := config.GetDefaultLocal()
		cfg.EnableTimeTravel = true
		cfg.TimeTravelHoldersIndex = on
		cfg.MaxAcctLookback = 4
		return cfg
	}
	// check compares the holders of the assets at every round with the oracle
	check := func(l *Ledger, assets []basics.AssetIndex) {
		for rnd := basics.Round(0); rnd <= l.Latest(); rnd++ {
			for _, aidx := range assets {
				w, exists := ttHolders(t, l, addrs, rnd, aidx)
				if !exists {
					continue
				}
				p, err := l.AssetHoldersAt(rnd, aidx, nil, 1000)
				require.NoError(t, err)
				require.Equal(t, w, p.Holders, "round %d asset %d", rnd, aidx)
			}
		}
	}

	t.Run("on", func(t *testing.T) {
		t.Parallel()
		var logged logBuffer
		log := logging.NewLogger()
		log.SetOutput(&logged)
		dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, newCfg(true), simpleLedgerNotArchival(), simpleLedgerLogger(log))
		defer dl.Close()
		l := dl.validator
		st := l.HoldersIndexState()
		require.Equal(t, timetravel.HoldersComplete, st)
		require.Contains(t, logged.String(), "indexing asset holders from the genesis round")
		p, err := l.AssetHoldersAt(0, genAsset, nil, 10)
		require.NoError(t, err)
		require.Len(t, p.Holders, 2)
		logged.Reset()
		require.NoError(t, l.InitHoldersIndex(true))
		require.NotContains(t, logged.String(), "holders", "the node's launch has nothing to say")

		assets, _ := holdersHistory(t, &dl, addrs, 9, 12)
		commitRoundLookback(0, l)
		st = l.HoldersIndexState()
		require.Equal(t, timetravel.HoldersComplete, st)
		check(l, append(assets, genAsset))
	})

	for name, cfg := range map[string]config.Local{"off": newCfg(false)} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var logged logBuffer
			log := logging.NewLogger()
			log.SetOutput(&logged)
			dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival(), simpleLedgerLogger(log))
			defer dl.Close()
			st := dl.validator.HoldersIndexState()
			require.Equal(t, timetravel.HoldersNone, st)
			require.NotContains(t, logged.String(), "the genesis round", "neither built nor tried")
		})
	}
}
