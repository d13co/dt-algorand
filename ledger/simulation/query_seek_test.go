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

package simulation_test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// seekSub is a subroutine that moves the program's reads to the round on the stack.
const seekSub = `
seek:
tt_seek
retsub`

// seekCounterApp adds 1 to its global "c" each time it is called, and logs the new count.
const seekCounterApp = `#pragma version 10
txn ApplicationID; bz create
byte "c"; byte "c"; app_global_get; int 1; +; dup; store 0; app_global_put
load 0; itob; log
int 1; return
create:
int 1`

// createSeekCounter creates a seekCounterApp.
func createSeekCounter(f *queryFixture) basics.AppIndex {
	return f.env.CreateApp(f.env.Accounts[0].Addr, simulationtesting.AppParams{
		ApprovalProgram:   seekCounterApp,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})
}

// callApp calls app in a block.
func callApp(f *queryFixture, app basics.AppIndex) {
	sender := f.env.Accounts[0]
	f.env.Txn(f.env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app}).Txn().Sign(sender.Sk))
}

// roundArg is a call whose first arg is the round r, followed by the strings a.
func roundArg(r basics.Round, a ...string) simulation.QueryCall {
	call := args(a...)
	call.Args = append([][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))}, call.Args...)
	return call
}

// TestQuerySeek: after a seek to round r, a program reads the state r left as if it ran in the
// block after r: its round and timestamp, its block window, accounts, and the apps it calls. A seek
// back to the query round returns to the program's own state, its writes included.
func TestQuerySeek(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	sender := f.env.Accounts[0]
	counter := createSeekCounter(f)
	past := f.env.TxnInfo.LatestRound()
	// the count is 0 at past, 1 at home
	callApp(f, counter)
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 1_000_000)
	late := f.env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 10\nint 1",
		ClearStateProgram: "#pragma version 10\nint 1",
	})
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 2_000_000)
	home := f.env.TxnInfo.LatestRound()

	program := fmt.Sprintf(`#pragma version 13
byte "x"; int 5; app_global_put
global Round; int 1; -; store 1
global OpcodeBudget; store 2
txna ApplicationArgs 0; btoi; callsub seek
txna ApplicationArgs 1; byte "read"; ==; bnz read
txna ApplicationArgs 1; byte "back"; ==; bnz back
txna ApplicationArgs 1; byte "next"; ==; bnz next
txna ApplicationArgs 1; byte "late"; ==; bnz late
txna ApplicationArgs 1; byte "reader"; ==; bnz reader
txna ApplicationArgs 1; byte "creator"; ==; bnz creator
txna ApplicationArgs 1; byte "count"; ==; bnz count
err
creator:
global CreatorAddress; log
txn Sender; log
int 1; return
count:
callsub inc
txna ApplicationArgs 0; btoi; callsub seek
callsub inc
load 1; callsub seek
callsub inc
callsub inc
int 1; return
inc:
itxn_begin
int appl; itxn_field TypeEnum
int %[5]d; itxn_field ApplicationID
itxn_submit
itxn Logs 0; log
retsub
read:
global Round; itob; log
global LatestTimestamp; itob; log
txna ApplicationArgs 0; btoi; block BlkTimestamp; itob; log
addr %[1]s; balance; itob; log
global OpcodeBudget; load 2; <; assert
int 1; return
back:
load 1; callsub seek
global Round; itob; log
addr %[1]s; balance; itob; log
byte "x"; app_global_get; itob; log
int 1; return
next:
txna ApplicationArgs 0; btoi; int 1; +; block BlkTimestamp; pop
int 1; return
reader:
itxn_begin
int appl; itxn_field TypeEnum
int %[4]d; itxn_field ApplicationID
byte "read"; itxn_field ApplicationArgs
itxn_submit
itxn Logs 0; log
itxn Logs 1; log
callsub inc
int 1; return
late:
itxn_begin
int appl; itxn_field TypeEnum
int %[2]d; itxn_field ApplicationID
itxn_submit
int 1; return
%[3]s`, f.other.Addr, late, seekSub, f.reader, counter)

	req := f.request(t, program,
		roundArg(past, "read"), roundArg(past, "back"), roundArg(past, "next"),
		roundArg(past, "late"), roundArg(home, "late"), roundArg(home, "read"), roundArg(past, "reader"),
		roundArg(past, "creator"), roundArg(past, "count"))
	req.GlobalSchema = basics.StateSchema{NumUint: 1}
	s := simulation.MakeSimulator(f.env.Ledger, false)
	result, err := s.Query(req)
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	calls := rr.Calls

	balance := func(r basics.Round) string {
		acct, _, _, err := f.env.Ledger.LookupAccount(r, f.other.Addr)
		require.NoError(t, err)
		return u64(acct.MicroAlgos.Raw)
	}
	timestamp := func(r basics.Round) string {
		hdr, err := f.env.Ledger.BlockHdr(r)
		require.NoError(t, err)
		return u64(uint64(hdr.TimeStamp))
	}

	require.Empty(t, calls[0].Error)
	require.Equal(t, []string{u64(uint64(past + 1)), timestamp(past), timestamp(past), balance(past)}, calls[0].Logs)
	require.NotEqual(t, balance(past), balance(home))

	require.Empty(t, calls[1].Error)
	require.Equal(t, []string{u64(uint64(home + 1)), balance(home), u64(5)}, calls[1].Logs)

	// the block window ends at the round sought to
	require.Contains(t, calls[2].Error, fmt.Sprintf("round %d is not available", past+1))

	// an app created after the round doesn't exist there
	require.Contains(t, calls[3].Error, fmt.Sprintf("app %d does not exist", late))
	require.Empty(t, calls[4].Error)

	// a seek to the query round is a seek home
	require.Empty(t, calls[5].Error)
	require.Equal(t, []string{u64(uint64(home + 1)), timestamp(home), timestamp(home), balance(home)}, calls[5].Logs)

	// an app called after a seek runs at the round, where the query's app has no account, and the
	// count is 0 (1 at home)
	require.Empty(t, calls[6].Error)
	require.Equal(t, []string{"boxval", u64(42), u64(1)}, calls[6].Logs)

	// the app's creator, though the app doesn't exist at the round
	require.Empty(t, calls[7].Error)
	require.Len(t, calls[7].Logs, 2)
	require.Equal(t, calls[7].Logs[1], calls[7].Logs[0])

	// each seek to the past round starts from the state it left (1, then 1 again); back home, the
	// writes stay for the rest of the program (2, then 3)
	require.Empty(t, calls[8].Error)
	require.Equal(t, []string{u64(1), u64(1), u64(2), u64(3)}, calls[8].Logs)
}

