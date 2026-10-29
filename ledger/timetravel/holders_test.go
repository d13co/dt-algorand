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

package timetravel

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestHolderIntervals(t *testing.T) {
	partitiontest.PartitionTest(t)

	for _, rounds := range [][]uint64{nil, {0}, {0, 1}, {5}, {3, 7, 8, 200, 1 << 40}, {1, 2, 3, 4, 5, 6}} {
		var enc []byte
		var at uint64
		for _, r := range rounds {
			enc = binary.AppendUvarint(enc, r-at)
			at = r
		}
		got, err := HolderIntervals(enc)
		require.NoError(t, err)
		require.Equal(t, rounds, got)
		n, last, err := boundaries(enc)
		require.NoError(t, err)
		require.Equal(t, len(rounds), n)
		if n > 0 {
			require.Equal(t, rounds[n-1], last)
		}
		// held from each creation through the round before its deletion
		held := func(r uint64) bool {
			h := false
			for _, b := range rounds {
				if b <= r {
					h = !h
				}
			}
			return h
		}
		var probes []uint64
		for _, b := range rounds {
			probes = append(probes, b, b+1)
			if b > 0 {
				probes = append(probes, b-1)
			}
		}
		probes = append(probes, 0, ^uint64(0))
		for _, r := range probes {
			require.Equal(t, held(r), HeldAt(enc, r), "rounds %v at %d", rounds, r)
		}
	}
	_, err := HolderIntervals([]byte{0x80})
	require.Error(t, err)
	_, _, err = boundaries([]byte{0x80})
	require.Error(t, err)
	// a delta past the last round overflows: bad, for every decoder
	overflow := binary.AppendUvarint(binary.AppendUvarint(binary.AppendUvarint(nil, 5), 2), ^uint64(0))
	_, err = HolderIntervals(overflow)
	require.Error(t, err)
	_, _, err = boundaries(overflow)
	require.Error(t, err)
	_, err = intervalsThrough(overflow, ^uint64(0))
	require.Error(t, err)
	require.False(t, HeldAt(overflow, ^uint64(0)))
	require.True(t, HeldAt(overflow, 6), "a round before the bad part's")
}

// TestStoreHoldersBadMeta checks that a bad meta record makes the index stale
// rather than fail Open, so that DropHoldersIndex (ttsnap fetch, the node with
// TimeTravelHoldersIndex off) and a truncation can delete it.
func TestStoreHoldersBadMeta(t *testing.T) {
	partitiontest.PartitionTest(t)

	changes, _ := holderRounds(9, 12)
	build := func(t *testing.T) string {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		for r := range changes {
			require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
		}
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		require.NoError(t, s.DB().Set(keyHoldersMeta, []byte{1, 2, 3}, pebble.Sync))
		require.NoError(t, s.Close())
		return dir
	}

	t.Run("drop", func(t *testing.T) {
		s, err := Open(build(t), Options{})
		require.NoError(t, err)
		defer s.Close()
		require.Equal(t, HoldersStale, s.HoldersIndexState())
		err = s.AssetHolders(1, nil, func([]byte, []byte) bool { return true })
		require.ErrorIs(t, err, ErrNoHoldersIndex)
		dropped, err := s.DropHoldersIndex()
		require.NoError(t, err)
		require.True(t, dropped)
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
	})

	t.Run("truncate", func(t *testing.T) {
		s, err := Open(build(t), Options{})
		require.NoError(t, err)
		defer s.Close()
		_, err = s.TruncateAfter(context.Background(), 4, TruncateOptions{})
		require.NoError(t, err)
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
	})

	t.Run("read-only", func(t *testing.T) {
		ro, err := Open(build(t), Options{ReadOnly: true})
		require.NoError(t, err)
		defer ro.Close()
		require.Equal(t, HoldersStale, ro.HoldersIndexState())
	})
}

func TestHoldingOf(t *testing.T) {
	partitiontest.PartitionTest(t)
	addr := basics.Address{1, 2, 3}
	a, asset, ok := HoldingOf(HoldingKey(addr, 1<<56|7))
	require.True(t, ok)
	require.Equal(t, addr, a)
	require.EqualValues(t, 1<<56|7, asset)
	for _, obj := range [][]byte{AccountKey(addr), LocalKey(addr, 7), AssetParamsKey(7), TotalsKey, nil, HoldingKey(addr, 7)[:40]} {
		_, _, ok := HoldingOf(obj)
		require.False(t, ok, "%x", obj)
	}
}

// holderAssets are the assets of holderObjects. Their ids have zero bytes,
// and one sorts after the others by its first byte.
var holderAssets = []uint64{1, 2, 1<<56 | 3}

// holderAddrs are the accounts of holderObjects, the last all 0xff bytes.
func holderAddrs() []basics.Address {
	var out []basics.Address
	for i := 0; i < 5; i++ {
		var a basics.Address
		a[0], a[31] = byte(i*50), byte(i)
		out = append(out, a)
	}
	var ff basics.Address
	for i := range ff {
		ff[i] = 0xff
	}
	return append(out, ff)
}

