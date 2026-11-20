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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestStateChangesDiscardInners checks that a failed ClearState program's inner
// transactions keep their budget entries, as failed, and lose their reads.
// Since IsolateClearState a ClearState program can't issue inner transactions,
// so only older rounds have them.
func TestStateChangesDiscardInners(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	tr := NewDevTracer(DevConfig{Include: IncludeBudget | IncludeReads})
	addReads := func(path ...int) {
		r := &scReads{TxnStateReads: TxnStateReads{Path: uint64Path(path)}, key: pathKey(path, len(path))}
		tr.reads = append(tr.reads, r)
		if tr.readsOf == nil {
			tr.readsOf = make(map[string]*scReads)
		}
		tr.readsOf[r.key] = r
	}
	tr.budget = []ProgramBudget{{Path: []uint64{3}}}
	addReads(3)
	f := devFrame{path: []int{5}, budgetMark: len(tr.budget), readsMark: len(tr.reads)}
	tr.budget = append(tr.budget,
		ProgramBudget{Path: []uint64{5}, Kind: ProgramClearState, Failed: true},
		ProgramBudget{Path: []uint64{5, 0}},
		ProgramBudget{Path: []uint64{5, 0, 0}},
		ProgramBudget{Path: []uint64{5, 1}})
	addReads(5, 0)
	addReads(5)
	addReads(5, 0, 0)

	tr.discardInners(&f)
	var failed []bool
	for _, b := range tr.budget {
		failed = append(failed, b.Failed)
	}
	require.Equal(t, []bool{false, true, true, true, true}, failed)
	require.Len(t, tr.reads, 2)
	require.Equal(t, []uint64{3}, tr.reads[0].Path)
	require.Equal(t, []uint64{5}, tr.reads[1].Path)
	require.Len(t, tr.readsOf, 2)
	require.Contains(t, tr.readsOf, pathKey([]int{5}, 1))
}

// guardTestTracer panics in BeforeOpcode, and holds what bytes says.
type guardTestTracer struct {
	logic.NullEvalTracer
	calls int
	bytes int64
}

func (g *guardTestTracer) BeforeOpcode(cx *logic.EvalContext) {
	g.calls++
	if g.calls == 2 {
		panic("boom")
	}
}

func (g *guardTestTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	g.calls++
}

func (g *guardTestTracer) Bytes() int64 { return g.bytes }

// TestStateChangesTraceGuard checks that a panic in the trace tracer fails the
// answer and stops the calls to it, and that the bytes it holds count against
// the limit.
func TestStateChangesTraceGuard(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	lim := &evalLimit{maxBytes: 400}
	inner := &guardTestTracer{bytes: 50}
	g := &traceGuard{lim: lim, inner: inner}
	g.BeforeOpcode(nil)
	require.EqualValues(t, 50, lim.bytes)
	inner.bytes = 120
	g.AfterOpcode(nil, nil)
	require.EqualValues(t, 120, lim.bytes, "only what it added since")
	require.False(t, lim.tooLarge)
	inner.bytes = 401
	g.AfterOpcode(nil, nil)
	require.True(t, lim.tooLarge)

	lim = &evalLimit{}
	inner = &guardTestTracer{}
	g = &traceGuard{lim: lim, inner: inner}
	g.BeforeOpcode(nil)
	g.BeforeOpcode(nil) // panics
	require.ErrorContains(t, lim.err, "tracing: boom")
	require.False(t, g.DetailedEvalErrors())
	g.AfterOpcode(nil, nil)
	require.Equal(t, 2, inner.calls, "not called after its failure")
}
