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

// What the programs of a past round did besides changing state (GET
// /v2/deltas/{round}/txn/state-changes with include=budget, reads or trace):
// each program's opcode budget, the app state each transaction read, and the
// execution traces of one group. Non-standard fork feature.

import (
	"errors"
	"fmt"
	"unsafe"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// ErrTraceNeedsOneTxn is returned for IncludeTrace without exactly one transaction in
// StateChangesFilter.TxIDs.
var ErrTraceNeedsOneTxn = errors.New("trace needs exactly one txid")

// ProgramKind is the kind of program a ProgramBudget is for.
//
//msgp:ignore ProgramKind
type ProgramKind uint8

const (
	// ProgramApproval is an app's approval program.
	ProgramApproval ProgramKind = iota + 1
	// ProgramClearState is an app's clear state program.
	ProgramClearState
	// ProgramLogicSig is a logic sig.
	ProgramLogicSig
)

// ProgramBudget is the opcode budget of one program run.
//
//msgp:ignore ProgramBudget
type ProgramBudget struct {
	Path []uint64          // the transaction, as in TxnStateChanges
	TxID transactions.Txid // top-level transactions only
	Kind ProgramKind
	App  basics.AppIndex // apps only
	Cost int             // what the program spent, without its inner transactions' programs
	// What the program could still spend when it began and when it ended:
	// the pooled budget where it is pooled. Inner app calls add to the pool
	// while the program runs, and an inner program that spends more than they
	// added takes the rest from it: Before-After can differ from Cost either way.
	BudgetBefore, BudgetAfter int
	Failed                    bool // it rejected or failed: a ClearState program, or a program inside one
}

// StateRead is an app state key a transaction read, with the value it read.
//
//msgp:ignore StateRead
type StateRead struct {
	App     basics.AppIndex
	Type    logic.AppStateEnum
	Account basics.Address // LocalState only
	Key     string
	Exists  bool
	Value   basics.TealValue // GlobalState and LocalState, when Exists
	Length  int              // BoxState, when Exists
}

// TxnStateReads are the app state keys a transaction read itself (its inner
// transactions are their own), first read first.
//
//msgp:ignore TxnStateReads
type TxnStateReads struct {
	Path  []uint64
	TxID  transactions.Txid
	Reads []StateRead
}

// GroupTrace is what a StateChangesFilter.Trace tracer recorded: the group of
// the one txid, and the programs it ran by hash.
//
//msgp:ignore GroupTrace
type GroupTrace struct {
	First    int
	TxIDs    []transactions.Txid
	Programs map[crypto.Digest][]byte
}

// scRunning is a program that is running, and its entry in the budget (-1: none).
type scRunning struct {
	cx  *logic.EvalContext
	idx int
}

// scReads accumulates a transaction's reads.
type scReads struct {
	TxnStateReads
	key  string // in readsOf
	seen map[scKey]bool
}

// devState is the part of stateChangeTracer for budget, reads and trace.
type devState struct {
	budget  []ProgramBudget
	running []scRunning

	reads   []*scReads
	readsOf map[string]*scReads // by pathKey

	sigLedger logic.LedgerForSignature

	trace      logic.EvalTracer // follows the group at traceFirst
	traceFirst int              // -1: none
	tracing    bool
	programs   map[crypto.Digest][]byte
}

// scTraceGuard is the trace tracer as stateChangeTracer calls it. A panic in it
// fails the answer, not the program it follows (the evaluator would make it the
// program's failure, which a ClearState program swallows), and the bytes it
// holds, when it says, count against the log's limit. After a failure it is no
// longer called.
type scTraceGuard struct {
	t     *stateChangeTracer
	inner logic.EvalTracer
	bytes int64
}

// traceBytes is what a trace tracer can implement to say how many bytes it holds.
type traceBytes interface {
	Bytes() int64
}

func (g *scTraceGuard) call(fn func()) {
	if g.t.err != nil {
		return
	}
	defer func() {
		if x := recover(); x != nil {
			g.t.fail(fmt.Errorf("tracing: %v", x))
		}
	}()
	fn()
	if s, ok := g.inner.(traceBytes); ok {
		n := s.Bytes()
		g.t.grow(n - g.bytes)
		g.bytes = n
	}
}

func (g *scTraceGuard) BeforeTxnGroup(ep *logic.EvalParams) {
	g.call(func() { g.inner.BeforeTxnGroup(ep) })
}

func (g *scTraceGuard) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	g.call(func() { g.inner.AfterTxnGroup(ep, deltas, evalError) })
}