// TestQuerySeekErrors: seeks after the query round, beyond MaxSeekRounds, malformed or grouped
// fail the call, and so do all seeks when MaxSeekRounds is 0.
func TestQuerySeekErrors(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	home := f.env.TxnInfo.LatestRound()

	// arg0 rounds of 8 bytes each are sought to in turn
	many := `#pragma version 13
int 0; store 0
loop:
load 0; txna ApplicationArgs 0; len; >=; bnz done
txna ApplicationArgs 0; load 0; extract_uint64; callsub seek
load 0; int 8; +; store 0
b loop
done:
int 1; return
` + seekSub
	rounds := func(rs ...basics.Round) simulation.QueryCall {
		var b []byte
		for _, r := range rs {
			b = binary.BigEndian.AppendUint64(b, uint64(r))
		}
		return simulation.QueryCall{Args: [][]byte{b}}
	}
	req := f.request(t, many,
		rounds(home+1),
		rounds(home-1, home-2, home-3),
		rounds(home-1, home-2, home-1, home, home-2, home-1),
		rounds(home-1, home-2, home, home-3))
	req.Limits.MaxSeekRounds = 2
	result, err := s.Query(req)
	require.NoError(t, err)
	calls := result.Rounds[0].Calls
	require.Contains(t, calls[0].Error, fmt.Sprintf("round %d is after the query round %d", home+1, home))
	require.Contains(t, calls[1].Error, "the call seeks to more than 2 rounds (MaxAPIQuerySeekRounds)")
	// seeking again to a round, or home, doesn't count
	require.Empty(t, calls[2].Error)
	require.Contains(t, calls[3].Error, "MaxAPIQuerySeekRounds")

	// 0 turns time travel off
	req = f.request(t, many, rounds(home-1))
	req.Limits.MaxSeekRounds = 0
	result, err = s.Query(req)
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Error)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "time travel is off on this node (MaxAPIQuerySeekRounds is 0)")

	// a seek while an inner group is being built fails
	result, err = s.Query(f.request(t, `#pragma version 13
itxn_begin
int pay; itxn_field TypeEnum
global CurrentApplicationAddress; itxn_field Receiver
global Round; int 2; -; tt_seek
itxn_submit
int 1`, args()))
	require.NoError(t, err)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "tt_seek between itxn_begin and itxn_submit")

	// a call whose budget can't pay for the first seek to a round fails before it builds the state
	req = f.request(t, many, rounds(home-1))
	req.Budget = 2000
	result, err = s.Query(req)
	require.NoError(t, err)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "dynamic cost budget exceeded, executing tt_seek")
}