// holderObjects returns the holdings of holderAddrs, their accounts (whose
// object keys start with 'a' too), a local state and the totals.
func holderObjects() [][]byte {
	objs := [][]byte{TotalsKey, LocalKey(holderAddrs()[0], 1)}
	for _, a := range holderAddrs() {
		objs = append(objs, AccountKey(a))
		for _, asset := range holderAssets {
			objs = append(objs, HoldingKey(a, basics.AssetIndex(asset)))
		}
	}
	return objs
}

// injectedHolders are the accounts whose holdings of asset 1 holderRounds
// changes besides the random ones.
var injectedHolders = []basics.Address{{7}, {8}}

// modelHolders returns the accounts holding asset at round r.
func modelHolders(history []map[string][]byte, r int, asset uint64) []basics.Address {
	all := append(holderAddrs(), injectedHolders...)
	slices.SortFunc(all, func(a, b basics.Address) int { return bytes.Compare(a[:], b[:]) })
	var out []basics.Address
	for _, a := range all {
		if _, ok := history[r][string(HoldingKey(a, basics.AssetIndex(asset)))]; ok {
			out = append(out, a)
		}
	}
	return out
}

// storeHolders returns the accounts holding asset at round r after after, by
// the store's index.
func storeHolders(t *testing.T, s *Store, asset, r uint64, after []byte) []basics.Address {
	var out []basics.Address
	require.NoError(t, s.AssetHolders(asset, after, func(addr, intervals []byte) bool {
		if HeldAt(intervals, r) {
			out = append(out, basics.Address(addr))
		}
		return true
	}))
	return out
}

// checkHolders compares the store's index with the model at every round.
func checkHolders(t *testing.T, s *Store, history []map[string][]byte) {
	for r := range history {
		for _, asset := range holderAssets {
			want := modelHolders(history, r, asset)
			require.Equal(t, want, storeHolders(t, s, asset, uint64(r), nil), "round %d asset %d", r, asset)
			if len(want) > 1 {
				require.Equal(t, want[1:], storeHolders(t, s, asset, uint64(r), want[0][:]), "after the first")
			}
		}
	}
}

// holdersRange returns the holders index entries, key=value in hex.
func holdersRange(t *testing.T, s *Store) []string {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{pfxHolders}, UpperBound: []byte{pfxHolders + 1}})
	require.NoError(t, err)
	defer it.Close()
	var out []string
	for it.First(); it.Valid(); it.Next() {
		out = append(out, hex.EncodeToString(it.Key())+"="+hex.EncodeToString(it.Value()))
	}
	require.NoError(t, it.Error())
	return out
}

// holdersMetaRecord returns the holders index's meta record, nil if absent.
func holdersMetaRecord(t *testing.T, s *Store) []byte {
	v, closer, err := s.DB().Get(keyHoldersMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil
	}
	require.NoError(t, err)
	defer closer.Close()
	return bytes.Clone(v)
}

// requireRebuildEqual checks that rebuilding the index changes none of its
// bytes.
func requireRebuildEqual(t *testing.T, s *Store, msg string) {
	before := holdersRange(t, s)
	_, err := s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, before, holdersRange(t, s), msg)
}

// holderRounds returns random rounds of changes to holderObjects, with
// holdings changed twice in a round, created and deleted within a round,
// deleted and recreated within a round, and deleted before they exist.
func holderRounds(seed int64, rounds int) ([][]Change, []map[string][]byte) {
	changes, history := modelRounds(seed, holderObjects(), rounds)
	inject := func(r int, key []byte, chs ...Change) {
		for i := range chs {
			chs[i].Key = key
		}
		changes[r] = append(changes[r], chs...)
		last := chs[len(chs)-1]
		for q := r; q < rounds; q++ {
			if last.Deleted {
				delete(history[q], string(key))
			} else {
				history[q][string(key)] = last.Value
			}
		}
	}
	set := func(v string) Change { return Change{Value: []byte(v)} }
	del := Change{Deleted: true}
	hx, hy := HoldingKey(injectedHolders[0], 1), HoldingKey(injectedHolders[1], 1)
	inject(2, hy, del)                   // a tombstone first
	inject(3, hx, set("v1"))             // created
	inject(4, hy, set("v"), del)         // created and deleted within a round: never held
	inject(5, hx, del, set("v2"))        // deleted and recreated within a round: held throughout
	inject(7, hx, set("v3"), del)        // deleted
	inject(8, hx, del)                   // deleting what is not there
	inject(9, hx, set("v4"), set("v5"))  // created, changed twice in the round
	inject(10, hx, set("v5"), set("v5")) // no-ops
	return changes, history
}

