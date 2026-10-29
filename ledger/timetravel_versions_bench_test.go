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
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// BenchmarkKeyVersions reads key= pages of an app with 100,000 versions, a
// 2 KB program and 64 global keys, whose key k changes 10 times: the scan
// alone (what any range page reads), and the scan with the key's values
// decoded and compared.
func BenchmarkKeyVersions(b *testing.B) {
	const rounds = 100_000
	s, err := timetravel.Open(b.TempDir(), timetravel.Options{})
	require.NoError(b, err)
	defer s.Close()
	var creator basics.Address
	params := basics.AppParams{ApprovalProgram: make([]byte, 2048), ClearStateProgram: make([]byte, 64), GlobalState: basics.TealKeyValue{}}
	for i := range 62 {
		params.GlobalState[fmt.Sprintf("key%02d", i)] = basics.TealValue{Type: basics.TealBytesType, Bytes: fmt.Sprintf("%064d", i)}
	}
	obj := timetravel.AppParamsKey(1)
	for r := uint64(0); r < rounds; r++ {
		params.GlobalState["counter"] = basics.TealValue{Type: basics.TealUintType, Uint: r}
		if r%(rounds/10) == 0 {
			params.GlobalState["k"] = basics.TealValue{Type: basics.TealUintType, Uint: r}
		}
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r)})
		ch := timetravel.Change{Key: obj, Value: append(creator[:], protocol.Encode(&params)...)}
		require.NoError(b, s.AddRound(r, []timetravel.Change{ch}, hdr, nil))
	}
	h := &histView{store: s, last: rounds - 1}

	b.Run("scan", func(b *testing.B) {
		for b.Loop() {
			n := 0
			require.NoError(b, h.VersionsPrev(obj, 0, rounds-1, false, func(uint64, []byte, bool, []byte, bool) bool { n++; return true }))
			require.Equal(b, rounds, n)
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*rounds), "ns/version")
	})
	b.Run("key", func(b *testing.B) {
		for b.Loop() {
			p, err := keyVersions(h, obj, VersionRange{Max: rounds - 1, Limit: 1000, Key: []byte("k")})
			require.NoError(b, err)
			require.Len(b, p.Versions, 10)
		}
		b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*rounds), "ns/version")
	})
	val, ok, err := s.Get(obj, rounds-1)
	require.NoError(b, err)
	require.True(b, ok)
	b.Run("decode", func(b *testing.B) { // one version's key
		for b.Loop() {
			_, err := stateKeyValue(obj, val, []byte("k"))
			require.NoError(b, err)
		}
	})
	b.Run("full-decode", func(b *testing.B) { // one version's key, by a full decode
		for b.Loop() {
			_, err := fullStateKeyValue(obj, val, []byte("k"))
			require.NoError(b, err)
		}
	})
}

// BenchmarkBoxPrefixVersions reads prefix= pages of an app with 1,000,000
// box writes, 100 a round, of which 0.1% are under the prefix: the cost per
// index entry stepped through, which sets MaxBoxIndexScanned.
func BenchmarkBoxPrefixVersions(b *testing.B) {
	const rounds, perRound = 10_000, 100
	s, err := timetravel.Open(b.TempDir(), timetravel.Options{})
	require.NoError(b, err)
	defer s.Close()
	val := make([]byte, 32)
	for r := uint64(0); r < rounds; r++ {
		binary.BigEndian.PutUint64(val, r)
		chs := make([]timetravel.Change, 0, perRound)
		for i := range uint64(perRound) {
			name := fmt.Sprintf("x%06d", (r*perRound+i)%50_000) // a churning map
			if (r*perRound+i)%1000 == 0 {
				name = fmt.Sprintf("u%04d", r%100) // the map read
			}
			chs = append(chs, timetravel.Change{Key: timetravel.KvKey(apps.MakeBoxKey(1, name)), Value: append([]byte(nil), val...)})
		}
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r)})
		require.NoError(b, s.AddRound(r, chs, hdr, nil))
	}
	h := &histView{store: s, last: rounds - 1}
	const entries = rounds * perRound
	for _, c := range []struct {
		name   string
		prefix []byte
		want   int
	}{{"all", nil, rounds}, {"prefix", []byte("u"), entries / 1000}} {
		b.Run(c.name, func(b *testing.B) {
			for b.Loop() {
				n := 0
				end, err := h.BoxChanges(1, 0, rounds-1, false, timetravel.BoxChangesOpts{Prefix: c.prefix}, func(uint64, []byte, []byte, bool) bool { n++; return true })
				require.NoError(b, err)
				require.False(b, end.More)
				require.EqualValues(b, entries, end.Scanned)
				if c.prefix != nil {
					require.Equal(b, c.want, n)
				}
			}
			b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*entries), "ns/entry")
		})
	}
}
