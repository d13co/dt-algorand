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

// What programs did besides changing state: each program's opcode budget, the
// app state each transaction read, and the execution traces of one group, as
// DevTracer records them during any evaluation. GET
// /v2/deltas/{round}/txn/state-changes (include=budget, reads or trace) runs it
// over a past round's block; a replay of a failed group runs it over groups
// that never made a block. Non-standard fork feature.

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
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
	// Failed marks a program that rejected or failed, and the programs inside a
	// ClearState program that did: the evaluator discards what they did. In a
	// top-level group that failed, which no block holds, each entry says how
	// its own program ended.
	Failed bool
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

// evalLimit is what the tracers of one evaluation share: the bytes they hold,
// bounded by maxBytes, and the first failure. Either stops the evaluation.
type evalLimit struct {
	bytes    int64
	maxBytes int64 // 0: no limit
	tooLarge bool  // bytes passed maxBytes: cancel was called, and programs abort
	err      error // the first inconsistency or lookup error
	cancel   context.CancelFunc
	onFull   []func() // called once, when bytes first pass maxBytes
}

// fail fails the answer with err, and stops the evaluation.
func (l *evalLimit) fail(err error) {
	if l.err == nil {
		l.err = err
		if l.cancel != nil {
			l.cancel()
		}
	}
}

// grow adds d to the bytes held, and stops the evaluation once they pass maxBytes.
func (l *evalLimit) grow(d int64) {
	l.bytes += d
	if l.maxBytes > 0 && l.bytes > l.maxBytes && !l.tooLarge {
		l.tooLarge = true
		for _, f := range l.onFull {
			f()
		}
		if l.cancel != nil {
			l.cancel()
		}
	}
}

