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
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"slices"
	"sort"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestPatchRoundTrip(t *testing.T) {
	partitiontest.PartitionTest(t)
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 5000; i++ {
		base := make([]byte, rng.Intn(3000))
		rng.Read(base)
		target := append([]byte(nil), base...)
		for e := rng.Intn(6); e > 0 && len(target) > 0; e-- {
			at := rng.Intn(len(target))
			ins := make([]byte, rng.Intn(20))
			rng.Read(ins)
			cut := min(at+rng.Intn(20), len(target))
			target = append(target[:at], append(ins, target[cut:]...)...)
		}
		if rng.Intn(10) == 0 {
			target = make([]byte, rng.Intn(100))
			rng.Read(target)
		}
		p := Diff(base, target)
		got, err := Apply(base, p)
		require.NoError(t, err)
		require.Equal(t, target, got)
		if len(base) > 1000 && len(target) == len(base) {
			require.Less(t, len(p), 200, "single-region edits must patch small")
		}
	}
	_, err := Apply([]byte("abc"), []byte{3, 7, 9})
	require.Error(t, err)
}

func TestKvKeyOrder(t *testing.T) {
	partitiontest.PartitionTest(t)
	keys := []string{"", "\x00", "\x00\x00", "\x00\x01", "a", "a\x00", "a\x00b", "a\x01", "ab", "b"}
	for i := range keys {
		require.Equal(t, keys[i], KvKeyString(KvKey(keys[i])))
		if i > 0 {
			require.Negative(t, bytes.Compare(KvKey(keys[i-1]), KvKey(keys[i])))
		}
		require.True(t, bytes.HasPrefix(KvKey(keys[i]+"x"), KvPrefix(keys[i])))
	}
}

// modelObjects returns the totals and 30 kv keys, some of them prefixes of others.
func modelObjects() [][]byte {
	objs := [][]byte{TotalsKey}
	for i := 0; i < 30; i++ {
		objs = append(objs, KvKey(fmt.Sprintf("bx:\x00\x01%c", 'a'+i%5)+string(make([]byte, i%3))))
	}
	return objs
}

// modelRounds returns rounds of random changes to objs, a tenth of them
// no-ops (the same value, or deleting an absent object), and the state after
// each round.
func modelRounds(seed int64, objs [][]byte, rounds int) ([][]Change, []map[string][]byte) {
	rng := rand.New(rand.NewSource(seed))
	state := map[string][]byte{}
	var changes [][]Change
	var history []map[string][]byte
	for r := 0; r < rounds; r++ {
		var chs []Change
		for _, obj := range objs {
			if rng.Intn(3) != 0 {
				continue
			}
			if rng.Intn(10) == 0 { // no-op write: the same value, or deleting an absent object
				if v, ok := state[string(obj)]; ok {
					chs = append(chs, Change{Key: obj, Value: v})
				} else {
					chs = append(chs, Change{Key: obj, Deleted: true})
				}
				continue
			}
			if _, ok := state[string(obj)]; ok && rng.Intn(5) == 0 {
				delete(state, string(obj))
				chs = append(chs, Change{Key: obj, Deleted: true})
				continue
			}
			v := append([]byte(nil), state[string(obj)]...)
			if len(v) == 0 {
				v = make([]byte, 50+rng.Intn(200))
				rng.Read(v)
			} else {
				v[rng.Intn(len(v))]++
			}
			state[string(obj)] = v
			chs = append(chs, Change{Key: obj, Value: v})
		}
		changes = append(changes, chs)
		history = append(history, maps.Clone(state))
	}
	return changes, history
}

func cloneChanges(chs []Change) []Change {
	out := make([]Change, len(chs))
	for i, ch := range chs {
		out[i] = Change{Key: bytes.Clone(ch.Key), Value: bytes.Clone(ch.Value), Deleted: ch.Deleted}
	}
	return out
}

// rawRecords returns the store's checkpoint records in key order.
func rawRecords(t *testing.T, s *Store) (keys, vals [][]byte) {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{pfxCheckpoint}, UpperBound: []byte{pfxCheckpoint + 1}})
	require.NoError(t, err)
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		keys = append(keys, bytes.Clone(it.Key()))
		vals = append(vals, bytes.Clone(it.Value()))
	}
	require.NoError(t, it.Error())
	return keys, vals
}

// recordObject returns the object of a record key.
func recordObject(k []byte) string { return string(k[1 : len(k)-8]) }

