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
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestKeyVersionsReadBudget checks that a key= page counts each record it
// scans once toward MaxReadBytes: a version's value, plus the value before
// the first version, which the scan reads for it alone.
func TestKeyVersionsReadBudget(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	const rounds = 20
	s, err := timetravel.Open(t.TempDir(), timetravel.Options{})
	require.NoError(t, err)
	defer s.Close()
	var creator basics.Address
	obj := timetravel.AppParamsKey(1)
	params := basics.AppParams{ApprovalProgram: make([]byte, 256), ClearStateProgram: make([]byte, 8), GlobalState: basics.TealKeyValue{}}
	var size int
	for r := uint64(0); r < rounds; r++ {
		// a fixed-width value: every record has the same size, and k changes every round
		params.GlobalState["k"] = basics.TealValue{Type: basics.TealBytesType, Bytes: fmt.Sprintf("%08d", r)}
		val := append(creator[:], protocol.Encode(&params)...)
		if r > 0 {
			require.Len(t, val, size)
		}
		size = len(val)
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r)})
		require.NoError(t, s.AddRound(r, []timetravel.Change{{Key: obj, Value: val}}, hdr, nil))
	}
	h := &histView{store: s, last: rounds - 1}

	const n = 5
	for _, desc := range []bool{false, true} {
		r := VersionRange{Min: 1, Max: rounds - 1, Desc: desc, Limit: 100, Key: []byte("k")}
		// the first version reads its value and the one before; each other one its value
		r.MaxReadBytes = uint64(size * (n + 1))
		p, err := keyVersions(h, obj, r)
		require.NoError(t, err)
		require.Len(t, p.Versions, n, "desc=%v", desc)
		require.True(t, p.More)
		next := basics.Round(1 + n)
		if desc {
			next = rounds - 1 - n
		}
		require.Equal(t, next, p.Next, "desc=%v", desc)

		r.MaxReadBytes--
		p, err = keyVersions(h, obj, r)
		require.NoError(t, err)
		require.Len(t, p.Versions, n-1, "desc=%v", desc)
	}
}