// DevConfig says what a DevTracer records.
type DevConfig struct {
	// Include selects IncludeBudget, IncludeReads and IncludeTrace; the rest is ignored.
	Include StateChangesInclude
	// Only, when not nil, limits budget and reads to the top-level transactions at these
	// indexes. A top-level transaction's index counts those before it in the evaluation, in
	// failed groups too, as a block's payset does.
	Only map[int]bool
	// TraceFirst is the index of the first transaction of the top-level group whose events go
	// to Trace, logic sigs first, with IncludeTrace.
	TraceFirst int
	// Trace follows the group at TraceFirst. A panic in it fails the tracer's answer (Err), not
	// the program it follows. If it has a Bytes() int64 method, the bytes it holds count
	// against MaxBytes.
	Trace logic.EvalTracer
	// SigLedger, when set, makes the tracer run the logic sigs of each top-level group it
	// reports itself, before the group: for an evaluation that doesn't run them (a block
	// re-evaluated without validation). A logic sig that rejects fails the tracer (Err).
	// Leave it nil when the caller runs them, before each group, with this tracer (simulate's
	// checks): it then follows those runs. Such a caller announces each group with NextGroup
	// before its checks, so that a group whose checks fail, and which never begins, still takes
	// its indexes, and its logic sigs are not taken for the next group's.
	SigLedger logic.LedgerForSignature
	// MaxBytes bounds what the tracer holds; past it, it calls Cancel, programs abort, and
	// TooLarge reports it. 0: no limit.
	MaxBytes int64
	// Cancel stops the evaluation, on a failure or past MaxBytes. Optional.
	Cancel context.CancelFunc
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

// devFrame is a transaction being evaluated.
type devFrame struct {
	ep     *logic.EvalParams
	gi     int
	path   []int
	inners int // inner transactions it has spawned

	budgetMark, readsMark int // len(budget) and len(reads) when it began
}

// DevTracer records, during an evaluation, the opcode budget of each program
// run, the app state each transaction read, and the events of one top-level
// group for a trace tracer. It changes nothing in the evaluation, except that
// it aborts the programs once it holds more than its limit.
type DevTracer struct {
	logic.NullEvalTracer
	*evalLimit

	include   StateChangesInclude
	only      map[int]bool
	sigLedger logic.LedgerForSignature

	frames     []devFrame
	tops       int // top-level transactions of the groups begun or announced, failed ones included
	groupFirst int // the index of the first transaction of the current top-level group
	announced  int // the size of the group NextGroup announced, until it begins (0: none)

	budget  []ProgramBudget
	running []scRunning

	reads      []*scReads
	readsOf    map[string]*scReads // by pathKey
	pending    scPending
	hasPending bool

	trace      logic.EvalTracer // the guarded DevConfig.Trace
	traceFirst int              // -1: none
	tracing    bool             // in the traced group
	programs   map[crypto.Digest][]byte
}

// NewDevTracer returns a tracer that records what cfg asks for.
func NewDevTracer(cfg DevConfig) *DevTracer {
	return newDevTracer(cfg, &evalLimit{maxBytes: cfg.MaxBytes, cancel: cfg.Cancel})
}

// newDevTracer returns a tracer that shares lim with the evaluation's other tracers.
func newDevTracer(cfg DevConfig, lim *evalLimit) *DevTracer {
	t := &DevTracer{
		evalLimit:  lim,
		include:    cfg.Include,
		only:       cfg.Only,
		sigLedger:  cfg.SigLedger,
		traceFirst: -1,
	}
	if cfg.Include&IncludeTrace != 0 && cfg.Trace != nil {
		t.trace = &traceGuard{lim: lim, inner: cfg.Trace}
		t.traceFirst = cfg.TraceFirst
	}
	return t
}

// Err is the tracer's first failure: what it records can't be trusted.
func (t *DevTracer) Err() error {
	return t.err
}

// TooLarge reports that the tracer passed DevConfig.MaxBytes and stopped recording.
func (t *DevTracer) TooLarge() bool {
	return t.tooLarge
}

// Budget returns the budget entries, in the order the programs began; a group's logic sigs
// come before its app programs.
func (t *DevTracer) Budget() []ProgramBudget {
	return t.budget
}

// Reads returns the reported transactions that read app state, top-level and inner, in the
// order they first read.
func (t *DevTracer) Reads() []TxnStateReads {
	var reads []TxnStateReads
	for _, r := range t.reads {
		reads = append(reads, r.TxnStateReads)
	}
	return reads
}

// Programs returns the programs the traced group ran, by hash.
func (t *DevTracer) Programs() map[crypto.Digest][]byte {
	return t.programs
}

// traceGuard is a trace tracer as DevTracer calls it. A panic in it fails the
// answer, not the program it follows (the evaluator would make it the
// program's failure, which a ClearState program swallows), and the bytes it
// holds, when it says, count against the limit. After a failure it is no
// longer called.
type traceGuard struct {
	lim   *evalLimit
	inner logic.EvalTracer
	bytes int64
}

// traceBytes is what a trace tracer can implement to say how many bytes it holds.
type traceBytes interface {
	Bytes() int64
}

func (g *traceGuard) call(fn func()) {
	if g.lim.err != nil {
		return
	}
	defer func() {
		if x := recover(); x != nil {
			g.lim.fail(fmt.Errorf("tracing: %v", x))
		}
	}()
	fn()
	if s, ok := g.inner.(traceBytes); ok {
		n := s.Bytes()
		g.lim.grow(n - g.bytes)
		g.bytes = n
	}
}

func (g *traceGuard) BeforeTxnGroup(ep *logic.EvalParams) {
	g.call(func() { g.inner.BeforeTxnGroup(ep) })
}

func (g *traceGuard) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	g.call(func() { g.inner.AfterTxnGroup(ep, deltas, evalError) })
}

func (g *traceGuard) BeforeTxn(ep *logic.EvalParams, groupIndex int) {
	g.call(func() { g.inner.BeforeTxn(ep, groupIndex) })
}

func (g *traceGuard) AfterTxn(ep *logic.EvalParams, groupIndex int, ad transactions.ApplyData, evalError error) {
	g.call(func() { g.inner.AfterTxn(ep, groupIndex, ad, evalError) })
}

