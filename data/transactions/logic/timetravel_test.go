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

import (
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// stubTraveller is a TimeTraveller of tests: each seek gets a new ledger at the round sought to,
// where app 888 has another creator, and fails at round 7.
type stubTraveller struct {
	creator basics.Address
	// sought lists the rounds Seek returned a ledger of, and reached has them.
	sought  []basics.Round
	reached map[basics.Round]bool
	// live counts the ledgers Seek returned that are not released, and released those released.
	live, released int
	// change answers Change, which records its requests in asked.
	change func(req TimeTravelChangeRequest) (TimeTravelChange, error)
	asked  []TimeTravelChangeRequest
}

func (s *stubTraveller) Change(req TimeTravelChangeRequest) (TimeTravelChange, error) {
	s.asked = append(s.asked, req)
	if s.change == nil {
		return TimeTravelChange{}, errors.New("no change lookups")
	}
	return s.change(req)
}

func (s *stubTraveller) Seek(rnd basics.Round, firstReach func() error) (TimeTravelLedger, func(), error) {
	if rnd == 7 {
		return nil, nil, errors.New("no state at round 7")
	}
	if !s.reached[rnd] {
		if err := firstReach(); err != nil {
			return nil, nil, err
		}
	}
	s.sought = append(s.sought, rnd)
	if s.reached == nil {
		s.reached = make(map[basics.Round]bool)
	}
	s.reached[rnd] = true
	l := NewLedger(nil)
	l.rnd = rnd + 1
	l.NewApp(s.creator, 888, basics.AppParams{})
	s.live++
	done := false
	return l, func() {
		if done {
			panic("released twice")
		}
		done = true
		s.live--
		s.released++
	}, nil
}

// makeTimeTravelEnv returns an app call of app 888 at round home+1, with a budget of 100,000, and
// the stub that serves its seeks.
func makeTimeTravelEnv(home basics.Round) (*EvalParams, *transactions.Transaction, *stubTraveller) {
	ep, tx, ledger := makeSampleEnv()
	ledger.rnd = home + 1
	ledger.NewApp(tx.Receiver, 888, basics.AppParams{})
	ledger.NewAccount(appAddr(888), 1_000_000)
	proto := *ep.Proto
	proto.MaxAppProgramCost = 100_000
	ep.Proto = &proto
	stub := &stubTraveller{creator: tx.Sender}
	ep.TimeTravel = stub
	return ep, tx, stub
}

// TestTimeTravelSeek: a seek moves the program's round and block window, but not its app's creator;
// a seek to the round it started at goes back to its own ledger, and a failed seek fails the
// program.
func TestTimeTravelSeek(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, tx, stub := makeTimeTravelEnv(1000)
	testApp(t, fmt.Sprintf(`global Round; int 1001; ==; assert
int 1000; block BlkTimestamp; pop
int 100; tt_seek
global Round; int 101; ==; assert
int 100; block BlkTimestamp; pop
global CreatorAddress; addr %s; ==; assert
int 1000; tt_seek
global Round; int 1001; ==; assert
int 1000; block BlkTimestamp; pop
int 1`, tx.Receiver), ep)
	require.Equal(t, []basics.Round{100}, stub.sought)

	testApp(t, "int 100; tt_seek; int 101; block BlkTimestamp; pop; int 1", ep, "round 101 is not available. It's outside [1-100]")
	testApp(t, "int 7; tt_seek; int 1", ep, "no state at round 7")
	testApp(t, "itxn_begin; int 100; tt_seek; int 1", ep, "tt_seek between itxn_begin and itxn_submit")
}

// TestTimeTravelSeekCost: a seek costs ttSeekCost, and ttFirstReachCost more the first time the call
// reaches a round, but not to go back to the round the program started at. A seek the budget can't
// pay fails before it builds the round's state.
func TestTimeTravelSeekCost(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, _, stub := makeTimeTravelEnv(1000)
	// global OpcodeBudget; int; tt_seek; global OpcodeBudget, around each seek
	testApp(t, fmt.Sprintf(`global OpcodeBudget; int 100; tt_seek; global OpcodeBudget; -; int %d; ==; assert
global OpcodeBudget; int 1000; tt_seek; global OpcodeBudget; -; int %d; ==; assert
global OpcodeBudget; int 100; tt_seek; global OpcodeBudget; -; int %d; ==; assert
int 1`, 2+ttSeekCost+ttFirstReachCost, 2+ttSeekCost, 2+ttSeekCost), ep)
	require.Equal(t, []basics.Round{100, 100}, stub.sought)

	ep, _, stub = makeTimeTravelEnv(1000)
	proto := *ep.Proto
	proto.MaxAppProgramCost = 2000
	ep.Proto = &proto
	testApp(t, "int 100; tt_seek; int 1", ep, "dynamic cost budget exceeded, executing tt_seek")
	require.Empty(t, stub.sought)
}

// TestTimeTravelSeekRelease: a program releases the ledger of a past round as it leaves it, by a
// seek elsewhere or home, and leaves the one it ends on to the end of the call.
func TestTimeTravelSeekRelease(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, _, stub := makeTimeTravelEnv(1000)
	testApp(t, "int 100; tt_seek; int 200; tt_seek; int 1000; tt_seek; int 100; tt_seek; int 1", ep)
	require.Equal(t, []basics.Round{100, 200, 100}, stub.sought)
	require.Equal(t, 2, stub.released)
	require.Equal(t, 1, stub.live)
}

// TestTimeTravelNotQuery: the time-travel opcodes run only in queries, which set
// EvalConstants.TimeTravel; elsewhere a program with them fails its check, as one with an opcode
// the AVM lacks.
func TestTimeTravelNotQuery(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, _, _ := makeTimeTravelEnv(1000)
	ep.TimeTravel = nil
	ops := testProg(t, "int 100; tt_seek; int 1", LogicVersion)
	err := CheckContract(ops.Program, 0, ep)
	require.ErrorContains(t, err, "tt_seek is only available in POST /v2/query")
	_, err = EvalApp(ops.Program, 0, 888, ep)
	require.ErrorContains(t, err, "tt_seek is only available in POST /v2/query")

	testLogic(t, "int 100; tt_seek; int 1", LogicVersion, nil, "tt_seek not allowed in current mode", "tt_seek not allowed in current mode")
	testProg(t, "int 100; tt_seek; int 1", timeTravelVersion-1, exp(1, "tt_seek opcode was introduced in v13"))
}

// TestTimeTravelOpcodes: the time-travel opcodes start with byte 0xff and disassemble to their names,
// and no upstream opcode starts with it.
func TestTimeTravelOpcodes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ops := testProg(t, "int 100; tt_seek; int 1", timeTravelVersion)
	require.Equal(t, []byte{0x81, 0x64, 0xff, 0x01, 0x81, 0x01}, ops.Program[1:])
	text, err := Disassemble(ops.Program)
	require.NoError(t, err)
	require.Contains(t, text, "\ntt_seek\n")

	for _, spec := range OpSpecs {
		require.Equal(t, IsTimeTravelOp(&spec), spec.Opcode == timeTravelPrefix)
	}
	require.Equal(t, append(append(slices.Clone(SeekOpcodes), ChangeOpcodes...), SearchOpcodes...), OpGroups[TimeTravelOpGroup])
	for _, name := range OpGroups[TimeTravelOpGroup] {
		require.Equal(t, uint64(timeTravelVersion), OpsByName[timeTravelVersion][name].Version, name)
		require.NotContains(t, OpsByName[timeTravelVersion-1], name)
	}
}

