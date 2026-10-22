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
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestStoreVerify(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, n := range []uint64{1, 4} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			t.Parallel()
			objs := modelObjects()
			const rounds = 40
			changes, _ := modelRounds(7, objs, rounds)
			hdr := func(r int) []byte { return []byte(fmt.Sprintf("header %d", r)) }
			expired := func(r int) *uint64 {
				if r%2 == 0 {
					return nil
				}
				e := uint64(r * 1000)
				return &e
			}

			s, err := Open(t.TempDir(), Options{CheckpointInterval: n})
			require.NoError(t, err)
			defer s.Close()
			for r := range rounds {
				require.NoError(t, s.AddRound(uint64(r), cloneChanges(changes[r]), hdr(r), expired(r)))
			}

			// every stored round verifies as it was added, including
			// its no-op writes, which the dedup cache did not store
			for r := range rounds {
				require.NoError(t, s.Verify(uint64(r), changes[r], hdr(r), expired(r)), "round %d", r)
			}

			r := 30
			for len(changes[r]) == 0 {
				r++
			}
			require.ErrorIs(t, s.Verify(uint64(r), changes[r], []byte("forged"), expired(r)), ErrDiverged)

			// an evaluation with a different value (its digest differs too;
			// TestStoreVerifyTamperedValue covers a stored value changed under
			// its digest)
			forged := cloneChanges(changes[r])
			i := 0
			for forged[i].Deleted {
				i++
			}
			forged[i].Value = append(forged[i].Value, 1)
			err = s.Verify(uint64(r), forged, hdr(r), expired(r))
			require.ErrorIs(t, err, ErrDiverged)

			// a missing or extra change: caught by the digest
			require.ErrorIs(t, s.Verify(uint64(r), changes[r][1:], hdr(r), expired(r)), ErrDiverged)

			odd, even := 31, 32
			e := uint64(1)
			require.ErrorIs(t, s.Verify(uint64(odd), changes[odd], hdr(odd), &e), ErrDiverged)
			require.ErrorIs(t, s.Verify(uint64(odd), changes[odd], hdr(odd), nil), ErrDiverged)
			// no record (an older build): accepted
			require.NoError(t, s.Verify(uint64(even), changes[even], hdr(even), &e))

			require.Error(t, s.Verify(rounds, nil, hdr(rounds), nil))
		})
	}
}

// TestStoreVerifyTamperedValue changes a stored value under its unchanged
// digest, as a forged snapshot would, and checks that Verify finds it.
func TestStoreVerifyTamperedValue(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := Open(t.TempDir(), Options{})
	require.NoError(t, err)
	defer s.Close()
	obj := KvKey("bx:\x00\x00\x00\x00\x00\x00\x00\x01k")
	chs := []Change{{Key: obj, Value: []byte("real")}}
	require.NoError(t, s.AddRound(0, nil, []byte("h0"), nil))
	require.NoError(t, s.AddRound(1, cloneChanges(chs), []byte("h1"), nil))
	require.NoError(t, s.Verify(1, chs, []byte("h1"), nil))

	key := verKey(pfxCheckpoint, obj, 1)
	require.NoError(t, s.db.Set(key, append([]byte{recFull}, "fake"...), pebble.Sync))
	require.ErrorIs(t, s.Verify(1, chs, []byte("h1"), nil), ErrDiverged)

	// the header record (digest and header) is untouched, so only the value
	// check could tell
	rec, closer, err := s.db.Get(binary.BigEndian.AppendUint64([]byte{pfxHeader}, 1))
	require.NoError(t, err)
	require.Equal(t, ChangesDigest(chs), rec[:DigestLen])
	closer.Close()
}

// TestStoreVerifyCorrupt checks that records Verify can't decode count as a
// difference, so that a store can't hide forged values behind them.
func TestStoreVerifyCorrupt(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	obj := KvKey("bx:\x00\x00\x00\x00\x00\x00\x00\x01k")
	chs := []Change{{Key: obj, Value: []byte("real")}}
	e := uint64(5)
	for name, tamper := range map[string]func(s *Store){
		"expired": func(s *Store) {
			require.NoError(t, s.db.Set(binary.BigEndian.AppendUint64([]byte{pfxExpired}, 1), nil, pebble.Sync))
		},
		"tag": func(s *Store) {
			require.NoError(t, s.db.Set(verKey(pfxCheckpoint, obj, 1), []byte("Zjunk"), pebble.Sync))
		},
		"header": func(s *Store) {
			require.NoError(t, s.db.Set(binary.BigEndian.AppendUint64([]byte{pfxHeader}, 1), []byte{1, 2}, pebble.Sync))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := Open(t.TempDir(), Options{})
			require.NoError(t, err)
			defer s.Close()
			require.NoError(t, s.AddRound(0, nil, []byte("h0"), nil))
			require.NoError(t, s.AddRound(1, cloneChanges(chs), []byte("h1"), &e))
			require.NoError(t, s.Verify(1, chs, []byte("h1"), &e))
			tamper(s)
			require.ErrorIs(t, s.Verify(1, chs, []byte("h1"), &e), ErrDiverged)
			require.ErrorIs(t, s.Verify(1, chs, []byte("h1"), &e), ErrCorrupt)
			if name != "expired" {
				require.ErrorIs(t, s.VerifyChanges(1, chs), ErrDiverged)
			}
		})
	}
}
