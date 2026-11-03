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
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// indexedStore writes rounds 0..99 of boxes and holdings (see
// addIndexedRounds).
func indexedStore(t *testing.T, o Options) *Store {
	o.HoldersIndex = true
	s, err := Open(filepath.Join(t.TempDir(), "tt"), o)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	addIndexedRounds(t, s, 0, 99)
	return s
}

// addIndexedRounds writes rounds lo..hi of boxes of 3 apps and holdings of 3
// assets, created, rewritten unchanged, changed and deleted now and then.
func addIndexedRounds(t *testing.T, s *Store, lo, hi uint64) {
	for rnd := lo; rnd <= hi; rnd++ {
		var changes []Change
		for app := uint64(1); app <= 3; app++ {
			for i := range 5 {
				if (rnd+uint64(i))%app != 0 {
					continue
				}
				ch := Change{Key: boxObject(app, []byte(fmt.Sprint("box", i))), Value: []byte(fmt.Sprint(rnd / 4))}
				ch.Deleted = rnd%9 == uint64(i)
				changes = append(changes, ch)
			}
		}
		for asset := uint64(1); asset <= 3; asset++ {
			for i := range 4 {
				if (rnd+uint64(i))%(asset+1) != 0 {
					continue
				}
				ch := Change{Key: HoldingKey(basics.Address{byte(i)}, basics.AssetIndex(asset)), Value: []byte(fmt.Sprint(rnd / 6))}
				ch.Deleted = rnd%11 == uint64(i)
				changes = append(changes, ch)
			}
		}
		changes = append(changes, Change{Key: TotalsKey, Value: []byte{byte(rnd)}})
		require.NoError(t, s.AddRound(rnd, changes, []byte("hdr"), nil))
	}
}

// checkIndexes returns the problems CheckIndexes reports.
func checkIndexes(t *testing.T, s *Store) ([]string, IndexCheckStats) {
	var problems []string
	st, err := s.CheckIndexes(func(format string, args ...any) { problems = append(problems, fmt.Sprintf(format, args...)) })
	require.NoError(t, err)
	require.Equal(t, len(problems), st.Mismatches)
	return problems, st
}

func TestCheckIndexesAgree(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, dedup := range []int{0, -1} { // -1: repeats stored and indexed
		s := indexedStore(t, Options{DedupEntries: dedup})
		problems, st := checkIndexes(t, s)
		require.Empty(t, problems)
		require.True(t, st.BoxIndex)
		require.Zero(t, st.BoxIndexFrom)
		require.EqualValues(t, 3, st.BoxApps)
		require.Positive(t, st.BoxEntries)
		require.Equal(t, HoldersComplete, st.Holders)
		require.EqualValues(t, 3, st.HolderAssets)
		require.Positive(t, st.HolderEntries)

		// RemoveNoops drops the repeats and rebuilds the box index without them
		removed, err := s.RemoveNoops(context.Background(), nil)
		require.NoError(t, err)
		if dedup < 0 {
			require.Positive(t, removed)
		}
		problems, _ = checkIndexes(t, s)
		require.Empty(t, problems)
	}
}

func TestCheckIndexesFindsDifferences(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	firstKey := func(s *Store, lo []byte) []byte {
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
		require.NoError(t, err)
		defer it.Close()
		require.True(t, it.First())
		return append([]byte(nil), it.Key()...)
	}
	requireEntry := func(s *Store, k []byte, want byte) {
		v, closer, err := s.db.Get(k)
		require.NoError(t, err)
		defer closer.Close()
		require.Equal(t, []byte{want}, v)
	}
	assetPfx := func(asset uint64) []byte { return binary.BigEndian.AppendUint64([]byte{pfxHolders}, asset) }
	cases := []struct {
		name   string
		o      Options
		damage func(s *Store)
		want   string
	}{
		{"box entry missing", Options{}, func(s *Store) {
			k := boxIndexKey(2, 3, []byte("box1")) // its first value
			requireEntry(s, k, recFull)
			require.NoError(t, s.db.Delete(k, nil))
		}, "app 2: box index disagrees with its box records from round 0"},
		{"box entry flipped", Options{}, func(s *Store) {
			k := boxIndexKey(3, 3, []byte("box0"))
			requireEntry(s, k, recFull)
			require.NoError(t, s.db.Set(k, []byte{recDeleted}, nil))
		}, "app 3: box index disagrees"},
		{"box entry of a repeat flipped", Options{DedupEntries: -1}, func(s *Store) {
			k := boxIndexKey(1, 5, []byte("box1")) // repeats round 4
			requireEntry(s, k, recFull)
			require.NoError(t, s.db.Set(k, []byte{recDeleted}, nil))
		}, "app 1: box index disagrees"},
		{"box entry extra", Options{}, func(s *Store) {
			require.NoError(t, s.db.Set(boxIndexKey(1, 50, []byte("nobox")), []byte{recFull}, nil))
		}, "app 1: box index disagrees"},
		{"box entries of an app without boxes", Options{}, func(s *Store) {
			require.NoError(t, s.db.Set(boxIndexKey(7, 50, []byte("nobox")), []byte{recFull}, nil))
		}, "app 7: box index has 1 entries from round 0, but the app has no box records"},
		{"bad box key", Options{}, func(s *Store) {
			k := binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, 1)
			require.NoError(t, s.db.Set(append(k, "xx"...), []byte{recFull}, nil))
		}, "box index: bad key"},
		{"bad box key of an app without boxes", Options{}, func(s *Store) {
			k := binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, 8)
			require.NoError(t, s.db.Set(append(k, "xx"...), []byte{recFull}, nil))
		}, "box index: bad key"},
		{"holders entry missing", Options{}, func(s *Store) {
			require.NoError(t, s.db.Delete(firstKey(s, assetPfx(2)), nil))
		}, "asset 2: holders index disagrees with its holding records"},
		{"holders entry wrong", Options{}, func(s *Store) {
			require.NoError(t, s.db.Set(firstKey(s, assetPfx(1)), []byte{0}, nil))
		}, "asset 1: holders index disagrees"},
		{"holders asset missing", Options{}, func(s *Store) {
			require.NoError(t, s.db.DeleteRange(assetPfx(3), prefixEnd(assetPfx(3)), nil))
		}, "asset 3: holders index has no entries for its 4 holdings"},
		{"holders asset extra", Options{}, func(s *Store) {
			require.NoError(t, s.db.Set(holdersKey(9, make([]byte, 32)), []byte{5}, nil))
		}, "asset 9: holders index disagrees with its holding records: 1 entries for 0 holdings"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := indexedStore(t, tc.o)
			tc.damage(s)
			problems, _ := checkIndexes(t, s)
			require.Len(t, problems, 1)
			require.Contains(t, problems[0], tc.want)
		})
	}
}