// TestTimeTravelSeekBoxes: what a program writes to boxes on every ledger it seeks to counts toward
// its one write budget, even when it writes a box it wrote before the seek, and a box deleted or
// shrunk after a seek gives nothing back.
func TestTimeTravelSeekBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// each program runs on a new ledger, with a write budget of 4 box references
	env := func() *EvalParams {
		ep, tx, _ := makeTimeTravelEnv(1000)
		tx.Boxes = []transactions.BoxRef{{Name: []byte("b")}, {Name: []byte("c")}, {Name: []byte("d")}, {Name: []byte("e")}}
		return ep
	}
	size := env().Proto.BytesPerBoxReference

	// creates "b" at home, then again after each of n seeks
	creates := func(n int) string {
		program := fmt.Sprintf(`byte "b"; int %d; box_create; assert`, size)
		for i := range n {
			program += fmt.Sprintf("\nint %d; tt_seek;", 100+i) + fmt.Sprintf(`byte "b"; int %d; box_create; assert`, size)
		}
		return program + "\nint 1"
	}
	testApp(t, creates(3), env())
	testApp(t, creates(4), env(), "write budget exceeded")

	// a box shrunk after a seek gives back only what was charged for it after the seek
	testApp(t, fmt.Sprintf(`byte "b"; int 1; box_create; assert
int 100; tt_seek; byte "b"; int %d; box_create; assert
byte "b"; int 1; box_resize
int 1`, size), env())

	// a box deleted after a seek home gives nothing back: n times, it is deleted and created again
	recreates := func(n int) string {
		program := fmt.Sprintf(`byte "b"; int %d; box_create; assert`, size)
		for range n {
			program += "\nint 100; tt_seek; int 1000; tt_seek;" + fmt.Sprintf(`byte "b"; box_del; assert; byte "b"; int %d; box_create; assert`, size)
		}
		return program + "\nint 1"
	}
	testApp(t, recreates(3), env())
	testApp(t, recreates(4), env(), "write budget exceeded")
}