// TestStoreModel checks every object at every round against an in-memory
// model, with and without the dedup cache.
func TestStoreModel(t *testing.T) {
	partitiontest.PartitionTest(t)
	objs := modelObjects()
	const rounds = 40
	changes, history := modelRounds(2, objs, rounds)
	// A store has a record per object changed in a round (some objects
	// appear twice in objs). One that remembers every object keeps those
	// that change the object's state, or are its first.
	written := map[string]bool{}
	var total, kept int
	for r, chs := range changes {
		var before map[string][]byte
		if r > 0 {
			before = history[r-1]
		}
		inRound := map[string]bool{}
		for _, ch := range chs {
			inRound[string(ch.Key)] = true
		}
		for k := range inRound {
			v, ok := before[k]
			after, afterOK := history[r][k]
			if ok != afterOK || !bytes.Equal(v, after) || !written[k] {
				kept++
			}
			total++
			written[k] = true
		}
	}
	require.Less(t, kept, total)
	var variants []Options
	for _, o := range []Options{{CheckpointInterval: 1}, {CheckpointInterval: 4}, {CheckpointInterval: 4, Incremental: true}, {CheckpointInterval: 7}} {
		// the default cache; one that turns over every round or two, and is
		// lost halfway by a reopen; none
		for _, dedup := range []int{0, 24, -1} {
			o.DedupEntries = dedup
			variants = append(variants, o)
		}
	}
	for _, o := range variants {
		t.Run(fmt.Sprintf("N=%d/inc=%v/dedup=%d", o.CheckpointInterval, o.Incremental, o.DedupEntries), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, o)
			require.NoError(t, err)
			rng := rand.New(rand.NewSource(2))
			for r := uint64(0); r < rounds; r++ {
				if o.DedupEntries > 0 && r == rounds/2 {
					require.NoError(t, s.Close())
					s, err = Open(dir, o)
					require.NoError(t, err)
				}
				var expired *uint64 // odd rounds carry an expired stake
				if r%2 == 1 {
					e := r << 40
					expired = &e
				}
				in := cloneChanges(changes[r])
				require.NoError(t, s.AddRound(r, changes[r], []byte{byte(r)}, expired))
				require.Equal(t, in, changes[r], "AddRound modified its changes")
			}
			require.NoError(t, s.AddRound(5, nil, nil, nil)) // replay is skipped
			require.Error(t, s.AddRound(rounds+1, nil, nil, nil))
			if o.CheckpointInterval == 1 {
				keys, vals := rawRecords(t, s)
				switch o.DedupEntries {
				case -1:
					require.Len(t, keys, total)
				case 0:
					require.Len(t, keys, kept)
					for i := 1; i < len(keys); i++ {
						if recordObject(keys[i]) == recordObject(keys[i-1]) {
							require.NotEqual(t, vals[i-1], vals[i], "record %x repeats the one before", keys[i])
						}
					}
				}
			}
			require.NoError(t, s.Close())

			s, err = Open(dir, Options{CheckpointInterval: 99})
			require.NoError(t, err)
			defer s.Close()
			require.Equal(t, o.CheckpointInterval, s.CheckpointInterval())
			last, ok := s.Last()
			require.True(t, ok)
			require.EqualValues(t, rounds-1, last)
			for r := uint64(0); r < rounds; r++ {
				for _, obj := range objs {
					v, ok, err := s.Get(obj, r)
					require.NoError(t, err)
					want, wantOK := history[r][string(obj)]
					require.Equal(t, wantOK, ok, "round %d obj %q", r, obj)
					require.Equal(t, want, v)
				}
				h, ok, err := s.Header(r)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, []byte{byte(r)}, h)
				d, ok, err := s.Digest(r)
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, ChangesDigest(changes[r]), d, "the digest covers every change")
				e, ok, err := s.ExpiredStake(r)
				require.NoError(t, err)
				require.Equal(t, r%2 == 1, ok)
				if ok {
					require.Equal(t, r<<40, e)
				}

				var want, got []string
				for k := range history[r] {
					if bytes.HasPrefix([]byte(k), KvPrefix("bx:\x00\x01c")) {
						want = append(want, k)
					}
				}
				sort.Strings(want)
				require.NoError(t, s.Scan(KvPrefix("bx:\x00\x01c"), nil, r, func(obj, val []byte) bool {
					got = append(got, string(obj))
					require.Equal(t, history[r][string(obj)], val)
					return true
				}))
				require.Equal(t, want, got)
				if len(want) > 1 {
					got = got[:0]
					require.NoError(t, s.Scan(KvPrefix("bx:\x00\x01c"), []byte(want[0]), r, func(obj, val []byte) bool {
						got = append(got, string(obj))
						return len(got) < 1
					}))
					require.Equal(t, want[1:2], got)
				}
			}

			var wantObjs, gotObjs []string
			for k := range written {
				if bytes.HasPrefix([]byte(k), KvPrefix("bx:\x00\x01c")) {
					wantObjs = append(wantObjs, k)
				}
			}
			sort.Strings(wantObjs)
			require.NoError(t, s.Objects(KvPrefix("bx:\x00\x01c"), func(obj []byte) bool {
				gotObjs = append(gotObjs, string(obj))
				return true
			}))
			require.Equal(t, wantObjs, gotObjs)

			// versions: the rounds at which an object's state differs from the round before
			type version struct {
				rnd uint64
				val []byte
				ok  bool
			}
			for _, obj := range objs {
				var all []version
				var prev []byte
				var prevOK bool
				for r := uint64(0); r < rounds; r++ {
					v, ok := history[r][string(obj)]
					if ok != prevOK || (ok && !bytes.Equal(v, prev)) {
						all = append(all, version{r, v, ok})
					}
					prev, prevOK = v, ok
				}
				ranges := [][2]uint64{{0, rounds}, {0, 0}, {rounds - 1, rounds - 1}, {5, 4}}
				for i := 0; i < 20; i++ {
					lo := uint64(rng.Intn(rounds))
					ranges = append(ranges, [2]uint64{lo, lo + uint64(rng.Intn(rounds))})
				}
				for _, rg := range ranges {
					for _, desc := range []bool{false, true} {
						var want, got []version
						for _, v := range all {
							if v.rnd >= rg[0] && v.rnd <= rg[1] {
								want = append(want, v)
							}
						}
						if desc {
							slices.Reverse(want)
						}
						require.NoError(t, s.Versions(obj, rg[0], rg[1], desc, func(rnd uint64, val []byte, ok bool) bool {
							got = append(got, version{rnd, val, ok})
							return true
						}))
						require.Equal(t, want, got, "obj %q range %v desc %v", obj, rg, desc)
						if len(want) > 2 {
							got = got[:0]
							require.NoError(t, s.Versions(obj, rg[0], rg[1], desc, func(rnd uint64, val []byte, ok bool) bool {
								got = append(got, version{rnd, val, ok})
								return len(got) < 2
							}))
							require.Equal(t, want[:2], got)
						}
					}
				}
			}
		})
	}
}