// TestStoreHoldersIndex checks the index that IndexHolders builds after
// any number of rounds and AddRound then maintains against the model, and
// against a rebuild byte for byte, with and without the dedup cache.
func TestStoreHoldersIndex(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3 // commit while scanning

	const rounds = 60
	changes, history := holderRounds(7, rounds)
	x, y := injectedHolders[0], injectedHolders[1]
	for _, o := range []Options{{}, {DedupEntries: 24}, {DedupEntries: -1}} {
		t.Run(fmt.Sprintf("dedup=%d", o.DedupEntries), func(t *testing.T) {
			var first []string // the index built after 0 rounds
			for _, k := range []int{0, 1, 4, 6, rounds / 3, rounds - 1, rounds} {
				dir := t.TempDir()
				s, err := Open(dir, o)
				require.NoError(t, err)
				for r := 0; r < rounds; r++ {
					if r == k {
						var calls int
						_, err := s.IndexHolders(context.Background(), func(uint64, uint64) { calls++ })
						require.NoError(t, err)
						require.Positive(t, calls)
						require.Equal(t, HoldersComplete, s.HoldersIndexState())
					}
					if r == 2*rounds/3 {
						// Open is neutral, and the reopened store reads the
						// index for the holdings the cache no longer knows;
						// the same options, so dedup=-1 runs without a cache
						require.NoError(t, s.Close())
						s, err = Open(dir, o)
						require.NoError(t, err)
					}
					require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
				}
				if k == rounds {
					_, err := s.IndexHolders(context.Background(), nil)
					require.NoError(t, err)
				}
				require.Equal(t, HoldersComplete, s.HoldersIndexState(), "k=%d", k)
				require.Equal(t, holdersMeta(rounds), holdersMetaRecord(t, s))
				checkHolders(t, s, history)
				// x was held in rounds 3-6 and from 9; y never
				var ivX []byte
				found := false
				require.NoError(t, s.AssetHolders(1, nil, func(addr, intervals []byte) bool {
					switch basics.Address(addr) {
					case x:
						ivX = intervals
					case y:
						found = true
					}
					return true
				}))
				require.False(t, found, "a holding that only had tombstones has no entry")
				got, err := HolderIntervals(ivX)
				require.NoError(t, err)
				require.Equal(t, []uint64{3, 7, 9}, got, "k=%d", k)

				entries := holdersRange(t, s)
				requireRebuildEqual(t, s, fmt.Sprintf("maintained from round %d", k))
				if first == nil {
					first = entries
				}
				require.Equal(t, first, entries, "k=%d", k)
				require.NoError(t, s.Close())
			}
		})
	}
}

// TestStoreHoldersIndexOption checks that a new store opened with
// Options.HoldersIndex has the index from its first round, which AddRound
// writes in that round's batch, whatever that round, with and without the
// dedup cache, and keeps it after a reopen without the option; that a first
// round that fails to commit leaves no index and its retry starts it; and that
// the option starts nothing on an existing store.
func TestStoreHoldersIndexOption(t *testing.T) {
	partitiontest.PartitionTest(t)

	const rounds = 30
	changes, history := holderRounds(11, rounds)
	add := func(s *Store, lo, hi int) {
		for r := lo; r < hi; r++ {
			require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
		}
	}

	for _, dedup := range []int{0, -1} {
		t.Run(fmt.Sprintf("dedup=%d", dedup), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, Options{HoldersIndex: true, DedupEntries: dedup})
			require.NoError(t, err)
			require.Equal(t, HoldersNone, s.HoldersIndexState())
			add(s, 0, 1)
			require.Equal(t, HoldersComplete, s.HoldersIndexState())
			require.Equal(t, holdersMeta(1), holdersMetaRecord(t, s))
			add(s, 1, rounds/2)
			require.NoError(t, s.Close())

			// the index is the store's: a reopen without the option keeps it
			s, err = Open(dir, Options{DedupEntries: dedup})
			require.NoError(t, err)
			defer s.Close()
			require.Equal(t, HoldersComplete, s.HoldersIndexState())
			add(s, rounds/2, rounds)
			checkHolders(t, s, history)
			requireRebuildEqual(t, s, "started with the first round")
		})
	}

	// first rounds with holderRounds' odd changes: a tombstone of what is not
	// there (2), a creation (3), a creation and deletion within the round (4),
	// a deletion and recreation within it (5), a deletion of what is not
	// there (8), a key changed twice (9) and no-ops (10)
	for _, first := range []int{2, 3, 4, 5, 8, 9, 10} {
		for _, dedup := range []int{0, -1} {
			t.Run(fmt.Sprintf("first round %d dedup=%d", first, dedup), func(t *testing.T) {
				s, err := Open(t.TempDir(), Options{HoldersIndex: true, DedupEntries: dedup})
				require.NoError(t, err)
				defer s.Close()
				add(s, first, first+1)
				require.Equal(t, HoldersComplete, s.HoldersIndexState())
				require.Equal(t, holdersMeta(uint64(first+1)), holdersMetaRecord(t, s))
				requireRebuildEqual(t, s, "the first round's index")
				add(s, first+1, rounds)
				require.Equal(t, holdersMeta(rounds), holdersMetaRecord(t, s))
				requireRebuildEqual(t, s, "maintained from the first round")
			})
		}
	}

	t.Run("first round fails", func(t *testing.T) {
		s, err := Open(t.TempDir(), Options{HoldersIndex: true})
		require.NoError(t, err)
		defer s.Close()
		defer func(f func(*pebble.Batch) error) { commitBatch = f }(commitBatch)
		commitBatch = func(*pebble.Batch) error { return errors.New("injected") }
		require.EqualError(t, s.AddRound(0, changes[0], nil, nil), "injected")
		_, ok := s.Last()
		require.False(t, ok)
		require.Equal(t, HoldersNone, s.HoldersIndexState(), "nothing was stored")
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
		commitBatch = func(b *pebble.Batch) error { return b.Commit(pebble.NoSync) }
		add(s, 0, rounds)
		require.Equal(t, HoldersComplete, s.HoldersIndexState(), "the retry starts the index")
		checkHolders(t, s, history)
		requireRebuildEqual(t, s, "started by the retry")
	})

	t.Run("existing store", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		add(s, 0, 1)
		require.NoError(t, s.Close())
		s, err = Open(dir, Options{HoldersIndex: true})
		require.NoError(t, err)
		defer s.Close()
		add(s, 1, 10)
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
	})
}

