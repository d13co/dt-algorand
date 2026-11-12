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
// to a past round, or find when state changed. Non-standard fork feature, kept in its own file to
// keep upstream merges of
// eval.go, opcodes.go and doc.go clean. Only EvalConstants.TimeTravel makes the opcodes work, and
// only queries set it: simulate and block evaluation refuse a program that has them, as they refuse
// an opcode the AVM lacks, so a node agrees with the chain on every program.

import (
	"errors"
	"fmt"
	"math"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
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
	// ttChangeCost is the cost of a change lookup: opening the history at the range.
	ttChangeCost = 420
	// ttVersionCost is what a change lookup costs more for each version it examined, charged after
	// it ran. The bytes a key or box prefix search reads besides are bounded by the scan limits.
	ttVersionCost = 20
)

// TimeTravelOpGroup is the OpGroups entry of the time-travel opcodes.
const TimeTravelOpGroup = "Time Travel"

// timeTravelOpSpecs are the time-travel opcodes.
var timeTravelOpSpecs = []OpSpec{
	{timeTravelPrefix, "tt_seek", opTTSeek, proto("i:"), timeTravelVersion, timeTravelOp(0x01, ttSeekCost)},

	{timeTravelPrefix, "tt_acct_change", ttChangeOp(ChangeOfAccount), proto("Aii:ii"), timeTravelVersion, timeTravelOp(0x10, ttChangeCost)},
	{timeTravelPrefix, "tt_holding_change", ttChangeOp(ChangeOfHolding), proto("Aiii:ii"), timeTravelVersion, timeTravelOp(0x11, ttChangeCost)},
	{timeTravelPrefix, "tt_local_change", ttChangeOp(ChangeOfLocal), proto("Aiii:ii"), timeTravelVersion, timeTravelOp(0x12, ttChangeCost)},
	{timeTravelPrefix, "tt_asset_change", ttChangeOp(ChangeOfAsset), proto("iii:ii"), timeTravelVersion, timeTravelOp(0x13, ttChangeCost)},
	{timeTravelPrefix, "tt_app_change", ttChangeOp(ChangeOfApp), proto("iii:ii"), timeTravelVersion, timeTravelOp(0x14, ttChangeCost)},
	{timeTravelPrefix, "tt_box_change", ttChangeOp(ChangeOfBox), proto("iNii:ii"), timeTravelVersion, timeTravelOp(0x15, ttChangeCost)},
	{timeTravelPrefix, "tt_global_key_change", ttChangeOp(ChangeOfGlobalKey), proto("iKii:iia"), timeTravelVersion, timeTravelOp(0x16, ttChangeCost)},
	{timeTravelPrefix, "tt_local_key_change", ttChangeOp(ChangeOfLocalKey), proto("AiKii:iia"), timeTravelVersion, timeTravelOp(0x17, ttChangeCost)},
	{timeTravelPrefix, "tt_box_prefix_change", ttChangeOp(ChangeOfBoxPrefix), proto("ibii:iib"), timeTravelVersion, timeTravelOp(0x18, ttChangeCost)},
}

// changeDoc is what the docs of every change lookup say.
var changeDoc = "POST /v2/query only (non-standard fork feature), on a node that keeps history; simulate and blocks refuse a program with it. Its last two args are the range's ends, from and to, inclusive: it searches oldest first when from <= to, else newest first. Status: 0 no change, 1 created, 2 changed, 3 deleted. Costs " + fmt.Sprint(ttVersionCost) + " more for each version it examined. Fails if from or to is after the query round or before the history's first round."

// searchDoc is what the docs of the key and box prefix searches add.
var searchDoc = " A search reads at most max-scan-step-bytes of history: then status is 4, and X the round to resume from, as from. The searches of a call read at most max-call-scan-bytes in all; a search once they are spent fails the call."