// TestQuerySeekMany: a call may seek as often as its budget allows, far more than the 256 inner
// transactions of a call. Each seek costs 70, and 2200 more the first time the call reaches a round.
func TestQuerySeekMany(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	home := f.env.TxnInfo.LatestRound()

	// arg0 times: seeks to the two rounds before the query round, then logs the balance it reads
	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; store 0
loop:
load 0; bz done
int %[1]d; tt_seek
int %[2]d; tt_seek
load 0; int 1; -; store 0
b loop
done:
addr %[3]s; balance; itob; log
int 1`, home-1, home-2, f.other.Addr)
	result, err := s.Query(f.request(t, program, args(u64(0)), args(u64(1)), args(u64(1000))))
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	for i, c := range rr.Calls {
		require.Empty(t, c.Error, "call %d", i)
	}
	acct, _, _, err := f.env.Ledger.LookupAccount(home-2, f.other.Addr)
	require.NoError(t, err)
	require.Equal(t, []string{u64(acct.MicroAlgos.Raw)}, rr.Calls[2].Logs)
	// each pass runs 9 ops besides its 2 seeks; the seeks cost 70 each, and 2200 more for the 2
	// first reaches
	require.Equal(t, 9+2*70+2*2200, rr.Calls[1].Cost-rr.Calls[0].Cost)
	require.Equal(t, 1000*9+2000*70+2*2200, rr.Calls[2].Cost-rr.Calls[0].Cost)
}

// TestQuerySeekState: each round a call seeks to counts toward MaxCallStateBytes, and whether a
// call is over doesn't depend on which calls ran before it on the worker, or built the states it
// seeks to.
func TestQuerySeekState(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// seeks to the arg0 rounds before the query round, nearest first
	program := `#pragma version 13
txna ApplicationArgs 0; btoi; store 0
global Round; int 1; -; store 1
loop:
load 0; bz done
load 1; load 0; -; callsub seek
load 0; int 1; -; store 0
b loop
done:
int 1; return
` + seekSub
	n := func(k uint64) simulation.QueryCall { return args(u64(k)) }
	calls := []simulation.QueryCall{n(4), n(3), n(0), n(4), n(3), n(1), n(3)}
	over := []bool{true, false, false, true, false, false, false}
	for _, workers := range []int{1, 3} {
		req := f.request(t, program, calls...)
		req.Limits.MaxCallStateBytes = 3*4096 + 4000
		req.Limits.Parallelism = workers
		result, err := s.Query(req)
		require.NoError(t, err)
		rr := result.Rounds[0]
		require.Empty(t, rr.Error)
		for i, c := range rr.Calls {
			if over[i] {
				require.Contains(t, c.Error, "MaxAPIQueryCallStateBytes", "workers %d call %d", workers, i)
			} else {
				require.Empty(t, c.Error, "workers %d call %d", workers, i)
			}
		}
	}
}

// TestQuerySeekLookupFailure: a lookup that fails at a round a call seeks to fails the call, not
// the round: the history may lack the round.
func TestQuerySeekLookupFailure(t *testing.T) { // nolint:paralleltest // sets the lookup fault of every query
	partitiontest.PartitionTest(t)

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	home := f.env.TxnInfo.LatestRound()
	defer simulation.SetLookupFault(func(method string, rnd basics.Round) error {
		if method == "LookupWithoutRewards" && rnd == home-2 {
			return errors.New("no such round in the history")
		}
		return nil
	})()

	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; callsub seek
addr %s; balance; itob; log
int 1; return
%s`, f.other.Addr, seekSub)
	result, err := s.Query(f.request(t, program, roundArg(home-2), roundArg(home-1)))
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Contains(t, rr.Calls[0].Error, "no such round in the history")
	require.Empty(t, rr.Calls[1].Error)
	require.Len(t, rr.Calls[1].Logs, 1)
}