func TestLastWritten(t *testing.T) {
	partitiontest.PartitionTest(t)
	set := func(k, v string) Change { return Change{Key: []byte(k), Value: []byte(v)} }
	del := func(k string) Change { return Change{Key: []byte(k), Deleted: true} }
	keys := func(chs []Change) (out []string) {
		for _, ch := range chs {
			out = append(out, string(ch.Key))
		}
		return out
	}

	var off *lastWritten
	chs := []Change{set("a", "1")}
	write, states := off.filter(chs)
	require.Equal(t, chs, write)
	off.update(states) // no-op

	lw := newLastWritten(8) // generations of 4
	write, states = lw.filter([]Change{set("a", "1"), del("b"), set("c", "")})
	require.Equal(t, []string{"a", "b", "c"}, keys(write)) // nothing known yet
	lw.update(states)
	write, states = lw.filter([]Change{set("a", "1"), del("b"), set("c", ""), set("c", "x"), set("d", "1")})
	require.Equal(t, []string{"c", "d"}, keys(write), "repeats are skipped, an empty value is not a deletion")
	lw.update(states)
	require.Len(t, lw.old, 4, "cur turned over")
	require.Empty(t, lw.cur)

	// a key changed twice in a round counts with its last change
	write, states = lw.filter([]Change{set("a", "2"), del("d"), set("a", "1"), del("d")})
	require.Equal(t, []Change{del("d")}, write)
	lw.update(states)
	s, ok := lw.get([]byte("a"))
	require.True(t, ok)
	require.Equal(t, sumOf(set("a", "1")), s, "promoted to cur with its last state")
	_, ok = lw.cur["c"]
	require.False(t, ok)
	write, _ = lw.filter([]Change{set("c", "x")})
	require.Empty(t, write, "found in old")

	// a round larger than the cache forgets everything
	var big []Change
	for i := 0; i < 20; i++ {
		big = append(big, set(fmt.Sprint(i), "v"))
	}
	_, states = lw.filter(big)
	lw.update(states)
	require.Empty(t, lw.cur)
	require.Empty(t, lw.old)
}

// TestStoreFailedCommit checks that a round whose batch failed to commit is
// stored in full when the tracker adds it again.
func TestStoreFailedCommit(t *testing.T) {
	partitiontest.PartitionTest(t)
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	x := KvKey("x")
	round := func(v string) []Change {
		return []Change{{Key: x, Value: []byte(v)}, {Key: TotalsKey, Value: []byte("t")}}
	}
	require.NoError(t, s.AddRound(0, round("v1"), nil, nil))

	defer func(f func(*pebble.Batch) error) { commitBatch = f }(commitBatch)
	commitBatch = func(*pebble.Batch) error { return errors.New("injected") }
	require.Error(t, s.AddRound(1, round("v2"), nil, nil))
	last, _ := s.Last()
	require.Zero(t, last)
	commitBatch = func(b *pebble.Batch) error { return b.Commit(pebble.NoSync) }

	require.NoError(t, s.AddRound(1, round("v2"), nil, nil))
	v, ok, err := s.Get(x, 1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte("v2"), v)
	require.NoError(t, s.AddRound(2, round("v2"), nil, nil))
	keys, _ := rawRecords(t, s)
	require.Len(t, keys, 3, "x at 0 and 1, the totals at 0")
}