// TestStoreHoldersMaintenance checks the entry of one holding after each
// round: only creations and deletions write it.
func TestStoreHoldersMaintenance(t *testing.T) {
	partitiontest.PartitionTest(t)
	addr := basics.Address{9}
	k, k2 := HoldingKey(addr, 5), HoldingKey(basics.Address{10}, 5)
	set := func(v string) Change { return Change{Key: k, Value: []byte(v)} }
	del := Change{Key: k, Deleted: true}
	steps := []struct {
		chs  []Change
		want []uint64
	}{
		{[]Change{set("v1"), {Key: k2, Deleted: true}}, []uint64{0}}, // created at round 0; k2 deleted before it exists
		{[]Change{set("v2")}, []uint64{0}},                           // an update
		{[]Change{del}, []uint64{0, 2}},                              // opt-out
		{[]Change{del}, []uint64{0, 2}},                              // deleting what is not there
		{[]Change{set("v3")}, []uint64{0, 2, 4}},                     // opt-in again
		{[]Change{del, set("v4")}, []uint64{0, 2, 4}},                // deleted and recreated within the round
		{[]Change{set("v4")}, []uint64{0, 2, 4}},                     // a no-op
		{[]Change{set("v5"), del}, []uint64{0, 2, 4, 7}},             // changed, then deleted
		{[]Change{set("v6"), del}, []uint64{0, 2, 4, 7}},             // created and deleted within the round
		{[]Change{set("v7"), set("v8")}, []uint64{0, 2, 4, 7, 9}},    // created, changed twice in the round
		{[]Change{{Key: AccountKey(addr), Value: []byte("a")}}, []uint64{0, 2, 4, 7, 9}},
	}
	for _, tc := range []struct {
		name   string
		o      Options
		reopen bool // before every round: no cache, every change reads the index
	}{{"cache", Options{}, false}, {"no dedup", Options{DedupEntries: -1}, false}, {"read path", Options{}, true}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, tc.o)
			require.NoError(t, err)
			_, err = s.IndexHolders(context.Background(), nil)
			require.NoError(t, err)
			for r, st := range steps {
				if tc.reopen {
					require.NoError(t, s.Close())
					s, err = Open(dir, tc.o)
					require.NoError(t, err)
					require.Equal(t, HoldersComplete, s.HoldersIndexState())
				}
				require.NoError(t, s.AddRound(uint64(r), st.chs, nil, nil))
				var got [][]byte
				require.NoError(t, s.AssetHolders(5, nil, func(a, intervals []byte) bool {
					require.Equal(t, addr[:], a, "k2 never existed")
					got = append(got, intervals)
					return true
				}))
				require.Len(t, got, 1)
				rounds, err := HolderIntervals(got[0])
				require.NoError(t, err)
				require.Equal(t, st.want, rounds, "round %d", r)
			}
			requireRebuildEqual(t, s, "maintained")
			require.NoError(t, s.Close())
		})
	}
}

