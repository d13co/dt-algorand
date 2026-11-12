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

package logic

// Time travel inside one POST /v2/query call: the time-travel opcodes, which move a program's reads
// to a past round. Non-standard fork feature, kept in its own file to keep upstream merges of
// eval.go, opcodes.go and doc.go clean. Only EvalConstants.TimeTravel makes the opcodes work, and
// only queries set it: simulate and block evaluation refuse a program that has them, as they refuse
// an opcode the AVM lacks, so a node agrees with the chain on every program.

import (
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/data/basics"
)

// timeTravelPrefix is the first byte of every time-travel opcode, whose second byte names it. No
// upstream opcode has it: registerTimeTravelOps panics if one ever does.
const timeTravelPrefix = 0xff

// timeTravelVersion is the first AVM version that has the time-travel opcodes.
const timeTravelVersion = 13

// Costs of the time-travel opcodes, measured against opcodes of known cost (sha256, sha512_256) on
// the same machine.
const (
	// ttSeekCost is the cost of tt_seek: a new ledger on a state the call already reached.
	ttSeekCost = 70
	// ttFirstReachCost is what tt_seek costs more the first time a call reaches a round: building
	// the state the round left. It is charged per call, whether or not the node kept the state from
	// an earlier call, so that a call's cost doesn't depend on which calls ran before it.
	ttFirstReachCost = 2200
)

// TimeTravelOpGroup is the OpGroups entry of the time-travel opcodes.
const TimeTravelOpGroup = "Time Travel"

// timeTravelOpSpecs are the time-travel opcodes.
var timeTravelOpSpecs = []OpSpec{
	{timeTravelPrefix, "tt_seek", opTTSeek, proto("i:"), timeTravelVersion, timeTravelOp(0x01, ttSeekCost)},
}

// timeTravelOpDescs are the docs of the time-travel opcodes.
var timeTravelOpDescs = map[string]OpDesc{
	"tt_seek": {"move the program's reads to the state round A left, as if it ran in the block after A. A the round the program started at returns to its own state",
		"POST /v2/query only (non-standard fork feature); simulate and blocks refuse a program with it. Costs " + fmt.Sprint(ttFirstReachCost) + " more the first time the call reaches A. Fails if A is after the query round, if the call reaches more than max-seek-rounds rounds, if the node has no state of A, or between `itxn_begin` and `itxn_submit`.",
		nil, ""},
}

// SeekOpcodes are the opcodes that move a program's reads, which a node offers when it allows
// seeks (max-seek-rounds above 0).
var SeekOpcodes = []string{"tt_seek"}

// registerTimeTravelOps adds the time-travel opcodes to OpSpecs, opDescByName and OpGroups. It runs
// as a package variable is initialized, before the init of opcodes.go builds the opcode tables.
func registerTimeTravelOps() bool {
	names := make([]string, 0, len(timeTravelOpSpecs))
	for _, spec := range OpSpecs {
		if spec.Opcode == timeTravelPrefix {
			panic(fmt.Sprintf("opcode 0x%02x of the time-travel opcodes is taken by %s", timeTravelPrefix, spec.Name))
		}
	}
	for _, spec := range timeTravelOpSpecs {
		names = append(names, spec.Name)
		opDescByName[spec.Name] = timeTravelOpDescs[spec.Name]
	}
	OpSpecs = append(OpSpecs, timeTravelOpSpecs...)
	OpGroups[TimeTravelOpGroup] = names
	return true
}

var _ = registerTimeTravelOps()

// IsTimeTravelOp reports whether spec is a time-travel opcode, which the docs of the chain's AVM
// leave out.
func IsTimeTravelOp(spec *OpSpec) bool {
	return spec.Opcode == timeTravelPrefix
}

// timeTravelOp is the OpDetails of time-travel opcode sub of the given cost.
func timeTravelOp(sub byte, cost int) OpDetails {
	d := subOp(sub).only(ModeApp).costs(cost)
	d.check = checkTimeTravel
	return d
}

// checkTimeTravel refuses a time-travel opcode outside a query, as the check of a program refuses an
// opcode the AVM lacks.
func checkTimeTravel(cx *EvalContext) error {
	if cx.TimeTravel == nil {
		return errNotQuery(cx.GetOpSpec().Name)
	}
	return nil
}

// errNotQuery is the error of time-travel opcode name outside a query.
func errNotQuery(name string) error {
	return fmt.Errorf("%s is only available in POST /v2/query", name)
}

// TimeTravelLedger is the state of a round a program seeks to.
type TimeTravelLedger interface {
	LedgerForLogic
	LedgerForSignature
}

