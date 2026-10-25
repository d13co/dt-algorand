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
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func truncExpired(r int) *uint64 {
	if r%2 == 0 {
		return nil
	}
	e := uint64(r) * 1000
	return &e
}

// addModelRounds adds rounds [from, to) of changes to s.
func addModelRounds(t *testing.T, s *Store, changes [][]Change, from, to int) {
	for r := from; r < to; r++ {
		require.NoError(t, s.AddRound(uint64(r), cloneChanges(changes[r]), hdrOf(uint64(r)), truncExpired(r)))
	}
}

func setRaw(t *testing.T, s *Store, key, val []byte) {
	require.NoError(t, s.DB().Set(key, val, pebble.Sync))
}

func hasRaw(t *testing.T, s *Store, key []byte) bool {
	_, closer, err := s.DB().Get(key)
	if err == pebble.ErrNotFound {
		return false
	}
	require.NoError(t, err)
	closer.Close()
	return true
}

// TestStoreTruncateAfter truncates a store that holds records no AddRound
// wrote, adds the later rounds again, and compares it with a store that
// added every round once.
func TestStoreTruncateAfter(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	objs := modelObjects()
	const rounds, b = 60, 30
	changes, history := modelRounds(11, objs, rounds)

	want, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	t.Cleanup(func() { want.Close() }) // after the parallel subtests
	addModelRounds(t, want, changes, 0, rounds)

	for _, workers := range []int{1, 4} {
		t.Run(fmt.Sprintf("workers=%d", workers), func(t *testing.T) {
			t.Parallel()
			s, err := Open(t.TempDir(), Options{})
			require.NoError(t, err)
			defer s.Close()
			addModelRounds(t, s, changes, 0, rounds)

			// what a producer could add: an object no round changed, a record
			// of an object at a round that did not change it, a box index
			// entry, patch and round index records, which N = 1 never writes
			forged := KvKey("forged")
			box := boxObject(modelApps[0], []byte("a"))
			untouched := -1
			for r := b + 1; r < rounds && untouched < 0; r++ {
				untouched = r
				for _, ch := range changes[r] {
					if string(ch.Key) == string(box) {
						untouched = -1
					}
				}
			}
			require.Positive(t, untouched, "a round after b leaves the box alone")
			forgedKeys := [][]byte{
				verKey(pfxCheckpoint, forged, b+5),
				verKey(pfxCheckpoint, box, uint64(untouched)),
				boxIndexKey(modelApps[0], uint64(untouched), []byte("a")),
				verKey(pfxPatch, box, b-3),
				binary.BigEndian.AppendUint64([]byte{pfxRoundIndex}, b-3),
			}
			for _, k := range forgedKeys {
				setRaw(t, s, k, []byte("Fforged"))
			}

			var progressed atomic.Bool
			deleted, err := s.truncateAfter(context.Background(), b, TruncateOptions{
				Workers: workers,
				Progress: func(scanned, _ uint64) {
					if scanned > 0 {
						progressed.Store(true)
					}
				},
			}, randomSplits(s, int64(workers), 7))
			require.NoError(t, err)
			require.Positive(t, deleted)
			require.True(t, progressed.Load())
			last, ok := s.Last()
			require.True(t, ok)
			require.EqualValues(t, b, last)
			for _, k := range forgedKeys {
				require.False(t, hasRaw(t, s, k), "%x", k)
			}
			for r := uint64(b + 1); r < rounds; r++ {
				_, ok, err := s.Header(r)
				require.NoError(t, err)
				require.False(t, ok, "round %d", r)
				_, ok, err = s.ExpiredStake(r)
				require.NoError(t, err)
				require.False(t, ok, "round %d", r)
			}
			keys, _ := rawRecords(t, s)
			for _, k := range keys {
				require.LessOrEqual(t, binary.BigEndian.Uint64(k[len(k)-8:]), uint64(b), "%x", k)
			}
			from, ok := s.BoxIndexFrom()
			require.True(t, ok)
			require.Zero(t, from)

			// the later rounds again: the store answers as if it had added
			// every round once
			addModelRounds(t, s, changes, b+1, rounds)
			require.Equal(t, commitOf(t, want, rounds-1, 1, nil), commitOf(t, s, rounds-1, 1, nil))
			read := append(slices.Clip(objs), forged)
			for r := uint64(0); r < rounds; r++ {
				for _, obj := range read {
					wv, wok, err := want.Get(obj, r)
					require.NoError(t, err)
					v, ok, err := s.Get(obj, r)
					require.NoError(t, err)
					require.Equal(t, wok, ok, "%x at %d", obj, r)
					require.Equal(t, wv, v, "%x at %d", obj, r)
				}
				wh, _, err := want.Header(r)
				require.NoError(t, err)
				h, _, err := s.Header(r)
				require.NoError(t, err)
				require.Equal(t, wh, h)
				we, wok, err := want.ExpiredStake(r)
				require.NoError(t, err)
				e, ok, err := s.ExpiredStake(r)
				require.NoError(t, err)
				require.Equal(t, wok, ok)
				require.Equal(t, we, e)
			}
			checkBoxChanges(t, s, objs, history, rand.New(rand.NewSource(3)))
		})
	}
}

