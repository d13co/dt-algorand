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

// Per-transaction app state changes of a past round (GET
// /v2/deltas/{round}/txn/state-changes). Non-standard fork feature: the
// round's block is re-evaluated with stateChangeTracer, which logs every state
// write in order, and a post-pass turns the log into before/after values per
// transaction, starting from the state at the round before.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"unsafe"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/protocol"
)

// StateChange is an app state key that a transaction changed, net over the
// transaction and its inner transactions.
//
//msgp:ignore StateChange
type StateChange struct {
	App     basics.AppIndex
	Type    logic.AppStateEnum // GlobalState, LocalState or BoxState
	Account basics.Address     // LocalState only
	Key     string             // a box's name for BoxState
	Before  *basics.TealValue  // nil: the key did not exist before
	After   *basics.TealValue  // nil: the key was deleted (or its state cleared)
}

// TxnStateChanges are the state changes of a transaction, inner transactions
// included.
//
//msgp:ignore TxnStateChanges
type TxnStateChanges struct {
	// Path is the index in the payset, then the inner indexes: an inner
	// transaction's index counts all of its parent's inner transactions, as
	// EvalDelta.InnerTxns does.
	Path    []uint64
	TxID    transactions.Txid // top-level transactions only
	Changes []StateChange
}

// StateChanges are the transactions of a round that changed app state, in
// execution order.
//
//msgp:ignore StateChanges
type StateChanges struct {
	Round basics.Round
	Txns  []TxnStateChanges
	// With StateChangesFilter.Include of groups: the reported groups, in block
	// order, the round's fee sink and its consensus version (for minimum balances).
	Groups  []GroupStateChanges
	FeeSink basics.Address
	Proto   protocol.ConsensusVersion
	// With IncludeBudget: each program run of the reported transactions, in the
	// order they began; a group's logic sigs come before its app programs.
	Budget []ProgramBudget
	// With IncludeReads: the reported transactions that read app state, top-level
	// and inner, in the order they first read.
	Reads []TxnStateReads
	// With IncludeTrace: the traced group.
	Trace *GroupTrace
}

// StateChangesFilter narrows StateChangesForRound's answer; the evaluation is
// the same.
//
//msgp:ignore StateChangesFilter
type StateChangesFilter struct {
	App      basics.AppIndex     // 0: every app
	TxIDs    []transactions.Txid // these top-level transactions (repeats ignored) and their inner ones; empty: every transaction. One not in the round is ErrTxnNotInRound, before any evaluation.
	MaxBytes uint64              // 0: no limit
	Include  StateChangesInclude // what to report besides app state changes (TxIDs narrows it all, App only Txns); 0: nothing
	// Trace, with IncludeTrace, makes the tracer of the group of TxIDs' one
	// transaction (ErrTraceNeedsOneTxn otherwise), which must not change the
	// evaluation. It sees that group's logic sigs run, then the group. A panic
	// in it fails the answer. If it has a Bytes() int64 method, the bytes it
	// holds count against the log's limit, as ErrStateChangesTooLarge.
	Trace func(group []transactions.SignedTxnWithAD) logic.EvalTracer
}

// ErrStateChangesTooLarge is returned when the state changes take more than
// StateChangesFilter.MaxBytes.
var ErrStateChangesTooLarge = errors.New("the state changes are too large")

// ErrTxnNotInRound is returned when one of StateChangesFilter.TxIDs is not a
// top-level transaction of the round.
var ErrTxnNotInRound = errors.New("the transaction is not in the round")

// stateChangesLogFactor bounds the bytes of the tracer's log at this many
// times StateChangesFilter.MaxBytes: the log holds values that a transaction's
// net leaves out (intermediate writes between inner calls), so it can outgrow
// the answer, but not by much in a block that isn't built to.
const stateChangesLogFactor = 4