// TimeTraveller serves the time-travel opcodes of the programs of one query call.
type TimeTraveller interface {
	// Seek returns a new ledger on the state the round rnd left, which a program that seeks to rnd
	// reads as if it ran in the block after it, and writes to until it leaves it; and release, which
	// drops the ledger and its writes, and which the program calls when it leaves it. The ledgers
	// the programs never leave are dropped when the call ends. When no program of the call sought to
	// rnd before, Seek calls firstReach before it builds the round's state, and fails with its
	// error. Its error fails the seek.
	Seek(rnd basics.Round, firstReach func() error) (l TimeTravelLedger, release func(), err error)
}

// timeTravel is what a program's seeks change: the ledgers it ran on before its first seek.
type timeTravel struct {
	moved   bool
	home    LedgerForLogic
	homeSig LedgerForSignature
	// release drops the ledger of the past round the program reads, nil while it reads its own.
	release func()
}

// opTTSeek moves the program's reads to the round on the stack.
func opTTSeek(cx *EvalContext) error {
	last := len(cx.Stack) - 1
	rnd := basics.Round(cx.Stack[last].Uint)
	cx.Stack = cx.Stack[:last]
	if cx.TimeTravel == nil {
		return errNotQuery("tt_seek")
	}
	if len(cx.subtxns) > 0 {
		// they were built against the state the program reads now
		return errors.New("tt_seek between itxn_begin and itxn_submit")
	}
	return cx.timeTravelSeek(rnd)
}

// timeTravelSeek moves the program of cx to round rnd: from then on, it reads the state rnd left,
// as if it ran in the block after it. A seek to the round the program started at goes back to the
// state it started on, its writes included.
func (cx *EvalContext) timeTravelSeek(rnd basics.Round) error {
	if !cx.tt.moved {
		cx.tt.home, cx.tt.homeSig = cx.Ledger, cx.SigLedger
	}
	if rnd+1 == cx.tt.home.Round() {
		cx.leavePast()
		cx.Ledger, cx.SigLedger = cx.tt.home, cx.tt.homeSig
	} else {
		l, release, err := cx.TimeTravel.Seek(rnd, func() error {
			return cx.chargeMore("tt_seek", ttFirstReachCost)
		})
		if err != nil {
			return err
		}
		if l == nil {
			return errors.New("time travel returned no ledger")
		}
		cx.leavePast()
		cx.tt.moved = true
		cx.tt.release = release
		cx.Ledger, cx.SigLedger = l, l
	}
	cx.available.timeTravelled()
	return nil
}

// leavePast drops the ledger of the past round the program reads, and its writes, as the program
// leaves it: each seek gets a new one, so nothing reads it again.
func (cx *EvalContext) leavePast() {
	if cx.tt.release != nil {
		cx.tt.release()
		cx.tt.release = nil
	}
}

// chargeMore takes cost from the program's budget, for work opcode name does beyond its own cost,
// and fails as step does when the budget can't pay it.
func (cx *EvalContext) chargeMore(name string, cost int) error {
	if cost > cx.remainingBudget() {
		return fmt.Errorf("dynamic cost budget exceeded, executing %s: local program cost was %d", name, cx.cost)
	}
	cx.cost += cost
	switch {
	case cx.PooledApplicationBudget != nil:
		*cx.PooledApplicationBudget -= cost
	case cx.PooledLogicSigBudget != nil:
		*cx.PooledLogicSigBudget -= cost
	}
	return nil
}

// timeTravelled readies the group's resources for a ledger a seek moved a program to, which may lack
// the boxes the group wrote, and the apps and assets it created, or hold others under their ids.
// Every box is clean again and every app's program bytes uncharged, so that the next write is
// charged in full, while the bytes charged so far stay charged: what the group writes on every
// ledger it seeks to stays within the one write budget, as it is all held until the call ends. The
// group's created apps and assets are forgotten, so that their boxes are looked up rather than
// taken to be empty. The resources are shared with the caller's frames, whose writes are charged
// again too.
func (r *resources) timeTravelled() {
	if r == nil {
		return
	}
	for br := range r.boxes {
		r.boxes[br] = false
	}
	clear(r.updateBytes)
	clear(r.createdApps)
	clear(r.createdAsas)
}

// startLedger is the ledger the program started on, before any seek.
func (cx *EvalContext) startLedger() LedgerForLogic {
	if cx.tt.moved {
		return cx.tt.home
	}
	return cx.Ledger
}

// timeTravelValidity is the validity availableRound checks the rounds of a query program that may
// seek against: that of a txn valid only in the round the program reads as the current one, so the
// window moves with each seek. Before any seek it is the validity of the query's call, whose first
// and last valid rounds are that round.
func (cx *EvalContext) timeTravelValidity() (firstValid, lastValid basics.Round) {
	current := cx.Ledger.Round()
	return current, current
}