// timeTravelOpDescs are the docs of the time-travel opcodes.
var timeTravelOpDescs = map[string]OpDesc{
	"tt_seek": {"move the program's reads to the state round A left, as if it ran in the block after A. A the round the program started at returns to its own state",
		"POST /v2/query only (non-standard fork feature); simulate and blocks refuse a program with it. The txns of the program's group then read A+1 as their FirstValid and LastValid, as do the inner txns it sends, and FirstValidTime the timestamp of A; an inner txn's FirstValidTime fails once a later seek's block window lacks A. Costs " + fmt.Sprint(ttFirstReachCost) + " more the first time the call reaches A. Fails if A is after the query round, if the call reaches more than max-seek-rounds rounds, if the node has no state of A, or between `itxn_begin` and `itxn_submit`.",
		nil, ""},

	"tt_acct_change":    {"round X and status Y of the first change of account A's base record (balance, status, keys and totals, not its holdings, local states or params) between rounds B and C", changeDoc, nil, ""},
	"tt_holding_change": {"round X and status Y of the first change of account A's holding of asset B between rounds C and D", changeDoc, nil, ""},
	"tt_local_change":   {"round X and status Y of the first change of account A's local state of app B between rounds C and D", changeDoc, nil, ""},
	"tt_asset_change":   {"round X and status Y of the first change of asset A's params between rounds B and C", changeDoc, nil, ""},
	"tt_app_change":     {"round X and status Y of the first change of app A's params (programs, schemas and global state) between rounds B and C", changeDoc, nil, ""},
	"tt_box_change":     {"round X and status Y of the first change of box B of app A between rounds C and D", changeDoc + " The box's contents are not returned: seek to X and read them.", nil, ""},
	"tt_global_key_change": {"round X and status Y of the first change of key B of app A's global state between rounds C and D, and Z its new value (0 if deleted or unchanged)",
		changeDoc + searchDoc, nil, ""},
	"tt_local_key_change": {"round X and status Y of the first change of key C of account A's local state of app B between rounds D and E, and Z its new value (0 if deleted or unchanged)",
		changeDoc + searchDoc, nil, ""},
	"tt_box_prefix_change": {"round X and status Y of the first change of a box of app A whose name starts with B between rounds C and D, and Z its name: of the boxes that changed in round X, the first by name",
		changeDoc + searchDoc + " The box index must reach the rounds searched.", nil, ""},
}

// SeekOpcodes are the opcodes that move a program's reads, which a node offers when it allows
// seeks (max-seek-rounds above 0).
var SeekOpcodes = []string{"tt_seek"}

// ChangeOpcodes are the change lookups of whole objects, which a node offers when it allows seeks
// and keeps history (EnableTimeTravel).
var ChangeOpcodes = []string{"tt_acct_change", "tt_holding_change", "tt_local_change", "tt_asset_change", "tt_app_change", "tt_box_change"}

// SearchOpcodes are the change lookups of state keys and box name prefixes, which a node offers
// when it offers ChangeOpcodes and its calls may search (max-call-scan-bytes above 0).
var SearchOpcodes = []string{"tt_global_key_change", "tt_local_key_change", "tt_box_prefix_change"}

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
	// Change finds the first change that req asks for. Its error fails the lookup.
	Change(req TimeTravelChangeRequest) (TimeTravelChange, error)
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
	moved bool
	// away is set while the program reads a past round rather than its own.
	away    bool
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
		cx.tt.away = false
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
		cx.tt.moved, cx.tt.away = true, true
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

// txnValidity is the first and last valid rounds of txn, of the program's group, as the program
// reads them (txn and gtxn fields, the block window of availableRound) and gives the inner txns it
// sends. While the program reads a past round, they are those of a txn valid only in the round it
// reads as the current one, so they move with each seek: the apps it calls see the round they run
// at, and can read its header (FirstValidTime). At its own round they are txn's own, as before any
// seek: the query's call, and the inner txns of a program that sought, are valid only in the round
// their program reads.
func (cx *EvalContext) txnValidity(txn *transactions.Transaction) (firstValid, lastValid basics.Round) {
	if cx.tt.away {
		current := cx.Ledger.Round()
		return current, current
	}
	return txn.FirstValid, txn.LastValid
}

// TimeTravelChangeKind is what a change lookup looks at.
type TimeTravelChangeKind uint8

const (
	// ChangeOfAccount is an account's base record: balance, status, keys and totals, without its
	// holdings, local states and params.
	ChangeOfAccount TimeTravelChangeKind = iota + 1
	// ChangeOfHolding is an account's holding of an asset.
	ChangeOfHolding
	// ChangeOfLocal is an account's whole local state of an app.
	ChangeOfLocal
	// ChangeOfAsset is an asset's params.
	ChangeOfAsset
	// ChangeOfApp is an app's params: its programs, schemas and whole global state.
	ChangeOfApp
	// ChangeOfBox is one box of an app.
	ChangeOfBox
	// ChangeOfGlobalKey is a key of an app's global state.
	ChangeOfGlobalKey
	// ChangeOfLocalKey is a key of an account's local state of an app.
	ChangeOfLocalKey
	// ChangeOfBoxPrefix is any box of an app whose name starts with a prefix.
	ChangeOfBoxPrefix
)

