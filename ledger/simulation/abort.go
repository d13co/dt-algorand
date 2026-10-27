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

// Stopping a simulation early: when its request is canceled (the client left, or the node's time
// limit passed), or when its execution trace grows too large. Non-standard fork feature, kept in
// its own file to keep upstream merges of simulator.go and tracer.go clean.

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"unsafe"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// WithContext returns r simulated under ctx: once ctx is done, the simulation stops within
// abortCheckCost of opcode budget, or before its next group, and Simulate returns
// context.Cause(ctx).
func (r Request) WithContext(ctx context.Context) Request {
	r.ctx = ctx
	return r
}

// Context returns the context set by WithContext, or context.Background.
func (r Request) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// WithTraceBudget returns r with its execution trace counted against b, which may be shared with
// the other simulations of one REST request. A nil b sets no limit.
func (r Request) WithTraceBudget(b *TraceBudget) Request {
	r.traceBudget = b
	return r
}

// ErrTraceTooLarge is the cause of a simulation stopped because its execution trace outgrew its
// TraceBudget. The error Simulate returns wraps it.
var ErrTraceTooLarge = errors.New("execution trace too large")

// TraceBudget limits the memory the execution traces of one REST request keep, across all of its
// groups and rounds. The count is an estimate of the trace kept in memory (see
// opcodeTraceUnitBytes); encoding the response takes a few times that.
type TraceBudget struct {
	limit uint64
	used  atomic.Uint64
}

// NewTraceBudget returns a budget of limit bytes, or nil (no limit) when limit is 0.
func NewTraceBudget(limit uint64) *TraceBudget {
	if limit == 0 {
		return nil
	}
	return &TraceBudget{limit: limit}
}

// Used returns the trace bytes counted so far.
func (b *TraceBudget) Used() uint64 {
	if b == nil {
		return 0
	}
	return b.used.Load()
}

// charge counts n more bytes and reports whether the budget is exceeded.
func (b *TraceBudget) charge(n uint64) bool {
	return b.used.Add(n) > b.limit
}

// err returns the error of an exceeded budget, nil otherwise.
func (b *TraceBudget) err() error {
	if b == nil || b.used.Load() <= b.limit {
		return nil
	}
	return fmt.Errorf("%w: over %d bytes; simulate fewer groups or rounds, or leave out stack, scratch or state changes", ErrTraceTooLarge, b.limit)
}

// abortState is the part of an evalTracer that stops the simulation.
type abortState struct {
	ctx         context.Context
	done        <-chan struct{} // ctx.Done(); nil when ctx can't be canceled
	traceBudget *TraceBudget
	// The opcode budget used since the last check, and the program and cost it was last counted at.
	sinceCheck int
	lastCx     *logic.EvalContext
	lastCost   int
	// aborted is set once the simulation must stop: every later opcode stops its program.
	aborted error
	// effort counts the budget the programs consumed (EnableEffortHeader); nil: not counted.
	effort *ledgercore.Effort
}

func makeAbortState(request Request) abortState {
	ctx := request.Context()
	return abortState{ctx: ctx, done: ctx.Done(), traceBudget: request.traceBudget, effort: ledgercore.EffortFromContext(ctx)}
}

// abortError returns why the simulation must stop, or nil. It polls the context, so it is asked
// before new work starts.
func (a *abortState) abortError() error {
	if a.aborted != nil {
		return a.aborted
	}
	if a.done != nil {
		select {
		case <-a.done:
			a.aborted = context.Cause(a.ctx)
			return a.aborted
		default:
		}
	}
	if err := a.traceBudget.err(); err != nil {
		a.aborted = err
	}
	return a.aborted
}

// stopped returns why the group that just ran was cut short, or nil: a program stopped part way,
// or the trace budget exceeded. Unlike abortError it does not poll the context, so a group that
// finished as the context ended keeps its result.
func (a *abortState) stopped() error {
	if a.aborted == nil {
		a.aborted = a.traceBudget.err()
	}
	return a.aborted
}

// abortCheckCost is how much opcode budget a simulation uses between two checks of its request's
// context and trace budget. Time per budget unit varies less than time per opcode (one
// ec_pairing_check can take milliseconds): 16,384 units take about a millisecond, cheap opcodes
// and crypto alike, while checking before every opcode would slow the cheapest ones by a fifth.
const abortCheckCost = 16384

// checkAbort stops the program cx runs, from BeforeOpcode, once the simulation must stop: when the
// request's context is done, or the trace budget, which the other simulations of the request
// share, is exceeded. It looks every abortCheckCost of opcode budget.
func (a *abortState) checkAbort(cx *logic.EvalContext) {
	if a.aborted == nil && (a.done != nil || a.traceBudget != nil) {
		cost := cx.Cost()
		if cx != a.lastCx {
			// another program, or an inner one, or back from one: count from here
			a.lastCx, a.lastCost = cx, cost
		}
		a.sinceCheck += max(cost-a.lastCost, 1)
		a.lastCost = cost
		if a.sinceCheck < abortCheckCost {
			return
		}
		a.sinceCheck = 0
		_ = a.abortError() // sets a.aborted
	}
	if a.aborted != nil {
		panic(logic.EvalAborted{Cause: a.aborted})
	}
}

// chargeTrace counts n bytes of execution trace. An exceeded budget stops the program at its next
// opcode: chargeTrace runs in AfterOpcode too, which may run while a panic unwinds.
func (a *abortState) chargeTrace(n uint64) {
	if a.traceBudget != nil && a.aborted == nil && a.traceBudget.charge(n) {
		a.aborted = a.traceBudget.err()
	}
}

// The estimated sizes of the parts of an execution trace, for the TraceBudget. Slices grow by
// doubling, so their spare capacity is counted too, roughly, by doubling the fixed sizes.
const (
	opcodeTraceUnitBytes = 2 * uint64(unsafe.Sizeof(OpcodeTraceUnit{}))
	tealValueBytes       = 2 * uint64(unsafe.Sizeof(basics.TealValue{}))
	scratchChangeBytes   = 2 * uint64(unsafe.Sizeof(ScratchChange{}))
	stateOperationBytes  = 2 * uint64(unsafe.Sizeof(StateOperation{}))
)

// changeBytes returns the estimated size of the stack, scratch and state changes recorded in o.
func (o *OpcodeTraceUnit) changeBytes() uint64 {
	var n uint64
	for _, v := range o.StackAdded {
		n += tealValueBytes + uint64(len(v.Bytes))
	}
	for _, c := range o.ScratchSlotChanges {
		n += scratchChangeBytes + uint64(len(c.NewValue.Bytes))
	}
	for _, c := range o.StateChanges {
		n += stateOperationBytes + uint64(len(c.Key)) + uint64(len(c.NewValue.Bytes))
	}
	return n
}