func (g *scTraceGuard) BeforeTxn(ep *logic.EvalParams, groupIndex int) {
	g.call(func() { g.inner.BeforeTxn(ep, groupIndex) })
}

func (g *scTraceGuard) AfterTxn(ep *logic.EvalParams, groupIndex int, ad transactions.ApplyData, evalError error) {
	g.call(func() { g.inner.AfterTxn(ep, groupIndex, ad, evalError) })
}

func (g *scTraceGuard) BeforeProgram(cx *logic.EvalContext) {
	g.call(func() { g.inner.BeforeProgram(cx) })
}

func (g *scTraceGuard) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	g.call(func() { g.inner.AfterProgram(cx, pass, evalError) })
}

func (g *scTraceGuard) BeforeOpcode(cx *logic.EvalContext) {
	g.call(func() { g.inner.BeforeOpcode(cx) })
}

func (g *scTraceGuard) AfterOpcode(cx *logic.EvalContext, evalError error) {
	g.call(func() { g.inner.AfterOpcode(cx, evalError) })
}

func (g *scTraceGuard) BeforeBlock(hdr *bookkeeping.BlockHeader) {}

func (g *scTraceGuard) AfterBlock(hdr *bookkeeping.BlockHeader) {}

func (g *scTraceGuard) DetailedEvalErrors() bool {
	return g.t.err == nil && g.inner.DetailedEvalErrors()
}

// readSize and budgetSize are the in-memory sizes counted for a read and a budget entry.
const (
	readSize   = int64(unsafe.Sizeof(StateRead{}))
	budgetSize = int64(unsafe.Sizeof(ProgramBudget{}))
)

// reports tells whether the top-level transaction at payset index top is one asked for.
func (t *stateChangeTracer) reports(top int) bool {
	return t.only == nil || t.only[top]
}

// current returns the path of the transaction being evaluated and whether it is one asked for.
func (t *stateChangeTracer) current() ([]int, bool) {
	n := len(t.frames)
	if n == 0 || t.tooLarge {
		return nil, false
	}
	path := t.frames[n-1].path
	return path, t.reports(path[0])
}

func uint64Path(path []int) []uint64 {
	out := make([]uint64, len(path))
	for i, p := range path {
		out[i] = uint64(p)
	}
	return out
}

func (t *stateChangeTracer) txidOf(path []int) transactions.Txid {
	if len(path) == 1 {
		return t.tops[path[0]].ID()
	}
	return transactions.Txid{}
}

// BeforeTxnGroup runs a top-level group's logic sigs, as the node checks them
// before it evaluates the group, and starts following the traced group.
func (t *stateChangeTracer) BeforeTxnGroup(ep *logic.EvalParams) {
	if ep.GetCaller() == nil && !t.tooLarge {
		first := len(t.tops)
		traced := t.trace != nil && first == t.traceFirst
		if traced || (t.include&IncludeBudget != 0 && t.groupReported(first, len(ep.TxnGroup))) {
			t.runLogicSigs(ep, first, traced)
		}
		t.tracing = traced
	}
	if t.tracing {
		t.trace.BeforeTxnGroup(ep)
	}
}

// AfterTxnGroup records the values a top-level group left, and stops following
// the traced group.
func (t *stateChangeTracer) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	t.groupDelta(ep, deltas, evalError)
	if t.tracing {
		t.trace.AfterTxnGroup(ep, deltas, evalError)
		if ep.GetCaller() == nil {
			t.tracing = false
		}
	}
}

// DetailedEvalErrors makes the traced group's errors (a failed ClearState
// program's) read as simulate's.
func (t *stateChangeTracer) DetailedEvalErrors() bool {
	return t.tracing && t.trace.DetailedEvalErrors()
}

