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

// GroupExecTracer records the execution traces of one top-level group of a block being
// re-evaluated, as simulate records them. It must see only that group's events: its logic sig
// runs first, then the group, from its BeforeTxnGroup to its AfterTxnGroup. It changes nothing
// in the evaluation: it leaves the evaluation constants and budgets as they are, and it never
// aborts. Unlike simulate, it reads no state before an opcode: it records no initial states.
type GroupExecTracer struct {
	*evalTracer
	bytes int64
}

// NewGroupExecTracer returns a tracer for group, a top-level group of the block of round rnd,
// recording what cfg asks for.
func NewGroupExecTracer(rnd basics.Round, group []transactions.SignedTxnWithAD, cfg ExecTraceConfig) *GroupExecTracer {
	txns := make([]transactions.SignedTxn, len(group))
	for i := range group {
		txns[i] = group[i].SignedTxn
	}
	request := Request{Round: rnd, TxnGroups: [][]transactions.SignedTxn{txns}, TraceConfig: cfg}
	t := makeEvalTracer(rnd, [][]transactions.SignedTxnWithAD{group}, request)
	// recording them reads the state an opcode reaches before it runs, which can change what
	// the evaluator caches
	t.result.InitialStates = nil
	t.startGroup(0)
	return &GroupExecTracer{evalTracer: t}
}

// BeforeTxnGroup starts a group as simulate does, but leaves the evaluation constants as the
// block's evaluation set them: simulate replaces them with its request's.
func (t *GroupExecTracer) BeforeTxnGroup(ep *logic.EvalParams) {
	constants := ep.EvalConstants
	t.evalTracer.BeforeTxnGroup(ep)
	if ep.GetCaller() == nil {
		ep.EvalConstants = constants
		return
	}
	// simulate keeps a copy of each inner group
	t.bytes += int64(len(ep.TxnGroup)) * signedTxnSize
}

// Sizes counted for what a trace holds.
const (
	signedTxnSize   = int64(unsafe.Sizeof(transactions.SignedTxnWithAD{}))
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

// Bytes is about how many bytes the traces hold: what their opcodes added, and the copies of
// the inner transactions.
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