// TestStoreTruncateDedup checks that a truncation forgets the states the
// dedup cache knew of the records it deleted: a later round that repeats one
// of them is a change again.
func TestStoreTruncateDedup(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	obj := KvKey("x")
	for r, v := range []string{"v0", "v1", "v2"} {
		require.NoError(t, s.AddRound(uint64(r), []Change{{Key: obj, Value: []byte(v)}}, hdrOf(uint64(r)), nil))
	}
	_, err = s.TruncateAfter(context.Background(), 1, TruncateOptions{})
	require.NoError(t, err)
	require.NoError(t, s.AddRound(2, []Change{{Key: obj, Value: []byte("v2")}}, hdrOf(2), nil))
	v, ok, err := s.Get(obj, 2)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "v2", string(v))
}

// TestStoreTruncateInstalled checks that a truncation lowers the last
// installed round to b, and takes a bad record as installed through b.
func TestStoreTruncateInstalled(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	build := func(t *testing.T) *Store {
		s, err := Open(t.TempDir(), Options{})
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })
		for r := range 10 {
			require.NoError(t, s.AddRound(uint64(r), []Change{{Key: KvKey("x"), Value: []byte{byte(r)}}}, hdrOf(uint64(r)), nil))
		}
		return s
	}
	through := func(t *testing.T, s *Store) (uint64, bool) {
		x, ok, err := s.InstalledThrough()
		require.NoError(t, err)
		return x, ok
	}

	s := build(t)
	_, ok := through(t, s)
	require.False(t, ok, "the node's own store")
	_, err := s.TruncateAfter(context.Background(), 5, TruncateOptions{})
	require.NoError(t, err)
	_, ok = through(t, s)
	require.False(t, ok, "still the node's own")

	s = build(t)
	require.NoError(t, s.SetInstalled("h", 7))
	x, ok := through(t, s)
	require.True(t, ok)
	require.EqualValues(t, 7, x)
	_, err = s.TruncateAfter(context.Background(), 8, TruncateOptions{})
	require.NoError(t, err)
	x, _ = through(t, s)
	require.EqualValues(t, 7, x, "below b: kept")
	_, err = s.TruncateAfter(context.Background(), 4, TruncateOptions{})
	require.NoError(t, err)
	x, _ = through(t, s)
	require.EqualValues(t, 4, x, "lowered to b")

	s = build(t)
	setRaw(t, s, keyInstalledThroughMeta, []byte("bad"))
	_, _, err = s.InstalledThrough()
	require.ErrorIs(t, err, ErrCorrupt)
	_, err = s.TruncateAfter(context.Background(), 5, TruncateOptions{})
	require.NoError(t, err)
	x, ok = through(t, s)
	require.True(t, ok)
	require.EqualValues(t, 5, x, "a bad record is taken as installed through b")
}

// TestStoreTruncateRefuses checks the stores TruncateAfter refuses, and that
// one whose object keys prefix one another is left diverged and open.
func TestStoreTruncateRefuses(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	build := func(t *testing.T, o Options) *Store {
		s, err := Open(t.TempDir(), o)
		require.NoError(t, err)
		t.Cleanup(func() { s.Close() })
		for r := range 10 {
			require.NoError(t, s.AddRound(uint64(r), []Change{{Key: KvKey("x"), Value: []byte{byte(r)}}}, hdrOf(uint64(r)), nil))
		}
		return s
	}
	ctx := context.Background()

	t.Run("N", func(t *testing.T) {
		s := build(t, Options{CheckpointInterval: 4})
		_, err := s.TruncateAfter(ctx, 5, TruncateOptions{})
		require.ErrorContains(t, err, "N = 1")
	})

	t.Run("short", func(t *testing.T) {
		s := build(t, Options{})
		_, err := s.TruncateAfter(ctx, 10, TruncateOptions{})
		require.ErrorContains(t, err, "ends at round 9")
	})

	prefix := [][]byte{verKey(pfxCheckpoint, []byte("ka"), 3), verKey(pfxCheckpoint, []byte("kab"), 3)}
	for name, tc := range map[string]struct {
		keys  [][]byte
		split [][]byte // nil: one range
	}{
		"prefix": {keys: prefix},
		// the workers' ranges hold one key each, and only the check across
		// ranges sees them together
		"prefix across ranges": {keys: prefix, split: prefix[1:]},
		"key":                  {keys: [][]byte{{pfxCheckpoint, 1, 2}}},
		// an object key and more, then a round up to b: no prefix check
		// sees it, but a read of the object takes it for a record of the
		// round in its middle bytes
		"shadow": {keys: [][]byte{verKey(pfxCheckpoint, binary.BigEndian.AppendUint64(KvKey("x"), 7), 1)}},
		"kind":   {keys: [][]byte{verKey(pfxCheckpoint, []byte("z"), 1)}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, Options{})
			require.NoError(t, err)
			for r := range 10 {
				require.NoError(t, s.AddRound(uint64(r), nil, hdrOf(uint64(r)), nil))
			}
			for _, k := range tc.keys {
				setRaw(t, s, k, []byte("F"))
			}
			split := func(*pebble.Snapshot, []byte, []byte, int) ([][]byte, error) { return tc.split, nil }
			_, err = s.truncateAfter(ctx, 5, TruncateOptions{Workers: 2}, split)
			require.ErrorIs(t, err, ErrCorrupt)
			_, diverged, err := s.Diverged()
			require.NoError(t, err)
			require.True(t, diverged)
			last, _ := s.Last()
			require.EqualValues(t, 5, last, "it ends where the truncation stopped")
			require.NoError(t, s.AddRound(6, nil, hdrOf(6), nil), "the truncation is over")
			require.NoError(t, s.Close())
			s, err = Open(dir, Options{})
			require.NoError(t, err, "a corrupt store still opens")
			_, diverged, err = s.Diverged()
			require.NoError(t, err)
			require.True(t, diverged)
			require.NoError(t, s.Close())
		})
	}
}

