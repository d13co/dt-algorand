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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// queryReaderApp is a deployed app that a query calls: on create it sets global v, on opt-in local
// l, on "setup" it writes box b, and on "read" it logs b and v.
const queryReaderApp = `#pragma version 10
txn ApplicationID; bz create
txn OnCompletion; int OptIn; ==; bnz optin
txna ApplicationArgs 0; byte "read"; ==; bnz read
byte "b"; byte "boxval"; box_put; int 1; return
read:
byte "b"; box_get; assert; log
byte "v"; app_global_get; itob; log
int 1; return
optin:
txn Sender; byte "l"; int 7; app_local_put; int 1; return
create:
byte "v"; int 42; app_global_put; int 1`

// queryFixture is a ledger with a reader app, an asset and an account that holds it and is opted
// in to the app.
type queryFixture struct {
	env    simulationtesting.Environment
	other  simulationtesting.Account
	reader basics.AppIndex
	asset  basics.AssetIndex
}

func makeQueryFixture(t *testing.T, configure ...func(*config.Local)) *queryFixture {
	env := simulationtesting.PrepareSimulatorTest(t, configure...)
	sender, other := env.Accounts[0], env.Accounts[1]
	reader := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   queryReaderApp,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
		LocalStateSchema:  basics.StateSchema{NumUint: 1},
	})
	env.TransferAlgos(sender.Addr, reader.Address(), 10_000_000)
	env.Txn(env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: reader,
		ApplicationArgs: [][]byte{[]byte("setup")}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}}).Txn().Sign(sender.Sk))
	env.OptIntoApp(other.Addr, reader)
	asset := env.CreateAsset(sender.Addr, basics.AssetParams{Total: 1000})
	env.OptIntoAsset(other.Addr, asset)
	env.TransferAsset(sender.Addr, other.Addr, asset, 17)
	return &queryFixture{env: env, other: other, reader: reader, asset: asset}
}

// ghost is a ghost-like program: it reads an account, an asset holding, app global and local state
// and the round's header, calls the reader app for its box, and logs everything.
func (f *queryFixture) ghost() string {
	return fmt.Sprintf(`#pragma version 10
addr %[1]s; balance; itob; log
addr %[1]s; int %[2]d; asset_holding_get AssetBalance; assert; itob; log
int %[3]d; byte "v"; app_global_get_ex; assert; itob; log
addr %[1]s; int %[3]d; byte "l"; app_local_get_ex; assert; itob; log
itxn_begin
int appl; itxn_field TypeEnum
int %[3]d; itxn_field ApplicationID
byte "read"; itxn_field ApplicationArgs
itxn_submit
itxn Logs 0; log
itxn Logs 1; log
global Round; itob; log
global LatestTimestamp; itob; log
global Round; int 1; -; block BlkTimestamp; itob; log
global CurrentApplicationID; itob; log
txna ApplicationArgs 0; log
int 1`, f.other.Addr, f.asset, f.reader)
}

func (f *queryFixture) request(t *testing.T, program string, calls ...simulation.QueryCall) simulation.QueryRequest {
	return simulation.QueryRequest{
		Program: assemble(t, program),
		Calls:   calls,
		Limits:  simulation.QueryLimitsFromConfig(f.env.Config),
	}
}

func args(a ...string) simulation.QueryCall {
	var call simulation.QueryCall
	for _, s := range a {
		call.Args = append(call.Args, []byte(s))
	}
	return call
}

func u64(v uint64) string {
	return string(binary.BigEndian.AppendUint64(nil, v))
}

