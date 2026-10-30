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
	"errors"
	"fmt"
	"maps"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"sync"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestStoreRefusesCheckpointInterval checks that a store created by a build
// with a checkpoint interval above 1 does not open, and that one with N = 1
// does, whatever its incremental flag.
func TestStoreRefusesCheckpointInterval(t *testing.T) {
	partitiontest.PartitionTest(t)
	for _, c := range []struct {
		n   uint64
		inc byte
	}{{4, 0}, {3, 1}} {
		dir := t.TempDir()
		s, err := Open(dir, Options{})
		require.NoError(t, err)
		require.NoError(t, s.AddRound(0, []Change{{Key: KvKey("x"), Value: []byte("v")}}, []byte("hdr"), nil))
		meta := binary.BigEndian.AppendUint64(nil, 0)
		meta = binary.BigEndian.AppendUint64(meta, c.n)
		require.NoError(t, s.DB().Set(keyMeta, append(meta, c.inc), pebble.Sync))
		require.NoError(t, s.Close())
		for _, ro := range []bool{false, true} {
			_, err = Open(dir, Options{ReadOnly: ro})
			require.ErrorContains(t, err, "no longer supported", "N %d, incremental %d, read-only %v", c.n, c.inc, ro)
		}
	}
	// N = 1 with the incremental flag (ttlab ingest -n 1 -inc) holds full
	// records only
	dir := t.TempDir()
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	require.NoError(t, s.AddRound(0, []Change{{Key: KvKey("x"), Value: []byte("v")}}, []byte("hdr"), nil))
	require.NoError(t, s.DB().Set(keyMeta, append(binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, 0), 1), 1), pebble.Sync))
	require.NoError(t, s.Close())
	s, err = Open(dir, Options{})
	require.NoError(t, err)
	v, ok, err := s.Get(KvKey("x"), 0)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "v", string(v))
	require.NoError(t, s.AddRound(1, nil, []byte("hdr"), nil))
	require.NoError(t, s.Close())

	// an N = 1 store's meta record is as those builds wrote it
	s, err = Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	require.NoError(t, s.AddRound(0, nil, []byte("hdr"), nil))
	meta, closer, err := s.DB().Get(keyMeta)
	require.NoError(t, err)
	defer closer.Close()
	require.Equal(t, []byte{0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0}, meta)
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

// modelApps are the apps of the boxes of modelObjects, and one without any.
// Their ids have zero bytes, which kv keys escape.
var modelApps = []uint64{1, 1<<56 | 1, 2}

// modelObjects returns the totals, 30 kv keys too short to be boxes, some of
// them prefixes of others, and the boxes of two apps.
func modelObjects() [][]byte {
	objs := [][]byte{TotalsKey}
	for i := 0; i < 30; i++ {
		objs = append(objs, KvKey(fmt.Sprintf("bx:\x00\x01%c", 'a'+i%5)+string(make([]byte, i%3))))
	}
	for _, app := range modelApps[:2] {
		for _, name := range []string{"", "\x00", "a", "a\x00b", "b", "a"} {
			objs = append(objs, boxObject(app, []byte(name)))
		}
	}
	return objs
}

// boxChange is a change BoxChanges reports.
type boxChange struct {
	rnd  uint64
	name string
	val  []byte
	ok   bool
}

// modelBoxChanges returns the changes of the boxes of app in [lo, hi]: the
// rounds at which a box's state differs from the round before.
func modelBoxChanges(objs [][]byte, history []map[string][]byte, app, lo, hi uint64, desc bool) []boxChange {
	return modelPrefixChanges(objs, history, app, lo, hi, desc, nil)
}

