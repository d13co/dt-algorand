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

package v2

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestDisassemblyLines checks that each opcode's line carries its pc, and that
// the lines rebuild the disassembly.
func TestDisassemblyLines(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ops, err := logic.AssembleString(`#pragma version 10
txn ApplicationID
bz done
byte "a\nb"
pop
callsub sub
done:
int 1
return
sub:
proto 0 0
retsub`)
	require.NoError(t, err)
	lines := disassemblyLines(ops.Program)
	text, err := logic.Disassemble(ops.Program)
	require.NoError(t, err)
	var texts []string
	pcs := map[string]uint64{}
	for _, l := range lines {
		texts = append(texts, l.Text)
		if l.Pc != nil {
			pcs[l.Text] = *l.Pc
		} else {
			require.True(t, strings.HasPrefix(l.Text, "#pragma") || strings.HasSuffix(l.Text, ":"), l.Text)
		}
	}
	require.Equal(t, text, strings.Join(texts, "\n")+"\n")
	require.Equal(t, uint64(1), pcs["txn ApplicationID"])
	require.Equal(t, uint64(3), pcs["bz label1"])
	require.Contains(t, pcs, "pushbytes 0x610a62 // 0x610a62", "a newline in bytes stays on its line")
	require.Len(t, pcs, 9)
	for text, pc := range pcs {
		require.Less(t, pc, uint64(len(ops.Program)), text)
	}

	// a program the disassembler can't read still gives a line
	require.Equal(t, []model.DisassemblyLine{{Text: "// invalid version"}}, disassemblyLines([]byte{}))
}

// TestConvertGroupTrace checks that the programs come in the order the traces
// first ran them, each once, inner ones included, and only those the traces name.
func TestConvertGroupTrace(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	prog := func(n int) []byte {
		ops, err := logic.AssembleString("#pragma version 10\n" + strings.Repeat("int 1\npop\n", n) + "int 1")
		require.NoError(t, err)
		return ops.Program
	}
	lsig, app, inner, unused := prog(1), prog(2), prog(3), prog(4)
	h := func(p []byte) crypto.Digest { return crypto.Hash(p) }
	g := &ledger.GroupTrace{First: 4, TxIDs: []transactions.Txid{{1}, {2}},
		Programs: map[crypto.Digest][]byte{h(lsig): lsig, h(app): app, h(inner): inner, h(unused): unused}}
	traces := []*simulation.TransactionTrace{
		{LogicSigHash: h(lsig), LogicSigTrace: []simulation.OpcodeTraceUnit{{PC: 1}}},
		{ApprovalProgramHash: h(app), ApprovalProgramTrace: []simulation.OpcodeTraceUnit{{PC: 1}},
			InnerTraces: []simulation.TransactionTrace{{ApprovalProgramHash: h(inner)}, {ApprovalProgramHash: h(app)}}},
	}
	out, programs := convertGroupTrace(g, traces)
	require.Len(t, *out, 2)
	require.Equal(t, transactions.Txid{1}.String(), (*out)[0].Txid)
	require.NotNil(t, (*out)[0].ExecTrace.LogicSigTrace)
	require.Len(t, *(*out)[1].ExecTrace.InnerTrace, 2)
	var got [][]byte
	for _, p := range *programs {
		got = append(got, p.Hash)
		require.NotEmpty(t, p.Lines)
	}
	require.Equal(t, [][]byte{h(lsig).ToSlice(), h(app).ToSlice(), h(inner).ToSlice()}, got)
}