func TestStoreRemoveNoops(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3 // commit while scanning

	objs := modelObjects()
	const rounds = 60
	changes, history := modelRounds(3, objs, rounds)
	// tombstones before any value: an object deleted twice before it exists,
	// and one that never exists
	pfx := KvPrefix("bx:\x00\x01c")
	late, phantom := KvKey("bx:\x00\x01c-late"), KvKey("bx:\x00\x01c-phantom")
	changes[3] = append(changes[3], Change{Key: late, Deleted: true}, Change{Key: phantom, Deleted: true})
	changes[4] = append(changes[4], Change{Key: late, Deleted: true})
	changes[9] = append(changes[9], Change{Key: phantom, Deleted: true})
	changes[10] = append(changes[10], Change{Key: late, Value: []byte("v")})
	for r := 10; r < rounds; r++ {
		history[r][string(late)] = []byte("v")
	}
	objs = append(objs, late, phantom)
	slices.SortFunc(objs, bytes.Compare)
	objs = slices.CompactFunc(objs, bytes.Equal)

	dir := t.TempDir()
	s, err := Open(dir, Options{DedupEntries: -1})
	require.NoError(t, err)
	for r, chs := range changes {
		require.NoError(t, s.AddRound(uint64(r), chs, []byte{byte(r)}, nil))
	}
	// every answer a reader can get, and the number of versions
	answers := func(s *Store) (out []string, versions int) {
		for _, obj := range objs {
			for r := uint64(0); r < rounds; r++ {
				v, ok, err := s.Get(obj, r)
				require.NoError(t, err)
				require.Equal(t, history[r][string(obj)], v)
				out = append(out, fmt.Sprintf("get %x %d %v %x", obj, r, ok, v))
			}
			for _, desc := range []bool{false, true} {
				require.NoError(t, s.Versions(obj, 0, rounds, desc, func(rnd uint64, val []byte, ok bool) bool {
					out = append(out, fmt.Sprintf("version %x %d %v %x", obj, rnd, ok, val))
					versions++
					return true
				}))
			}
		}
		for r := uint64(0); r < rounds; r++ {
			require.NoError(t, s.Scan(pfx, nil, r, func(obj, val []byte) bool {
				out = append(out, fmt.Sprintf("scan %d %x %x", r, obj, val))
				return true
			}))
		}
		return out, versions / 2
	}
	objects := func(s *Store) (out []string) {
		require.NoError(t, s.Objects(pfx, func(obj []byte) bool {
			out = append(out, string(obj))
			return true
		}))
		return out
	}
	want, versions := answers(s)
	wantObjs := objects(s)
	require.Contains(t, wantObjs, string(phantom))
	before, _ := rawRecords(t, s)
	require.Greater(t, len(before), versions)

	var calls int
	removed, err := s.RemoveNoops(context.Background(), func(scanned, removed uint64) { calls++ })
	require.NoError(t, err)
	require.EqualValues(t, len(before)-versions, removed)
	require.Greater(t, calls, 1)
	require.NoError(t, s.CompactCheckpoints(context.Background()))
	require.NoError(t, s.Close())

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	got, _ := answers(s)
	require.Equal(t, want, got)
	require.Equal(t, slices.DeleteFunc(wantObjs, func(o string) bool { return o == string(phantom) }), objects(s))
	keys, vals := rawRecords(t, s)
	require.Len(t, keys, versions, "every record is a version")
	for i := range keys {
		if i == 0 || recordObject(keys[i]) != recordObject(keys[i-1]) {
			require.NotEqual(t, []byte{recDeleted}, vals[i], "record %x is a tombstone before any value", keys[i])
		} else {
			require.NotEqual(t, vals[i-1], vals[i], "record %x repeats the one before", keys[i])
		}
	}
	for r := uint64(0); r < rounds; r++ {
		d, ok, err := s.Digest(r)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, ChangesDigest(changes[r]), d)
	}

	p, err := Open(t.TempDir(), Options{CheckpointInterval: 4})
	require.NoError(t, err)
	defer p.Close()
	_, err = p.RemoveNoops(context.Background(), nil)
	require.Error(t, err)
}