// TestStoreHoldersState checks when the index exists and when AddRound
// maintains it: never unless it is complete.
func TestStoreHoldersState(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3

	const rounds = 30
	changes, history := holderRounds(8, rounds)
	add := func(s *Store, lo, hi int) {
		for r := lo; r < hi; r++ {
			require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
		}
	}
	noIndex := func(s *Store) {
		err := s.AssetHolders(1, nil, func([]byte, []byte) bool { return true })
		require.ErrorIs(t, err, ErrNoHoldersIndex)
	}

	t.Run("none", func(t *testing.T) {
		s, err := Open(t.TempDir(), Options{})
		require.NoError(t, err)
		defer s.Close()
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		add(s, 0, rounds)
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
		noIndex(s)
		err = s.AssetHolders(1, nil, func([]byte, []byte) bool { return true })
		require.EqualError(t, err, "the asset holders index is missing")
		dropped, err := s.DropHoldersIndex()
		require.NoError(t, err)
		require.False(t, dropped)
	})

	t.Run("stale", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		add(s, 0, 10)
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		add(s, 10, 15)
		// a build without the index adds a round
		s.skipHolders = true
		add(s, 15, 16)
		require.NoError(t, s.Close())
		s, err = Open(dir, Options{})
		require.NoError(t, err)
		require.Equal(t, HoldersStale, s.HoldersIndexState())
		entries, meta := holdersRange(t, s), holdersMetaRecord(t, s)
		require.Equal(t, holdersMeta(15), meta)
		add(s, 16, 20)
		require.Equal(t, entries, holdersRange(t, s), "AddRound leaves a stale index alone")
		require.Equal(t, meta, holdersMetaRecord(t, s))
		err = s.AssetHolders(1, nil, func([]byte, []byte) bool { return true })
		require.ErrorIs(t, err, ErrNoHoldersIndex)
		require.EqualError(t, err, "the asset holders index is stale")
		require.NoError(t, s.Close())

		// read-only, the state is the same and nothing is written
		ro, err := Open(dir, Options{ReadOnly: true})
		require.NoError(t, err)
		require.Equal(t, HoldersStale, ro.HoldersIndexState())
		require.NoError(t, ro.Close())

		s, err = Open(dir, Options{})
		require.NoError(t, err)
		defer s.Close()
		require.Equal(t, HoldersStale, s.HoldersIndexState())
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		add(s, 20, rounds)
		checkHolders(t, s, history)
		requireRebuildEqual(t, s, "rebuilt after staleness")
	})

	t.Run("cancelled", func(t *testing.T) {
		s, err := Open(t.TempDir(), Options{})
		require.NoError(t, err)
		defer s.Close()
		add(s, 0, 20)
		ctx, cancel := context.WithCancel(context.Background())
		_, err = s.IndexHolders(ctx, func(uint64, uint64) { cancel() })
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		half := holdersRange(t, s)
		require.NotEmpty(t, half, "some batches were committed")
		require.Nil(t, holdersMetaRecord(t, s))
		noIndex(s)
		add(s, 20, 25)
		require.Equal(t, half, holdersRange(t, s), "AddRound leaves a half-built index alone")
		require.Nil(t, holdersMetaRecord(t, s))
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		add(s, 25, rounds)
		checkHolders(t, s, history)
		requireRebuildEqual(t, s, "rerun")
	})

	t.Run("drop", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		add(s, 0, 20)
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		require.NotEmpty(t, holdersRange(t, s))
		dropped, err := s.DropHoldersIndex()
		require.NoError(t, err)
		require.True(t, dropped)
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		require.Empty(t, holdersRange(t, s))
		require.Nil(t, holdersMetaRecord(t, s))
		noIndex(s)
		add(s, 20, 22)
		require.Empty(t, holdersRange(t, s), "AddRound does not maintain a dropped index")
		dropped, err = s.DropHoldersIndex()
		require.NoError(t, err)
		require.False(t, dropped, "nothing left to drop")

		// half built: entries, no meta record
		ctx, cancel := context.WithCancel(context.Background())
		_, err = s.IndexHolders(ctx, func(uint64, uint64) { cancel() })
		require.ErrorIs(t, err, context.Canceled)
		require.NotEmpty(t, holdersRange(t, s))
		require.NoError(t, s.Close())
		s, err = Open(dir, Options{})
		require.NoError(t, err)
		defer s.Close()
		require.Equal(t, HoldersNone, s.HoldersIndexState())
		dropped, err = s.DropHoldersIndex()
		require.NoError(t, err)
		require.True(t, dropped)
		require.Empty(t, holdersRange(t, s))
		require.NoError(t, s.CompactHoldersIndex(context.Background()))
	})

	t.Run("read-only", func(t *testing.T) {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		add(s, 0, rounds)
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		entries := holdersRange(t, s)
		require.NoError(t, s.Close())
		ro, err := Open(dir, Options{ReadOnly: true})
		require.NoError(t, err)
		defer ro.Close()
		require.Equal(t, HoldersComplete, ro.HoldersIndexState())
		checkHolders(t, ro, history)
		_, err = ro.IndexHolders(context.Background(), nil)
		require.ErrorIs(t, err, ErrReadOnly)
		_, err = ro.DropHoldersIndex()
		require.ErrorIs(t, err, ErrReadOnly)
		require.ErrorIs(t, ro.CompactHoldersIndex(context.Background()), ErrReadOnly)
		require.Equal(t, entries, holdersRange(t, ro))
	})

	t.Run("RemoveNoops", func(t *testing.T) {
		s, err := Open(t.TempDir(), Options{DedupEntries: -1}) // every no-op is a record
		require.NoError(t, err)
		defer s.Close()
		add(s, 0, rounds)
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		entries := holdersRange(t, s)
		removed, err := s.RemoveNoops(context.Background(), nil)
		require.NoError(t, err)
		require.Positive(t, removed)
		require.Equal(t, HoldersComplete, s.HoldersIndexState())
		require.Equal(t, entries, holdersRange(t, s), "RemoveNoops does not touch the index")
		requireRebuildEqual(t, s, "the records left give the same index")
		require.NoError(t, s.CompactRecords(context.Background()))
		checkHolders(t, s, history)
	})
}

