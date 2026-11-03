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

	"github.com/cockroachdb/pebble/v2"
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
		case 250: // one account diverges until its next write, at 252
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
	require.Equal(t, 52, c.objsChecked) // 50 accounts, the totals and B's extra account
}

// record is the key of obj's record at rnd.
func record(obj []byte, rnd uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte{pfxRecord}, obj...), rnd)
}

// mutate writes to the store at path with f, past the store's API.
func mutate(t *testing.T, path string, f func(db *pebble.DB)) {
	s, err := timetravel.Open(path, timetravel.Options{})
	require.NoError(t, err)
	f(s.DB())
	require.NoError(t, s.Close())
}

func set(t *testing.T, db *pebble.DB, key []byte, val string) {
	require.NoError(t, db.Set(key, []byte(val), nil))
}

// dropHeadersBefore makes the store start at round r for commonRange.
func dropHeadersBefore(t *testing.T, db *pebble.DB, r uint64) {
	require.NoError(t, db.DeleteRange([]byte{pfxHeader}, binary.BigEndian.AppendUint64([]byte{pfxHeader}, r), nil))
}

// TestFullLaterFirstRound compares from a common first round after 0, where
// each object starts from its state at that round.
func TestFullLaterFirstRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	acct := func(b byte) []byte { return timetravel.AccountKey(basics.Address{b}) }
	kv := timetravel.KvKey("zz") // sorts after every other object
	a := buildStore(t, timetravel.Options{DedupEntries: -1}, 500, nil)
	b := buildStore(t, timetravel.Options{}, 500, nil)
	mutate(t, a, func(db *pebble.DB) {
		dropHeadersBefore(t, db, 100)
		// records before the first round only, with a repeat in A only
		set(t, db, record(acct(1), 5), "Fq")
		set(t, db, record(acct(1), 50), "Fq")
		// the last object: before the first round, and a repeat after it
		set(t, db, record(kv, 7), "Fv")
		set(t, db, record(kv, 300), "Fv")
		// deleted before the first round in A, never there in B
		set(t, db, record(acct(3), 9), "Fz")
		set(t, db, record(acct(3), 10), "D")
	})
	mutate(t, b, func(db *pebble.DB) {
		set(t, db, record(acct(1), 5), "Fq")
		set(t, db, record(kv, 99), "Fv")
		// created after the last common round
		set(t, db, record(acct(4), 900), "Fw")
		// a tombstone as its first record, after the first round
		set(t, db, record(acct(6), 200), "D")
	})
	c := compareFull(t, a, b)
	require.Zero(t, c.diffs)
	require.Equal(t, 401, c.roundsChecked)
	require.Equal(t, 53, c.objsChecked) // and acct(1) and kv
	c = compareFull(t, b, a)
	require.Zero(t, c.diffs)
	require.Equal(t, 53, c.objsChecked)

	// states at the first round that differ
	mutate(t, b, func(db *pebble.DB) {
		set(t, db, record(acct(1), 5), "Fr")
		set(t, db, record(kv, 99), "Fu")
	})
	c = compareFull(t, a, b)
	require.Equal(t, 2, c.diffs)
}

// TestFullCorrupt checks that a malformed record stops the comparison before
// it reports the other store's later records as differences.
func TestFullCorrupt(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for name, corrupt := range map[string]func(db *pebble.DB){
		"record":     func(db *pebble.DB) { set(t, db, record(timetravel.AccountKey(basics.Address{0}), 3), "Dx") },
		"object key": func(db *pebble.DB) { set(t, db, record([]byte("a-short"), 3), "F") },
		"round key": func(db *pebble.DB) { // sorts between rounds 50 and 51
			set(t, db, append(binary.BigEndian.AppendUint64([]byte{pfxHeader}, 50), 0), "")
		},
		"header record": func(db *pebble.DB) { set(t, db, binary.BigEndian.AppendUint64([]byte{pfxHeader}, 50), "short") },
		// a uvarint cut short, in a round that has no expired stake in B
		"expired stake": func(db *pebble.DB) { set(t, db, binary.BigEndian.AppendUint64([]byte{pfxExpired}, 50), "\x80") },
	} {
		t.Run(name, func(t *testing.T) {
			a := buildStore(t, timetravel.Options{}, 100, nil)
			b := buildStore(t, timetravel.Options{}, 100, nil)
			mutate(t, a, corrupt)
			c := &comparer{maxDiffs: 10, seenObjs: map[string]bool{}}
			err := c.runFull(a, b)
			require.ErrorIs(t, err, timetravel.ErrCorrupt)
			require.ErrorContains(t, err, "store A")
			require.Zero(t, c.diffs)
		})
	}
}

// TestFullChecksIndexes checks that -full finds a store whose box or holders
// index disagrees with its records, although the records agree.
func TestFullChecksIndexes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	boxKey := func(app uint64, name string) []byte {
		return timetravel.KvKey("bx:" + string(binary.BigEndian.AppendUint64(nil, app)) + name)
	}
	extra := func(rnd uint64) []timetravel.Change {
		if rnd%10 != 0 {
			return nil
		}
		return []timetravel.Change{
			{Key: boxKey(3, "box"), Value: []byte(fmt.Sprint(rnd))},
			{Key: timetravel.HoldingKey(basics.Address{byte(rnd / 10)}, 5), Value: []byte("h")},
		}
	}
	a := buildStore(t, timetravel.Options{HoldersIndex: true}, 200, extra)
	b := buildStore(t, timetravel.Options{HoldersIndex: true}, 200, extra)
	c := compareFull(t, a, b)
	require.Zero(t, c.diffs)

	// B loses the box index entry of round 100 and the holders index entry
	// of the account that opted in at round 50
	s, err := timetravel.Open(b, timetravel.Options{})
	require.NoError(t, err)
	k := binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64([]byte{'b'}, 3), 100)
	require.NoError(t, s.DB().Delete(append(k, "box"...), nil))
	k = binary.BigEndian.AppendUint64([]byte{'a'}, 5)
	addr := basics.Address{5}
	require.NoError(t, s.DB().Delete(append(k, addr[:]...), nil))
	require.NoError(t, s.Close())
	c = compareFull(t, a, b)
	require.Equal(t, 2, c.diffs)
}
