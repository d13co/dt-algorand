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
	require.Equal(t, SeekOpcodes, OpGroups[TimeTravelOpGroup])
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