func (g *traceGuard) BeforeProgram(cx *logic.EvalContext) {
	g.call(func() { g.inner.BeforeProgram(cx) })
}

func (g *traceGuard) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	g.call(func() { g.inner.AfterProgram(cx, pass, evalError) })
}

func (g *traceGuard) BeforeOpcode(cx *logic.EvalContext) {
	g.call(func() { g.inner.BeforeOpcode(cx) })
}

func (g *traceGuard) AfterOpcode(cx *logic.EvalContext, evalError error) {
	g.call(func() { g.inner.AfterOpcode(cx, evalError) })
}

func (g *traceGuard) BeforeBlock(hdr *bookkeeping.BlockHeader) {}

func (g *traceGuard) AfterBlock(hdr *bookkeeping.BlockHeader) {}

func (g *traceGuard) DetailedEvalErrors() bool {
	if g.lim.err != nil {
		return false
	}
	detailed := false
	g.call(func() { detailed = g.inner.DetailedEvalErrors() })
	return detailed
}

// readSize and budgetSize are the in-memory sizes counted for a read and a budget entry.
const (
	readSize   = int64(unsafe.Sizeof(StateRead{}))
	budgetSize = int64(unsafe.Sizeof(ProgramBudget{}))
)

// reports tells whether the top-level transaction at index top is one asked for.
func (t *DevTracer) reports(top int) bool {
	return t.only == nil || t.only[top]
}

// groupReported tells whether a top-level group, at indexes first to first+n-1, has a
// transaction asked for.
func groupReported(only map[int]bool, first, n int) bool {
	if only == nil {
		return true
	}
	for i := first; i < first+n; i++ {
		if only[i] {
			return true
		}
	}
	return false
}

// current returns the path of the transaction being evaluated and whether it is one asked for.
func (t *DevTracer) current() ([]int, bool) {
	n := len(t.frames)
	if n == 0 || t.tooLarge {
		return nil, false
	}
	path := t.frames[n-1].path
	return path, t.reports(path[0])
}

// txidOf returns the txid of the top-level transaction at path, or zero for an inner one.
func (t *DevTracer) txidOf(path []int) transactions.Txid {
	if len(path) != 1 || len(t.frames) == 0 {
		return transactions.Txid{}
	}
	f := &t.frames[0]
	return f.ep.TxnGroup[f.gi].ID()
}

func uint64Path(path []int) []uint64 {
	out := make([]uint64, len(path))
	for i, p := range path {
		out[i] = uint64(p)
	}
	return out
}

// frameOf returns the index of the innermost frame of transaction gi of ep, or -1.
func (t *DevTracer) frameOf(ep *logic.EvalParams, gi int) int {
	for i := len(t.frames) - 1; i >= 0; i-- {
		if t.frames[i].ep == ep && t.frames[i].gi == gi {
			return i
		}
	}
	return -1
}

// sigTraced tells whether a logic sig running now belongs to the traced group:
// logic sigs run before their group begins.
func (t *DevTracer) sigTraced() bool {
	return t.trace != nil && len(t.frames) == 0 && t.tops == t.traceFirst
}

// NextGroup announces the top-level group of n transactions that the caller
// checks next (its logic sigs, which the tracer follows), then evaluates. A
// group announced before it that never began, its checks having failed, takes
// its indexes all the same.
func (t *DevTracer) NextGroup(n int) {
	t.tops += t.announced
	t.announced = n
}

// BeforeTxnGroup begins a top-level group: it runs the group's logic sigs when
// it has a SigLedger, and starts following the traced group.
func (t *DevTracer) BeforeTxnGroup(ep *logic.EvalParams) {
	if ep.GetCaller() == nil {
		t.announced = 0 // the announced group begins
		first, n := t.tops, len(ep.TxnGroup)
		traced := t.trace != nil && first == t.traceFirst
		if t.sigLedger != nil && !t.tooLarge && (traced || (t.include&IncludeBudget != 0 && groupReported(t.only, first, n))) {
			t.runLogicSigs(ep)
		}
		t.frames, t.running = t.frames[:0], t.running[:0]
		t.groupFirst = first
		t.tops += n
		t.tracing = traced
	}
	if t.tracing {
		t.trace.BeforeTxnGroup(ep)
	}
}