// runLogicSigs evaluates the logic sigs of the top-level group of ep, which
// begins at payset index first.
func (t *stateChangeTracer) runLogicSigs(ep *logic.EvalParams, first int, traced bool) {
	var group []transactions.SignedTxn
	for i := range ep.TxnGroup {
		if ep.TxnGroup[i].Lsig.HasProgram() {
			group = make([]transactions.SignedTxn, len(ep.TxnGroup))
			break
		}
	}
	if group == nil {
		return
	}
	for i := range ep.TxnGroup {
		group[i] = ep.TxnGroup[i].SignedTxn
	}
	if t.sigLedger == nil {
		t.fail(errors.New("no ledger for logic sigs"))
		return
	}
	sep := logic.NewSigEvalParams(group, ep.Proto, t.sigLedger)
	st := &scSigTracer{t: t, first: first}
	if traced {
		st.forward = t.trace
	}
	sep.Tracer = st
	for gi := range group {
		if !group[gi].Lsig.HasProgram() {
			continue
		}
		pass, _, err := logic.EvalSignatureFull(gi, sep)
		if err == nil && !pass {
			err = errors.New("rejected")
		}
		if err != nil {
			t.fail(fmt.Errorf("the logic sig of payset transaction %d: %w", first+gi, err))
			return
		}
	}
}

// startBudget adds a budget entry for the program cx begins, and returns its index.
func (t *stateChangeTracer) startBudget(cx *logic.EvalContext, path []int, txid transactions.Txid, kind ProgramKind) int {
	t.budget = append(t.budget, ProgramBudget{
		Path: uint64Path(path), TxID: txid, Kind: kind, App: cx.AppID(),
		BudgetBefore: cx.RemainingBudget(),
	})
	t.grow(budgetSize + int64(len(path))*8)
	return len(t.budget) - 1
}

func (t *stateChangeTracer) endBudget(idx int, cx *logic.EvalContext, pass bool, evalError error) {
	if idx < 0 || idx >= len(t.budget) {
		return
	}
	b := &t.budget[idx]
	b.Cost = cx.Cost()
	b.BudgetAfter = cx.RemainingBudget()
	b.Failed = !pass || evalError != nil
}

// BeforeProgram starts a budget entry for an app program.
func (t *stateChangeTracer) BeforeProgram(cx *logic.EvalContext) {
	if t.tracing {
		t.trace.BeforeProgram(cx)
		t.keepProgram(cx)
	}
	if cx.RunMode() != logic.ModeApp || t.include&IncludeBudget == 0 {
		return
	}
	idx := -1
	if path, ok := t.current(); ok {
		kind := ProgramApproval
		if cx.TxnGroup[cx.GroupIndex()].Txn.OnCompletion == transactions.ClearStateOC {
			kind = ProgramClearState
		}
		idx = t.startBudget(cx, path, t.txidOf(path), kind)
	}
	t.running = append(t.running, scRunning{cx: cx, idx: idx})
}

// endProgram completes the budget entry of the program cx ends.
func (t *stateChangeTracer) endProgram(cx *logic.EvalContext, pass bool, evalError error) {
	if t.tracing {
		t.trace.AfterProgram(cx, pass, evalError)
	}
	if cx.RunMode() != logic.ModeApp || t.include&IncludeBudget == 0 {
		return
	}
	for i := len(t.running) - 1; i >= 0; i-- {
		if t.running[i].cx == cx {
			t.endBudget(t.running[i].idx, cx, pass, evalError)
			t.running = t.running[:i]
			return
		}
	}
}

// keepProgram keeps the program cx runs, for the trace's disassembly.
func (t *stateChangeTracer) keepProgram(cx *logic.EvalContext) {
	program := cx.GetProgram()
	h := crypto.Hash(program)
	if _, ok := t.programs[h]; ok {
		return
	}
	if t.programs == nil {
		t.programs = make(map[crypto.Digest][]byte)
	}
	t.programs[h] = program
	t.grow(int64(len(program)) + int64(len(h)))
}

// readOpcode logs a read of app state by the current transaction, once per key.
// The opcode has run: the value is what it read.
func (t *stateChangeTracer) readOpcode(cx *logic.EvalContext, p scPending) {
	path, ok := t.current()
	if !ok {
		return
	}
	pk := pathKey(path, len(path))
	r := t.readsOf[pk]
	if r == nil {
		r = &scReads{TxnStateReads: TxnStateReads{Path: uint64Path(path), TxID: t.txidOf(path)}, key: pk, seen: make(map[scKey]bool)}
		if t.readsOf == nil {
			t.readsOf = make(map[string]*scReads)
		}
		t.readsOf[pk] = r
		t.reads = append(t.reads, r)
		t.grow(readSize + int64(len(path))*8)
	}
	k := scKey{typ: p.typ, app: p.app, key: p.key}
	if p.typ == logic.LocalState {
		k.acct = p.acct
	}
	if r.seen[k] {
		return
	}
	val, exists, err := logic.AppStateValue(cx, p.typ, p.op, p.app, p.acct, p.key)
	if err != nil {
		t.fail(fmt.Errorf("reading app %d state after a read: %w", p.app, err))
		return
	}
	r.seen[k] = true
	sr := StateRead{App: k.app, Type: k.typ, Account: k.acct, Key: k.key, Exists: exists}
	size := readSize + int64(len(k.key))
	if exists {
		if k.typ == logic.BoxState {
			sr.Length = len(val.Bytes)
		} else {
			sr.Value = val
			size += int64(len(val.Bytes))
		}
	}
	r.Reads = append(r.Reads, sr)
	t.grow(size)
}