// StateChangesForRound returns the app state changes of each transaction of
// round rnd, from its block re-evaluated against the history at rnd-1 (see
// reevalBlock, including its errors). It needs the time travel history.
func (l *Ledger) StateChangesForRound(ctx context.Context, rnd basics.Round, f StateChangesFilter) (StateChanges, error) {
	if l.history == nil {
		return StateChanges{}, errors.New("state changes need the time travel history")
	}
	if rnd == 0 {
		return StateChanges{}, errors.New("round 0 has no block to evaluate")
	}
	ledgercore.EffortFromContext(ctx).Lookup(false) // the block re-evaluated
	if l.historyAt(rnd) == nil {
		return StateChanges{}, fmt.Errorf("round %d is not in the history", rnd)
	}
	t := newStateChangeTracer(f)
	var blk *bookkeeping.Block // loaded for the txids, else by reevalBlock
	var payset []transactions.SignedTxnWithAD
	if len(f.TxIDs) > 0 {
		// an unknown txid costs no evaluation
		var err error
		blk, payset, t.only, err = l.paysetIndexes(rnd, f.TxIDs)
		if err != nil {
			return StateChanges{}, err
		}
		for i := range t.only {
			t.lastTop = max(t.lastTop, i)
		}
	}
	var dev *DevTracer
	var trace *GroupTrace
	if f.Include&devIncludes != 0 {
		cfg := DevConfig{Include: f.Include, Only: t.only}
		if f.Include&(IncludeBudget|IncludeTrace) != 0 {
			// the re-evaluation doesn't run the logic sigs: the tracer does
			cfg.SigLedger = pastLedger{l.Counted(ledgercore.EffortFromContext(ctx)), rnd - 1}
		}
		if f.Include&IncludeTrace != 0 {
			if len(t.only) != 1 || f.Trace == nil {
				return StateChanges{}, ErrTraceNeedsOneTxn
			}
			var top int
			for i := range t.only {
				top = i
			}
			first, end := groupBounds(payset, top)
			trace = &GroupTrace{First: first, TxIDs: make([]transactions.Txid, end-first)}
			group := make([]transactions.SignedTxnWithAD, end-first)
			for i := range group {
				group[i] = payset[first+i]
				trace.TxIDs[i] = group[i].ID()
			}
			cfg.TraceFirst, cfg.Trace = first, f.Trace(group)
		}
		dev = newDevTracer(cfg, t.evalLimit)
	}
	evalCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	t.cancel = cancel // stops the evaluation once the log is too large, or on a failure
	// dev first: past the byte limit, its trace tracer sees an opcode begin before the
	// programs abort
	tracer := MuxTracer(dev, t)
	var sc StateChanges
	// The post-pass runs in the reeval slot too: its reads of rnd-1 can reach
	// the history on disk. No trackerMu here: reevalBlock and the post-pass's
	// lookups take it.
	_, err := l.reevalBlock(evalCtx, rnd, blk, ReevalAdmissionWait, tracer, func() error {
		if t.tooLarge {
			return ErrStateChangesTooLarge
		}
		if t.err != nil {
			return fmt.Errorf("%w: round %d: %w", ErrDeltaReevaluation, rnd, t.err)
		}
		var err error
		sc, err = t.changes(ctx, pastLedger{l.Counted(ledgercore.EffortFromContext(ctx)), rnd - 1}, rnd, f)
		if err != nil && ctx.Err() == nil && !errors.Is(err, ErrStateChangesTooLarge) {
			err = fmt.Errorf("%w: round %d: reading the state before the round: %w", ErrDeltaReevaluation, rnd, err)
		}
		if err == nil && dev != nil {
			sc.Budget, sc.Reads = dev.Budget(), dev.Reads()
			if trace != nil {
				trace.Programs = dev.Programs()
				sc.Trace = trace
			}
		}
		return err
	})
	if t.tooLarge && ctx.Err() == nil {
		return StateChanges{}, ErrStateChangesTooLarge
	}
	if t.err != nil && ctx.Err() == nil { // it cancelled the evaluation
		return StateChanges{}, fmt.Errorf("%w: round %d: %w", ErrDeltaReevaluation, rnd, t.err)
	}
	return sc, err
}

// paysetIndexes returns block rnd, its payset and the set of payset indexes of
// its top-level transactions txids, which may repeat.
func (l *Ledger) paysetIndexes(rnd basics.Round, txids []transactions.Txid) (*bookkeeping.Block, []transactions.SignedTxnWithAD, map[int]bool, error) {
	blk, err := l.Block(rnd)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("round %d: its block is not stored: %w", rnd, err)
	}
	payset, err := blk.DecodePaysetFlat()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: round %d: %w", ErrDeltaReevaluation, rnd, err)
	}
	want := make(map[transactions.Txid]bool, len(txids))
	for _, txid := range txids {
		want[txid] = true
	}
	tops := make(map[int]bool, len(want))
	for i := range payset {
		if want[payset[i].ID()] {
			tops[i] = true
			delete(want, payset[i].ID())
		}
	}
	for _, txid := range txids { // the first missing one, in the request's order
		if want[txid] {
			return nil, nil, nil, fmt.Errorf("%w: %s, round %d", ErrTxnNotInRound, txid.String(), rnd)
		}
	}
	return &blk, payset, tops, nil
}

