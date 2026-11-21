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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestStateChangesTraceBudgetReads re-evaluates a committed group (a logic sig
// payment, then an app call whose inner call reads app state) with
// include=budget,reads,trace: the traces must be simulate's for the same group,
// the budget simulate's costs, and the reads what the inner call read.
func TestStateChangesTraceBudgetReads(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender := env.Accounts[0]
	for _, acct := range env.Accounts {
		if acct.AcctData.MicroAlgos.Raw > sender.AcctData.MicroAlgos.Raw {
			sender = acct
		}
	}

	// OptIn sets local l1; arg "i" sets g1 and box bx; arg "L" reads locals;
	// no arg reads globals and boxes, writes w and reads it back
	reader := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 10
txn ApplicationID
bz done
txn OnCompletion
int OptIn
==
bz call
txn Sender; byte "l1"; byte "L1"; app_local_put
b done
call:
txn NumAppArgs
bz read
txn ApplicationArgs 0
byte "i"
==
bz local
byte "g1"; byte "G1"; app_global_put
byte "bx"; byte "BX"; box_put
b done
local:
txn Sender; byte "l1"; app_local_get; pop
txn Sender; byte "l2"; app_local_get; pop
txn Sender; byte "l1"; app_local_get; pop
b done
read:
byte "g1"; app_global_get; pop
byte "nope"; app_global_get; pop
byte "g1"; app_global_get; pop
byte "bx"; box_len; pop; pop
byte "nobox"; box_len; pop; pop
byte "w"; byte "v"; app_global_put
byte "w"; app_global_get; pop
done: int 1`,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 2},
		LocalStateSchema:  basics.StateSchema{NumByteSlice: 1},
	})
	caller := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 10
txn ApplicationID
bz done
itxn_begin
int appl; itxn_field TypeEnum
txn Applications 1; itxn_field ApplicationID
itxn_submit
done: int 1`,
		ClearStateProgram: "#pragma version 10\nint 1",
	})
	env.TransferAlgos(sender.Addr, reader.Address(), 1_000_000)
	env.TransferAlgos(sender.Addr, caller.Address(), 1_000_000)
	init := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: reader,
		ApplicationArgs: [][]byte{[]byte("i")}, Boxes: []transactions.BoxRef{{Name: []byte("bx")}}})
	env.Txn(init.SignedTxn())
	env.OptIntoApp(sender.Addr, reader)

	lsigProgram, err := logic.AssembleString("#pragma version 10\ntxn Amount\nint 1000\n<=")
	require.NoError(t, err)
	lsigAddr := basics.Address(logic.HashProgram(lsigProgram.Program))
	env.TransferAlgos(sender.Addr, lsigAddr, 1_000_000)

	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: lsigAddr, Receiver: sender.Addr, Amount: 1000})
	call := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: caller,
		ForeignApps: []basics.AppIndex{reader}, Fee: 3000,
		Boxes: []transactions.BoxRef{{Index: 1, Name: []byte("bx")}, {Index: 1, Name: []byte("nobox")}}})
	local := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: reader,
		ApplicationArgs: [][]byte{[]byte("L")}})
	group := txntest.Group(&pay, &call, &local)
	group[0].Lsig = transactions.LogicSig{Logic: lsigProgram.Program}
	other := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr, Note: []byte{1}})
	// a group after the traced one, whose events the tracer must not see
	later := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: caller,
		ForeignApps: []basics.AppIndex{reader}, Fee: 3000, Note: []byte{2},
		Boxes: []transactions.BoxRef{{Index: 1, Name: []byte("bx")}, {Index: 1, Name: []byte("nobox")}}})
	blk := env.Block([]transactions.SignedTxn{other.SignedTxn()}, group, []transactions.SignedTxn{later.SignedTxn()})
	r := blk.Round()

	// simulate the group at the round before: the block's first group pays a
	// fee that changes nothing the group reads
	sim, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
		Round: r - 1, TxnGroups: [][]transactions.SignedTxn{group}, AllowEmptySignatures: true,
		TraceConfig: simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true},
	})
	require.NoError(t, err)
	require.Empty(t, sim.TxnGroups[0].FailureMessage)

	var tracer *simulation.GroupExecTracer
	f := ledger.StateChangesFilter{
		Include: ledger.IncludeBudget | ledger.IncludeReads | ledger.IncludeTrace,
		TxIDs:   []transactions.Txid{group[1].ID()},
		Trace: func(g []transactions.SignedTxnWithAD) logic.EvalTracer {
			require.Len(t, g, 3)
			tracer = simulation.NewGroupExecTracer(r, g, simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true})
			return tracer
		},
	}
	ctx := context.Background()
	sc, err := env.Ledger.StateChangesForRound(ctx, r, f)
	require.NoError(t, err)

	// trace
	require.NotNil(t, sc.Trace)
	require.Equal(t, 1, sc.Trace.First)
	require.Equal(t, []transactions.Txid{group[0].ID(), group[1].ID(), group[2].ID()}, sc.Trace.TxIDs)
	traces := tracer.Traces()
	require.Len(t, traces, 3)
	for i := range traces {
		require.Equal(t, sim.TxnGroups[0].Txns[i].Trace, traces[i], "txn %d", i)
	}
	require.NotEmpty(t, traces[0].LogicSigTrace)
	require.Len(t, traces[1].InnerTraces, 1)
	require.NotEmpty(t, traces[1].InnerTraces[0].ApprovalProgramTrace)
	hashes := map[crypto.Digest]bool{}
	for h := range sc.Trace.Programs {
		hashes[h] = true
	}
	require.Equal(t, map[crypto.Digest]bool{traces[0].LogicSigHash: true, traces[1].ApprovalProgramHash: true,
		traces[1].InnerTraces[0].ApprovalProgramHash: true, traces[2].ApprovalProgramHash: true}, hashes)
	require.Len(t, hashes, 3, "the inner call and the last txn run the same program")
	require.Positive(t, tracer.Bytes())

	// budget: only the txid's programs, the logic sig of its group first
	require.Len(t, sc.Budget, 2)
	a, b := sc.Budget[0], sc.Budget[1]
	require.Equal(t, []uint64{2}, a.Path)
	require.Equal(t, group[1].ID(), a.TxID)
	require.Equal(t, ledger.ProgramApproval, a.Kind)
	require.Equal(t, caller, a.App)
	require.Equal(t, []uint64{2, 0}, b.Path)
	require.Zero(t, b.TxID)
	require.Equal(t, reader, b.App)
	require.Equal(t, sim.TxnGroups[0].Txns[1].AppBudgetConsumed, a.Cost+b.Cost)
	proto := config.Consensus[blk.CurrentProtocol]
	require.Equal(t, 2*proto.MaxAppProgramCost, a.BudgetBefore, "two app calls pool their budgets")
	// the caller spends all but its last op (int 1) before the inner call,
	// which adds its budget before it runs
	require.Equal(t, a.BudgetBefore-(a.Cost-1)+proto.MaxAppProgramCost, b.BudgetBefore)
	require.Equal(t, b.BudgetBefore-b.Cost, b.BudgetAfter)
	require.Equal(t, b.BudgetAfter-1, a.BudgetAfter)
	require.False(t, a.Failed || b.Failed)

	// the logic sig, with the whole group
	all, err := env.Ledger.StateChangesForRound(ctx, r, ledger.StateChangesFilter{Include: ledger.IncludeBudget})
	require.NoError(t, err)
	require.Len(t, all.Budget, 6)
	require.Equal(t, []uint64{4}, all.Budget[4].Path, "the later group")
	require.Equal(t, []uint64{4, 0}, all.Budget[5].Path)
	l := all.Budget[0]
	require.Equal(t, []uint64{1}, l.Path)
	require.Equal(t, group[0].ID(), l.TxID)
	require.Equal(t, ledger.ProgramLogicSig, l.Kind)
	require.Zero(t, l.App)
	require.Equal(t, sim.TxnGroups[0].Txns[0].LogicSigBudgetConsumed, l.Cost)
	require.Equal(t, l.BudgetBefore-l.Cost, l.BudgetAfter)
	require.Equal(t, sc.Budget, all.Budget[1:3])
	require.Equal(t, []uint64{3}, all.Budget[3].Path)
	require.Equal(t, sim.TxnGroups[0].Txns[2].AppBudgetConsumed, all.Budget[3].Cost)
	require.Equal(t, a.BudgetAfter, all.Budget[3].BudgetBefore, "the pool goes on to the next txn")

	// reads: the inner call's, each key once, as first read
	tv := func(s string) basics.TealValue { return basics.TealValue{Type: basics.TealBytesType, Bytes: s} }
	require.Equal(t, []ledger.TxnStateReads{{Path: []uint64{2, 0}, Reads: []ledger.StateRead{
		{App: reader, Type: logic.GlobalState, Key: "g1", Exists: true, Value: tv("G1")},
		{App: reader, Type: logic.GlobalState, Key: "nope"},
		{App: reader, Type: logic.BoxState, Key: "bx", Exists: true, Length: 2},
		{App: reader, Type: logic.BoxState, Key: "nobox"},
		{App: reader, Type: logic.GlobalState, Key: "w", Exists: true, Value: tv("v")},
	}}}, sc.Reads)
	allReads, err := env.Ledger.StateChangesForRound(ctx, r, ledger.StateChangesFilter{Include: ledger.IncludeReads})
	require.NoError(t, err)
	require.Len(t, allReads.Reads, 3)
	require.Equal(t, append(sc.Reads, ledger.TxnStateReads{Path: []uint64{3}, TxID: group[2].ID(), Reads: []ledger.StateRead{
		{App: reader, Type: logic.LocalState, Account: sender.Addr, Key: "l1", Exists: true, Value: tv("L1")},
		{App: reader, Type: logic.LocalState, Account: sender.Addr, Key: "l2"},
	}}), allReads.Reads[:2])
	require.Equal(t, []uint64{4, 0}, allReads.Reads[2].Path, "the later group")
	require.Empty(t, allReads.Budget)

	// trace needs exactly one txid
	for _, txids := range [][]transactions.Txid{nil, {group[0].ID(), group[2].ID()}} {
		f.TxIDs = txids
		_, err = env.Ledger.StateChangesForRound(ctx, r, f)
		require.ErrorIs(t, err, ledger.ErrTraceNeedsOneTxn)
	}
	// the logic sig's txid traces the same group
	f.TxIDs = []transactions.Txid{group[0].ID()}
	sc2, err := env.Ledger.StateChangesForRound(ctx, r, f)
	require.NoError(t, err)
	require.Equal(t, sc.Trace.TxIDs, sc2.Trace.TxIDs)
	for i, tr := range tracer.Traces() {
		require.Equal(t, sim.TxnGroups[0].Txns[i].Trace, tr, "txn %d", i)
	}
	require.Equal(t, []ledger.ProgramBudget{l}, sc2.Budget)
	require.Empty(t, sc2.Reads)

	// the trace's bytes count against the log's limit (four times MaxBytes)
	f.TxIDs = []transactions.Txid{group[1].ID()}
	f.MaxBytes = uint64(tracer.Bytes() / 8)
	_, err = env.Ledger.StateChangesForRound(ctx, r, f)
	require.ErrorIs(t, err, ledger.ErrStateChangesTooLarge)
	f.Include &^= ledger.IncludeTrace
	_, err = env.Ledger.StateChangesForRound(ctx, r, f)
	require.NoError(t, err, "without the trace, the rest fits")
}