// modelPrefixChanges is modelBoxChanges for the boxes whose names start with
// prefix.
func modelPrefixChanges(objs [][]byte, history []map[string][]byte, app, lo, hi uint64, desc bool, prefix []byte) []boxChange {
	var names []string
	for _, obj := range objs {
		if a, name, ok := BoxOf(obj); ok && a == app && bytes.HasPrefix(name, prefix) {
			names = append(names, string(name))
		}
	}
	slices.Sort(names)
	names = slices.Compact(names)
	var out []boxChange
	for r := lo; r <= hi && r < uint64(len(history)); r++ {
		for _, name := range names {
			k := string(boxObject(app, []byte(name)))
			v, ok := history[r][k]
			var pv []byte
			var pok bool
			if r > 0 {
				pv, pok = history[r-1][k]
			}
			if ok != pok || (ok && !bytes.Equal(v, pv)) {
				out = append(out, boxChange{r, name, v, ok})
			}
		}
	}
	if desc {
		slices.Reverse(out)
	}
	return out
}

// boxChanges returns what BoxChanges reports, up to limit changes (0: all).
func boxChanges(t *testing.T, s *Store, app, lo, hi uint64, desc bool, limit int) []boxChange {
	var out []boxChange
	end, err := s.BoxChanges(app, lo, hi, desc, BoxChangesOpts{}, func(rnd uint64, name, val []byte, ok bool) bool {
		out = append(out, boxChange{rnd, string(name), val, ok})
		return limit == 0 || len(out) < limit
	})
	require.NoError(t, err)
	require.False(t, end.More)
	return out
}

// pagedBoxChanges reads BoxChanges with o, resuming from each stop, and
// returns every change and the error that ended the last scan. With
// MaxScanned 1, a scan reads a single round.
func pagedBoxChanges(t *testing.T, s *Store, app, lo, hi uint64, desc bool, o BoxChangesOpts) ([]boxChange, error) {
	var out []boxChange
	for {
		n := len(out)
		end, err := s.BoxChanges(app, lo, hi, desc, o, func(rnd uint64, name, val []byte, ok bool) bool {
			out = append(out, boxChange{rnd, string(name), val, ok})
			return true
		})
		if err != nil || !end.More {
			return out, err
		}
		require.Positive(t, end.Scanned)
		require.GreaterOrEqual(t, end.Scanned, o.MaxScanned, "a scan stops once the budget is spent")
		if o.MaxScanned == 1 {
			for _, c := range out[n:] {
				require.Equal(t, out[n].rnd, c.rnd, "one round a scan")
			}
		}
		if len(out) > n {
			last := out[len(out)-1].rnd
			if desc {
				require.Less(t, end.Next, last)
			} else {
				require.Greater(t, end.Next, last)
			}
		}
		if desc {
			require.Less(t, end.Next, hi)
			hi = end.Next
		} else {
			require.Greater(t, end.Next, lo)
			lo = end.Next
		}
	}
}

// boxPrefixes are name prefixes of the model's boxes: names that share
// prefixes, contain 0x00, and one ("a") that is also a whole name.
var boxPrefixes = [][]byte{[]byte("\x00"), []byte("a"), []byte("a\x00"), []byte("a\x00b"), []byte("b"), []byte("zz")}

// checkBoxPrefixes compares BoxChanges with a name prefix and a scan budget
// with the model over the ranges.
func checkBoxPrefixes(t *testing.T, s *Store, objs [][]byte, history []map[string][]byte, app uint64, ranges [][2]uint64) {
	for _, rg := range ranges {
		for _, desc := range []bool{false, true} {
			for _, budget := range []uint64{0, 1, 2} {
				want := modelBoxChanges(objs, history, app, rg[0], rg[1], desc)
				got, err := pagedBoxChanges(t, s, app, rg[0], rg[1], desc, BoxChangesOpts{MaxScanned: budget})
				require.NoError(t, err)
				require.Equal(t, want, got, "app %d range %v desc %v budget %d", app, rg, desc, budget)
				for _, pfx := range boxPrefixes {
					want := modelPrefixChanges(objs, history, app, rg[0], rg[1], desc, pfx)
					got, err := pagedBoxChanges(t, s, app, rg[0], rg[1], desc, BoxChangesOpts{Prefix: pfx, MaxScanned: budget})
					require.NoError(t, err)
					require.Equal(t, want, got, "app %d range %v desc %v prefix %q budget %d", app, rg, desc, pfx, budget)
				}
			}
		}
	}
}