// scKind is the kind of a stateChangeTracer event.
//
//msgp:ignore scKind
type scKind uint8

const (
	scWrite        scKind = iota // a key written or deleted by an opcode
	scClearLocals                // CloseOut or ClearState: the sender's local state is gone
	scClearGlobals               // app delete: its globals are gone, its boxes stay
)

// scEvent is a state change in the order of evaluation.
type scEvent struct {
	path   []int // the transaction that made it
	kind   scKind
	typ    logic.AppStateEnum // scWrite
	app    basics.AppIndex
	acct   basics.Address // LocalState writes and scClearLocals
	key    string         // scWrite
	exists bool           // scWrite: false is a delete
	val    basics.TealValue
}

// scFrame is a transaction being evaluated.
type scFrame struct {
	ep     *logic.EvalParams
	gi     int
	path   []int
	mark   int // len(log) when it began
	inners int // inner transactions it has spawned
}

// scPending is the state an opcode is about to write.
type scPending struct {
	typ  logic.AppStateEnum
	op   logic.AppStateOpEnum
	app  basics.AppIndex
	acct basics.Address
	key  string
}

// stateChangeTracer logs every app state change of a block's evaluation, in
// order, with the transaction that made it. Changes later discarded (a failed
// ClearState program) leave the log.
//
// Writes of a key within one stretch of a transaction's program (between its
// inner calls) share one event: transactions' nets need only the first value
// before and the last after. The log's bytes are bounded (maxBytes).
type stateChangeTracer struct {
	logic.NullEvalTracer

	*evalLimit // the log's bytes, as eventBytes counts them, shared with a DevTracer's

	frames     []scFrame
	tops       []*transactions.Transaction // top-level transactions of the groups begun, in payset order
	groupMark  int                         // len(log) when the current top-level group began
	log        []scEvent
	pending    scPending
	hasPending bool

	app     basics.AppIndex // log only this app's changes (0: all)
	only    map[int]bool    // the payset indexes of StateChangesFilter.TxIDs (nil: every transaction)
	lastTop int             // log nothing after this top-level transaction (-1: no limit)

	lastIdx map[scKey]int // the last write event of each key
	barrier int           // events before it are of an earlier stretch: never rewritten

	include StateChangesInclude
	latest  map[gKey]gVal // each record's value after the latest group that changed it
	groups  []*scGroup    // the reported groups, in block order
	feeSink basics.Address
	proto   protocol.ConsensusVersion
}

func newStateChangeTracer(f StateChangesFilter) *stateChangeTracer {
	t := &stateChangeTracer{
		evalLimit: &evalLimit{maxBytes: int64(min(f.MaxBytes, math.MaxInt64/stateChangesLogFactor)) * stateChangesLogFactor},
		app:       f.App,
		lastTop:   -1,
		lastIdx:   make(map[scKey]int),
		include:   f.Include,
		latest:    make(map[gKey]gVal),
	}
	t.onFull = append(t.onFull, func() { t.log, t.lastIdx = nil, nil })
	return t
}

// devIncludes are the includes a DevTracer records.
const devIncludes = IncludeBudget | IncludeReads | IncludeTrace

// scEventSize is the in-memory size of an scEvent, without what it points to.
const scEventSize = int64(unsafe.Sizeof(scEvent{}))

// eventBytes counts an event's share of the log: the event, its key and value.
// Its path is shared with its frame's.
func eventBytes(ev *scEvent) int64 {
	return scEventSize + int64(len(ev.key)+len(ev.val.Bytes))
}

// logs reports whether a change of app by the current transaction belongs in the log.
func (t *stateChangeTracer) logs(app basics.AppIndex) bool {
	if t.tooLarge || (t.app != 0 && app != t.app) || len(t.frames) == 0 {
		return false
	}
	return t.lastTop < 0 || t.frames[0].path[0] <= t.lastTop
}