// AfterTxnGroup ends a group, and stops following the traced one. A top-level
// group that failed can leave transactions and programs open.
func (t *DevTracer) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	if t.tracing {
		t.trace.AfterTxnGroup(ep, deltas, evalError)
	}
	if ep.GetCaller() == nil {
		t.tracing = false
		t.frames, t.running = t.frames[:0], t.running[:0]
	}
}

// BeforeTxn starts a frame: a top-level transaction at its index, an inner one
// at the next index of its parent's inner transactions.
func (t *DevTracer) BeforeTxn(ep *logic.EvalParams, groupIndex int) {
	var path []int
	if n := len(t.frames); n == 0 {
		path = []int{t.groupFirst + groupIndex}
	} else {
		parent := &t.frames[n-1]
		path = append(slices.Clip(parent.path), parent.inners)
		parent.inners++
	}
	t.frames = append(t.frames, devFrame{ep: ep, gi: groupIndex, path: path,
		budgetMark: len(t.budget), readsMark: len(t.reads)})
	if t.tracing {
		t.trace.BeforeTxn(ep, groupIndex)
	}
}

// AfterTxn ends the frame, and one an inner transaction that failed left open.
func (t *DevTracer) AfterTxn(ep *logic.EvalParams, groupIndex int, ad transactions.ApplyData, evalError error) {
	if t.tracing {
		t.trace.AfterTxn(ep, groupIndex, ad, evalError)
	}
	i := t.frameOf(ep, groupIndex)
	if i < 0 {
		t.fail(fmt.Errorf("transaction %d of a group ended without starting", groupIndex))
		return
	}
	t.frames = t.frames[:i]
}

// DetailedEvalErrors makes the traced group's errors (a failed ClearState
// program's) read as simulate's.
func (t *DevTracer) DetailedEvalErrors() bool {
	return (t.tracing || t.sigTraced()) && t.trace.DetailedEvalErrors()
}

// runLogicSigs evaluates the logic sigs of the top-level group of ep, which
// begins at index t.tops, with this tracer following them.
func (t *DevTracer) runLogicSigs(ep *logic.EvalParams) {
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
	sep := logic.NewSigEvalParams(group, ep.Proto, t.sigLedger)
	sep.Tracer = t
	for gi := range group {
		if !group[gi].Lsig.HasProgram() {
			continue
		}
		pass, _, err := logic.EvalSignatureFull(gi, sep)
		if err == nil && !pass {
			err = errors.New("rejected")
		}
		if err != nil {
			t.fail(fmt.Errorf("the logic sig of transaction %d: %w", t.tops+gi, err))
			return
		}
	}
}

// startBudget adds a budget entry for the program cx begins, and returns its index.
func (t *DevTracer) startBudget(cx *logic.EvalContext, path []int, txid transactions.Txid, kind ProgramKind) int {
	t.budget = append(t.budget, ProgramBudget{
		Path: uint64Path(path), TxID: txid, Kind: kind, App: cx.AppID(),
		BudgetBefore: cx.RemainingBudget(),
	})
	t.grow(budgetSize + int64(len(path))*8)
	return len(t.budget) - 1
}

func (t *DevTracer) endBudget(idx int, cx *logic.EvalContext, pass bool, evalError error) {
	if idx < 0 || idx >= len(t.budget) {
		return
	}
	b := &t.budget[idx]
	b.Cost = cx.Cost()
	b.BudgetAfter = cx.RemainingBudget()
	b.Failed = !pass || evalError != nil
}