// checkBoxChanges compares BoxChanges with the model over random ranges.
func checkBoxChanges(t *testing.T, s *Store, objs [][]byte, history []map[string][]byte, rng *rand.Rand) {
	rounds := uint64(len(history))
	ranges := [][2]uint64{{0, rounds}, {0, 0}, {rounds - 1, rounds - 1}, {5, 4}}
	for i := 0; i < 20; i++ {
		lo := uint64(rng.Intn(int(rounds)))
		ranges = append(ranges, [2]uint64{lo, lo + uint64(rng.Intn(int(rounds)))})
	}
	for _, app := range modelApps {
		for _, rg := range ranges {
			for _, desc := range []bool{false, true} {
				want := modelBoxChanges(objs, history, app, rg[0], rg[1], desc)
				require.Equal(t, want, boxChanges(t, s, app, rg[0], rg[1], desc, 0), "app %d range %v desc %v", app, rg, desc)
				if len(want) > 2 {
					require.Equal(t, want[:2], boxChanges(t, s, app, rg[0], rg[1], desc, 2))
				}
			}
		}
		if app != modelApps[2] {
			require.NotEmpty(t, modelBoxChanges(objs, history, app, 0, rounds, false))
		}
		checkBoxPrefixes(t, s, objs, history, app, ranges[:6])
	}
	for _, pfx := range boxPrefixes[:5] {
		require.NotEmpty(t, modelPrefixChanges(objs, history, modelApps[0], 0, rounds, false, pfx), "prefix %q", pfx)
	}
}

// boxIndexEntries returns the number of entries in the box index.
func boxIndexEntries(t *testing.T, s *Store) int {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{pfxBoxIndex}, UpperBound: []byte{pfxBoxIndex + 1}})
	require.NoError(t, err)
	defer it.Close()
	n := 0
	for it.First(); it.Valid(); it.Next() {
		n++
	}
	require.NoError(t, it.Error())
	return n
}

// boxVersions returns the number of versions of the boxes in objs.
func boxVersions(t *testing.T, s *Store, objs [][]byte) int {
	n := 0
	seen := map[string]bool{}
	for _, obj := range objs {
		if _, _, ok := BoxOf(obj); !ok || seen[string(obj)] {
			continue
		}
		seen[string(obj)] = true
		require.NoError(t, s.Versions(obj, 0, ^uint64(0)-1, false, func(uint64, []byte, bool) bool { n++; return true }))
	}
	return n
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

// rawRecords returns the store's version records in key order.
func rawRecords(t *testing.T, s *Store) (keys, vals [][]byte) {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{pfxRecord}, UpperBound: []byte{pfxRecord + 1}})
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
	// the default cache; one that turns over every round or two, and is lost
	// halfway by a reopen; none
	for _, dedup := range []int{0, 24, -1} {
		variants = append(variants, Options{DedupEntries: dedup})
	}
	for _, o := range variants {
		t.Run(fmt.Sprintf("dedup=%d", o.DedupEntries), func(t *testing.T) {
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
			require.NoError(t, s.Close())

			s, err = Open(dir, Options{})
			require.NoError(t, err)
			defer s.Close()
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
				rnd    uint64
				val    []byte
				ok     bool
				prev   []byte // VersionsPrev: the state at rnd-1
				prevOK bool
			}
			for _, obj := range objs {
				var all []version
				var prev []byte
				var prevOK bool
				for r := uint64(0); r < rounds; r++ {
					v, ok := history[r][string(obj)]
					if ok != prevOK || (ok && !bytes.Equal(v, prev)) {
						all = append(all, version{r, v, ok, prev, prevOK})
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
						var want, wantPrev, got []version
						for _, v := range all {
							if v.rnd >= rg[0] && v.rnd <= rg[1] {
								wantPrev = append(wantPrev, v)
								v.prev, v.prevOK = nil, false
								want = append(want, v)
							}
						}
						if desc {
							slices.Reverse(want)
							slices.Reverse(wantPrev)
						}
						require.NoError(t, s.Versions(obj, rg[0], rg[1], desc, func(rnd uint64, val []byte, ok bool) bool {
							got = append(got, version{rnd: rnd, val: val, ok: ok})
							return true
						}))
						require.Equal(t, want, got, "obj %q range %v desc %v", obj, rg, desc)
						got = got[:0]
						require.NoError(t, s.VersionsPrev(obj, rg[0], rg[1], desc, func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
							if !prevOK {
								prev = nil
							}
							got = append(got, version{rnd, val, ok, prev, prevOK})
							return true
						}))
						require.Equal(t, wantPrev, got, "VersionsPrev: obj %q range %v desc %v", obj, rg, desc)
						if len(want) > 2 {
							got = got[:0]
							require.NoError(t, s.Versions(obj, rg[0], rg[1], desc, func(rnd uint64, val []byte, ok bool) bool {
								got = append(got, version{rnd: rnd, val: val, ok: ok})
								return len(got) < 2
							}))
							require.Equal(t, want[:2], got)
						}
					}
				}
			}

			from, ok := s.BoxIndexFrom()
			require.True(t, ok)
			require.Zero(t, from, "a new store indexes its boxes from the start")
			checkBoxChanges(t, s, objs, history, rng)
		})
	}
}