// TestQuerySeekGenesis: with time travel a call seeks to round 0, the genesis state, whatever the
// tracker has flushed; the history holds every round from 0. Without it, round 0 is like any round
// the tracker has flushed: the seek fails the call.
func TestQuerySeekGenesis(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	genesis, _, _, err := f.env.Ledger.LookupAccount(0, f.other.Addr)
	require.NoError(t, err)
	home := f.env.TxnInfo.LatestRound()
	now, _, _, err := f.env.Ledger.LookupAccount(home, f.other.Addr)
	require.NoError(t, err)
	require.NotEqual(t, genesis.MicroAlgos, now.MicroAlgos)

	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; callsub seek
global Round; itob; log
addr %s; balance; itob; log
int 1; return
%s`, f.other.Addr, seekSub)
	result, err := s.Query(f.request(t, program, roundArg(0)))
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Empty(t, rr.Calls[0].Error)
	require.Equal(t, []string{string(binary.BigEndian.AppendUint64(nil, 1)), string(binary.BigEndian.AppendUint64(nil, genesis.MicroAlgos.Raw))}, rr.Calls[0].Logs)
}

// TestQuerySeekProtocolUpgrade: a call seeks to a round under the protocol of its home round, also
// when the chain upgraded in between and the home round needs no override. The chain runs v41, whose
// AVM is v12, up to r+1, then v42: at r, the call creates an app of AVM v13.
func TestQuerySeekProtocolUpgrade(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV41, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender := env.Accounts[0]
	transfers := func() {
		for range 3 {
			env.TransferAlgos(sender.Addr, env.Accounts[1].Addr, 1000)
		}
	}
	transfers()
	r := env.TxnInfo.LatestRound() - 1
	// The upgrade to v42. A vote takes thousands of rounds, so the next block just switches: blocks
	// are added unvalidated.
	hdr := env.TxnInfo.LatestHeader
	blk := bookkeeping.MakeBlock(hdr)
	blk.UpgradeVote = bookkeeping.UpgradeVote{}
	blk.UpgradeState = bookkeeping.UpgradeState{CurrentProtocol: protocol.ConsensusV42}
	blk.TxnCounter = hdr.TxnCounter
	require.NoError(t, env.Ledger.AddBlock(blk, agreement.Certificate{}))
	env.TxnInfo.LatestHeader = blk.BlockHeader
	transfers()
	home := env.TxnInfo.LatestRound()
	next, err := env.Ledger.BlockHdr(r + 1)
	require.NoError(t, err)
	require.Equal(t, protocol.ConsensusV41, next.CurrentProtocol)

	newer := assemble(t, "#pragma version 13\nint 1")
	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; callsub seek
itxn_begin
int appl; itxn_field TypeEnum
byte 0x%[1]x; itxn_field ApprovalProgram
byte 0x%[1]x; itxn_field ClearStateProgram
int 0; itxn_field Fee
itxn_submit
global Round; itob; log
int 1; return
%[2]s`, newer, seekSub)
	result, err := simulation.MakeSimulator(env.Ledger, false).Query(simulation.QueryRequest{
		Program: assemble(t, program),
		Sender:  sender.Addr,
		Calls:   []simulation.QueryCall{roundArg(r), roundArg(home)},
		Limits:  simulation.QueryLimitsFromConfig(env.Config),
	})
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Equal(t, home, rr.Round)
	require.Empty(t, rr.Error)
	require.Nil(t, rr.Protocol, "home runs v42 without an override")
	for i, want := range []basics.Round{r + 1, home + 1} {
		require.Empty(t, rr.Calls[i].Error, "call %d", i)
		require.Equal(t, []string{u64(uint64(want))}, rr.Calls[i].Logs, "call %d", i)
	}
}