// discardInners marks the budget entries of f's inner transactions failed,
// and drops their reads: a failed ClearState program discards them, and the
// block has none of them.
func (t *stateChangeTracer) discardInners(f *scFrame) {
	for i := f.budgetMark; i < len(t.budget); i++ {
		// what was logged since f began is f's or an inner transaction's
		if len(t.budget[i].Path) > len(f.path) {
			t.budget[i].Failed = true
		}
	}
	kept := t.reads[:f.readsMark]
	for _, r := range t.reads[f.readsMark:] {
		if len(r.Path) > len(f.path) {
			delete(t.readsOf, r.key)
		} else {
			kept = append(kept, r)
		}
	}
	clear(t.reads[len(kept):])
	t.reads = kept
}

// devResults returns the budget and reads, in the answer's form.
func (t *stateChangeTracer) devResults() ([]ProgramBudget, []TxnStateReads) {
	var reads []TxnStateReads
	for _, r := range t.reads {
		reads = append(reads, r.TxnStateReads)
	}
	return t.budget, reads
}

// scSigTracer follows the logic sigs of a group for stateChangeTracer.
type scSigTracer struct {
	logic.NullEvalTracer
	t       *stateChangeTracer
	first   int              // the group's payset index
	forward logic.EvalTracer // the traced group's tracer, or nil
	idx     int
}

func (s *scSigTracer) BeforeProgram(cx *logic.EvalContext) {
	if s.forward != nil {
		s.forward.BeforeProgram(cx)
		s.t.keepProgram(cx)
	}
	s.idx = -1
	if top := s.first + cx.GroupIndex(); s.t.include&IncludeBudget != 0 && s.t.reports(top) {
		s.idx = s.t.startBudget(cx, []int{top}, cx.TxnGroup[cx.GroupIndex()].ID(), ProgramLogicSig)
	}
}

func (s *scSigTracer) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	if s.forward != nil {
		s.forward.AfterProgram(cx, pass, evalError)
	}
	s.t.endBudget(s.idx, cx, pass, evalError)
}

func (s *scSigTracer) BeforeOpcode(cx *logic.EvalContext) {
	if s.forward != nil {
		s.forward.BeforeOpcode(cx)
	}
	if s.t.tooLarge {
		panic(logic.EvalAborted{Cause: ErrStateChangesTooLarge}) // the trace grew too large
	}
}

func (s *scSigTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	if s.forward != nil {
		s.forward.AfterOpcode(cx, evalError)
	}
}

func (s *scSigTracer) DetailedEvalErrors() bool {
	return s.forward != nil && s.forward.DetailedEvalErrors()
}

// startTrace finds the group of the one transaction asked for in blk, the block
// of round rnd, and makes its tracer.
func (t *stateChangeTracer) startTrace(blk *bookkeeping.Block, rnd basics.Round, mk func([]transactions.SignedTxnWithAD) logic.EvalTracer) (*GroupTrace, error) {
	var top int
	for i := range t.only {
		top = i
	}
	payset, err := blk.DecodePaysetFlat()
	if err != nil {
		return nil, fmt.Errorf("%w: round %d: %w", ErrDeltaReevaluation, rnd, err)
	}
	first, end := top, top+1
	if g := payset[top].Txn.Group; !g.IsZero() {
		for first > 0 && payset[first-1].Txn.Group == g {
			first--
		}
		for end < len(payset) && payset[end].Txn.Group == g {
			end++
		}
	}
	group := payset[first:end]
	tr := &GroupTrace{First: first, TxIDs: make([]transactions.Txid, len(group))}
	for i := range group {
		tr.TxIDs[i] = group[i].ID()
	}
	t.trace = &scTraceGuard{t: t, inner: mk(group)}
	t.traceFirst = first
	return tr, nil
}