// add appends ev to the log, or rewrites the key's event of the current stretch.
func (t *stateChangeTracer) add(ev scEvent) {
	if ev.kind == scWrite {
		k := scKey{typ: ev.typ, app: ev.app, acct: ev.acct, key: ev.key}
		// i < len(log) and the key check skip an entry left by a truncation
		if i, ok := t.lastIdx[k]; ok && i >= t.barrier && i < len(t.log) && t.log[i].kind == scWrite && t.log[i].scKey() == k {
			old := &t.log[i]
			d := int64(len(ev.val.Bytes) - len(old.val.Bytes))
			old.exists, old.val = ev.exists, ev.val
			t.grow(d)
			return
		}
		t.lastIdx[k] = len(t.log)
	}
	t.log = append(t.log, ev)
	t.grow(eventBytes(&ev))
}

// truncate drops the events from mark on, and starts a new stretch.
func (t *stateChangeTracer) truncate(mark int) {
	if t.tooLarge {
		return
	}
	for i := mark; i < len(t.log); i++ {
		ev := &t.log[i]
		t.bytes -= eventBytes(ev)
		if ev.kind == scWrite {
			if k := ev.scKey(); t.lastIdx[k] == i {
				delete(t.lastIdx, k)
			}
		}
	}
	t.log = t.log[:mark]
	t.barrier = mark
}

// frameOf returns the index of the innermost frame of transaction gi of ep, or -1.
func (t *stateChangeTracer) frameOf(ep *logic.EvalParams, gi int) int {
	for i := len(t.frames) - 1; i >= 0; i-- {
		if t.frames[i].ep == ep && t.frames[i].gi == gi {
			return i
		}
	}
	return -1
}

// BeforeTxnGroup counts a top-level group's transactions, in payset order,
// before any of them begins: one that fails leaves the later ones unbegun.
func (t *stateChangeTracer) BeforeTxnGroup(ep *logic.EvalParams) {
	if ep.GetCaller() != nil {
		return
	}
	t.frames = t.frames[:0]
	t.groupMark = len(t.log)
	t.barrier = len(t.log)
	for i := range ep.TxnGroup {
		t.tops = append(t.tops, &ep.TxnGroup[i].Txn)
	}
}

// AfterTxnGroup records the values a top-level group left. A top-level group
// that failed, which no block holds (a replay), leaves nothing: the evaluator
// discards all of it. One that panicked arrives without its error (the
// evaluator recovers after this call), but with transactions left open.
func (t *stateChangeTracer) AfterTxnGroup(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	if ep.GetCaller() == nil && (evalError != nil || len(t.frames) != 0) {
		t.truncate(t.groupMark)
		t.frames = t.frames[:0]
		return
	}
	t.groupDelta(ep, deltas, evalError)
}

// BeforeTxn starts a frame: a top-level transaction is at its payset index, an
// inner one at the next index of its parent's inner transactions.
func (t *stateChangeTracer) BeforeTxn(ep *logic.EvalParams, groupIndex int) {
	var path []int
	if n := len(t.frames); n == 0 {
		// the group's transactions are the last ones counted
		path = []int{len(t.tops) - len(ep.TxnGroup) + groupIndex}
	} else {
		parent := &t.frames[n-1]
		path = append(slices.Clip(parent.path), parent.inners)
		parent.inners++
	}
	t.frames = append(t.frames, scFrame{ep: ep, gi: groupIndex, path: path, mark: len(t.log)})
	t.barrier = len(t.log)
}

// AfterTxn ends the frame. A local state cleared by CloseOut or ClearState,
// and the globals of a deleted app, are cleared after the approval program's
// writes, as apply.ApplicationCall does.
func (t *stateChangeTracer) AfterTxn(ep *logic.EvalParams, groupIndex int, ad transactions.ApplyData, evalError error) {
	i := t.frameOf(ep, groupIndex)
	if i < 0 {
		t.fail(fmt.Errorf("transaction %d of a group ended without starting", groupIndex))
		return
	}
	f := t.frames[i]
	if evalError != nil {
		// not in a committed block: its changes are discarded
		t.truncate(f.mark)
		t.frames = t.frames[:i]
		return
	}
	t.barrier = len(t.log)
	txn := &ep.TxnGroup[groupIndex].Txn
	if txn.Type == protocol.ApplicationCallTx {
		app := txn.ApplicationID
		if app == 0 {
			app = ad.ApplicationID
		}
		if t.logs(app) {
			switch txn.OnCompletion {
			case transactions.CloseOutOC, transactions.ClearStateOC:
				t.add(scEvent{path: f.path, kind: scClearLocals, app: app, acct: txn.Sender})
			case transactions.DeleteApplicationOC:
				t.add(scEvent{path: f.path, kind: scClearGlobals, app: app})
			}
		}
	}
	t.frames = t.frames[:i]
}