// BeforeProgram starts a budget entry for the program, and keeps the traced group's programs.
func (t *DevTracer) BeforeProgram(cx *logic.EvalContext) {
	sig := cx.RunMode() == logic.ModeSig
	if (sig && t.sigTraced()) || (!sig && t.tracing) {
		t.trace.BeforeProgram(cx)
		t.keepProgram(cx)
	}
	if t.include&IncludeBudget == 0 {
		return
	}
	idx := -1
	if sig {
		// before its group begins
		if top := t.tops + cx.GroupIndex(); !t.tooLarge && t.reports(top) {
			idx = t.startBudget(cx, []int{top}, cx.TxnGroup[cx.GroupIndex()].ID(), ProgramLogicSig)
		}
	} else if path, ok := t.current(); ok {
		kind := ProgramApproval
		if cx.TxnGroup[cx.GroupIndex()].Txn.OnCompletion == transactions.ClearStateOC {
			kind = ProgramClearState
		}
		idx = t.startBudget(cx, path, t.txidOf(path), kind)
	}
	t.running = append(t.running, scRunning{cx: cx, idx: idx})
}

// AfterProgram completes the program's budget entry. A failed ClearState
// program's inner transactions are discarded by the evaluator: their entries
// are marked failed and their reads dropped.
func (t *DevTracer) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	sig := cx.RunMode() == logic.ModeSig
	if (sig && t.sigTraced()) || (!sig && t.tracing) {
		t.trace.AfterProgram(cx, pass, evalError)
	}
	if t.include&IncludeBudget != 0 {
		for i := len(t.running) - 1; i >= 0; i-- {
			if t.running[i].cx == cx {
				t.endBudget(t.running[i].idx, cx, pass, evalError)
				t.running = t.running[:i]
				break
			}
		}
	}
	if sig || (pass && evalError == nil) || cx.TxnGroup[cx.GroupIndex()].Txn.OnCompletion != transactions.ClearStateOC {
		return
	}
	i := t.frameOf(cx.EvalParams, cx.GroupIndex())
	if i < 0 {
		t.fail(errors.New("a ClearState program ended outside its transaction"))
		return
	}
	// an inner transaction that failed before AfterTxn left its frame
	t.frames = t.frames[:i+1]
	f := &t.frames[i]
	t.discardInners(f)
	f.inners = 0
}

// BeforeOpcode remembers the state a read opcode reads, with IncludeReads, and
// aborts the program once the tracer holds too much.
func (t *DevTracer) BeforeOpcode(cx *logic.EvalContext) {
	sig := cx.RunMode() == logic.ModeSig
	if (sig && t.sigTraced()) || (!sig && t.tracing) {
		t.trace.BeforeOpcode(cx)
	}
	if t.tooLarge {
		// a ClearState program swallows the abort; the answer fails all the same
		panic(logic.EvalAborted{Cause: ErrStateChangesTooLarge})
	}
	if sig || t.include&IncludeReads == 0 {
		return
	}
	if _, reported := t.current(); !reported {
		return // the groups before a replayed one cost no lookup
	}
	p, ok, err := explainStateOp(cx)
	if err != nil {
		t.fail(err)
	}
	if ok && p.op == logic.AppStateRead {
		t.pending, t.hasPending = p, true
	}
}

// AfterOpcode logs the read the opcode made: the value it read.
func (t *DevTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	sig := cx.RunMode() == logic.ModeSig
	if (sig && t.sigTraced()) || (!sig && t.tracing) {
		t.trace.AfterOpcode(cx, evalError)
	}
	if !t.hasPending {
		return
	}
	p := t.pending
	t.hasPending = false
	if evalError == nil {
		t.readOpcode(cx, p)
	}
}