// TestCheckIndexesSkipsIncomplete checks that a holders index that is not
// complete is not checked, and that box index entries before BoxIndexFrom
// are not.
func TestCheckIndexesSkipsIncomplete(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s := indexedStore(t, Options{})
	_, err := s.DropHoldersIndex()
	require.NoError(t, err)
	require.NoError(t, s.setBoxFrom(40))
	// an entry the index need not have, and one it may hold
	require.NoError(t, s.db.Set(boxIndexKey(1, 10, []byte("nobox")), []byte{recFull}, nil))
	problems, st := checkIndexes(t, s)
	require.Empty(t, problems)
	require.Equal(t, HoldersNone, st.Holders)
	require.Zero(t, st.HolderAssets)
	require.EqualValues(t, 40, st.BoxIndexFrom)
}

// TestCheckIndexesLifecycle checks that the indexes agree with the records
// through truncations, rebuilds, and a box index that starts after the first
// round, as a build without it leaves it.
func TestCheckIndexesLifecycle(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ctx := context.Background()
	for _, dedup := range []int{0, -1} {
		dir := filepath.Join(t.TempDir(), "tt")
		o := Options{DedupEntries: dedup, HoldersIndex: true}
		s, err := Open(dir, o)
		require.NoError(t, err)
		check := func(stage string) IndexCheckStats {
			problems, st := checkIndexes(t, s)
			require.Empty(t, problems, "dedup %d, %s", dedup, stage)
			require.Equal(t, HoldersComplete, st.Holders, stage)
			require.Positive(t, st.HolderEntries, stage)
			return st
		}
		addIndexedRounds(t, s, 0, 99)
		_, err = s.TruncateAfter(ctx, 60, TruncateOptions{})
		require.NoError(t, err)
		check("truncated")
		addIndexedRounds(t, s, 61, 120)
		check("extended")
		_, err = s.IndexBoxes(ctx, func(uint64, uint64) {})
		require.NoError(t, err)
		_, err = s.IndexHolders(ctx, nil)
		require.NoError(t, err)
		check("rebuilt")

		require.NoError(t, s.db.DeleteRange([]byte{pfxBoxIndex}, []byte{pfxBoxIndex + 1}, nil))
		require.NoError(t, s.db.Delete(keyBoxIndexMeta, nil))
		require.NoError(t, s.Close())
		s, err = Open(dir, o)
		require.NoError(t, err)
		from, ok := s.BoxIndexFrom()
		require.True(t, ok)
		require.EqualValues(t, 121, from)
		addIndexedRounds(t, s, 121, 160)
		st := check("box index from 121")
		require.EqualValues(t, 121, st.BoxIndexFrom)
		require.Positive(t, st.BoxEntries)
		_, err = s.TruncateAfter(ctx, 110, TruncateOptions{}) // before it starts
		require.NoError(t, err)
		check("truncated before the box index")
		addIndexedRounds(t, s, 111, 140)
		check("extended again")
		require.NoError(t, s.Close())

		// read-only
		s, err = Open(dir, Options{ReadOnly: true})
		require.NoError(t, err)
		check("read-only")
		require.NoError(t, s.Close())
	}
}