// TestStoreIndexBoxes checks that a store written without the box index
// serves box changes once IndexBoxes builds it, and from the rounds added
// since meanwhile.
func TestStoreIndexBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 3 // commit while scanning

	objs := modelObjects()
	const rounds = 40
	changes, history := modelRounds(4, objs, rounds)
	for _, o := range []Options{{}, {DedupEntries: -1}} {
		t.Run(fmt.Sprintf("dedup=%d", o.DedupEntries), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, o)
			require.NoError(t, err)
			for r := uint64(0); r < rounds/2; r++ {
				require.NoError(t, s.AddRound(r, changes[r], nil, nil))
			}
			// as a build without the index leaves it
			require.NoError(t, s.DB().DeleteRange([]byte{pfxBoxIndex}, []byte{pfxBoxIndex + 1}, nil))
			require.NoError(t, s.DB().Delete(keyBoxIndexMeta, nil))
			require.NoError(t, s.Close())

			s, err = Open(dir, o)
			require.NoError(t, err)
			from, ok := s.BoxIndexFrom()
			require.True(t, ok)
			require.EqualValues(t, rounds/2, from, "the index starts with the next round")
			for r := uint64(rounds / 2); r < rounds; r++ {
				require.NoError(t, s.AddRound(r, changes[r], nil, nil))
				if r == rounds*3/4 {
					// a build without the index adds a round
					require.NoError(t, s.DB().Set(keyBoxIndexMeta, boxIndexMeta(from, r), nil))
					require.NoError(t, s.Close())
					s, err = Open(dir, o)
					require.NoError(t, err)
					from, _ = s.BoxIndexFrom()
					require.EqualValues(t, r+1, from, "rounds a build without the index added are not indexed")
				}
			}
			_, err = s.BoxChanges(modelApps[0], from-1, rounds, false, BoxChangesOpts{}, func(uint64, []byte, []byte, bool) bool { return true })
			require.ErrorIs(t, err, ErrNoBoxIndex)
			for _, desc := range []bool{false, true} {
				require.Equal(t, modelBoxChanges(objs, history, modelApps[0], from, rounds, desc), boxChanges(t, s, modelApps[0], from, rounds, desc, 0))
			}
			// descending, the changes since the index starts come first
			want := modelBoxChanges(objs, history, modelApps[0], from, rounds, true)
			require.NotEmpty(t, want)
			var got []boxChange
			_, err = s.BoxChanges(modelApps[0], 0, rounds, true, BoxChangesOpts{}, func(rnd uint64, name, val []byte, ok bool) bool {
				got = append(got, boxChange{rnd, string(name), val, ok})
				return true
			})
			require.ErrorIs(t, err, ErrNoBoxIndex)
			require.Equal(t, want, got)
			// so do scans with a prefix and a budget: they reach the start
			// after the later changes
			for _, budget := range []uint64{0, 1, 2} {
				pfx := []byte("a")
				got, err := pagedBoxChanges(t, s, modelApps[0], 0, rounds, true, BoxChangesOpts{Prefix: pfx, MaxScanned: budget})
				require.ErrorIs(t, err, ErrNoBoxIndex)
				require.Equal(t, modelPrefixChanges(objs, history, modelApps[0], from, rounds, true, pfx), got, "budget %d", budget)
			}
			require.Equal(t, want[:1], boxChanges(t, s, modelApps[0], 0, rounds, true, 1), "a page that stops before the start needs no more")
			// read-only, the start is the same and nothing is written
			require.NoError(t, s.Close())
			s, err = Open(dir, Options{ReadOnly: true})
			require.NoError(t, err)
			roFrom, ok := s.BoxIndexFrom()
			require.True(t, ok)
			require.Equal(t, from, roFrom)
			require.NoError(t, s.Close())
			s, err = Open(dir, o)
			require.NoError(t, err)

			var calls int
			indexed, err := s.IndexBoxes(context.Background(), func(scanned, indexed uint64) { calls++ })
			require.NoError(t, err)
			require.Greater(t, calls, 1)
			require.EqualValues(t, boxVersions(t, s, objs), indexed, "an entry per version")
			require.EqualValues(t, indexed, boxIndexEntries(t, s))
			require.NoError(t, s.CompactBoxIndex(context.Background()))
			from, ok = s.BoxIndexFrom()
			require.True(t, ok)
			require.Zero(t, from)
			checkBoxChanges(t, s, objs, history, rand.New(rand.NewSource(4)))

			// it lasts
			require.NoError(t, s.Close())
			s, err = Open(dir, o)
			require.NoError(t, err)
			defer s.Close()
			from, _ = s.BoxIndexFrom()
			require.Zero(t, from)
			again, err := s.IndexBoxes(context.Background(), nil)
			require.NoError(t, err)
			require.Equal(t, indexed, again, "idempotent")
			checkBoxChanges(t, s, objs, history, rand.New(rand.NewSource(5)))
		})
	}

	// a store without the index opened read-only: nothing to build it with
	roDir := t.TempDir()
	ro, err := Open(roDir, Options{})
	require.NoError(t, err)
	for r := uint64(0); r < 10; r++ {
		require.NoError(t, ro.AddRound(r, changes[r], nil, nil))
	}
	require.NoError(t, ro.DB().Delete(keyBoxIndexMeta, nil))
	require.NoError(t, ro.Close())
	ro, err = Open(roDir, Options{ReadOnly: true})
	require.NoError(t, err)
	roFrom, _ := ro.BoxIndexFrom()
	require.EqualValues(t, 10, roFrom)
	require.NoError(t, ro.Close())
	ro, err = Open(roDir, Options{})
	require.NoError(t, err)
	_, closer, err := ro.DB().Get(keyBoxIndexMeta)
	require.NoError(t, err, "the writable open records it")
	closer.Close()
	require.NoError(t, ro.Close())

	// an interrupted rebuild leaves the index complete from the next round
	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	for r := uint64(0); r < 10; r++ {
		require.NoError(t, s.AddRound(r, changes[r], nil, nil))
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = s.IndexBoxes(ctx, nil)
	require.ErrorIs(t, err, context.Canceled)
	from, ok := s.BoxIndexFrom()
	require.True(t, ok)
	require.EqualValues(t, 10, from)

	// a store without boxes needs no rebuild
	dir := t.TempDir()
	s2, err := Open(dir, Options{})
	require.NoError(t, err)
	require.NoError(t, s2.AddRound(0, []Change{{Key: TotalsKey, Value: []byte("t")}}, nil, nil))
	require.NoError(t, s2.DB().Delete(keyBoxIndexMeta, nil))
	require.NoError(t, s2.Close())
	s2, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s2.Close()
	from, _ = s2.BoxIndexFrom()
	require.Zero(t, from)
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

func TestStoreCompactInSteps(t *testing.T) {
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

	// flushes over separate keys, then tombstones over all of them; automatic
	// compactions may merge the tables, but several remain
	const tables, objs = 4, 100
	obj := func(i, j int) []byte { return KvKey(fmt.Sprintf("%d-%03d", i, j)) }
	for i := 0; i < tables; i++ {
		b := s.db.NewBatch()
		for j := 0; j < objs; j++ {
			require.NoError(t, b.Set(verKey(pfxRecord, obj(i, j), 0), []byte("v"), nil))
		}
		require.NoError(t, b.Commit(pebble.NoSync))
		require.NoError(t, s.db.Flush())
	}
	b := s.db.NewBatch()
	for i := 0; i < tables; i++ {
		for j := 0; j < objs; j += 2 {
			require.NoError(t, b.Delete(verKey(pfxRecord, obj(i, j), 0), nil))
		}
	}
	require.NoError(t, b.Commit(pebble.NoSync))
	require.NoError(t, s.db.Flush())

	require.NoError(t, s.CompactRecords(context.Background()))
	require.Greater(t, len(steps), 1)
	require.Equal(t, []byte{pfxBoxIndex}, steps[0][0])
	for i := 1; i < len(steps); i++ {
		require.Equal(t, steps[i-1][1], steps[i][0], "step %d starts where step %d ends", i, i-1)
	}
	require.Equal(t, []byte{pfxRecord + 1}, steps[len(steps)-1][1])
	levels, err := s.db.SSTables(pebble.WithProperties())
	require.NoError(t, err)
	var entries uint64
	for l, ts := range levels {
		for _, tb := range ts {
			require.Equal(t, len(levels)-1, l, "table %d is above the bottom level", tb.FileNum)
			require.Zero(t, tb.Properties.NumDeletions, "table %d keeps tombstones", tb.FileNum)
			entries += tb.Properties.NumEntries
		}
	}
	keys, _ := rawRecords(t, s)
	require.Len(t, keys, tables*objs/2)
	it, err := s.db.NewIter(nil)
	require.NoError(t, err)
	var live uint64
	for it.First(); it.Valid(); it.Next() {
		live++
	}
	require.NoError(t, it.Close())
	require.Equal(t, live, entries, "the tables hold only live records")
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
		for _, app := range modelApps {
			want := modelBoxChanges(objs, history, app, 0, rounds, false)
			require.Equal(t, want, boxChanges(t, s, app, 0, rounds, false, 0))
			out = append(out, fmt.Sprint(want))
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
	require.NoError(t, s.CompactRecords(context.Background()))
	require.NoError(t, s.Close())

	s, err = Open(dir, Options{})
	require.NoError(t, err)
	defer s.Close()
	got, _ := answers(s)
	require.Equal(t, want, got)
	require.Equal(t, boxVersions(t, s, objs), boxIndexEntries(t, s), "RemoveNoops rebuilt the box index")
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
}

// TestStoreReadOnly checks that a read-only store sees every added round,
// including those only in the WAL, writes nothing, and keeps the lock.
func TestStoreReadOnly(t *testing.T) {
	partitiontest.PartitionTest(t)
	dir := t.TempDir()

	_, err := Open(filepath.Join(dir, "missing"), Options{ReadOnly: true})
	require.Error(t, err)
	_, err = os.Stat(filepath.Join(dir, "missing"))
	require.True(t, os.IsNotExist(err), "a read-only open must not create a store")

	path := filepath.Join(dir, "store")
	s, err := Open(path, Options{})
	require.NoError(t, err)
	x := KvKey("x")
	for rnd := uint64(0); rnd < 5; rnd++ {
		hdr := []byte(fmt.Sprintf("hdr%d", rnd))
		require.NoError(t, s.AddRound(rnd, []Change{{Key: x, Value: []byte(fmt.Sprintf("v%d", rnd))}}, hdr, nil))
	}
	// unflushed: the rounds are only in the WAL and the memtable
	require.NoError(t, s.Close())

	ro, err := Open(path, Options{ReadOnly: true})
	require.NoError(t, err)
	defer ro.Close()
	last, ok := ro.Last()
	require.True(t, ok)
	require.EqualValues(t, 4, last)
	for rnd := uint64(0); rnd < 5; rnd++ {
		v, ok, err := ro.Get(x, rnd)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("v%d", rnd), string(v))
		h, ok, err := ro.Header(rnd)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, fmt.Sprintf("hdr%d", rnd), string(h))
	}

	require.ErrorIs(t, ro.AddRound(5, nil, nil, nil), ErrReadOnly)
	_, err = ro.RemoveNoops(context.Background(), nil)
	require.ErrorIs(t, err, ErrReadOnly)
	require.ErrorIs(t, ro.CompactRecords(context.Background()), ErrReadOnly)
	require.ErrorIs(t, ro.Sync(), ErrReadOnly)

	_, err = Open(path, Options{ReadOnly: true})
	require.Error(t, err, "the store is locked while open")
}