// TestStoreAssetHoldersPages checks that AssetHolders stops when fn says so
// and resumes after any address.
func TestStoreAssetHoldersPages(t *testing.T) {
	partitiontest.PartitionTest(t)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	addrs := holderAddrs()
	var chs []Change
	for _, a := range addrs {
		chs = append(chs, Change{Key: HoldingKey(a, 1), Value: []byte("h")}, Change{Key: HoldingKey(a, 2), Value: []byte("h")})
	}
	require.NoError(t, s.AddRound(0, chs, nil, nil))
	var got []basics.Address
	require.NoError(t, s.AssetHolders(1, nil, func(addr, _ []byte) bool {
		got = append(got, basics.Address(addr))
		return len(got) < 2
	}))
	require.Equal(t, addrs[:2], got)
	require.Equal(t, addrs[2:], storeHolders(t, s, 1, 0, addrs[1][:]))
	require.Empty(t, storeHolders(t, s, 1, 0, addrs[len(addrs)-1][:]), "nothing after the last address")
	require.Empty(t, storeHolders(t, s, 3, 0, nil))
	// an address that is not a holder's
	between := addrs[2]
	between[31]++
	require.Equal(t, addrs[3:], storeHolders(t, s, 2, 0, between[:]))
	require.Equal(t, slices.Clone(addrs), storeHolders(t, s, 2, 0, nil))
}

// TestStoreCompactHoldersIndexInSteps checks that CompactHoldersIndex
// compacts the index in consecutive steps, as CompactRecords does, and
// drops the entries a rebuild replaced.
func TestStoreCompactHoldersIndexInSteps(t *testing.T) {
	partitiontest.PartitionTest(t)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	defer func(n uint64) { compactChunk = n }(compactChunk)
	compactChunk = 1
	defer func(f func(context.Context, *pebble.DB, []byte, []byte) error) { compactStep = f }(compactStep)
	var steps [][2][]byte
	compactStep = func(ctx context.Context, db *pebble.DB, start, end []byte) error {
		steps = append(steps, [2][]byte{slices.Clone(start), slices.Clone(end)})
		return db.Compact(ctx, start, end, true)
	}

	// index entries of separate assets in separate tables, then a range
	// tombstone over half of them, as a rebuild leaves the entries it replaced
	const tables, addrs = 4, 100
	key := func(i, j int) []byte {
		var a basics.Address
		binary.BigEndian.PutUint64(a[:], uint64(j))
		return holdersKey(uint64(i+1), a[:])
	}
	for i := 0; i < tables; i++ {
		b := s.db.NewBatch()
		for j := 0; j < addrs; j++ {
			require.NoError(t, b.Set(key(i, j), binary.AppendUvarint(nil, 1), nil))
		}
		require.NoError(t, b.Commit(pebble.NoSync))
		require.NoError(t, s.db.Flush())
	}
	require.NoError(t, s.db.DeleteRange(holdersKey(1, nil), holdersKey(uint64(tables/2+1), nil), pebble.Sync))
	require.NoError(t, s.db.Flush())

	require.NoError(t, s.CompactHoldersIndex(context.Background()))
	require.Greater(t, len(steps), 1)
	require.Equal(t, []byte{pfxHolders}, steps[0][0])
	for i := 1; i < len(steps); i++ {
		require.Equal(t, steps[i-1][1], steps[i][0], "step %d starts where step %d ends", i, i-1)
	}
	require.Equal(t, []byte{pfxHolders + 1}, steps[len(steps)-1][1])
	levels, err := s.db.SSTables(pebble.WithProperties())
	require.NoError(t, err)
	var entries uint64
	for l, ts := range levels {
		for _, tb := range ts {
			require.Equal(t, len(levels)-1, l, "table %d is above the bottom level", tb.FileNum)
			require.Zero(t, tb.Properties.NumRangeDeletions, "table %d keeps range tombstones", tb.FileNum)
			entries += tb.Properties.NumEntries
		}
	}
	it, err := s.db.NewIter(nil)
	require.NoError(t, err)
	var live, index uint64
	for it.First(); it.Valid(); it.Next() {
		live++
		if it.Key()[0] == pfxHolders {
			index++
		}
	}
	require.NoError(t, it.Close())
	require.Equal(t, uint64(tables/2*addrs), index)
	require.Equal(t, live, entries, "the tables hold only live records")
}

