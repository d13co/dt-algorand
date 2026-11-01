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

package main

import (
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// buildStore writes rounds 0..last of a deterministic history: a few accounts
// changing now and then, and the totals rewritten in every round, changed or
// not. extra adds changes to a round.
func buildStore(t *testing.T, o timetravel.Options, last uint64, extra func(rnd uint64) []timetravel.Change) string {
	path := filepath.Join(t.TempDir(), "ledger.timetravel")
	s, err := timetravel.Open(path, o)
	require.NoError(t, err)
	defer s.Close()
	for rnd := uint64(0); rnd <= last; rnd++ {
		var changes []timetravel.Change
		for i := range 50 {
			addr := basics.Address{byte(i * 5)}
			if rnd%uint64(i+3) == 0 {
				changes = append(changes, timetravel.Change{Key: timetravel.AccountKey(addr), Value: []byte(fmt.Sprint(rnd / uint64(i+3)))})
			} else if rnd%7 == 0 { // rewrite unchanged
				changes = append(changes, timetravel.Change{Key: timetravel.AccountKey(addr), Value: []byte(fmt.Sprint(rnd / uint64(i+3)))})
			}
		}
		changes = append(changes, timetravel.Change{Key: timetravel.TotalsKey, Value: []byte(fmt.Sprint(rnd / 10))})
		if extra != nil {
			changes = append(changes, extra(rnd)...)
		}
		hdr := bookkeeping.BlockHeader{Round: basics.Round(rnd)}
		hdr.TxnCounter = 1000
		require.NoError(t, s.AddRound(rnd, changes, protocol.Encode(&hdr), nil))
	}
	require.NoError(t, s.Sync())
	return path
}

func compare(t *testing.T, a, b string) *comparer {
	c := &comparer{rng: rand.New(rand.NewPCG(1, 1)), span: 300, maxVersions: 5000, maxDiffs: 10, seenObjs: map[string]bool{}}
	require.NoError(t, c.run(a, b, 100, 50, 5))
	return c
}

func TestCompareIgnoresNoops(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	a := buildStore(t, timetravel.Options{DedupEntries: -1}, 500, nil)
	b := buildStore(t, timetravel.Options{}, 500, nil) // no-ops skipped
	c := compare(t, a, b)
	require.Zero(t, c.diffs)
	require.Positive(t, c.versionsChecked)
}

func TestCompareFindsDifferences(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	a := buildStore(t, timetravel.Options{}, 500, nil)
	b := buildStore(t, timetravel.Options{}, 500, func(rnd uint64) []timetravel.Change {
		if rnd%50 == 25 { // every account diverges for a round
			var chs []timetravel.Change
			for i := range 50 {
				chs = append(chs, timetravel.Change{Key: timetravel.AccountKey(basics.Address{byte(i * 5)}), Value: []byte("x")})
			}
			return chs
		}
		return nil
	})
	c := compare(t, a, b)
	require.Positive(t, c.diffs)
}

func compareFull(t *testing.T, a, b string) *comparer {
	c := &comparer{maxDiffs: 10, seenObjs: map[string]bool{}}
	require.NoError(t, c.runFull(a, b))
	return c
}

func TestFullIgnoresNoops(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// A keeps every repeated write, plus a tombstone of an object that never
	// had a value (as one created and deleted in a round leaves); B skips the
	// repeats, and is 100 rounds ahead
	a := buildStore(t, timetravel.Options{DedupEntries: -1}, 500, nil)
	s, err := timetravel.Open(a, timetravel.Options{})
	require.NoError(t, err)
	key := append([]byte{pfxRecord}, timetravel.AccountKey(basics.Address{1})...)
	require.NoError(t, s.DB().Set(binary.BigEndian.AppendUint64(key, 7), []byte{'D'}, nil))
	require.NoError(t, s.Close())
	b := buildStore(t, timetravel.Options{}, 600, nil)
	c := compareFull(t, a, b)
	require.Zero(t, c.diffs)
	require.Equal(t, 501, c.roundsChecked)
	require.Equal(t, 51, c.objsChecked) // 50 accounts and the totals
	require.Positive(t, c.versionsChecked)

	// the same the other way around
	c = compareFull(t, b, a)
	require.Zero(t, c.diffs)
}

func TestFullFindsDifferences(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	a := buildStore(t, timetravel.Options{}, 500, nil)
	b := buildStore(t, timetravel.Options{}, 500, func(rnd uint64) []timetravel.Change {
		switch rnd {
		case 250: // one account diverges for a round
			return []timetravel.Change{{Key: timetravel.AccountKey(basics.Address{5}), Value: []byte("x")}}
		case 300: // an object only B has
			return []timetravel.Change{{Key: timetravel.AccountKey(basics.Address{2}), Value: []byte("y")}}
		case 400: // an account deleted in B only
			return []timetravel.Change{{Key: timetravel.AccountKey(basics.Address{10}), Deleted: true}}
		}
		return nil
	})
	c := compareFull(t, a, b)
	// 3 change digests and 3 objects
	require.Equal(t, 6, c.diffs)
	require.Equal(t, 51, c.objsChecked-1) // and B's extra account
}