// BeforeOpcode remembers the state a write or delete opcode targets. It reads
// no value: a read before the op could change what the evaluator caches.
func (t *stateChangeTracer) BeforeOpcode(cx *logic.EvalContext) {
	if t.tooLarge {
		// Stop the program, and so the evaluation, at once. A ClearState
		// program swallows the abort (EvalContract makes it an EvalError) and
		// the evaluation goes on: the tooLarge checks in StateChangesForRound,
		// not this abort, are what keep a partial answer out.
		panic(logic.EvalAborted{Cause: ErrStateChangesTooLarge})
	}
	p, ok, err := explainStateOp(cx)
	if err != nil {
		t.fail(err)
	}
	if ok && (p.op == logic.AppStateWrite || p.op == logic.AppStateDelete) {
		t.pending, t.hasPending = p, true
	}
}

// AfterOpcode logs the value the opcode left, read from the evaluation's ledger.
func (t *stateChangeTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	if !t.hasPending {
		return
	}
	p := t.pending
	t.hasPending = false
	if evalError != nil {
		return
	}
	n := len(t.frames)
	if n == 0 {
		t.fail(errors.New("a state write outside a transaction"))
		return
	}
	if !t.logs(p.app) {
		return
	}
	val, exists, err := logic.AppStateValue(cx, p.typ, p.op, p.app, p.acct, p.key)
	if err != nil {
		t.fail(fmt.Errorf("reading app %d state after a write: %w", p.app, err))
		return
	}
	ev := scEvent{path: t.frames[n-1].path, kind: scWrite, typ: p.typ, app: p.app, key: p.key, exists: exists}
	if exists {
		ev.val = val
	}
	if p.typ == logic.LocalState {
		ev.acct = p.acct
	}
	t.add(ev)
}

// AfterProgram drops what a failed ClearState program changed, inner
// transactions included: the evaluator discards it and the block has none of
// it. The clear itself still happens (AfterTxn).
func (t *stateChangeTracer) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	if pass && evalError == nil {
		return
	}
	// a LogicSig on a ClearState call fires this too
	if cx.RunMode() != logic.ModeApp || cx.TxnGroup[cx.GroupIndex()].Txn.OnCompletion != transactions.ClearStateOC {
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
	t.truncate(f.mark)
	f.inners = 0
}

// scKey is an app state key.
type scKey struct {
	typ  logic.AppStateEnum
	app  basics.AppIndex
	acct basics.Address
	key  string
}

func (ev *scEvent) scKey() scKey {
	return scKey{typ: ev.typ, app: ev.app, acct: ev.acct, key: ev.key}
}

// scContainer is an app's globals or an account's local state of an app.
type scContainer struct {
	typ  logic.AppStateEnum
	app  basics.AppIndex
	acct basics.Address
}

// scValue is a key's value; ok is false when the key does not exist.
type scValue struct {
	ok  bool
	val basics.TealValue
}

func (v scValue) ptr() *basics.TealValue {
	if !v.ok {
		return nil
	}
	val := v.val
	return &val
}

// scNet is a key's change over a transaction's subtree: before the first event, after the last.
type scNet struct {
	before, after scValue
}

// scTxn accumulates a transaction's changes.
type scTxn struct {
	path []int
	keys []scKey // first touched first
	nets map[scKey]*scNet
}

// stateChangesBase reads the state before the round.
type stateChangesBase interface {
	GetCreatorForRound(basics.Round, basics.CreatableIndex, basics.CreatableType) (basics.Address, bool, error)
	LookupApplication(basics.Round, basics.Address, basics.AppIndex) (ledgercore.AppResource, error)
	LookupAsset(basics.Round, basics.Address, basics.AssetIndex) (ledgercore.AssetResource, error)
	LookupKvExists(basics.Round, string) ([]byte, bool, error)
	LookupWithoutRewards(basics.Round, basics.Address) (ledgercore.AccountData, basics.Round, error)
}

// scReplay holds the running state of the post-pass.
type scReplay struct {
	base       stateChangesBase
	rnd        basics.Round // the round before the block
	containers map[scContainer]map[string]basics.TealValue
	boxes      map[scKey]scValue // a deleted box stays, without its value, so that it isn't read again
	used       int64             // the log's bytes, then what is read from the round before
	limit      int64             // the tracer's maxBytes, shared with the log; 0: no limit
}