// holdersIndexOf returns the index entries that IndexHolders builds from
// rounds 0..b of changes.
func holdersIndexOf(t *testing.T, changes [][]Change, b int) []string {
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	for r := 0; r <= b; r++ {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	return holdersRange(t, s)
}

// TestStoreHoldersTruncate checks that TruncateAfter(b) trims the index to
// what IndexHolders builds from the rounds up to b, keeps a complete index
// complete, and that AddRound then maintains it over different rounds.
func TestStoreHoldersTruncate(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3 // commit while trimming

	const rounds = 40
	changes, history := holderRounds(7, rounds)
	other, _ := holderRounds(8, rounds) // the rounds written again after b
	for _, b := range []int{0, 3, 5, 9, rounds / 2, rounds - 1} {
		t.Run(fmt.Sprintf("b=%d", b), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, Options{DedupEntries: 24})
			require.NoError(t, err)
			for r := 0; r < rounds; r++ {
				require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
			}
			_, err = s.IndexHolders(context.Background(), nil)
			require.NoError(t, err)
			_, err = s.TruncateAfter(context.Background(), uint64(b), TruncateOptions{})
			require.NoError(t, err)
			require.Equal(t, HoldersComplete, s.HoldersIndexState())
			require.Equal(t, holdersMeta(uint64(b+1)), holdersMetaRecord(t, s))
			require.Equal(t, holdersIndexOf(t, changes, b), holdersRange(t, s))
			checkHolders(t, s, history[:b+1])

			// the node writes the rounds after b again, differently
			for r := b + 1; r < rounds; r++ {
				require.NoError(t, s.AddRound(uint64(r), other[r], nil, nil))
			}
			require.Equal(t, HoldersComplete, s.HoldersIndexState())
			requireRebuildEqual(t, s, "maintained after the truncation")
			require.NoError(t, s.Close())
			s, err = Open(dir, Options{})
			require.NoError(t, err)
			require.Equal(t, HoldersComplete, s.HoldersIndexState())
			require.NoError(t, s.Close())
		})
	}
}

// TestStoreHoldersTruncateStale checks that a truncation drops a stale index,
// whatever round it covered: an older build may have truncated it without
// trimming.
func TestStoreHoldersTruncateStale(t *testing.T) {
	partitiontest.PartitionTest(t)

	const rounds, k = 30, 15 // the index covers rounds 0..k-1
	changes, _ := holderRounds(3, rounds)
	for _, b := range []int{k - 5, k - 1, k, k + 5} {
		t.Run(fmt.Sprintf("b=%d", b), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, Options{})
			require.NoError(t, err)
			for r := 0; r < rounds; r++ {
				if r == k {
					_, err = s.IndexHolders(context.Background(), nil)
					require.NoError(t, err)
					s.skipHolders = true // a build without the index adds the rest
				}
				require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
			}
			s.skipHolders = false
			require.NoError(t, s.Close())
			s, err = Open(dir, Options{})
			require.NoError(t, err)
			defer s.Close()
			require.Equal(t, HoldersStale, s.HoldersIndexState())

			_, err = s.TruncateAfter(context.Background(), uint64(b), TruncateOptions{})
			require.NoError(t, err)
			require.Equal(t, HoldersNone, s.HoldersIndexState())
			require.Empty(t, holdersRange(t, s))
			require.Nil(t, holdersMetaRecord(t, s))
		})
	}
}