// TestQueryMatchesSimulate runs a ghost-like program through Simulate, as clients do (an app
// create from the fee sink, unsigned, with unnamed resources and extra budget), and through Query:
// the logs are the same.
func TestQueryMatchesSimulate(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	txn := f.env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            f.env.FeeSinkAccount.Addr,
		Fee:               1_000_000,
		ApprovalProgram:   f.ghost(),
		ClearStateProgram: "#pragma version 10\nint 1",
		ApplicationArgs:   [][]byte{[]byte("arg")},
	})
	// In the next block, as query runs: the window of rounds block may read follows from it.
	txn.FirstValid = f.env.TxnInfo.LatestRound() + 1
	txn.LastValid = txn.FirstValid
	simResult, err := s.Simulate(simulation.Request{
		TxnGroups:             [][]transactions.SignedTxn{{txn.SignedTxn()}},
		AllowEmptySignatures:  true,
		AllowUnnamedResources: true,
		ExtraOpcodeBudget:     100_000,
	})
	require.NoError(t, err)
	require.Empty(t, simResult.TxnGroups[0].FailureMessage)
	simLogs := simResult.TxnGroups[0].Txns[0].Txn.ApplyData.EvalDelta.Logs
	require.Len(t, simLogs, 11)

	result, err := s.Query(f.request(t, f.ghost(), args("arg")))
	require.NoError(t, err)
	require.Equal(t, f.env.TxnInfo.LatestRound(), result.LastRound)
	require.Len(t, result.Rounds, 1)
	rr := result.Rounds[0]
	require.Equal(t, f.env.TxnInfo.LatestRound(), rr.Round)
	require.Empty(t, rr.Error)
	require.Nil(t, rr.Protocol, "the latest protocol is the round's: no override")
	require.Len(t, rr.Calls, 1)
	require.Empty(t, rr.Calls[0].Error)
	require.Equal(t, simLogs, rr.Calls[0].Logs)
	// The values the logs hold, beyond agreeing with simulate.
	require.Equal(t, u64(17), rr.Calls[0].Logs[1])
	require.Equal(t, "boxval", rr.Calls[0].Logs[4])
	require.Equal(t, u64(uint64(rr.Round)+1), rr.Calls[0].Logs[6])
	require.Equal(t, u64(f.env.TxnInfo.LatestHeader.TxnCounter+1), rr.Calls[0].Logs[9])
	require.Equal(t, simResult.TxnGroups[0].AppBudgetConsumed, rr.Calls[0].Cost)
}

// TestQueryCallsIsolated: every call sees the state the round left, a failed call keeps the logs it
// made, and does not affect the others.
func TestQueryCallsIsolated(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// Logs global k and whether box bx exists (0 on the fresh app), then sets both: a later call
	// would see them if writes leaked. A "fail" call fails after logging.
	program := fmt.Sprintf(`#pragma version 10
byte "k"; app_global_get; itob; log
byte "bx"; box_len; swap; pop; itob; log
addr %s; balance; itob; log
byte "k"; int 1; app_global_put
byte "bx"; int 8; box_create; assert
txna ApplicationArgs 0; byte "fail"; !=; assert
int 1`, f.other.Addr)
	req := f.request(t, program, args("a"), args("fail"), args("b"))
	req.GlobalSchema = basics.StateSchema{NumUint: 1}
	result, err := s.Query(req)
	require.NoError(t, err)
	calls := result.Rounds[0].Calls
	require.Len(t, calls, 3)
	other, _, _, err := f.env.Ledger.LookupAccount(f.env.TxnInfo.LatestRound(), f.other.Addr)
	require.NoError(t, err)
	for i, c := range calls {
		require.Equal(t, []string{u64(0), u64(0), u64(other.MicroAlgos.Raw)}, c.Logs, "call %d", i)
		require.NotZero(t, c.Cost, "call %d", i)
	}
	require.Empty(t, calls[0].Error)
	require.Contains(t, calls[1].Error, "assert failed")
	require.Empty(t, calls[2].Error)
}

// TestQueryAppID calls a deployed app instead of creating one.
func TestQueryAppID(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	result, err := s.Query(simulation.QueryRequest{
		AppID:  f.reader,
		Calls:  []simulation.QueryCall{args("read"), args("nope")},
		Limits: simulation.QueryLimitsFromConfig(f.env.Config),
	})
	require.NoError(t, err)
	calls := result.Rounds[0].Calls
	require.Empty(t, calls[0].Error)
	require.Equal(t, []string{"boxval", u64(42)}, calls[0].Logs)
	// Not "read": the setup path writes the box, which a query may do; the write is dropped.
	require.Empty(t, calls[1].Error)
	require.Empty(t, calls[1].Logs)
}

