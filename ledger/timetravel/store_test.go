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
	"fmt"
	"math/rand"
	"sort"
	"testing"

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

// TestStoreModel checks every object at every round against an in-memory model.
func TestStoreModel(t *testing.T) {
	partitiontest.PartitionTest(t)
	for _, o := range []Options{{CheckpointInterval: 1}, {CheckpointInterval: 4}, {CheckpointInterval: 4, Incremental: true}, {CheckpointInterval: 7}} {
		t.Run(fmt.Sprintf("N=%d/inc=%v", o.CheckpointInterval, o.Incremental), func(t *testing.T) {
			dir := t.TempDir()
			s, err := Open(dir, o)
			require.NoError(t, err)
			rng := rand.New(rand.NewSource(2))
			objs := [][]byte{TotalsKey}
			for i := 0; i < 30; i++ {
				objs = append(objs, KvKey(fmt.Sprintf("bx:\x00\x01%c", 'a'+i%5)+string(make([]byte, i%3))))
			}
			state := map[string][]byte{}
			var history []map[string][]byte
			const rounds = 40
			for r := uint64(0); r < rounds; r++ {
				var chs []Change
				for _, obj := range objs {
					if rng.Intn(3) != 0 {
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
				var expired *uint64 // odd rounds carry an expired stake
				if r%2 == 1 {
					e := r << 40
					expired = &e
				}
				require.NoError(t, s.AddRound(r, chs, []byte{byte(r)}, expired))
				snap := map[string][]byte{}
				for k, v := range state {
					snap[k] = v
				}
				history = append(history, snap)
			}
			require.NoError(t, s.AddRound(5, nil, nil, nil)) // replay is skipped
			require.Error(t, s.AddRound(rounds+1, nil, nil, nil))
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
		})
	}
}
