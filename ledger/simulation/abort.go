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

// Stopping a simulation early, when its request is canceled (the client left, or the node's time
// limit passed). Non-standard fork feature, kept in its own file to keep upstream merges of
// simulator.go and tracer.go clean.

import (
	"context"

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

// abortState is the part of an evalTracer that stops the simulation.
type abortState struct {
	ctx  context.Context
	done <-chan struct{} // ctx.Done(); nil when ctx can't be canceled
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
	return abortState{ctx: ctx, done: ctx.Done(), effort: ledgercore.EffortFromContext(ctx)}
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
	return a.aborted
}

// stopped returns why the group that just ran was cut short, or nil: a program stopped part way.
// Unlike abortError it does not poll the context, so a group that finished as the context ended
// keeps its result.
func (a *abortState) stopped() error {
	return a.aborted
}

// abortCheckCost is how much opcode budget a simulation uses between two checks of its request's
// context. Time per budget unit varies less than time per opcode (one
// ec_pairing_check can take milliseconds): 16,384 units take about a millisecond, cheap opcodes
// and crypto alike, while checking before every opcode would slow the cheapest ones by a fifth.
const abortCheckCost = 16384

// checkAbort stops the program cx runs, from BeforeOpcode, once the request's context is done. It
// looks every abortCheckCost of opcode budget.
func (a *abortState) checkAbort(cx *logic.EvalContext) {
	if a.aborted == nil && a.done != nil {
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
