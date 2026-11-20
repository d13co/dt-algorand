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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/transactions/verify"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestDevTracerFailedGroup runs a DevTracer over groups that make no block, as
// a replay does: a top-level group whose second transaction fails, then two
// groups after it, the last one a logic sig checked before its group as
// simulate checks it. The failed group's transactions keep their indexes, its
// traces are simulate's, and the evaluation goes on after it.
func TestDevTracerFailedGroup(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	// reads "k"; a first arg of 0 rejects
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 10
txn ApplicationID
bz ok
byte "k"; app_global_get; pop
txn NumAppArgs
bz ok
txn ApplicationArgs 0
btoi
assert
ok: int 1`,
		ClearStateProgram: "#pragma version 10\nint 1",
	})
	lsigProgram, err := logic.AssembleString("#pragma version 10\ntxn Amount\nint 1000\n<=")
	require.NoError(t, err)
	lsigAddr := basics.Address(logic.HashProgram(lsigProgram.Program))
	env.TransferAlgos(sender.Addr, lsigAddr, 1_000_000)

	callWith := func(arg byte, note byte) txntest.Txn {
		return env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
			ApplicationArgs: [][]byte{{arg}}, Note: []byte{note}})
	}
	ok, bad := callWith(1, 1), callWith(0, 2)
	failing := txntest.Group(&ok, &bad)
	for i := range failing {
		failing[i] = failing[i].Txn.Sign(sender.Sk)
	}
	after := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app, Note: []byte{3}}).Txn().Sign(sender.Sk)
	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: lsigAddr, Receiver: sender.Addr, Amount: 1000}).Txn()
	lsigPay := transactions.SignedTxn{Txn: pay, Lsig: transactions.LogicSig{Logic: lsigProgram.Program}}

	traceCfg := simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true}
	latest := env.Ledger.Latest()
	failingAD := transactions.WrapSignedTxnsWithAD(failing)
	trace := simulation.NewGroupExecTracer(latest, failingAD, traceCfg, simulation.GroupExecOptions{})
	dev := ledger.NewDevTracer(ledger.DevConfig{
		Include:    ledger.IncludeBudget | ledger.IncludeReads | ledger.IncludeTrace,
		TraceFirst: 0,
		Trace:      trace,
	})

	prev, err := env.Ledger.BlockHdr(latest)
	require.NoError(t, err)
	hdr := bookkeeping.MakeBlock(prev).BlockHeader
	ev, err := env.Ledger.StartEvaluator(hdr, 0, 0, dev)
	require.NoError(t, err)
	require.ErrorContains(t, ev.TransactionGroup(failingAD...), "assert failed")
	require.NoError(t, ev.TransactionGroup(transactions.WrapSignedTxnsWithAD([]transactions.SignedTxn{after})...))
	// the evaluation doesn't run logic sigs: simulate's checks do, with the tracer
	_, err = verify.TxnGroupWithTracer([]transactions.SignedTxn{lsigPay}, &hdr, nil, env.Ledger, dev)
	require.NoError(t, err)
	require.NoError(t, ev.TransactionGroup(transactions.WrapSignedTxnsWithAD([]transactions.SignedTxn{lsigPay})...))
	require.NoError(t, dev.Err())

	// the failed group's transactions are 0 and 1, the next ones 2 and 3
	budget := dev.Budget()
	require.Len(t, budget, 4)
	paths := make([][]uint64, len(budget))
	for i, b := range budget {
		paths[i] = b.Path
	}
	require.Equal(t, [][]uint64{{0}, {1}, {2}, {3}}, paths)
	require.Equal(t, failing[0].ID(), budget[0].TxID)
	require.Equal(t, failing[1].ID(), budget[1].TxID)
	require.Equal(t, after.ID(), budget[2].TxID)
	require.Equal(t, lsigPay.ID(), budget[3].TxID)
	require.False(t, budget[0].Failed)
	require.True(t, budget[1].Failed, "the program that failed")
	require.False(t, budget[2].Failed)
	require.Equal(t, ledger.ProgramLogicSig, budget[3].Kind)
	require.Positive(t, budget[3].Cost)

	reads := dev.Reads()
	require.Len(t, reads, 3)
	for i, r := range reads {
		require.Equal(t, []uint64{uint64(i)}, r.Path)
		require.Equal(t, []ledger.StateRead{{App: app, Type: logic.GlobalState, Key: "k"}}, r.Reads)
	}

	// the failed group's traces are simulate's
	sim, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
		TxnGroups: [][]transactions.SignedTxn{failing}, TraceConfig: traceCfg})
	require.NoError(t, err)
	require.Contains(t, sim.TxnGroups[0].FailureMessage, "assert failed")
	traces := trace.Traces()
	require.Len(t, traces, 2)
	for i := range traces {
		require.Equal(t, sim.TxnGroups[0].Txns[i].Trace, traces[i], "txn %d", i)
	}
	require.Len(t, dev.Programs(), 1, "both run the app's approval program")
}

// TestDevTracerTracesLogicSig checks that the traced group's logic sigs, run
// before the group by simulate's checks, are traced as simulate traces them.
func TestDevTracerTracesLogicSig(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	lsigProgram, err := logic.AssembleString("#pragma version 10\ntxn Amount\nint 1000\n<=")
	require.NoError(t, err)
	lsigAddr := basics.Address(logic.HashProgram(lsigProgram.Program))
	env.TransferAlgos(sender.Addr, lsigAddr, 1_000_000)
	first := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr}).Txn().Sign(sender.Sk)
	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: lsigAddr, Receiver: sender.Addr, Amount: 1000}).Txn()
	lsigPay := transactions.SignedTxn{Txn: pay, Lsig: transactions.LogicSig{Logic: lsigProgram.Program}}

	traceCfg := simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true}
	latest := env.Ledger.Latest()
	group := []transactions.SignedTxn{lsigPay}
	trace := simulation.NewGroupExecTracer(latest, transactions.WrapSignedTxnsWithAD(group), traceCfg, simulation.GroupExecOptions{})
	dev := ledger.NewDevTracer(ledger.DevConfig{Include: ledger.IncludeTrace, TraceFirst: 1, Trace: trace})

	prev, err := env.Ledger.BlockHdr(latest)
	require.NoError(t, err)
	hdr := bookkeeping.MakeBlock(prev).BlockHeader
	ev, err := env.Ledger.StartEvaluator(hdr, 0, 0, dev)
	require.NoError(t, err)
	for _, g := range [][]transactions.SignedTxn{{first}, group} {
		_, err = verify.TxnGroupWithTracer(g, &hdr, nil, env.Ledger, dev)
		require.NoError(t, err)
		require.NoError(t, ev.TransactionGroup(transactions.WrapSignedTxnsWithAD(g)...))
	}
	require.NoError(t, dev.Err())

	sim, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
		TxnGroups: [][]transactions.SignedTxn{group}, TraceConfig: traceCfg})
	require.NoError(t, err)
	require.Empty(t, sim.TxnGroups[0].FailureMessage)
	require.NotEmpty(t, trace.Traces()[0].LogicSigTrace)
	require.Equal(t, sim.TxnGroups[0].Txns[0].Trace, trace.Traces()[0])
	require.Contains(t, dev.Programs(), crypto.Hash(lsigProgram.Program))
}

// TestDevTracerNextGroup follows groups checked as simulate checks them, each
// announced with NextGroup: a group whose logic sig rejects never begins, yet
// takes its index, and its logic sig is neither traced as the next group's nor
// counted under the next group's index.
func TestDevTracerNextGroup(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	// a first arg of 0 rejects
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 10
txn ApplicationID
bz ok
txn NumAppArgs
bz ok
txn ApplicationArgs 0
btoi
assert
ok: int 1`,
		ClearStateProgram: "#pragma version 10\nint 1",
	})
	lsigProgram, err := logic.AssembleString("#pragma version 10\ntxn Amount\nint 1000\n<=")
	require.NoError(t, err)
	lsigAddr := basics.Address(logic.HashProgram(lsigProgram.Program))
	env.TransferAlgos(sender.Addr, lsigAddr, 1_000_000)

	tooMuch := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: lsigAddr, Receiver: sender.Addr, Amount: 2000}).Txn()
	refused := []transactions.SignedTxn{{Txn: tooMuch, Lsig: transactions.LogicSig{Logic: lsigProgram.Program}}}
	bad := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
		ApplicationArgs: [][]byte{{0}}}).Txn().Sign(sender.Sk)
	traced := []transactions.SignedTxn{bad}
	ok := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app}).Txn().Sign(sender.Sk)

	traceCfg := simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true}
	latest := env.Ledger.Latest()
	trace := simulation.NewGroupExecTracer(latest, transactions.WrapSignedTxnsWithAD(traced), traceCfg, simulation.GroupExecOptions{})
	dev := ledger.NewDevTracer(ledger.DevConfig{Include: ledger.IncludeBudget | ledger.IncludeTrace, TraceFirst: 1, Trace: trace})

	prev, err := env.Ledger.BlockHdr(latest)
	require.NoError(t, err)
	hdr := bookkeeping.MakeBlock(prev).BlockHeader
	ev, err := env.Ledger.StartEvaluator(hdr, 0, 0, dev)
	require.NoError(t, err)
	var checkErrs, evalErrs []error
	for _, g := range [][]transactions.SignedTxn{refused, traced, {ok}} {
		dev.NextGroup(len(g))
		_, err := verify.TxnGroupWithTracer(g, &hdr, nil, env.Ledger, dev)
		checkErrs = append(checkErrs, err)
		if err == nil {
			evalErrs = append(evalErrs, ev.TransactionGroup(transactions.WrapSignedTxnsWithAD(g)...))
		}
	}
	require.Error(t, checkErrs[0], "the logic sig rejects")
	require.NoError(t, checkErrs[1])
	require.NoError(t, checkErrs[2])
	require.ErrorContains(t, evalErrs[0], "assert failed")
	require.NoError(t, evalErrs[1])
	require.NoError(t, dev.Err())

	budget := dev.Budget()
	require.Len(t, budget, 3)
	require.Equal(t, []uint64{0}, budget[0].Path)
	require.Equal(t, ledger.ProgramLogicSig, budget[0].Kind)
	require.True(t, budget[0].Failed, "it rejected")
	require.Equal(t, []uint64{1}, budget[1].Path)
	require.Equal(t, bad.ID(), budget[1].TxID)
	require.True(t, budget[1].Failed)
	require.Equal(t, []uint64{2}, budget[2].Path)
	require.False(t, budget[2].Failed)

	// the trace is the app call's alone, as simulate traces it
	sim, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
		TxnGroups: [][]transactions.SignedTxn{traced}, TraceConfig: traceCfg})
	require.NoError(t, err)
	require.Equal(t, sim.TxnGroups[0].Txns[0].Trace, trace.Traces()[0])
	require.Empty(t, trace.Traces()[0].LogicSigTrace)
	require.NotContains(t, dev.Programs(), crypto.Hash(lsigProgram.Program))
	require.Len(t, dev.Programs(), 1)
}

// TestMuxTracerNil checks that MuxTracer leaves out nil tracers, typed nil
// pointers included.
func TestMuxTracerNil(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var dev *ledger.DevTracer
	require.Nil(t, ledger.MuxTracer(nil, dev))
	one := ledger.NewDevTracer(ledger.DevConfig{})
	require.Same(t, one, ledger.MuxTracer(dev, one, nil))
}
