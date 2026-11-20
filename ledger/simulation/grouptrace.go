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

package simulation

// Execution traces of a committed group (GET /v2/deltas/{round}/txn/state-changes with
// include=trace). Non-standard fork feature, in its own file to keep upstream merges clean.

import (
	"unsafe"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
)

// GroupExecTracer records the execution traces of one top-level group of an evaluation, as
// simulate records them: a group of a block being re-evaluated, or one being replayed. It must
// see only that group's events: its logic sigs first, then the group, from its BeforeTxnGroup to
// its AfterTxnGroup (ledger.DevTracer forwards them so). It changes nothing in the evaluation (no
// extra budget, eval constants or unnamed resource policy, unlike simulate's tracer) and never
// aborts.
type GroupExecTracer struct {
	*evalTracer
	bytes int64
}

// GroupExecOptions are what a GroupExecTracer records besides the traces.
type GroupExecOptions struct {
	// InitialStates records, as simulate's initial-states, the app state each opcode reaches,
	// read before it runs (with cfg.State). The reads can change what the evaluator caches:
	// leave it off where the result is checked against a block's (a re-evaluated round).
	InitialStates bool
}

// NewGroupExecTracer returns a tracer for group, a top-level group of the block after round
// lastRound, recording what cfg and opts ask for.
func NewGroupExecTracer(lastRound basics.Round, group []transactions.SignedTxnWithAD, cfg ExecTraceConfig, opts GroupExecOptions) *GroupExecTracer {
	txns := make([]transactions.SignedTxn, len(group))
	for i := range group {
		txns[i] = group[i].SignedTxn
	}
	request := Request{Round: lastRound, TxnGroups: [][]transactions.SignedTxn{txns}, TraceConfig: cfg}
	t := makeEvalTracer(lastRound, [][]transactions.SignedTxnWithAD{group}, request)
	t.passive = true
	if !opts.InitialStates {
		t.result.InitialStates = nil
	}
	t.startGroup(0)
	return &GroupExecTracer{evalTracer: t}
}

// InitialStates returns the initial states recorded with GroupExecOptions.InitialStates, else nil.
func (t *GroupExecTracer) InitialStates() *ResourcesInitialStates {
	return t.result.InitialStates
}

// Sizes counted for what an opcode adds to a trace.
const (
	opcodeUnitSize  = int64(unsafe.Sizeof(OpcodeTraceUnit{}))
	tealValueSize   = int64(unsafe.Sizeof(basics.TealValue{}))
	scratchSize     = int64(unsafe.Sizeof(ScratchChange{}))
	stateChangeSize = int64(unsafe.Sizeof(StateOperation{}))
)

// AfterOpcode records the opcode's effects, and counts the bytes they take.
func (t *GroupExecTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	t.evalTracer.AfterOpcode(cx, evalError)
	t.bytes += opcodeUnitSize
	if evalError != nil || !t.result.ReturnTrace() {
		return
	}
	// as evalTracer.AfterOpcode finds it
	var trace *TransactionTrace
	if cx.RunMode() == logic.ModeSig {
		trace = t.currentGroup().Txns[cx.GroupIndex()].Trace
	} else {
		trace = t.execTraceStack[len(t.execTraceStack)-1]
	}
	units := *trace.programTraceRef
	u := &units[len(units)-1]
	for _, v := range u.StackAdded {
		t.bytes += tealValueSize + int64(len(v.Bytes))
	}
	for _, sc := range u.ScratchSlotChanges {
		t.bytes += scratchSize + int64(len(sc.NewValue.Bytes))
	}
	for _, so := range u.StateChanges {
		t.bytes += stateChangeSize + int64(len(so.Key)+len(so.NewValue.Bytes))
	}
}

// Bytes is about how many bytes the traces hold: what their opcodes added.
func (t *GroupExecTracer) Bytes() int64 {
	return t.bytes
}

// Traces returns the trace of each transaction of the group, in group order. As in simulate,
// every transaction has one, empty for a transaction that ran no program.
func (t *GroupExecTracer) Traces() []*TransactionTrace {
	txns := t.result.TxnGroups[0].Txns
	out := make([]*TransactionTrace, len(txns))
	for i := range txns {
		out[i] = txns[i].Trace
	}
	return out
}