// TimeTravelChangeRequest is a change lookup a program asks for: the first change of an object from
// round From toward round To, both inclusive, oldest first if From <= To, else newest first.
type TimeTravelChangeRequest struct {
	Kind    TimeTravelChangeKind
	Address basics.Address // the account, for ChangeOfAccount, Holding, Local and LocalKey
	ID      uint64         // the asset or app
	Key     []byte         // the box name, the state key, or the box name prefix
	From    basics.Round
	To      basics.Round
}

// Statuses of a TimeTravelChange.
const (
	// TimeTravelNoChange: nothing changed in the range.
	TimeTravelNoChange uint8 = iota
	// TimeTravelCreated: absent before the round, present after it.
	TimeTravelCreated
	// TimeTravelChanged: present before and after the round, with another value.
	TimeTravelChanged
	// TimeTravelDeleted: present before the round, absent after it.
	TimeTravelDeleted
	// TimeTravelMore: the search spent its scan step before finding a change. Round is the round to
	// pass as from to resume it.
	TimeTravelMore
)

// TimeTravelChange is what a change lookup found.
type TimeTravelChange struct {
	Round  basics.Round
	Status uint8
	// Value is a state key's new value, nil if it was deleted or nothing changed.
	Value *basics.TealValue
	// Name is the name of the box that changed, for a box name prefix.
	Name []byte
	// Versions is the number of versions the lookup examined, which it is charged for.
	Versions uint64
}

// ttChangeArgs are the args of each change lookup before from and to: 'a' an address, 'i' an app or
// asset id, 'b' a name or key.
var ttChangeArgs = map[TimeTravelChangeKind]string{
	ChangeOfAccount:   "a",
	ChangeOfHolding:   "ai",
	ChangeOfLocal:     "ai",
	ChangeOfAsset:     "i",
	ChangeOfApp:       "i",
	ChangeOfBox:       "ib",
	ChangeOfGlobalKey: "ib",
	ChangeOfLocalKey:  "aib",
	ChangeOfBoxPrefix: "ib",
}

// ttChangeOp is the op of the change lookup of kind: it pops its args, then from and to, and pushes
// the round and status of the change, then for a state key its new value (0 if none), and for a box
// name prefix the box's name.
func ttChangeOp(kind TimeTravelChangeKind) evalFunc {
	types := ttChangeArgs[kind]
	return func(cx *EvalContext) error {
		name := cx.GetOpSpec().Name
		if cx.TimeTravel == nil {
			return errNotQuery(name)
		}
		n := len(types) + 2
		args := cx.Stack[len(cx.Stack)-n:]
		req := TimeTravelChangeRequest{Kind: kind, From: basics.Round(args[n-2].Uint), To: basics.Round(args[n-1].Uint)}
		for i, t := range []byte(types) {
			switch t {
			case 'a':
				if len(args[i].Bytes) != len(basics.Address{}) {
					return fmt.Errorf("%s: an address is 32 bytes, not %d", name, len(args[i].Bytes))
				}
				copy(req.Address[:], args[i].Bytes)
			case 'i':
				req.ID = args[i].Uint
			case 'b':
				req.Key = args[i].Bytes
			}
		}
		c, err := cx.TimeTravel.Change(req)
		if err != nil {
			return err
		}
		if err = cx.chargeMore(name, int(min(c.Versions, uint64(math.MaxInt32/ttVersionCost)))*ttVersionCost); err != nil {
			return err
		}
		cx.Stack = append(cx.Stack[:len(cx.Stack)-n], stackValue{Uint: uint64(c.Round)}, stackValue{Uint: uint64(c.Status)})
		switch kind {
		case ChangeOfGlobalKey, ChangeOfLocalKey:
			var v stackValue
			if c.Value != nil {
				if v, err = stackValueFromTealValue(*c.Value); err != nil {
					return err
				}
			}
			cx.Stack = append(cx.Stack, v)
		case ChangeOfBoxPrefix:
			box := c.Name
			if box == nil {
				box = []byte{}
			}
			cx.Stack = append(cx.Stack, stackValue{Bytes: box})
		}
		return nil
	}
}