// TestQuerySeekSearch searches the time-travel history by bisection, as a date-to-round search
// does: it finds the first round where an account's balance reached a target, seeking to rounds
// whose blocks the node no longer has.
func TestQuerySeekSearch(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer f.env.Close()
	sender := f.env.Accounts[0]
	first := f.env.TxnInfo.LatestRound() + 1
	type point struct {
		round   basics.Round
		balance uint64
	}
	var points []point
	for i := range 1200 {
		f.env.TransferAlgos(sender.Addr, f.other.Addr, uint64(1000+i))
		r := f.env.TxnInfo.LatestRound()
		acct, _, _, err := f.env.Ledger.LookupAccount(r, f.other.Addr)
		require.NoError(t, err)
		points = append(points, point{r, acct.MicroAlgos.Raw})
	}
	_, err := f.env.Ledger.Block(first)
	require.Error(t, err, "block %d should be pruned", first)

	// arg0 is the target balance, arg1 the lowest round to search
	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; store 2
txna ApplicationArgs 1; btoi; store 0
global Round; int 1; -; store 1
loop:
load 0; load 1; >=; bnz done
load 0; load 1; +; int 2; /; store 3
load 3; callsub seek
addr %s; balance; load 2; >=; bz higher
load 3; store 1; b loop
higher:
load 3; int 1; +; store 0; b loop
done:
load 1; itob; log
int 1; return
%s`, f.other.Addr, seekSub)
	targets := []int{1, 17, 600, 1199}
	var calls []simulation.QueryCall
	for _, i := range targets {
		calls = append(calls, simulation.QueryCall{Args: [][]byte{[]byte(u64(points[i].balance)), []byte(u64(uint64(first)))}})
	}
	s := simulation.MakeSimulator(f.env.Ledger, false)
	result, err := s.Query(f.request(t, program, calls...))
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	for k, i := range targets {
		c := rr.Calls[k]
		require.Empty(t, c.Error, "target %d", i)
		require.Equal(t, u64(uint64(points[i].round)), c.Logs[0], "target %d", i)
	}
}

// logInnerLogs logs the logs of the last inner txn.
const logInnerLogs = `
int 0; store 5
inner_logs:
load 5; itxn NumLogs; ==; bnz inner_logs_done
load 5; itxnas Logs; log
load 5; int 1; +; store 5
b inner_logs
inner_logs_done:
`

// TestQuerySeekInner: an app a query creates and calls may seek too (an app on the chain can't: the
// chain has no time-travel opcodes). Its seeks don't move its caller; it starts at the round its
// caller reads, and a seek back to that round returns to its own state; a seek of it to the query
// round gets the round as the node has it, without the query's writes there; and a round's state a
// later call of the worker reuses holds none of an earlier call's writes.
func TestQuerySeekInner(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	sender := f.env.Accounts[0]
	counter := createSeekCounter(f)
	// arg0 is the round it seeks to, arg1 an app it calls there (0: none); then it seeks back to
	// the round it started at. It logs its round after each seek, the app's log, and its "w".
	seeker := assemble(t, `#pragma version 13
txn ApplicationID; bz create
global Round; int 1; -; store 1
byte "w"; int 7; app_global_put
txna ApplicationArgs 0; btoi; callsub seek
global Round; itob; log
txna ApplicationArgs 1; btoi; dup; store 2; bz back
itxn_begin
int appl; itxn_field TypeEnum
load 2; itxn_field ApplicationID
itxn_submit
itxn Logs 0; log
back:
load 1; callsub seek
global Round; itob; log
byte "w"; app_global_get; itob; log
int 1; return
create:
int 1; return
`+seekSub)
	clear := assemble(t, "#pragma version 13\nint 1")
	past := f.env.TxnInfo.LatestRound()
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 1_000_000)
	mid := f.env.TxnInfo.LatestRound()
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 1_000_000)
	home := f.env.TxnInfo.LatestRound()

	// counts at home, seeks to arg0 (0: stays), creates the seeker there and calls it with arg1 and
	// arg2, and logs its logs and the round it reads after it
	program := fmt.Sprintf(`#pragma version 13
itxn_begin
int appl; itxn_field TypeEnum
int %[1]d; itxn_field ApplicationID
itxn_submit
itxn Logs 0; log
txna ApplicationArgs 0; btoi; dup; bz stay
callsub seek
b called
stay:
pop
called:
itxn_begin
int appl; itxn_field TypeEnum
byte 0x%[2]x; itxn_field ApprovalProgram
byte 0x%[5]x; itxn_field ClearStateProgram
int 1; itxn_field GlobalNumUint
itxn_submit
itxn_begin
int appl; itxn_field TypeEnum
itxn CreatedApplicationID; itxn_field ApplicationID
txna ApplicationArgs 1; itxn_field ApplicationArgs
txna ApplicationArgs 2; itxn_field ApplicationArgs
itxn_submit
%[3]s
global Round; itob; log
int 1; return
%[4]s`, counter, seeker, logInnerLogs, seekSub, clear)

	r := func(rnd basics.Round) string { return u64(uint64(rnd)) }
	call := func(callerRound, seekerRound basics.Round, app basics.AppIndex) simulation.QueryCall {
		return args(r(callerRound), r(seekerRound), u64(uint64(app)))
	}
	req := f.request(t, program,
		call(0, past, 0),
		call(past, mid, 0),
		call(past, home, counter),
		call(past, home, counter))
	req.GlobalSchema = basics.StateSchema{NumUint: 1}
	req.Limits.Parallelism = 1
	s := simulation.MakeSimulator(f.env.Ledger, false)
	result, err := s.Query(req)
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	for i, c := range rr.Calls {
		require.Empty(t, c.Error, "call %d", i)
	}
	calls := rr.Calls

	// the seeker's seek doesn't move the query's app, which reads home after it
	require.Equal(t, []string{u64(1), r(past + 1), r(home + 1), u64(7), r(home + 1)}, calls[0].Logs)
	// the seeker starts at past, where the query's app sought to, and goes back there
	require.Equal(t, []string{u64(1), r(mid + 1), r(past + 1), u64(7), r(past + 1)}, calls[1].Logs)
	// the seeker's seek to home gets home as the node has it: a count of 1, not the query's 2
	require.Equal(t, []string{u64(1), r(home + 1), u64(1), r(past + 1), u64(7), r(past + 1)}, calls[2].Logs)
	// and so it does again on the worker's kept state of home
	require.Equal(t, calls[2].Logs, calls[3].Logs)
}

// TestQuerySeekBoxes: the box bytes a call writes on every round it seeks to count toward its one
// write budget, even a box it wrote before the seek; and an app id a call creates at one round
// doesn't hide the boxes of the app with that id at another.
func TestQuerySeekBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	sender := f.env.Accounts[0]
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// the write budget of a call of a deployed app, over the largest box
	proto := config.Consensus[protocol.ConsensusCurrentVersion]
	budget := uint64(proto.MaxTxGroupSize*proto.MaxAppTotalTxnReferences) * proto.BytesPerBoxReference
	fits := budget / proto.MaxBoxSize
	require.Greater(t, fits, uint64(1))

	// creates the largest box
	boxer := f.env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: fmt.Sprintf(`#pragma version 10
txn ApplicationID; bz create
byte "b"; int %d; box_create; assert
create:
int 1; return`, proto.MaxBoxSize),
		ClearStateProgram: "#pragma version 10\nint 1",
	})
	for range fits {
		f.env.TransferAlgos(sender.Addr, f.other.Addr, 1000)
	}
	// calls the boxer at the query round, then again after each of arg0 seeks, to the rounds
	// before it in turn
	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; store 0
global Round; int 1; -; store 1
callsub box
loop:
load 0; bz done
load 1; load 0; -; callsub seek
callsub box
load 0; int 1; -; store 0
b loop
done:
int 1; return
box:
itxn_begin
int appl; itxn_field TypeEnum
int %d; itxn_field ApplicationID
itxn_submit
retsub
%s`, boxer, seekSub)
	result, err := s.Query(f.request(t, program, args(u64(fits-1)), args(u64(fits))))
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Empty(t, rr.Calls[0].Error)
	require.Contains(t, rr.Calls[1].Error, "write budget exceeded")

	// An app a call creates at a round gets the id of the second txn of the next block, as its call
	// would be the first. The target app is created in the block after one of a single txn, so a
	// call that seeks to the round before that gets the target's id. Back at the query round, the
	// target still reads its box.
	pre := f.env.TxnInfo.LatestRound()
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 1000)
	target := f.env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   queryReaderApp,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})
	f.env.TransferAlgos(sender.Addr, target.Address(), 200_000)
	f.env.Txn(f.env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: target,
		ApplicationArgs: [][]byte{[]byte("setup")}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}}).Txn().Sign(sender.Sk))
	home := f.env.TxnInfo.LatestRound()
	program = fmt.Sprintf(`#pragma version 13
int %[1]d; callsub seek
itxn_begin
int appl; itxn_field TypeEnum
byte 0x0a8101; itxn_field ApprovalProgram
byte 0x0a8101; itxn_field ClearStateProgram
itxn_submit
itxn CreatedApplicationID; itob; log
int %[2]d; callsub seek
itxn_begin
int appl; itxn_field TypeEnum
int %[3]d; itxn_field ApplicationID
byte "read"; itxn_field ApplicationArgs
itxn_submit
itxn Logs 0; log
int 1; return
%[4]s`, pre, home, target, seekSub)
	result, err = s.Query(f.request(t, program, args()))
	require.NoError(t, err)
	rr = result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Empty(t, rr.Calls[0].Error)
	require.Equal(t, []string{u64(uint64(target)), "boxval"}, rr.Calls[0].Logs)
}