// TestStoreTruncateResumes interrupts a truncation and checks that the store
// refuses rounds until it is reopened, which finishes it.
func TestStoreTruncateResumes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dir := t.TempDir()
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	obj := KvKey("x")
	for r := range 20 {
		require.NoError(t, s.AddRound(uint64(r), []Change{{Key: obj, Value: []byte{byte(r)}}}, hdrOf(uint64(r)), nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.TruncateAfter(ctx, 10, TruncateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorIs(t, s.AddRound(20, nil, hdrOf(20), nil), errTruncating)
	_, err = s.TruncateAfter(context.Background(), 12, TruncateOptions{})
	require.ErrorContains(t, err, "unfinished", "a later round than the unfinished truncation's")
	require.NoError(t, s.Close())

	_, err = Open(dir, Options{ReadOnly: true})
	require.ErrorContains(t, err, "unfinished")

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	last, _ := s.Last()
	require.EqualValues(t, 10, last)
	require.False(t, hasRaw(t, s, keyTruncateMeta))
	require.False(t, hasRaw(t, s, verKey(pfxCheckpoint, obj, 11)))
	require.NoError(t, s.AddRound(11, []Change{{Key: obj, Value: []byte{11}}}, hdrOf(11), nil))

	// an earlier round than an unfinished truncation's finishes it there
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = s.TruncateAfter(ctx, 9, TruncateOptions{})
	require.ErrorIs(t, err, context.Canceled)
	_, err = s.TruncateAfter(context.Background(), 8, TruncateOptions{})
	require.NoError(t, err)
	last, _ = s.Last()
	require.EqualValues(t, 8, last)
	v, ok, err := s.Get(obj, 11)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte{8}, v)
}

// TestStoreTruncateResumesPartial interrupts a truncation once it has
// deleted some object records, and checks that reopening the store finishes
// it: the store then holds what one that stopped at b would. Not parallel: it
// sets removeNoopsBatchOps.
func TestStoreTruncateResumesPartial(t *testing.T) {
	partitiontest.PartitionTest(t)

	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 1
	objs := modelObjects()
	const rounds, b = 40, 15
	changes, _ := modelRounds(5, objs, rounds)
	want, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer want.Close()
	addModelRounds(t, want, changes, 0, b+1)

	dir := t.TempDir()
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	addModelRounds(t, s, changes, 0, rounds)
	after := func(s *Store) int {
		keys, _ := rawRecords(t, s)
		n := 0
		for _, k := range keys {
			if binary.BigEndian.Uint64(k[len(k)-8:]) > b {
				n++
			}
		}
		return n
	}
	before := after(s)
	require.Positive(t, before)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, err = s.truncateAfter(ctx, b, TruncateOptions{Workers: 1, Progress: func(_, deleted uint64) {
		if deleted > 0 {
			cancel()
		}
	}}, randomSplits(s, 1, 20))
	require.ErrorIs(t, err, context.Canceled)
	left := after(s)
	require.Positive(t, left, "cut short")
	require.Less(t, left, before, "after deleting some")
	require.NoError(t, s.Close())

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	require.Zero(t, after(s))
	last, _ := s.Last()
	require.EqualValues(t, b, last)
	require.Equal(t, commitOf(t, want, b, 1, nil), commitOf(t, s, b, 1, nil))
}