// charge counts n bytes read from the round before against the limit.
func (r *scReplay) charge(n int64) error {
	r.used += n
	if r.limit > 0 && r.used > r.limit {
		return ErrStateChangesTooLarge
	}
	return nil
}

// container returns a container's running keys, loading them from the round
// before the first time. An app or local state that did not exist then (made
// in the block) starts empty.
func (r *scReplay) container(c scContainer) (map[string]basics.TealValue, error) {
	if kv, ok := r.containers[c]; ok {
		return kv, nil
	}
	var src basics.TealKeyValue
	switch c.typ {
	case logic.GlobalState:
		creator, ok, err := r.base.GetCreatorForRound(r.rnd, basics.CreatableIndex(c.app), basics.AppCreatable)
		if err != nil {
			return nil, err
		}
		if ok {
			res, err := r.base.LookupApplication(r.rnd, creator, c.app)
			if err != nil {
				return nil, err
			}
			if res.AppParams != nil {
				src = res.AppParams.GlobalState
			}
		}
	case logic.LocalState:
		res, err := r.base.LookupApplication(r.rnd, c.acct, c.app)
		if err != nil {
			return nil, err
		}
		if res.AppLocalState != nil {
			src = res.AppLocalState.KeyValue
		}
	}
	n := int64(len(src)) * scEventSize // a map entry and a change made from it
	for k, v := range src {
		n += int64(len(k) + len(v.Bytes))
	}
	if err := r.charge(n); err != nil {
		return nil, err
	}
	kv := make(map[string]basics.TealValue, len(src))
	maps.Copy(kv, src)
	r.containers[c] = kv
	return kv, nil
}

// box returns a box's running value, loading it from the round before the
// first time.
func (r *scReplay) box(k scKey) (scValue, error) {
	if v, ok := r.boxes[k]; ok {
		return v, nil
	}
	b, ok, err := r.base.LookupKvExists(r.rnd, apps.MakeBoxKey(uint64(k.app), k.key))
	if err != nil {
		return scValue{}, err
	}
	if err := r.charge(scEventSize + int64(len(k.key)+len(b))); err != nil {
		return scValue{}, err
	}
	v := scValue{ok: ok}
	if ok {
		v.val = basics.TealValue{Type: basics.TealBytesType, Bytes: string(b)}
	}
	r.boxes[k] = v
	return v, nil
}

// apply applies a change to a key, and returns its value before.
func (r *scReplay) apply(k scKey, after scValue) (scValue, error) {
	if k.typ == logic.BoxState {
		before, err := r.box(k)
		if err != nil {
			return scValue{}, err
		}
		r.boxes[k] = after
		return before, nil
	}
	kv, err := r.container(scContainer{typ: k.typ, app: k.app, acct: k.acct})
	if err != nil {
		return scValue{}, err
	}
	old, ok := kv[k.key]
	if after.ok {
		kv[k.key] = after.val
	} else {
		delete(kv, k.key)
	}
	return scValue{ok: ok, val: old}, nil
}

// pathKey encodes the first n indexes of path as a map key.
func pathKey(path []int, n int) string {
	b := make([]byte, 0, 2*n)
	for _, i := range path[:n] {
		b = binary.AppendUvarint(b, uint64(i))
	}
	return string(b)
}

// Lower bounds of the encoded size of an answer, in JSON and in msgpack, where
// field names alone take more: a change has at least its app-id,
// app-state-type and key fields (32 bytes), a value its type, uint and bytes
// (24), a local state change its account (32); a transaction its path and
// changes fields (16), one byte per path index, and a top-level one its txid.
const (
	changeSizeMin  = 32
	valueSizeMin   = 24
	accountSizeMin = 32
	txnSizeMin     = 16
	txidSize       = 52
)

func valueSizeBound(v scValue) uint64 {
	if !v.ok {
		return 0
	}
	return valueSizeMin + 1 + uint64(len(v.val.Bytes))
}

func changeSizeBound(k scKey, net *scNet) uint64 {
	size := changeSizeMin + uint64(len(k.key)) + valueSizeBound(net.before) + valueSizeBound(net.after)
	if k.typ == logic.LocalState {
		size += accountSizeMin
	}
	return size
}

func txnSizeBound(pathLen int) uint64 {
	size := uint64(txnSizeMin + pathLen)
	if pathLen == 1 {
		size += txidSize
	}
	return size
}