// TestTimeTravelChange: each change lookup passes its args, and from and to, to the traveller, and
// pushes the round and status of the change, the new value of a state key (as its type, 0 when there
// is none) and the name of a box under a prefix.
func TestTimeTravelChange(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, tx, stub := makeTimeTravelEnv(1000)
	stub.change = func(req TimeTravelChangeRequest) (TimeTravelChange, error) {
		c := TimeTravelChange{Round: req.From + 1, Status: TimeTravelChanged}
		switch string(req.Key) {
		case "u":
			c.Value = &basics.TealValue{Type: basics.TealUintType, Uint: 9}
		case "b":
			c.Value = &basics.TealValue{Type: basics.TealBytesType, Bytes: "nine"}
		case "gone":
			c.Status = TimeTravelDeleted
		case "p":
			c.Name = []byte("p1")
		}
		return c, nil
	}
	addr := tx.Sender
	testApp(t, fmt.Sprintf(`addr %[1]s; int 10; int 20; tt_acct_change; int 2; ==; assert; int 11; ==; assert
addr %[1]s; int 7; int 10; int 20; tt_holding_change; pop; pop
addr %[1]s; int 8; int 10; int 20; tt_local_change; pop; pop
int 7; int 10; int 20; tt_asset_change; pop; pop
int 8; int 10; int 20; tt_app_change; pop; pop
int 8; byte "box"; int 10; int 20; tt_box_change; pop; pop
int 8; byte "u"; int 20; int 10; tt_global_key_change; int 9; ==; assert; pop; pop
int 8; byte "b"; int 20; int 10; tt_global_key_change; byte "nine"; ==; assert; pop; pop
addr %[1]s; int 8; byte "gone"; int 10; int 20; tt_local_key_change; !; assert; int 3; ==; assert; pop
int 8; byte "p"; int 10; int 20; tt_box_prefix_change; byte "p1"; ==; assert; pop; pop
int 1`, addr), ep)
	want := []TimeTravelChangeRequest{
		{Kind: ChangeOfAccount, Address: addr, From: 10, To: 20},
		{Kind: ChangeOfHolding, Address: addr, ID: 7, From: 10, To: 20},
		{Kind: ChangeOfLocal, Address: addr, ID: 8, From: 10, To: 20},
		{Kind: ChangeOfAsset, ID: 7, From: 10, To: 20},
		{Kind: ChangeOfApp, ID: 8, From: 10, To: 20},
		{Kind: ChangeOfBox, ID: 8, Key: []byte("box"), From: 10, To: 20},
		{Kind: ChangeOfGlobalKey, ID: 8, Key: []byte("u"), From: 20, To: 10},
		{Kind: ChangeOfGlobalKey, ID: 8, Key: []byte("b"), From: 20, To: 10},
		{Kind: ChangeOfLocalKey, Address: addr, ID: 8, Key: []byte("gone"), From: 10, To: 20},
		{Kind: ChangeOfBoxPrefix, ID: 8, Key: []byte("p"), From: 10, To: 20},
	}
	require.Equal(t, want, stub.asked)

	// a stack address must be 32 bytes, as the assembler can't tell of one computed
	testApp(t, "byte 0x01; dup; concat; int 1; int 2; tt_acct_change; int 1", ep, "tt_acct_change: an address is 32 bytes, not 2")
	// the traveller's error fails the program
	stub.change = func(TimeTravelChangeRequest) (TimeTravelChange, error) {
		return TimeTravelChange{}, errors.New("no history here")
	}
	testApp(t, "int 8; int 1; int 2; tt_app_change; int 1", ep, "no history here")
}

// TestTimeTravelChangeCost: a change lookup costs ttChangeCost, and ttVersionCost more for each
// version it examined; a lookup the budget can't pay for fails after it ran.
func TestTimeTravelChangeCost(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, _, stub := makeTimeTravelEnv(1000)
	versions := uint64(0)
	stub.change = func(TimeTravelChangeRequest) (TimeTravelChange, error) {
		return TimeTravelChange{Versions: versions}, nil
	}
	for _, versions = range []uint64{0, 1, 7} {
		// 6: the three ints, the two pops and the second global OpcodeBudget
		testApp(t, fmt.Sprintf(`global OpcodeBudget; int 8; int 1; int 2; tt_app_change; pop; pop; global OpcodeBudget; -; int %d; ==`,
			6+ttChangeCost+int(versions)*ttVersionCost), ep)
	}

	proto := *ep.Proto
	proto.MaxAppProgramCost = 1000
	ep.Proto = &proto
	versions = 100
	testApp(t, "int 8; int 1; int 2; tt_app_change; pop; pop; int 1", ep, "dynamic cost budget exceeded, executing tt_app_change")
}