// TestQuerySeekRetry: a seek to a round whose state the node failed to build fails its call only:
// the next call of the worker that seeks there builds it again.
func TestQuerySeekRetry(t *testing.T) { // nolint:paralleltest // sets the lookup fault of every query
	partitiontest.PartitionTest(t)

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	home := f.env.TxnInfo.LatestRound()
	failed := false
	defer simulation.SetLookupFault(func(method string, rnd basics.Round) error {
		if method == "seek" && rnd == home-2 && !failed {
			failed = true
			return fmt.Errorf("%s failed once at round %d", method, rnd)
		}
		return nil
	})()

	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; btoi; callsub seek
addr %s; balance; itob; log
int 1; return
%s`, f.other.Addr, seekSub)
	req := f.request(t, program, roundArg(home-2), roundArg(home-2))
	req.Limits.Parallelism = 1
	result, err := s.Query(req)
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Contains(t, rr.Calls[0].Error, fmt.Sprintf("seeking to round %d", home-2))
	require.Contains(t, rr.Calls[0].Error, "seek failed once")
	require.Empty(t, rr.Calls[1].Error)
	require.Len(t, rr.Calls[1].Logs, 1)
}

// TestSimulateNoTimeTravel: outside queries there are no time-travel opcodes: simulate refuses a
// program with one, as a block does, when it checks the app's programs.
func TestSimulateNoTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	txn := f.env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            f.env.FeeSinkAccount.Addr,
		Fee:               1_000_000,
		ApprovalProgram:   "#pragma version 13\nint 1;" + "callsub seek\nint 1; return\n" + seekSub,
		ClearStateProgram: "#pragma version 13\nint 1",
	})
	result, err := s.Simulate(simulation.Request{
		TxnGroups:             [][]transactions.SignedTxn{{txn.SignedTxn()}},
		AllowEmptySignatures:  true,
		AllowUnnamedResources: true,
	})
	require.NoError(t, err)
	require.Contains(t, result.TxnGroups[0].FailureMessage, "tt_seek is only available in POST /v2/query")
}