// TestQueryForeignBoxReads: a query reads the boxes of an app that allowed no foreign reads, with
// app_box_get, app_box_extract and app_box_len, also from an app it calls, but may not write them.
// Simulate refuses the same reads.
func TestQueryForeignBoxReads(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// relay logs box b of the app its argument names.
	relay := f.env.CreateApp(f.env.Accounts[1].Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 13
txn ApplicationID; bz ok
txna ApplicationArgs 0; btoi; byte "b"; app_box_get; assert; log
ok: int 1`,
		ClearStateProgram: "#pragma version 13\nint 1",
	})
	program := fmt.Sprintf(`#pragma version 13
int %[1]d; byte "b"; app_box_get; assert; log
int %[1]d; byte "b"; int 0; int 3; app_box_extract; log
int %[1]d; byte "b"; app_box_len; assert; itob; log
int %[1]d; byte "nope"; app_box_get; itob; log; log
itxn_begin
int appl; itxn_field TypeEnum
int %[2]d; itxn_field ApplicationID
int %[1]d; itob; itxn_field ApplicationArgs
itxn_submit
itxn Logs 0; log
txna ApplicationArgs 0; byte "write"; ==; bz done
int %[1]d; byte "b"; byte "xxxxxx"; app_box_put
done: int 1`, f.reader, relay)

	result, err := s.Query(f.request(t, program, args("read"), args("write")))
	require.NoError(t, err)
	calls := result.Rounds[0].Calls
	require.Empty(t, calls[0].Error)
	require.Equal(t, []string{"boxval", "box", u64(6), u64(0), "", "boxval"}, calls[0].Logs)
	require.Contains(t, calls[1].Error, fmt.Sprintf("may not write box of %d", f.reader))

	txn := f.env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            f.env.FeeSinkAccount.Addr,
		Fee:               1_000_000,
		ApprovalProgram:   program,
		ClearStateProgram: "#pragma version 13\nint 1",
		ApplicationArgs:   [][]byte{[]byte("read")},
	})
	simResult, err := s.Simulate(simulation.Request{
		TxnGroups:             [][]transactions.SignedTxn{{txn.SignedTxn()}},
		AllowEmptySignatures:  true,
		AllowUnnamedResources: true,
	})
	require.NoError(t, err)
	require.Contains(t, simResult.TxnGroups[0].FailureMessage, fmt.Sprintf("may not read box of %d", f.reader))
}

// TestQueryBudget: the budget bounds each call, and cost reports what it used.
func TestQueryBudget(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// About 4 budget per iteration, so 5,000 iterations need about 20,000.
	program := `#pragma version 10
int 5000
loop: int 1; -; dup; bnz loop
pop; byte "done"; log; int 1`
	req := f.request(t, program, args())
	result, err := s.Query(req)
	require.NoError(t, err)
	call := result.Rounds[0].Calls[0]
	require.Empty(t, call.Error)
	require.Greater(t, call.Cost, 15_000)

	req.Budget = 1000
	result, err = s.Query(req)
	require.NoError(t, err)
	call = result.Rounds[0].Calls[0]
	require.Contains(t, call.Error, "dynamic cost budget exceeded")
	require.LessOrEqual(t, call.Cost, 1000+10)
}

// TestQueryValidate: requests that are invalid as a whole fail before anything runs.
func TestQueryValidate(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	limits := simulation.QueryLimitsFromConfig(f.env.Config)
	program := assemble(t, "#pragma version 10\nint 1")

	many := make([]simulation.QueryCall, limits.MaxCalls+1)
	tooManyRounds := make([]basics.Round, limits.MaxRounds+1)
	for _, tc := range []struct {
		name string
		req  simulation.QueryRequest
		want string
	}{
		{"neither", simulation.QueryRequest{Calls: many[:1]}, "exactly one of program and app-id"},
		{"both", simulation.QueryRequest{Program: program, AppID: f.reader, Calls: many[:1]}, "exactly one of program and app-id"},
		{"schema with app-id", simulation.QueryRequest{AppID: f.reader, GlobalSchema: basics.StateSchema{NumUint: 1}, Calls: many[:1]}, "need program"},
		{"no calls", simulation.QueryRequest{Program: program}, "at least 1 call"},
		{"too many calls", simulation.QueryRequest{Program: program, Calls: many}, "too many calls"},
		{"too many rounds", simulation.QueryRequest{Program: program, Calls: many[:1], Rounds: tooManyRounds}, "too many rounds"},
		{"budget over", simulation.QueryRequest{Program: program, Calls: many[:1], Budget: limits.MaxCallBudget + 1}, "over the limit"},
		{"budget negative", simulation.QueryRequest{Program: program, Calls: many[:1], Budget: -1}, "negative"},
		{"old program", simulation.QueryRequest{Program: assemble(t, "#pragma version 2\nint 1"), Calls: many[:1]}, "send a clear-program"},
		{"too long", simulation.QueryRequest{Program: append([]byte{10}, make([]byte, 17000)...), Calls: many[:1]}, "more than the"},
		{"bad protocol", simulation.QueryRequest{Program: program, Calls: many[:1], Protocol: "v1000"}, "unknown protocol"},
		{"future round", simulation.QueryRequest{Program: program, Calls: many[:1], Rounds: []basics.Round{f.env.TxnInfo.LatestRound() + 1}}, "after the latest round"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := tc.req
			req.Limits = limits
			_, err := s.Query(req)
			var invalid simulation.InvalidRequestError
			require.ErrorAs(t, err, &invalid)
			require.ErrorContains(t, err, tc.want)
		})
	}

	disabled := simulation.QueryRequest{Program: program, Calls: many[:1]}
	_, err := s.Query(disabled)
	require.ErrorContains(t, err, "disabled")

	// A pre-v3 program runs with its own clear program.
	old := simulation.QueryRequest{Program: assemble(t, "#pragma version 2\nint 1"), ClearProgram: assemble(t, "#pragma version 2\nint 1"), Calls: many[:1], Limits: limits}
	result, err := s.Query(old)
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Calls[0].Error)

	// A program over the 4 pages apps get without extra fees runs: queries charge no fees, so the
	// absolute page limit applies.
	chunk := "pushbytes 0x" + strings.Repeat("ab", 3000) + "\npop\n"
	large := assemble(t, "#pragma version 10\n"+strings.Repeat(chunk, 3)+"int 1")
	require.Greater(t, len(large), 8192)
	result, err = s.Query(simulation.QueryRequest{Program: large, Calls: many[:1], Limits: limits})
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Calls[0].Error)
}

// TestQueryLogLimits: a call's logs are capped per program, and the request's in total.
func TestQueryLogLimits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// Logs arg 0 logs of 100 bytes: more than the protocol's 32 logs and 1024 bytes.
	program := `#pragma version 10
txna ApplicationArgs 0; btoi
loop: int 100; bzero; log; int 1; -; dup; bnz loop
pop; int 1`
	call := func(n uint64) simulation.QueryCall {
		return simulation.QueryCall{Args: [][]byte{binary.BigEndian.AppendUint64(nil, n*100)}}
	}
	req := f.request(t, program, call(2), call(7))
	req.Limits.MaxCallLogBytes = 64 * 1024
	result, err := s.Query(req)
	require.NoError(t, err)
	calls := result.Rounds[0].Calls
	require.Empty(t, calls[0].Error)
	require.Len(t, calls[0].Logs, 200)
	require.Contains(t, calls[1].Error, "program logs too large")
	require.Len(t, calls[1].Logs, 655, "the logs up to the limit are kept")

	// The logs of a call are capped at 2048, as with simulate's allow-more-logging.
	empty := f.request(t, `#pragma version 10
int 3000
loop: byte ""; log; int 1; -; dup; bnz loop
pop; int 1`, args())
	empty.Limits.MaxCallLogBytes = 64 * 1024
	result, err = s.Query(empty)
	require.NoError(t, err)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "too many log calls")
	require.Len(t, result.Rounds[0].Calls[0].Logs, 2048)

	// A call of 100 logs of 100 bytes counts 100 × (100 + 48) bytes: 14,800.
	const callBytes = 14800
	req.Calls = []simulation.QueryCall{call(1), call(1), call(1)}
	req.Limits.MaxResponseBytes = 3*callBytes - 1
	_, err = s.Query(req)
	require.ErrorIs(t, err, simulation.ErrQueryResponseTooLarge)
	req.Limits.MaxResponseBytes = 3 * callBytes
	_, err = s.Query(req)
	require.NoError(t, err)
	// A round listed twice is in the response twice.
	req.Rounds = []basics.Round{0, 0}
	_, err = s.Query(req)
	require.ErrorIs(t, err, simulation.ErrQueryResponseTooLarge)
	req.Limits.MaxResponseBytes = 6 * callBytes
	_, err = s.Query(req)
	require.NoError(t, err)
}

// TestQueryWriteBudget: the box bytes a call writes are bounded, as simulate bounds them, while
// the boxes it reads are not.
func TestQueryWriteBudget(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// Creates boxes of 32 KiB until it fails, logging the count before each.
	req := f.request(t, `#pragma version 10
int 0
loop: dup; itob; log; dup; itob; int 32768; box_create; assert; int 1; +; b loop`, args())
	result, err := s.Query(req)
	require.NoError(t, err)
	call := result.Rounds[0].Calls[0]
	require.Contains(t, call.Error, "write budget")
	// A full group's 16 × 8 box references.
	proto := f.env.TxnInfo.CurrentProtocolParams()
	boxes := proto.MaxTxGroupSize * proto.MaxAppTotalTxnReferences * int(proto.BytesPerBoxReference) / 32768
	require.Len(t, call.Logs, boxes+1)

	// An app whose programs need extra pages is created, and may still write its boxes.
	big := strings.Repeat("int 1; pop\n", 2000) + "byte \"b\"; int 1024; box_create"
	result, err = s.Query(f.request(t, "#pragma version 10\n"+big, args()))
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Calls[0].Error)
}

// TestQueryCanceled: a canceled request stops, with its context's cause.
func TestQueryCanceled(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// An endless loop, stopped only by the request ending.
	cause := errors.New("client left")
	ctx, cancel := context.WithCancelCause(context.Background())
	req := f.request(t, "#pragma version 10\nloop: b loop", args(), args())
	req.Limits.MaxCallBudget = 1_000_000_000
	req.Budget = 1_000_000_000
	time.AfterFunc(50*time.Millisecond, func() { cancel(cause) })
	_, err := s.Query(req.WithContext(ctx))
	require.ErrorIs(t, err, cause)
}

// TestQueryPastRound: a query at a past round sees that round's state and header.
func TestQueryPastRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	past := f.env.TxnInfo.LatestRound()
	f.env.TransferAlgos(f.env.Accounts[0].Addr, f.other.Addr, 1_000_000)

	program := fmt.Sprintf(`#pragma version 10
addr %s; balance; itob; log
txna ApplicationArgs 0; btoi; block BlkTimestamp; itob; log
int 1`, f.other.Addr)
	at := func(r basics.Round) simulation.QueryCall {
		return simulation.QueryCall{Args: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))}}
	}
	req := f.request(t, program, at(past), at(past+1))
	req.Rounds = []basics.Round{past, 0, past}
	result, err := s.Query(req)
	require.NoError(t, err)
	require.Len(t, result.Rounds, 3)
	require.Equal(t, []basics.Round{past, past + 1, past}, []basics.Round{result.Rounds[0].Round, result.Rounds[1].Round, result.Rounds[2].Round})
	require.Equal(t, result.Rounds[0], result.Rounds[2])

	before, _, _, err := f.env.Ledger.LookupAccount(past, f.other.Addr)
	require.NoError(t, err)
	after, _, _, err := f.env.Ledger.LookupAccount(past+1, f.other.Addr)
	require.NoError(t, err)
	require.Equal(t, u64(before.MicroAlgos.Raw), result.Rounds[0].Calls[0].Logs[0])
	require.Empty(t, result.Rounds[0].Calls[0].Error)
	// block refuses rounds from the queried one's next on, before it reads the ledger.
	require.Contains(t, result.Rounds[0].Calls[1].Error, "not available")
	require.Equal(t, u64(after.MicroAlgos.Raw), result.Rounds[1].Calls[1].Logs[0])
	require.Empty(t, result.Rounds[1].Calls[1].Error)
}

// TestQueryParallel runs many calls at several rounds in several workers, which take slots: the
// results are in call order, and the extra workers return their slots.
func TestQueryParallel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	for range 2 {
		f.env.TransferAlgos(f.env.Accounts[0].Addr, f.other.Addr, 1000)
	}
	s := simulation.MakeSimulator(f.env.Ledger, false)
	latest := f.env.TxnInfo.LatestRound()

	calls := make([]simulation.QueryCall, 50)
	for i := range calls {
		calls[i] = args(fmt.Sprintf("call %d", i))
	}
	req := f.request(t, f.ghost(), calls...)
	req.Rounds = []basics.Round{latest, latest - 1, latest - 2}
	req.Limits.Parallelism = 8
	checkResult := func(t *testing.T, result simulation.QueryResult) {
		for j, rr := range result.Rounds {
			require.Empty(t, rr.Error)
			require.Len(t, rr.Calls, len(calls))
			for i, c := range rr.Calls {
				require.Empty(t, c.Error, "round %d call %d", j, i)
				require.Equal(t, fmt.Sprintf("call %d", i), c.Logs[len(c.Logs)-1])
			}
		}
	}

	t.Run("free", func(t *testing.T) {
		var held, maxHeld atomic.Int64
		result, err := s.Query(req.WithSlots(func() bool {
			n := held.Add(1)
			for {
				m := maxHeld.Load()
				if n <= m || maxHeld.CompareAndSwap(m, n) {
					break
				}
			}
			return true
		}, func() { held.Add(-1) }))
		require.NoError(t, err)
		require.Zero(t, held.Load())
		require.LessOrEqual(t, maxHeld.Load(), int64(7))
		require.Positive(t, maxHeld.Load(), "the extra workers take slots")
		checkResult(t, result)
	})

	t.Run("busy", func(t *testing.T) {
		// No slot is free: each extra worker tries once and stops, and the first does all the work.
		var tries atomic.Int64
		result, err := s.Query(req.WithSlots(func() bool {
			tries.Add(1)
			return false
		}, func() { t.Error("released a slot that was never taken") }))
		require.NoError(t, err)
		require.LessOrEqual(t, tries.Load(), int64(7))
		checkResult(t, result)
	})
}

// TestQueryProtocolOverride runs a program that needs AVM 11 at a v35 round under v42, and reports
// the notes simulate reports for it.
func TestQueryProtocolOverride(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	groups := protocolOverrideGroups(t, &env)
	s := simulation.MakeSimulator(env.Ledger, true)
	sender := env.Accounts[0]

	req := simulation.QueryRequest{
		Program: assemble(t, newerAVMApp),
		Sender:  sender.Addr,
		Calls:   []simulation.QueryCall{{}, {}},
		Limits:  simulation.QueryLimitsFromConfig(env.Config),
	}
	// The latest protocol is v35, as is the round's: the program is refused.
	result, err := s.Query(req)
	require.NoError(t, err)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "greater than protocol supported version 7")
	req.Protocol = simulation.ProtocolRound
	result, err = s.Query(req)
	require.NoError(t, err)
	require.Nil(t, result.Rounds[0].Protocol)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "greater than protocol supported version 7")

	req.Protocol = "v42"
	result, err = s.Query(req)
	require.NoError(t, err)
	rr := result.Rounds[0]
	for i, c := range rr.Calls {
		require.Empty(t, c.Error, "call %d", i)
		require.Len(t, c.Logs, 3)
	}

	simResult, err := s.Simulate(simulation.Request{TxnGroups: groups[:1], Protocol: "v42"})
	require.NoError(t, err)
	require.Empty(t, simResult.TxnGroups[0].FailureMessage)
	require.Equal(t, simResult.TxnGroups[0].Txns[0].Txn.ApplyData.EvalDelta.Logs, rr.Calls[0].Logs)

	po := rr.Protocol
	require.NotNil(t, po)
	require.True(t, po.Applied)
	require.Equal(t, protocol.ConsensusV35, po.From)
	require.Equal(t, protocol.ConsensusV42, po.To)
	require.Equal(t, noteCodes(simResult.ProtocolOverride), noteCodes(po))
	for _, note := range simResult.ProtocolOverride.Notes {
		if len(note.Hits) == 0 {
			continue
		}
		require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}}, note.Hits, note.Code)
		require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}, {Group: 1, Path: simulation.TxnPath{0}}},
			findNote(t, po, note.Code).Hits, note.Code)
	}
}

// TestQueryTimeTravel queries rounds whose blocks and tracker state are long gone.
func TestQueryTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer f.env.Close()
	sender := f.env.Accounts[0]
	balances := map[basics.Round]uint64{}
	first := f.env.TxnInfo.LatestRound() + 1
	for i := range 1200 {
		f.env.TransferAlgos(sender.Addr, f.other.Addr, uint64(1000+i))
		r := f.env.TxnInfo.LatestRound()
		acct, _, _, err := f.env.Ledger.LookupAccount(r, f.other.Addr)
		require.NoError(t, err)
		balances[r] = acct.MicroAlgos.Raw
	}
	_, err := f.env.Ledger.Block(first)
	require.Error(t, err, "block %d should be pruned", first)

	s := simulation.MakeSimulator(f.env.Ledger, false)
	rounds := []basics.Round{first, first + 1, first + 500, f.env.TxnInfo.LatestRound()}
	req := f.request(t, fmt.Sprintf("#pragma version 10\naddr %s; balance; itob; log; int 1", f.other.Addr), args())
	req.Rounds = rounds
	result, err := s.Query(req)
	require.NoError(t, err)
	for i, r := range rounds {
		rr := result.Rounds[i]
		require.Empty(t, rr.Error, "round %d", r)
		require.Empty(t, rr.Calls[0].Error, "round %d", r)
		require.Equal(t, []string{u64(balances[r])}, rr.Calls[0].Logs, "round %d", r)
	}
}

// TestQueryCallStateLimit: a call that reads more ledger state than MaxCallStateBytes fails and the
// others run on; whether a call is over depends only on what it reads, not on what the calls before
// it on the same worker left cached, so the answers are the same with one worker or several.
func TestQueryCallStateLimit(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)

	// reads the balances of the accounts numbered arg0 up to arg1, 256 bytes of state each
	program := `#pragma version 10
txna ApplicationArgs 0; btoi; store 0
txna ApplicationArgs 1; btoi; store 1
loop:
load 0; load 1; >=; bnz done
load 0; itob; int 24; bzero; concat; balance; pop
load 0; int 1; +; store 0
b loop
done:
int 1`
	span := func(lo, hi uint64) simulation.QueryCall {
		return simulation.QueryCall{Args: [][]byte{[]byte(u64(lo)), []byte(u64(hi))}}
	}
	// 80 accounts (under half the limit, so the cache is kept) and 150 fit in 50,000 bytes alone,
	// but not together; 250 never fit
	calls := []simulation.QueryCall{span(0, 80), span(80, 230), span(0, 250), span(0, 0), span(300, 450)}
	over := []bool{false, false, true, false, false}
	for _, workers := range []int{1, 3} {
		req := f.request(t, program, calls...)
		req.Limits.MaxCallStateBytes = 50_000
		req.Limits.Parallelism = workers
		result, err := s.Query(req)
		require.NoError(t, err)
		rr := result.Rounds[0]
		require.Empty(t, rr.Error)
		for i, c := range rr.Calls {
			if over[i] {
				require.Contains(t, c.Error, "more than 50000 bytes of ledger state (MaxAPIQueryCallStateBytes)", "workers %d call %d", workers, i)
			} else {
				require.Empty(t, c.Error, "workers %d call %d", workers, i)
				require.Positive(t, c.Cost)
			}
		}
	}

	// 0: no limit
	req := f.request(t, program, calls...)
	req.Limits.MaxCallStateBytes = 0
	result, err := s.Query(req)
	require.NoError(t, err)
	for i, c := range result.Rounds[0].Calls {
		require.Empty(t, c.Error, "call %d", i)
	}
}

// TestQueryLookupFailure: when the node fails to read the round while the calls run, the round
// reports the failure as its error, without calls, instead of passing it off as the calls' errors,
// and its calls' logs no longer count toward the response limit. The online account lookups, which
// read the balance round, fail only their call.
func TestQueryLookupFailure(t *testing.T) { // nolint:paralleltest // sets the lookup fault of every query
	partitiontest.PartitionTest(t)

	f := makeQueryFixture(t)
	defer f.env.Close()
	s := simulation.MakeSimulator(f.env.Ledger, false)
	latest := f.env.TxnInfo.LatestRound()

	fault := func(method string, rnd basics.Round) error { return nil }
	defer simulation.SetLookupFault(func(method string, rnd basics.Round) error { return fault(method, rnd) })()

	// every box read fails: the ghost program reads the reader app's box
	fault = func(method string, rnd basics.Round) error {
		if method == "LookupKv" {
			return errors.New("disk on fire")
		}
		return nil
	}
	req := f.request(t, f.ghost(), args("a"), args("b"))
	req.Rounds = []basics.Round{latest, latest - 1}
	result, err := s.Query(req)
	require.NoError(t, err)
	for i, rr := range result.Rounds {
		require.Equal(t, fmt.Sprintf("reading round %d failed: disk on fire", req.Rounds[i]), rr.Error)
		require.Empty(t, rr.Calls)
	}
	// a program that reads no box is not affected
	result, err = s.Query(f.request(t, "#pragma version 10\nint 1", args()))
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Error)
	require.Empty(t, result.Rounds[0].Calls[0].Error)

	// box reads fail at one round only. Each call logs 100 bytes (148 counted); "box" also reads
	// a box. The failed round's first call ran and was counted before its second failed the round;
	// the other round's two calls fit only once that is given back.
	fault = func(method string, rnd basics.Round) error {
		if method == "LookupKv" && rnd == latest-1 {
			return errors.New("disk on fire")
		}
		return nil
	}
	program := fmt.Sprintf(`#pragma version 13
txna ApplicationArgs 0; byte "box"; ==; bz skip
int %d; byte "b"; app_box_get; assert; pop
skip:
byte "%s"; log
int 1`, f.reader, strings.Repeat("x", 100))
	req = f.request(t, program, args("a"), args("box"))
	req.Rounds = []basics.Round{latest - 1, latest}
	req.Limits.Parallelism = 1
	req.Limits.MaxResponseBytes = 2 * 148
	result, err = s.Query(req)
	require.NoError(t, err)
	require.Equal(t, fmt.Sprintf("reading round %d failed: disk on fire", latest-1), result.Rounds[0].Error)
	require.Empty(t, result.Rounds[1].Error)
	require.Len(t, result.Rounds[1].Calls, 2)
	for _, c := range result.Rounds[1].Calls {
		require.Empty(t, c.Error)
	}

	// an online stake the history lacks fails the call that reads it, not the round
	fault = func(method string, rnd basics.Round) error {
		if method == "OnlineCirculation" {
			return errors.New("no online stake recorded")
		}
		return nil
	}
	req = f.request(t, "#pragma version 11\ntxna ApplicationArgs 0; byte \"stake\"; ==; bz done; online_stake; pop\ndone: int 1",
		args("stake"), args("none"))
	result, err = s.Query(req)
	require.NoError(t, err)
	rr := result.Rounds[0]
	require.Empty(t, rr.Error)
	require.Contains(t, rr.Calls[0].Error, "no online stake recorded")
	require.Empty(t, rr.Calls[1].Error)
}