// TestStoreIndexBoxesConcurrent checks that readers running while the box
// index is rebuilt get either every change or ErrNoBoxIndex.
func TestStoreIndexBoxesConcurrent(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(n uint32) { removeNoopsBatchOps = n }(removeNoopsBatchOps)
	removeNoopsBatchOps = 2

	objs := modelObjects()
	const rounds = 40
	changes, history := modelRounds(6, objs, rounds)
	s, err := Open(t.TempDir(), Options{DedupEntries: -1})
	require.NoError(t, err)
	defer s.Close()
	for r := uint64(0); r < rounds; r++ {
		require.NoError(t, s.AddRound(r, changes[r], nil, nil))
	}
	want := map[bool][]boxChange{}
	for _, desc := range []bool{false, true} {
		want[desc] = modelBoxChanges(objs, history, modelApps[0], 0, rounds, desc)
	}
	done := make(chan struct{})
	rebuilt := make(chan error, 1)
	go func() {
		defer close(done)
		for i := 0; i < 20; i++ {
			var err error
			if i%2 == 0 {
				_, err = s.IndexBoxes(context.Background(), nil)
			} else {
				_, err = s.RemoveNoops(context.Background(), nil)
			}
			if err != nil {
				rebuilt <- err
				return
			}
		}
	}()
	var answered, refused int
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		desc := (answered+refused)%2 == 1
		var got []boxChange
		_, err := s.BoxChanges(modelApps[0], 0, rounds, desc, BoxChangesOpts{}, func(rnd uint64, name, val []byte, ok bool) bool {
			got = append(got, boxChange{rnd, string(name), val, ok})
			return true
		})
		if errors.Is(err, ErrNoBoxIndex) {
			refused++
			continue
		}
		require.NoError(t, err)
		require.Equal(t, want[desc], got)
		answered++
	}
	select {
	case err := <-rebuilt:
		require.NoError(t, err)
	default:
	}
	require.Positive(t, answered)
	t.Logf("%d answered, %d refused during rebuilds", answered, refused)

	// a reader whose iterators open mid-rebuild, and that checks the index
	// once it is complete again
	started, opened, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
	errc := make(chan error, 1)
	defer func() { boxChangesOpened = nil }()
	boxChangesOpened = func() {
		close(opened)
		<-finished
	}
	go func() {
		var once sync.Once
		_, err := s.IndexBoxes(context.Background(), func(uint64, uint64) {
			once.Do(func() {
				close(started)
				<-opened
			})
		})
		errc <- err
		close(finished)
	}()
	<-started
	_, err = s.BoxChanges(modelApps[0], 0, rounds, false, BoxChangesOpts{}, func(uint64, []byte, []byte, bool) bool { return true })
	require.ErrorIs(t, err, ErrNoBoxIndex, "the iterators may lack the rebuild's last entries")
	require.NoError(t, <-errc)
	boxChangesOpened = nil
	require.Equal(t, want[false], boxChanges(t, s, modelApps[0], 0, rounds, false, 0))
}