// keepProgram keeps the program cx runs, for the trace's disassembly.
func (t *DevTracer) keepProgram(cx *logic.EvalContext) {
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
func (t *DevTracer) readOpcode(cx *logic.EvalContext, p scPending) {
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
func (t *DevTracer) discardInners(f *devFrame) {
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

// explainStateOp returns the app state the opcode cx is about to run reads,
// writes or deletes, and false for other opcodes. It reads no value: a read
// before the op could change what the evaluator caches.
func explainStateOp(cx *logic.EvalContext) (p scPending, ok bool, err error) {
	if cx.RunMode() != logic.ModeApp {
		return scPending{}, false, nil
	}
	spec := cx.GetOpSpec()
	if spec.AppStateExplain == nil {
		return scPending{}, false, nil
	}
	// The op fails on a short stack (possible in a ClearState program, whose
	// failure the block keeps); explain must not panic first, which would
	// change how the program fails.
	if len(cx.Stack) < len(spec.Arg.Types) {
		return scPending{}, false, nil
	}
	defer func() {
		if x := recover(); x != nil {
			p, ok, err = scPending{}, false, fmt.Errorf("explaining %s: %v", spec.Name, x)
		}
	}()
	typ, op, app, acct, key := spec.AppStateExplain(cx)
	return scPending{typ: typ, op: op, app: app, acct: acct, key: key}, true, nil
}

// groupBounds returns the indexes of the first transaction of the top-level
// group holding payset[top], and of the one after its last.
func groupBounds(payset []transactions.SignedTxnWithAD, top int) (first, end int) {
	first, end = top, top+1
	if g := payset[top].Txn.Group; !g.IsZero() {
		for first > 0 && payset[first-1].Txn.Group == g {
			first--
		}
		for end < len(payset) && payset[end].Txn.Group == g {
			end++
		}
	}
	return first, end
}

// muxTracer calls each of its tracers in turn, in order, for every event.
type muxTracer []logic.EvalTracer

// MuxTracer returns a tracer that calls each of ts, in order, for every event:
// one that changes the evaluation (simulate's) must come first, so that the
// others see what it set. Nil tracers, typed nil pointers included, are left
// out; with none left it returns nil.
func MuxTracer(ts ...logic.EvalTracer) logic.EvalTracer {
	var m muxTracer
	for _, t := range ts {
		if t == nil {
			continue
		}
		if v := reflect.ValueOf(t); v.Kind() == reflect.Pointer && v.IsNil() {
			continue
		}
		m = append(m, t)
	}
	if len(m) == 0 {
		return nil
	}
	if len(m) == 1 {
		return m[0]
	}
	return m
}

func (m muxTracer) BeforeBlock(hdr *bookkeeping.BlockHeader) {
	for _, t := range m {
		t.BeforeBlock(hdr)
	}
}

func (m muxTracer) BeforeTxnGroup(ep *logic.EvalParams) {
	for _, t := range m {
		t.BeforeTxnGroup(ep)
	}
}

func (m muxTracer) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	for _, t := range m {
		t.AfterTxnGroup(ep, deltas, evalError)
	}
}

func (m muxTracer) BeforeTxn(ep *logic.EvalParams, groupIndex int) {
	for _, t := range m {
		t.BeforeTxn(ep, groupIndex)
	}
}

func (m muxTracer) AfterTxn(ep *logic.EvalParams, groupIndex int, ad transactions.ApplyData, evalError error) {
	for _, t := range m {
		t.AfterTxn(ep, groupIndex, ad, evalError)
	}
}

func (m muxTracer) BeforeProgram(cx *logic.EvalContext) {
	for _, t := range m {
		t.BeforeProgram(cx)
	}
}

func (m muxTracer) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	for _, t := range m {
		t.AfterProgram(cx, pass, evalError)
	}
}

func (m muxTracer) BeforeOpcode(cx *logic.EvalContext) {
	for _, t := range m {
		t.BeforeOpcode(cx)
	}
}

func (m muxTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	for _, t := range m {
		t.AfterOpcode(cx, evalError)
	}
}

func (m muxTracer) AfterBlock(hdr *bookkeeping.BlockHeader) {
	for _, t := range m {
		t.AfterBlock(hdr)
	}
}

// DetailedEvalErrors is true when any of the tracers asks for detailed errors.
func (m muxTracer) DetailedEvalErrors() bool {
	for _, t := range m {
		if t.DetailedEvalErrors() {
			return true
		}
	}
	return false
}