// TestStoreHoldersTruncateResumes checks that a truncation cancelled before
// it trims the index leaves it unreadable, and trims it when the store is next
// opened, the index's state then read for the truncated store.
func TestStoreHoldersTruncateResumes(t *testing.T) {
	partitiontest.PartitionTest(t)

	const rounds, b = 30, 12
	changes, history := holderRounds(5, rounds)
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	for r := 0; r < rounds; r++ {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	before := holdersRange(t, s)
	_, err = s.TruncateAfter(ctx, b, TruncateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.Equal(t, before, holdersRange(t, s), "cancelled before trimming")
	require.Equal(t, HoldersStale, s.HoldersIndexState())
	require.ErrorIs(t, s.AssetHolders(1, nil, func([]byte, []byte) bool { return true }), ErrNoHoldersIndex)
	require.NoError(t, s.Close())

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	last, _ := s.Last()
	require.EqualValues(t, b, last)
	require.Equal(t, HoldersComplete, s.HoldersIndexState())
	require.Equal(t, holdersIndexOf(t, changes, b), holdersRange(t, s))
	checkHolders(t, s, history[:b+1])
}

// TestStoreHoldersTruncateBadEntry checks that a bad index entry drops the
// index rather than fail the truncation.
func TestStoreHoldersTruncateBadEntry(t *testing.T) {
	partitiontest.PartitionTest(t)

	changes, _ := holderRounds(5, 20)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	for r := range changes {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, s.db.Set(holdersKey(1, make([]byte, 32)), []byte{0x80}, pebble.Sync))
	_, err = s.TruncateAfter(context.Background(), 10, TruncateOptions{})
	require.NoError(t, err)
	last, _ := s.Last()
	require.EqualValues(t, 10, last)
	require.Equal(t, HoldersNone, s.HoldersIndexState())
	require.Empty(t, holdersRange(t, s))
	require.Nil(t, holdersMetaRecord(t, s))
}

func TestIntervalsThrough(t *testing.T) {
	partitiontest.PartitionTest(t)
	enc := func(rounds ...uint64) []byte {
		var out []byte
		var at uint64
		for _, r := range rounds {
			out = binary.AppendUvarint(out, r-at)
			at = r
		}
		return out
	}
	iv := enc(3, 7, 200, 1<<40)
	for _, tc := range []struct {
		b    uint64
		want []byte
	}{{0, nil}, {2, nil}, {3, enc(3)}, {6, enc(3)}, {7, enc(3, 7)}, {199, enc(3, 7)}, {200, enc(3, 7, 200)}, {1 << 40, iv}, {^uint64(0), iv}} {
		n, err := intervalsThrough(iv, tc.b)
		require.NoError(t, err)
		require.Equal(t, len(tc.want), n, "b=%d", tc.b)
		require.True(t, bytes.Equal(tc.want, iv[:n]), "b=%d", tc.b)
	}
	_, err := intervalsThrough([]byte{0x80}, 5)
	require.Error(t, err)
	_, err = intervalsThrough(append(enc(1<<63), binary.AppendUvarint(nil, 1<<63)...), ^uint64(0))
	require.Error(t, err, "boundaries that wrap around")
}

// TestStoreHoldersTruncateResumesPartial cancels a truncation once it has
// trimmed some entries, and checks that readers get ErrNoHoldersIndex while it
// runs and that reopening the store finishes the trim. Not parallel: it sets
// removeNoopsBatchOps.
func TestStoreHoldersTruncateResumesPartial(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 1

	const rounds, b = 40, 8
	changes, history := holderRounds(11, rounds)
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	for r := 0; r < rounds; r++ {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	before, want := holdersRange(t, s), holdersIndexOf(t, changes, b)
	require.NotEqual(t, before, want)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var reads int
	_, err = s.TruncateAfter(ctx, b, TruncateOptions{Progress: func(_, deleted uint64) {
		require.Equal(t, HoldersStale, s.HoldersIndexState())
		require.ErrorIs(t, s.AssetHolders(1, nil, func([]byte, []byte) bool { return true }), ErrNoHoldersIndex)
		reads++
		if deleted > 0 {
			cancel()
		}
	}})
	require.ErrorIs(t, err, context.Canceled)
	require.Positive(t, reads)
	mid := holdersRange(t, s)
	require.NotEqual(t, before, mid, "trimmed some")
	require.NotEqual(t, want, mid, "cut short")
	require.NoError(t, s.Close())

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	require.Equal(t, HoldersComplete, s.HoldersIndexState())
	require.Equal(t, want, holdersRange(t, s))
	checkHolders(t, s, history[:b+1])
}

// TestStoreHoldersTruncateAbandoned checks that a truncation that finds the
// store corrupt drops the index: records after b that it did not reach remain.
func TestStoreHoldersTruncateAbandoned(t *testing.T) {
	partitiontest.PartitionTest(t)

	changes, _ := holderRounds(5, 20)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	for r := range changes {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)
	require.NoError(t, s.db.Set(verKey(pfxRecord, []byte("z"), 1), []byte("F"), pebble.Sync)) // no object kind
	_, err = s.TruncateAfter(context.Background(), 10, TruncateOptions{})
	require.ErrorIs(t, err, ErrCorrupt)
	_, diverged, err := s.Diverged()
	require.NoError(t, err)
	require.True(t, diverged)
	require.Equal(t, HoldersNone, s.HoldersIndexState())
	require.Empty(t, holdersRange(t, s))
	require.Nil(t, holdersMetaRecord(t, s))
}

// TestStoreHoldersTruncateHalfBuilt checks that a truncation leaves a
// half-built index (no meta record) alone: IndexHolders clears it first.
func TestStoreHoldersTruncateHalfBuilt(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3

	changes, _ := holderRounds(5, 20)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	for r := range changes {
		require.NoError(t, s.AddRound(uint64(r), changes[r], nil, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	_, err = s.IndexHolders(ctx, func(uint64, uint64) { cancel() })
	require.ErrorIs(t, err, context.Canceled)
	half := holdersRange(t, s)
	require.NotEmpty(t, half)
	require.Nil(t, holdersMetaRecord(t, s))
	_, err = s.TruncateAfter(context.Background(), 5, TruncateOptions{})
	require.NoError(t, err)
	require.Equal(t, HoldersNone, s.HoldersIndexState())
	require.Equal(t, half, holdersRange(t, s))
}