// TestStateChangesTraceShortStack traces ClearState programs that fail before
// their op runs, where explaining the op would read past the stack or the call
// stack: at a state opcode and at stores on a short stack, at retsub and
// frame_bury without a frame, and at frame_bury above the stack (whose pop
// count must not go negative). The block keeps their failure, and tracing them
// must neither panic nor change how they fail.
func TestStateChangesTraceShortStack(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	creator := env.Accounts[0]

	// the number of app args picks the op; past a label the assembler no
	// longer knows the stack, and lets the ops assemble on an empty one
	app := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram: "#pragma version 10\nint 1",
		ClearStateProgram: `#pragma version 10
txn NumAppArgs
switch g s r f a
err
g:
app_global_put
s:
stores
r:
retsub
f:
int 1
frame_bury 0
a:
callsub sub
int 1
return
sub:
proto 0 0
b x
x:
int 1
frame_bury 5
retsub`,
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 1},
	})
	cases := []struct{ op, err string }{
		{"app_global_put", "stack underflow in app_global_put"},
		{"stores", "stack underflow in stores"},
		{"retsub", "retsub with empty callstack"},
		{"frame_bury", "frame_bury with empty callstack"},
		{"frame_bury above", "frame_bury above stack"},
	}
	var groups [][]transactions.SignedTxn
	for i := range cases {
		sender := env.Accounts[1+i].Addr
		env.OptIntoApp(sender, app)
		clear := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender, ApplicationID: app,
			OnCompletion: transactions.ClearStateOC, ApplicationArgs: make([][]byte, i)})
		groups = append(groups, []transactions.SignedTxn{clear.SignedTxn()})
	}
	blk := env.Block(groups...)
	r := blk.Round()

	cfg := simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true}
	ctx := context.Background()
	for i, c := range cases {
		// simulated alone at the round before: the other clears change nothing it reads
		sim, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
			Round: r - 1, TxnGroups: groups[i : i+1], AllowEmptySignatures: true, TraceConfig: cfg,
		})
		require.NoError(t, err, c.op)
		g := sim.TxnGroups[0]
		require.Empty(t, g.FailureMessage, c.op)
		units := g.Txns[0].Trace.ClearStateProgramTrace
		require.NotEmpty(t, units, c.op)
		require.Empty(t, units[len(units)-1].StateChanges, c.op)
		require.Empty(t, units[len(units)-1].ScratchSlotChanges, c.op)
		require.GreaterOrEqual(t, units[len(units)-1].StackPopCount, 0, c.op)
		require.Contains(t, g.Txns[0].Trace.ClearStateRollbackError, c.err)

		var tracer *simulation.GroupExecTracer
		sc, err := env.Ledger.StateChangesForRound(ctx, r, ledger.StateChangesFilter{
			Include: ledger.IncludeBudget | ledger.IncludeReads | ledger.IncludeTrace,
			TxIDs:   []transactions.Txid{groups[i][0].ID()},
			Trace: func(g []transactions.SignedTxnWithAD) logic.EvalTracer {
				tracer = simulation.NewGroupExecTracer(r, g, cfg)
				return tracer
			},
		})
		require.NoError(t, err, c.op)
		require.Equal(t, g.Txns[0].Trace, tracer.Traces()[0], c.op)
		require.Len(t, sc.Budget, 1, c.op)
		require.Equal(t, ledger.ProgramClearState, sc.Budget[0].Kind, c.op)
		require.True(t, sc.Budget[0].Failed, c.op)
	}
}

// TestGroupExecTracerKeepsConstants checks that a GroupExecTracer leaves the
// evaluation constants of a top-level group as the evaluation set them.
func TestGroupExecTracerKeepsConstants(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	group := []transactions.SignedTxnWithAD{{SignedTxn: txntest.Txn{Type: protocol.PaymentTx}.SignedTxn()}}
	tracer := simulation.NewGroupExecTracer(1, group, simulation.ExecTraceConfig{Enable: true})
	constants := logic.RuntimeEvalConstants()
	constants.MaxLogCalls = 3
	constants.MaxLogSize = 5
	ep := &logic.EvalParams{TxnGroup: group, EvalConstants: constants}
	tracer.BeforeTxnGroup(ep)
	require.Equal(t, constants, ep.EvalConstants)
}