// changes replays the log from the state at rnd-1, read from base, and returns
// each transaction's net changes: each key's value before the transaction's
// first change and after its last, inner transactions included. Keys that end
// as they began, and transactions left without changes, are left out.
func (t *stateChangeTracer) changes(ctx context.Context, base stateChangesBase, rnd basics.Round, f StateChangesFilter) (StateChanges, error) {
	r := &scReplay{
		base:       base,
		rnd:        rnd - 1,
		containers: make(map[scContainer]map[string]basics.TealValue),
		boxes:      make(map[scKey]scValue),
		used:       t.bytes,
		limit:      t.maxBytes,
	}
	txns := make(map[string]*scTxn)
	record := func(path []int, k scKey, before, after scValue) {
		for n := 1; n <= len(path); n++ {
			pk := pathKey(path, n)
			tx := txns[pk]
			if tx == nil {
				tx = &scTxn{path: path[:n], nets: make(map[scKey]*scNet)}
				txns[pk] = tx
			}
			net := tx.nets[k]
			if net == nil {
				net = &scNet{before: before}
				tx.nets[k] = net
				tx.keys = append(tx.keys, k)
			}
			net.after = after
		}
	}
	for i := range t.log {
		if i%1024 == 0 && ctx.Err() != nil {
			return StateChanges{}, context.Cause(ctx)
		}
		ev := &t.log[i]
		if f.App != 0 && ev.app != f.App {
			continue // apps' state never mix
		}
		if t.lastTop >= 0 && ev.path[0] > t.lastTop {
			break // later transactions can't change what the filtered ones did
		}
		// the transactions before a filtered one are replayed, not attributed:
		// they set the state it starts from
		attribute := t.only == nil || t.only[ev.path[0]]
		switch ev.kind {
		case scWrite:
			k := scKey{typ: ev.typ, app: ev.app, acct: ev.acct, key: ev.key}
			after := scValue{ok: ev.exists, val: ev.val}
			before, err := r.apply(k, after)
			if err != nil {
				return StateChanges{}, err
			}
			if attribute {
				record(ev.path, k, before, after)
			}
		case scClearLocals, scClearGlobals:
			c := scContainer{typ: logic.LocalState, app: ev.app, acct: ev.acct}
			if ev.kind == scClearGlobals {
				c = scContainer{typ: logic.GlobalState, app: ev.app}
			}
			kv, err := r.container(c)
			if err != nil {
				return StateChanges{}, err
			}
			keys := make([]string, 0, len(kv))
			for k := range kv {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			for _, key := range keys {
				before := scValue{ok: true, val: kv[key]}
				delete(kv, key)
				if attribute {
					record(ev.path, scKey{typ: c.typ, app: c.app, acct: c.acct, key: key}, before, scValue{})
				}
			}
		}
	}

	list := make([]*scTxn, 0, len(txns))
	for _, tx := range txns {
		list = append(list, tx)
	}
	slices.SortFunc(list, func(a, b *scTxn) int { return slices.Compare(a.path, b.path) })
	out := StateChanges{Round: rnd}
	var size uint64
	for _, tx := range list {
		var changes []StateChange
		for _, k := range tx.keys {
			net := tx.nets[k]
			if net.before == net.after {
				continue
			}
			changes = append(changes, StateChange{
				App: k.app, Type: k.typ, Account: k.acct, Key: k.key,
				Before: net.before.ptr(), After: net.after.ptr(),
			})
			size += changeSizeBound(k, net)
			if f.MaxBytes > 0 && size > f.MaxBytes {
				return StateChanges{}, ErrStateChangesTooLarge
			}
		}
		if len(changes) == 0 {
			continue
		}
		size += txnSizeBound(len(tx.path))
		e := TxnStateChanges{Path: make([]uint64, len(tx.path)), Changes: changes}
		for i, p := range tx.path {
			e.Path[i] = uint64(p)
		}
		if len(tx.path) == 1 {
			e.TxID = t.tops[tx.path[0]].ID()
		}
		out.Txns = append(out.Txns, e)
	}
	if t.include&includeGroups != 0 {
		groups, err := t.groupChanges(ctx, r, f, &size)
		if err != nil {
			return StateChanges{}, err
		}
		out.Groups, out.FeeSink, out.Proto = groups, t.feeSink, t.proto
	}
	return out, nil
}
