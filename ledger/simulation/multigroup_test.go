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
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// simulateGroups simulates request with the developer API on, allowing up to MaxSimulateGroups
// groups unless the request sets MaxGroups, and checks the result against its block.
func simulateGroups(t *testing.T, env simulationtesting.Environment, request simulation.Request) simulation.Result {
	t.Helper()
	if request.MaxGroups == 0 {
		request.MaxGroups = simulation.MaxSimulateGroups
	}
	result, err := simulation.MakeSimulator(env.Ledger, true).Simulate(request)
	require.NoError(t, err)
	require.Len(t, result.TxnGroups, len(request.TxnGroups))
	validateSimulationResult(t, result)
	return result
}

// bothModes runs f once with sequential groups and once with parallel groups.
func bothModes(t *testing.T, f func(t *testing.T, parallel bool)) {
	t.Helper()
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%t", parallel), func(t *testing.T) {
			t.Parallel()
			f(t, parallel)
		})
	}
}

func newTestAccount(seed byte) (basics.Address, *crypto.SignatureSecrets) {
	var s crypto.Seed
	s[0] = 0xa7
	s[1] = seed
	secrets := crypto.GenerateSignatureSecrets(s)
	return basics.Address(secrets.SignatureVerifier), secrets
}

func requireGroupSucceeded(t *testing.T, result simulation.Result, g int) {
	t.Helper()
	require.Empty(t, result.TxnGroups[g].FailureMessage, "group %d", g)
	require.Nil(t, result.TxnGroups[g].FailedAt, "group %d", g)
}

func uintLog(t *testing.T, result simulation.Result, g int) uint64 {
	t.Helper()
	requireGroupSucceeded(t, result, g)
	logs := result.TxnGroups[g].Txns[0].Txn.EvalDelta.Logs
	require.Len(t, logs, 1)
	return bytesToUint64([]byte(logs[0]))
}

// balanceLoggerApp logs the balance of txn.Accounts[1].
const balanceLoggerApp = `#pragma version 8
txn ApplicationID
bz done
txn Accounts 1
balance
itob
log
done:
int 1`

// TestMultipleGroupsSharedState checks that sequential groups see the effects of the groups
// before them, that parallel groups don't, and that a failed group is rolled back.
func TestMultipleGroupsSharedState(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		funder := env.Accounts[0]
		fresh, freshSk := newTestAccount(1)
		app := env.CreateApp(funder.Addr, simulationtesting.AppParams{
			ApprovalProgram:   balanceLoggerApp,
			ClearStateProgram: "#pragma version 8\nint 1",
		})

		fund := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   funder.Addr,
			Receiver: fresh,
			Amount:   1_000_000,
		}).Txn().Sign(funder.Sk)
		spend := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   fresh,
			Receiver: funder.Addr,
			Amount:   500_000,
		}).Txn().Sign(freshSk)
		logBalance := env.TxnInfo.NewTxn(txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        funder.Addr,
			ApplicationID: app,
			Accounts:      []basics.Address{fresh},
		}).Txn().Sign(funder.Sk)

		t.Run("dependent", func(t *testing.T) {
			result := simulateGroups(t, env, simulation.Request{
				TxnGroups:      [][]transactions.SignedTxn{{fund}, {spend}, {logBalance}},
				ParallelGroups: parallel,
			})
			require.Equal(t, parallel, result.EvalOverrides.ParallelGroups)
			requireGroupSucceeded(t, result, 0)
			if parallel {
				// The fresh account has no algos at the start round.
				require.Contains(t, result.TxnGroups[1].FailureMessage, "overspend")
				require.Equal(t, simulation.TxnPath{0}, result.TxnGroups[1].FailedAt)
				require.Zero(t, uintLog(t, result, 2))
			} else {
				requireGroupSucceeded(t, result, 1)
				fee := env.TxnInfo.CurrentProtocolParams().MinTxnFee
				require.Equal(t, 1_000_000-500_000-fee, uintLog(t, result, 2))
			}
		})

		t.Run("failed group rolled back", func(t *testing.T) {
			fundAgain := env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   funder.Addr,
				Receiver: fresh,
				Amount:   2_000_000,
			})
			overspend := env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   env.Accounts[1].Addr,
				Receiver: env.Accounts[2].Addr,
				Amount:   env.Accounts[1].AcctData.MicroAlgos.Raw,
			})
			failing := txntest.Group(&fundAgain, &overspend)
			failing[0] = failing[0].Txn.Sign(funder.Sk)
			failing[1] = failing[1].Txn.Sign(env.Accounts[1].Sk)

			result := simulateGroups(t, env, simulation.Request{
				TxnGroups:      [][]transactions.SignedTxn{failing, {logBalance}},
				ParallelGroups: parallel,
			})
			require.Contains(t, result.TxnGroups[0].FailureMessage, "overspend")
			require.Equal(t, simulation.TxnPath{1}, result.TxnGroups[0].FailedAt)
			// The first payment of the failed group is not seen.
			require.Zero(t, uintLog(t, result, 1))
		})
	})
}

// globalStateApp writes its first arg to global "k" when given one, else logs "k". It creates "k"
// with value 3.
const globalStateApp = `#pragma version 8
txn ApplicationID
bz create
txn NumAppArgs
bz read
byte "k"
txna ApplicationArgs 0
btoi
app_global_put
int 1
return
read:
byte "k"
app_global_get
itob
log
int 1
return
create:
byte "k"
int 3
app_global_put
int 1`

// TestMultipleGroupsAppState checks app state across groups, and that budgets, traces and initial
// states are reported per group.
func TestMultipleGroupsAppState(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		sender := env.Accounts[0]
		app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
			ApprovalProgram:   globalStateApp,
			ClearStateProgram: "#pragma version 8\nint 1",
			GlobalStateSchema: basics.StateSchema{NumUint: 1},
		})

		write := env.TxnInfo.NewTxn(txntest.Txn{
			Type:            protocol.ApplicationCallTx,
			Sender:          sender.Addr,
			ApplicationID:   app,
			ApplicationArgs: [][]byte{uint64ToBytes(7)},
		}).Txn().Sign(sender.Sk)
		read := env.TxnInfo.NewTxn(txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        sender.Addr,
			ApplicationID: app,
		}).Txn().Sign(sender.Sk)

		const extraBudget = 100
		result := simulateGroups(t, env, simulation.Request{
			TxnGroups:         [][]transactions.SignedTxn{{write}, {read}},
			ParallelGroups:    parallel,
			ExtraOpcodeBudget: extraBudget,
			TraceConfig:       simulation.ExecTraceConfig{Enable: true, State: true},
		})

		if parallel {
			require.Equal(t, uint64(3), uintLog(t, result, 1))
		} else {
			require.Equal(t, uint64(7), uintLog(t, result, 1))
		}

		proto := env.TxnInfo.CurrentProtocolParams()
		for g, groupResult := range result.TxnGroups {
			requireGroupSucceeded(t, result, g)
			// Each group gets the extra budget.
			require.Equal(t, int(proto.MaxAppProgramCost)+extraBudget, groupResult.AppBudgetAdded, "group %d", g)
			require.Positive(t, groupResult.AppBudgetConsumed, "group %d", g)
			require.Equal(t, groupResult.Txns[0].AppBudgetConsumed, groupResult.AppBudgetConsumed, "group %d", g)
			trace := groupResult.Txns[0].Trace
			require.NotNil(t, trace, "group %d", g)
			require.NotEmpty(t, trace.ApprovalProgramTrace, "group %d", g)
		}

		var writes []simulation.StateOperation
		for _, unit := range result.TxnGroups[0].Txns[0].Trace.ApprovalProgramTrace {
			writes = append(writes, unit.StateChanges...)
		}
		require.Equal(t, []simulation.StateOperation{{
			AppStateOp: logic.AppStateWrite,
			AppState:   logic.GlobalState,
			AppID:      app,
			Key:        "k",
			NewValue:   basics.TealValue{Type: basics.TealUintType, Uint: 7},
		}}, writes)

		// The initial state is the one before the first group, in both modes.
		require.NotNil(t, result.InitialStates)
		require.Equal(t, simulation.AppKVPairs{"k": {Type: basics.TealUintType, Uint: 3}},
			result.InitialStates.AllAppsInitialStates[app].AppGlobals)
	})
}

// TestMultipleGroupsFixSigners checks that the FixSigners pre-pass of a group sees the rekeys of the
// groups before it.
func TestMultipleGroupsFixSigners(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	unsignedPay := func(env simulationtesting.Environment, sender basics.Address, rekeyTo basics.Address, note byte) transactions.SignedTxn {
		return env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender,
			Receiver: sender,
			RekeyTo:  rekeyTo,
			Note:     []byte{note},
		}).SignedTxn()
	}
	simulate := func(t *testing.T, env simulationtesting.Environment, parallel bool, groups ...transactions.SignedTxn) simulation.Result {
		t.Helper()
		request := simulation.Request{
			AllowEmptySignatures: true,
			FixSigners:           true,
			ParallelGroups:       parallel,
		}
		for _, stxn := range groups {
			request.TxnGroups = append(request.TxnGroups, []transactions.SignedTxn{stxn})
		}
		return simulateGroups(t, env, request)
	}

	t.Run("rekey", func(t *testing.T) {
		t.Parallel()
		bothModes(t, func(t *testing.T, parallel bool) {
			env := simulationtesting.PrepareSimulatorTest(t)
			defer env.Close()
			a, b := env.Accounts[0], env.Accounts[1]

			rekey := env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   a.Addr,
				Receiver: a.Addr,
				RekeyTo:  b.Addr,
			}).Txn().Sign(a.Sk)
			result := simulate(t, env, parallel, rekey, unsignedPay(env, a.Addr, basics.Address{}, 1))
			requireGroupSucceeded(t, result, 0)
			requireGroupSucceeded(t, result, 1)
			if parallel {
				require.Zero(t, result.TxnGroups[1].Txns[0].FixedSigner)
			} else {
				require.Equal(t, b.Addr, result.TxnGroups[1].Txns[0].FixedSigner)
			}
		})
	})

	t.Run("rekey by an inner txn", func(t *testing.T) {
		t.Parallel()
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()
		a, b := env.Accounts[0], env.Accounts[1]

		app := env.CreateApp(a.Addr, simulationtesting.AppParams{
			ApprovalProgram: `#pragma version 8
txn ApplicationID
bz done
itxn_begin
int pay
itxn_field TypeEnum
global CurrentApplicationAddress
itxn_field Receiver
txn Accounts 1
itxn_field RekeyTo
itxn_submit
done:
int 1`,
			ClearStateProgram: "#pragma version 8\nint 1",
		})
		env.TransferAlgos(a.Addr, app.Address(), 1_000_000)

		call := env.TxnInfo.NewTxn(txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        a.Addr,
			ApplicationID: app,
			Accounts:      []basics.Address{b.Addr},
			Fee:           2 * env.TxnInfo.CurrentProtocolParams().MinTxnFee,
		}).Txn().Sign(a.Sk)
		result := simulate(t, env, false, call, unsignedPay(env, app.Address(), basics.Address{}, 1))
		requireGroupSucceeded(t, result, 0)
		requireGroupSucceeded(t, result, 1)
		require.Equal(t, b.Addr, result.TxnGroups[1].Txns[0].FixedSigner)
	})

	t.Run("rekey, then un-rekey", func(t *testing.T) {
		t.Parallel()
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()
		a, b := env.Accounts[0], env.Accounts[1]

		rekey := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   a.Addr,
			Receiver: a.Addr,
			RekeyTo:  b.Addr,
		}).Txn().Sign(a.Sk)
		result := simulate(t, env, false, rekey, unsignedPay(env, a.Addr, a.Addr, 1), unsignedPay(env, a.Addr, basics.Address{}, 2))
		for g := range result.TxnGroups {
			requireGroupSucceeded(t, result, g)
		}
		require.Equal(t, b.Addr, result.TxnGroups[1].Txns[0].FixedSigner)
		require.Zero(t, result.TxnGroups[2].Txns[0].FixedSigner)
	})

	t.Run("rekey in a failed group", func(t *testing.T) {
		t.Parallel()
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()
		a, b, c := env.Accounts[0], env.Accounts[1], env.Accounts[2]

		rekey := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   a.Addr,
			Receiver: a.Addr,
			RekeyTo:  b.Addr,
		})
		overspend := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   c.Addr,
			Receiver: a.Addr,
			Amount:   c.AcctData.MicroAlgos.Raw,
		})
		failing := txntest.Group(&rekey, &overspend)
		failing[0] = failing[0].Txn.Sign(a.Sk)
		failing[1] = failing[1].Txn.Sign(c.Sk)

		// The rekey was rolled back with its group, so the next group is signed by A itself.
		result := simulateGroups(t, env, simulation.Request{
			TxnGroups:            [][]transactions.SignedTxn{failing, {unsignedPay(env, a.Addr, basics.Address{}, 1)}},
			AllowEmptySignatures: true,
			FixSigners:           true,
		})
		require.Contains(t, result.TxnGroups[0].FailureMessage, "overspend")
		requireGroupSucceeded(t, result, 1)
		require.Zero(t, result.TxnGroups[1].Txns[0].FixedSigner)
	})

	t.Run("rekey in a group that panics", func(t *testing.T) {
		t.Parallel()
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()
		a, b := env.Accounts[0], env.Accounts[1]

		rekey := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   a.Addr,
			Receiver: a.Addr,
			RekeyTo:  b.Addr,
		}).Txn().Sign(a.Sk)
		request := simulation.WithAfterTxnHook(simulation.Request{
			TxnGroups:            [][]transactions.SignedTxn{{rekey}, {unsignedPay(env, a.Addr, basics.Address{}, 1)}},
			AllowEmptySignatures: true,
			FixSigners:           true,
		}, func(g, i int) {
			// Before the commit: the evaluator recovers, rolls the group back and stays usable.
			if g == 0 {
				panic("injected")
			}
		})
		// The tracer's AfterTxnGroup sees no error for the panicked group, but its rekey was rolled
		// back, so the next group is signed by A itself.
		result := simulateGroups(t, env, request)
		require.Contains(t, result.TxnGroups[0].FailureMessage, "injected")
		requireGroupSucceeded(t, result, 1)
		require.Zero(t, result.TxnGroups[1].Txns[0].FixedSigner)
	})

	t.Run("un-rekey an account rekeyed at the start round", func(t *testing.T) {
		t.Parallel()
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()
		a, b := env.Accounts[0], env.Accounts[1]
		env.Rekey(a.Addr, b.Addr)

		// The un-rekey recorded by the first group is used as is: the second group must not fall
		// back to the ledger's AuthAddr at the start round.
		result := simulate(t, env, false, unsignedPay(env, a.Addr, a.Addr, 1), unsignedPay(env, a.Addr, basics.Address{}, 2))
		requireGroupSucceeded(t, result, 0)
		requireGroupSucceeded(t, result, 1)
		require.Equal(t, b.Addr, result.TxnGroups[0].Txns[0].FixedSigner)
		require.Zero(t, result.TxnGroups[1].Txns[0].FixedSigner)
	})
}

// TestMultipleGroupsBadSignature checks that a bad signature fails only its own group.
func TestMultipleGroupsBadSignature(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		groups := make([][]transactions.SignedTxn, 3)
		for g := range groups {
			sender := env.Accounts[g]
			stxn := env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   sender.Addr,
				Receiver: sender.Addr,
			}).Txn().Sign(sender.Sk)
			groups[g] = []transactions.SignedTxn{stxn}
		}
		groups[1][0].Sig[0]++

		result := simulateGroups(t, env, simulation.Request{TxnGroups: groups, ParallelGroups: parallel})
		requireGroupSucceeded(t, result, 0)
		// The batch verifier can't tell which txn has the bad signature.
		require.Equal(t, crypto.ErrBatchHasFailedSigs.Error(), result.TxnGroups[1].FailureMessage)
		require.Nil(t, result.TxnGroups[1].FailedAt)
		requireGroupSucceeded(t, result, 2)

		// Alone, the group is still an invalid request.
		_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(simulation.Request{
			TxnGroups:      groups[1:2],
			ParallelGroups: parallel,
		})
		var invalid simulation.InvalidRequestError
		require.ErrorAs(t, err, &invalid)
		require.ErrorIs(t, err, crypto.ErrBatchHasFailedSigs)
	})
}

// TestMultipleGroupsDuplicate checks duplicate detection across sequential groups, and its absence
// across parallel groups.
func TestMultipleGroupsDuplicate(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		sender := env.Accounts[0]
		stxn := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: env.Accounts[1].Addr,
			Amount:   1,
		}).Txn().Sign(sender.Sk)

		result := simulateGroups(t, env, simulation.Request{
			TxnGroups:      [][]transactions.SignedTxn{{stxn}, {stxn}},
			ParallelGroups: parallel,
		})
		requireGroupSucceeded(t, result, 0)
		if parallel {
			requireGroupSucceeded(t, result, 1)
		} else {
			require.Contains(t, result.TxnGroups[1].FailureMessage, "already in ledger")
		}
	})
}

// TestMultipleGroupsBlockFull checks that sequential groups share the block's byte limit: a group
// that doesn't fit fails, and a smaller group after it still fits.
func TestMultipleGroupsBlockFull(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	proto := env.TxnInfo.CurrentProtocolParams()
	sender := env.Accounts[0]
	// Each call logs simulation.LogBytesLimit bytes, the most allowed with AllowMoreLogging.
	const logSize = 4096
	program := "#pragma version 8\ntxn ApplicationID\nbz done\n" +
		strings.Repeat(fmt.Sprintf("int %d\nbzero\nlog\n", logSize), simulation.LogBytesLimit/logSize) +
		"done:\nint 1"
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   program,
		ClearStateProgram: "#pragma version 8\nint 1",
	})

	// Each full group logs MaxTxGroupSize * LogBytesLimit bytes. With the rest of the txns, the last
	// of these groups no longer fits.
	groupBytes := proto.MaxTxGroupSize * simulation.LogBytesLimit
	require.Zero(t, proto.MaxTxnBytesPerBlock%groupBytes)
	fullGroups := proto.MaxTxnBytesPerBlock / groupBytes
	require.Less(t, fullGroups, simulation.MaxSimulateGroups)

	var groups [][]transactions.SignedTxn
	for g := range fullGroups {
		txns := make([]*txntest.Txn, proto.MaxTxGroupSize)
		for i := range txns {
			txn := env.TxnInfo.NewTxn(txntest.Txn{
				Type:          protocol.ApplicationCallTx,
				Sender:        sender.Addr,
				ApplicationID: app,
				Note:          []byte{byte(g), byte(i)},
			})
			txns[i] = &txn
		}
		group := txntest.Group(txns...)
		for i := range group {
			group[i] = group[i].Txn.Sign(sender.Sk)
		}
		groups = append(groups, group)
	}
	pay := env.TxnInfo.NewTxn(txntest.Txn{
		Type:     protocol.PaymentTx,
		Sender:   sender.Addr,
		Receiver: sender.Addr,
	}).Txn().Sign(sender.Sk)
	groups = append(groups, []transactions.SignedTxn{pay})

	result := simulateGroups(t, env, simulation.Request{
		TxnGroups:        groups,
		AllowMoreLogging: true,
	})
	last := fullGroups - 1
	for g := range last {
		requireGroupSucceeded(t, result, g)
	}
	require.Equal(t, ledgercore.ErrNoSpace.Error(), result.TxnGroups[last].FailureMessage)
	require.Nil(t, result.TxnGroups[last].FailedAt)
	requireGroupSucceeded(t, result, fullGroups)
}

// TestMultipleGroupsUnblamableVerifyFailure checks that a group whose verification failure can't be
// blamed on one transaction fails alone, with no failed-at, among several groups, while a request
// with only that group is still an invalid request.
func TestMultipleGroupsUnblamableVerifyFailure(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		proto := env.TxnInfo.CurrentProtocolParams()
		ops, err := logic.AssembleString("#pragma version 8\nint 1")
		require.NoError(t, err)
		program := logic.Program(ops.Program)
		escrow := basics.Address(crypto.HashObj(&program))

		// LogicSig args larger than the group's size pool.
		lsigTxn := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   escrow,
			Receiver: escrow,
		}).SignedTxn()
		lsigTxn.Lsig = transactions.LogicSig{
			Logic: ops.Program,
			Args:  [][]byte{make([]byte, proto.LogicSigMaxSize+1)},
		}

		pay := func(i int) transactions.SignedTxn {
			sender := env.Accounts[i]
			return env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   sender.Addr,
				Receiver: sender.Addr,
			}).Txn().Sign(sender.Sk)
		}

		result := simulateGroups(t, env, simulation.Request{
			TxnGroups:      [][]transactions.SignedTxn{{pay(0)}, {lsigTxn}, {pay(1)}},
			ParallelGroups: parallel,
		})
		requireGroupSucceeded(t, result, 0)
		require.Contains(t, result.TxnGroups[1].FailureMessage, "more than the available size pool")
		require.Nil(t, result.TxnGroups[1].FailedAt)
		requireGroupSucceeded(t, result, 2)

		_, err = simulation.MakeSimulator(env.Ledger, false).Simulate(simulation.Request{
			TxnGroups:      [][]transactions.SignedTxn{{lsigTxn}},
			ParallelGroups: parallel,
		})
		var invalid simulation.InvalidRequestError
		require.ErrorAs(t, err, &invalid)
		require.ErrorContains(t, err, "more than the available size pool")
	})
}

// TestMultipleGroupsInconsistentGroupIDs checks that a group with mixed Group IDs, which
// verification does not catch, fails in evaluation without affecting the other groups.
func TestMultipleGroupsInconsistentGroupIDs(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	sender := env.Accounts[0]
	pay := func(note byte) txntest.Txn {
		return env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: sender.Addr,
			Note:     []byte{note},
		})
	}
	sign := func(stxns []transactions.SignedTxn) []transactions.SignedTxn {
		for i := range stxns {
			stxns[i] = stxns[i].Txn.Sign(sender.Sk)
		}
		return stxns
	}
	a1, a2, b1, b2 := pay(1), pay(2), pay(3), pay(4)
	groupA := sign(txntest.Group(&a1, &a2))
	groupB := sign(txntest.Group(&b1, &b2))
	mixed := []transactions.SignedTxn{groupA[0], groupB[1]}

	result := simulateGroups(t, env, simulation.Request{
		TxnGroups: [][]transactions.SignedTxn{groupB[:1:1], mixed, {groupA[1]}},
	})
	// The lone txns are incomplete groups.
	require.Contains(t, result.TxnGroups[0].FailureMessage, "incomplete group")
	require.Contains(t, result.TxnGroups[1].FailureMessage, "inconsistent group values")
	require.Equal(t, simulation.TxnPath{1}, result.TxnGroups[1].FailedAt)
	require.Contains(t, result.TxnGroups[2].FailureMessage, "incomplete group")
}

// TestMultipleGroupsPostCheckFailure checks that a failed placeholder PQ post-check, which runs
// after its group is committed, stops the later sequential groups.
func TestMultipleGroupsPostCheckFailure(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		pqAuthorizer, pqSig := makePlaceholderPQSigForSimulation(t, 4)
		pqSig.Salt ^= 1
		txgroup := makeAppThenPlaceholderPQFixSignersGroup(t, &env, pqAuthorizer, pqSig)

		groups := [][]transactions.SignedTxn{txgroup}
		for i := 2; i < 4; i++ {
			sender := env.Accounts[i]
			groups = append(groups, []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   sender.Addr,
				Receiver: sender.Addr,
			}).Txn().Sign(sender.Sk)})
		}

		result, err := simulation.MakeSimulator(env.Ledger, false).Simulate(simulation.Request{
			TxnGroups:            groups,
			AllowEmptySignatures: true,
			FixSigners:           true,
			ParallelGroups:       parallel,
			MaxGroups:            3,
		})
		require.NoError(t, err)
		require.Contains(t, result.TxnGroups[0].FailureMessage, "pq signature authorizer mismatch")
		require.Equal(t, simulation.TxnPath{1}, result.TxnGroups[0].FailedAt)
		// The failed group keeps its results.
		require.Equal(t, pqAuthorizer, result.TxnGroups[0].Txns[1].FixedSigner)
		require.Nil(t, result.Block)
		for g := 1; g < 3; g++ {
			if parallel {
				requireGroupSucceeded(t, result, g)
			} else {
				require.Equal(t, "not simulated: group index 0 failed a post-evaluation check", result.TxnGroups[g].FailureMessage)
				require.Nil(t, result.TxnGroups[g].FailedAt)
			}
		}
	})
}

// TestMultipleGroupsUnnamedResourcesMultiplier checks that every group gets its own unnamed
// resource limits, scaled by the multiplier.
func TestMultipleGroupsUnnamedResourcesMultiplier(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		proto := env.TxnInfo.CurrentProtocolParams()
		sender := env.Accounts[0]
		app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
			ApprovalProgram: `#pragma version 10
txn ApplicationID
bz end
int 0
store 0
loop:
load 0
txn NumAppArgs
<
bz end
load 0
txnas ApplicationArgs
acct_params_get AcctBalance
pop
pop
load 0
int 1
+
store 0
b loop
end:
int 1`,
			ClearStateProgram: "#pragma version 10\nint 1",
		})

		// One more account than one app call may reference, in a group filled up to
		// MaxTxGroupSize, so the app call's own limits are the group's limits.
		accounts := make([][]basics.Address, 2)
		groups := make([][]transactions.SignedTxn, 2)
		for g := range groups {
			accounts[g] = make([]basics.Address, proto.MaxAppTotalTxnReferences+1)
			args := make([][]byte, len(accounts[g]))
			for i := range accounts[g] {
				accounts[g][i] = basics.Address{0xcc, byte(g), byte(i + 1)}
				args[i] = accounts[g][i][:]
			}
			call := env.TxnInfo.NewTxn(txntest.Txn{
				Type:            protocol.ApplicationCallTx,
				Sender:          sender.Addr,
				ApplicationID:   app,
				ApplicationArgs: args,
			})
			txns := []*txntest.Txn{&call}
			for i := 1; i < proto.MaxTxGroupSize; i++ {
				pay := env.TxnInfo.NewTxn(txntest.Txn{
					Type:     protocol.PaymentTx,
					Sender:   sender.Addr,
					Receiver: sender.Addr,
					Note:     []byte{byte(g), byte(i)},
				})
				txns = append(txns, &pay)
			}
			groups[g] = txntest.Group(txns...)
			for i := range groups[g] {
				groups[g][i] = groups[g][i].Txn.Sign(sender.Sk)
			}
		}

		for _, multiplier := range []uint64{1, 2} {
			result := simulateGroups(t, env, simulation.Request{
				TxnGroups:               groups,
				AllowUnnamedResources:   true,
				ParallelGroups:          parallel,
				ResourceLimitMultiplier: multiplier,
			})
			for g := range groups {
				if multiplier == 1 {
					require.Contains(t, result.TxnGroups[g].FailureMessage, fmt.Sprintf("unavailable Account %s", accounts[g][len(accounts[g])-1]))
					continue
				}
				requireGroupSucceeded(t, result, g)
				unnamed := result.TxnGroups[g].UnnamedResourcesAccessed
				require.NotNil(t, unnamed, "group %d", g)
				require.ElementsMatch(t, accounts[g], slices.Collect(maps.Keys(unnamed.Accounts)), "group %d", g)
			}
		}
	})
}

// TestMultipleGroupsTraceAfterFailedInner checks that a group after one whose inner txn failed is
// traced as if it were alone.
func TestMultipleGroupsTraceAfterFailedInner(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	sender := env.Accounts[0]
	// The inner payment overspends: the app account has no algos to spare.
	failingApp := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 8
txn ApplicationID
bz done
itxn_begin
int pay
itxn_field TypeEnum
txn Sender
itxn_field Receiver
int 1000000000
itxn_field Amount
itxn_submit
done:
int 1`,
		ClearStateProgram: "#pragma version 8\nint 1",
	})
	env.TransferAlgos(sender.Addr, failingApp.Address(), 1_000_000)
	tracedApp := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   globalStateApp,
		ClearStateProgram: "#pragma version 8\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})

	failing := env.TxnInfo.NewTxn(txntest.Txn{
		Type:          protocol.ApplicationCallTx,
		Sender:        sender.Addr,
		ApplicationID: failingApp,
		Fee:           2 * env.TxnInfo.CurrentProtocolParams().MinTxnFee,
	}).Txn().Sign(sender.Sk)
	traced := env.TxnInfo.NewTxn(txntest.Txn{
		Type:            protocol.ApplicationCallTx,
		Sender:          sender.Addr,
		ApplicationID:   tracedApp,
		ApplicationArgs: [][]byte{uint64ToBytes(9)},
	}).Txn().Sign(sender.Sk)

	traceConfig := simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true}
	both := simulateGroups(t, env, simulation.Request{
		TxnGroups:   [][]transactions.SignedTxn{{failing}, {traced}},
		TraceConfig: traceConfig,
	})
	require.Contains(t, both.TxnGroups[0].FailureMessage, "overspend")
	require.Equal(t, simulation.TxnPath{0, 0}, both.TxnGroups[0].FailedAt)

	alone := simulateGroups(t, env, simulation.Request{
		TxnGroups:   [][]transactions.SignedTxn{{traced}},
		TraceConfig: traceConfig,
	})
	requireGroupSucceeded(t, both, 1)
	require.Equal(t, alone.TxnGroups[0], both.TxnGroups[1])
}

// TestParallelGroupsSameCreatedApp checks that parallel groups may create the same app ID, and
// that the merged initial states leave that app out.
func TestParallelGroupsSameCreatedApp(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	sender := env.Accounts[0]
	existing := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   globalStateApp,
		ClearStateProgram: "#pragma version 8\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})

	var groups [][]transactions.SignedTxn
	for g := range 2 {
		create := env.TxnInfo.NewTxn(txntest.Txn{
			Type:              protocol.ApplicationCallTx,
			Sender:            sender.Addr,
			ApprovalProgram:   globalStateApp,
			ClearStateProgram: "#pragma version 8\nint 1",
			GlobalStateSchema: basics.StateSchema{NumUint: 1},
			Note:              []byte{byte(g)},
		})
		read := env.TxnInfo.NewTxn(txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        sender.Addr,
			ApplicationID: existing,
			Note:          []byte{byte(g)},
		})
		group := txntest.Group(&create, &read)
		for i := range group {
			group[i] = group[i].Txn.Sign(sender.Sk)
		}
		groups = append(groups, group)
	}

	result := simulateGroups(t, env, simulation.Request{
		TxnGroups:      groups,
		ParallelGroups: true,
		TraceConfig:    simulation.ExecTraceConfig{Enable: true, State: true},
	})
	requireGroupSucceeded(t, result, 0)
	requireGroupSucceeded(t, result, 1)
	created := result.TxnGroups[0].Txns[0].Txn.ApplicationID
	require.NotZero(t, created)
	require.Equal(t, created, result.TxnGroups[1].Txns[0].Txn.ApplicationID)

	require.True(t, result.InitialStates.CreatedApp.Contains(created))
	require.Len(t, result.InitialStates.CreatedApp, 1)
	require.NotContains(t, result.InitialStates.AllAppsInitialStates, created)
	require.Equal(t, simulation.AppKVPairs{"k": {Type: basics.TealUintType, Uint: 3}},
		result.InitialStates.AllAppsInitialStates[existing].AppGlobals)
}

// TestParallelGroupsInitialStatesDeterministic checks that merged initial states don't depend on
// the order in which parallel groups finish.
func TestParallelGroupsInitialStatesDeterministic(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	sender := env.Accounts[0]
	// Writes global "new" when given an arg, else reads it.
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 8
txn ApplicationID
bz done
byte "new"
txn NumAppArgs
bz read
int 5
app_global_put
b done
read:
app_global_get
pop
done:
int 1`,
		ClearStateProgram: "#pragma version 8\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})

	const n = 4
	groups := make([][]transactions.SignedTxn, n)
	for g := range groups {
		txn := txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        sender.Addr,
			ApplicationID: app,
			Note:          []byte{byte(g)},
		}
		if g%2 == 0 {
			// The even groups create "new", the odd ones read it while it doesn't exist.
			txn.ApplicationArgs = [][]byte{{1}}
		}
		groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txn).Txn().Sign(sender.Sk)}
	}

	var states []*simulation.ResourcesInitialStates
	for _, delay := range []func(g int) time.Duration{
		func(g int) time.Duration { return time.Duration(g) * 20 * time.Millisecond },
		func(g int) time.Duration { return time.Duration(n-g) * 20 * time.Millisecond },
	} {
		request := simulation.WithParallelGroupHook(simulation.Request{
			TxnGroups:      groups,
			ParallelGroups: true,
			TraceConfig:    simulation.ExecTraceConfig{Enable: true, State: true},
		}, func(g int) error { time.Sleep(delay(g)); return nil })
		result := simulateGroups(t, env, request)
		for g := range groups {
			requireGroupSucceeded(t, result, g)
		}
		states = append(states, result.InitialStates)
	}
	require.Equal(t, states[0], states[1])
	// Group 0, which created "new", wins over the groups that read it.
	appStates := states[0].AllAppsInitialStates[app]
	require.True(t, appStates.CreatedGlobals.Contains("new"))
	require.NotContains(t, appStates.AppGlobals, "new")
}

// TestParallelGroupsPanic checks that a panic in a parallel group is the request's error, that
// every worker is done before Simulate returns, and that no group slot is leaked.
func TestParallelGroupsPanic(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	groups := make([][]transactions.SignedTxn, 3)
	for g := range groups {
		sender := env.Accounts[g]
		groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: sender.Addr,
		}).Txn().Sign(sender.Sk)}
	}

	var mu deadlock.Mutex
	started, finished := make(map[int]bool), make(map[int]bool)
	request := simulation.WithParallelGroupHook(simulation.Request{
		TxnGroups:      groups,
		ParallelGroups: true,
		MaxGroups:      3,
	}, func(g int) error {
		if g == 0 {
			panic("injected")
		}
		mu.Lock()
		started[g] = true
		mu.Unlock()
		// The other groups are slower than the panicking one.
		time.Sleep(100 * time.Millisecond)
		mu.Lock()
		finished[g] = true
		mu.Unlock()
		return nil
	})
	_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
	require.ErrorContains(t, err, "simulating transaction group 0: panic: injected")

	// Every group that started was done before Simulate returned.
	mu.Lock()
	require.Equal(t, started, finished)
	mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	require.NoError(t, simulation.AcquireAllParallelGroupSlots(ctx))
}

// TestParallelGroupsError checks that an error in a parallel group is the request's error, and
// that the groups not started yet are skipped.
func TestParallelGroupsError(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	n := min(2*runtime.GOMAXPROCS(0)+1, simulation.MaxSimulateGroups)
	groups := make([][]transactions.SignedTxn, n)
	for g := range groups {
		sender := env.Accounts[0]
		groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: sender.Addr,
			Note:     []byte{byte(g)},
		}).Txn().Sign(sender.Sk)}
	}

	injected := errors.New("injected")
	var mu deadlock.Mutex
	started, finished := make(map[int]bool), make(map[int]bool)
	request := simulation.WithParallelGroupHook(simulation.Request{
		TxnGroups:      groups,
		ParallelGroups: true,
		MaxGroups:      uint64(n),
	}, func(g int) error {
		if g == 0 {
			return injected
		}
		mu.Lock()
		started[g] = true
		mu.Unlock()
		time.Sleep(50 * time.Millisecond)
		mu.Lock()
		finished[g] = true
		mu.Unlock()
		return nil
	})
	_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
	require.ErrorIs(t, err, injected)

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, started, finished)
	// At most the groups the other workers took before the error ran.
	require.Less(t, len(started), n-1)
}

// TestMultipleGroupsCorruptedEvaluator checks that a panic while a sequential group is committed
// fails that group, stops the later groups and discards the block, without failing the request.
func TestMultipleGroupsCorruptedEvaluator(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for target := range 3 {
		t.Run(fmt.Sprintf("group %d", target), func(t *testing.T) {
			t.Parallel()
			env := simulationtesting.PrepareSimulatorTest(t)
			defer env.Close()

			groups := make([][]transactions.SignedTxn, 3)
			for g := range groups {
				sender := env.Accounts[g]
				groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
					Type:     protocol.PaymentTx,
					Sender:   sender.Addr,
					Receiver: sender.Addr,
				}).Txn().Sign(sender.Sk)}
			}
			request := simulation.WithAfterGroupHook(simulation.Request{
				TxnGroups: groups,
				MaxGroups: 3,
			}, func(g int, evalError error) {
				// The group has been committed: the panic leaves the evaluator corrupted.
				if g == target && evalError == nil {
					panic("injected")
				}
			})
			// Not simulateGroups: when the last group is the one stopped, nothing marks the block
			// as discarded for validateSimulationResult.
			result, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
			require.NoError(t, err)
			require.Nil(t, result.Block)
			for g := range target {
				requireGroupSucceeded(t, result, g)
			}
			require.Contains(t, result.TxnGroups[target].FailureMessage, "injected")
			for g := target + 1; g < 3; g++ {
				require.Equal(t, fmt.Sprintf("not simulated: group index %d left the evaluator state corrupted", target), result.TxnGroups[g].FailureMessage)
			}
		})
	}
}

// TestParallelGroupsStress runs more parallel groups than CPUs while the ledger commits new blocks.
func TestParallelGroupsStress(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	app := env.CreateApp(env.Accounts[0].Addr, simulationtesting.AppParams{
		ApprovalProgram:   globalStateApp,
		ClearStateProgram: "#pragma version 8\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})
	start := env.TxnInfo.LatestRound()

	n := min(2*runtime.NumCPU()+1, simulation.MaxSimulateGroups)
	groups := make([][]transactions.SignedTxn, n)
	for g := range groups {
		sender := env.Accounts[g%len(env.Accounts)]
		groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
			Type:            protocol.ApplicationCallTx,
			Sender:          sender.Addr,
			ApplicationID:   app,
			ApplicationArgs: [][]byte{uint64ToBytes(uint64(g))},
			Note:            []byte{byte(g)},
		}).Txn().Sign(sender.Sk)}
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Stay within MaxAcctLookback of the start round.
		for range 3 {
			env.TransferAlgos(env.Accounts[0].Addr, env.Accounts[1].Addr, 1)
		}
	}()
	for range 3 {
		result, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{
			Round:          start,
			TxnGroups:      groups,
			ParallelGroups: true,
			MaxGroups:      uint64(n),
			TraceConfig:    simulation.ExecTraceConfig{Enable: true, State: true},
		})
		require.NoError(t, err)
		require.Equal(t, start, result.LastRound)
		for g := range groups {
			requireGroupSucceeded(t, result, g)
		}
	}
	<-done
}

// TestRequestValidate checks the round-independent request checks.
func TestRequestValidate(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	group := []transactions.SignedTxn{{}}
	groups := func(n int) [][]transactions.SignedTxn {
		return make([][]transactions.SignedTxn, n)
	}
	for _, tc := range []struct {
		name         string
		request      simulation.Request
		developerAPI bool
		err          string
	}{
		{"valid", simulation.Request{TxnGroups: [][]transactions.SignedTxn{group}}, false, ""},
		{"valid multi-group", simulation.Request{TxnGroups: groups(3), MaxGroups: 3, ParallelGroups: true}, false, ""},
		{"valid trace", simulation.Request{TxnGroups: groups(1), TraceConfig: simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true}}, true, ""},
		{"valid max extra budget", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget}, false, ""},
		{"fix signers", simulation.Request{TxnGroups: groups(1), FixSigners: true}, false, "FixSigners requires AllowEmptySignatures to be enabled"},
		{"skip validity window", simulation.Request{TxnGroups: groups(1), SkipValidityWindow: true}, false, ""},
		{"rewrite validity window", simulation.Request{TxnGroups: groups(1), RewriteValidityWindow: true, AllowEmptySignatures: true}, false, ""},
		{"both validity window modes", simulation.Request{TxnGroups: groups(1), SkipValidityWindow: true, RewriteValidityWindow: true, AllowEmptySignatures: true}, false, "skip-validity-window and rewrite-validity-window are mutually exclusive"},
		{"rewrite without empty signatures", simulation.Request{TxnGroups: groups(1), RewriteValidityWindow: true}, false, "rewrite-validity-window requires allow-empty-signatures"},
		{"no groups", simulation.Request{}, false, "expected at least 1 transaction group, got 0"},
		{"too many groups", simulation.Request{TxnGroups: groups(3), MaxGroups: 2}, false, "too many transaction groups: got 3, the limit is 2"},
		{"zero max groups", simulation.Request{TxnGroups: groups(2)}, false, "too many transaction groups: got 2, the limit is 1"},
		{"max groups clamped", simulation.Request{TxnGroups: groups(simulation.MaxSimulateGroups + 1), MaxGroups: 1000}, false, fmt.Sprintf("too many transaction groups: got %d, the limit is %d", simulation.MaxSimulateGroups+1, simulation.MaxSimulateGroups)},
		{"state proof txn", simulation.Request{TxnGroups: [][]transactions.SignedTxn{group, {{Txn: transactions.Transaction{Type: protocol.StateProofTx}}}}, MaxGroups: 2}, false, "cannot simulate StateProof transactions"},
		{"extra budget", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget + 1}, false, "extra budget 320001 > simulation extra budget limit 320000"},
		{"negative extra budget", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: -1}, false, "extra budget -1 is negative"},
		{"raised extra budget limit", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: 1_000_000, MaxExtraOpcodeBudget: 1_000_000}, false, ""},
		{"over raised extra budget limit", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: 1_000_001, MaxExtraOpcodeBudget: 1_000_000}, false, "extra budget 1000001 > simulation extra budget limit 1000000"},
		{"lowered extra budget limit", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: 101, MaxExtraOpcodeBudget: 100}, false, "extra budget 101 > simulation extra budget limit 100"},
		{"extra budget at ceiling", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudgetCeiling, MaxExtraOpcodeBudget: ^uint64(0)}, false, ""},
		{"extra budget limit clamped", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudgetCeiling + 1, MaxExtraOpcodeBudget: ^uint64(0)}, false, fmt.Sprintf("extra budget %d > simulation extra budget limit %d", simulation.MaxExtraOpcodeBudgetCeiling+1, simulation.MaxExtraOpcodeBudgetCeiling)},
		{"trace at upstream extra budget", simulation.Request{TxnGroups: groups(3), MaxGroups: 3, ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget, MaxExtraOpcodeBudget: 1_000_000, TraceConfig: simulation.ExecTraceConfig{Enable: true}}, true, ""},
		{"trace over upstream extra budget without developer API", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget + 1, MaxExtraOpcodeBudget: 1_000_000, TraceConfig: simulation.ExecTraceConfig{Enable: true}}, false, "the local configuration of the node has `EnableDeveloperAPI` turned off, while requesting for execution trace"},
		{"trace over upstream extra budget", simulation.Request{TxnGroups: groups(1), ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget + 1, MaxExtraOpcodeBudget: 1_000_000, TraceConfig: simulation.ExecTraceConfig{Enable: true}}, true, "extra budget 320001 > 320000, the limit for a request with an execution trace"},
		{"trace without developer API", simulation.Request{TxnGroups: groups(1), TraceConfig: simulation.ExecTraceConfig{Enable: true}}, false, "the local configuration of the node has `EnableDeveloperAPI` turned off, while requesting for execution trace"},
		{"stack without trace", simulation.Request{TxnGroups: groups(1), TraceConfig: simulation.ExecTraceConfig{Stack: true}}, true, "basic trace must be enabled when enabling stack tracing"},
		{"scratch without trace", simulation.Request{TxnGroups: groups(1), TraceConfig: simulation.ExecTraceConfig{Scratch: true}}, true, "basic trace must be enabled when enabling scratch slot change tracing"},
		{"state without trace", simulation.Request{TxnGroups: groups(1), TraceConfig: simulation.ExecTraceConfig{State: true}}, true, "basic trace must be enabled when enabling app state change tracing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := tc.request.Validate(tc.developerAPI)
			if tc.err == "" {
				require.NoError(t, err)
				return
			}
			var invalid simulation.InvalidRequestError
			require.ErrorAs(t, err, &invalid)
			require.EqualError(t, err, tc.err)
		})
	}
}

// TestConcurrentSimulateSharedRequest checks that simulations of one request running at once on one
// ledger, as the rounds of a REST request with "rounds" do, give the same result and leave the
// request they share unchanged, FixSigners included.
func TestConcurrentSimulateSharedRequest(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	env.TransferAlgos(a.Addr, b.Addr, 1)
	start := env.TxnInfo.LatestRound()
	env.TransferAlgos(a.Addr, b.Addr, 1)

	rekey := env.TxnInfo.NewTxn(txntest.Txn{
		Type:     protocol.PaymentTx,
		Sender:   a.Addr,
		Receiver: a.Addr,
		RekeyTo:  b.Addr,
	}).Txn().Sign(a.Sk)
	unsigned := env.TxnInfo.NewTxn(txntest.Txn{
		Type:     protocol.PaymentTx,
		Sender:   a.Addr,
		Receiver: a.Addr,
		Note:     []byte{1},
	}).SignedTxn()
	request := simulation.Request{
		TxnGroups:            [][]transactions.SignedTxn{{rekey}, {unsigned}},
		AllowEmptySignatures: true,
		FixSigners:           true,
		MaxGroups:            2,
	}
	want := [][]transactions.SignedTxn{slices.Clone(request.TxnGroups[0]), slices.Clone(request.TxnGroups[1])}

	const runs = 4
	for _, parallel := range []bool{false, true} {
		results := make([]simulation.Result, 2*runs)
		errs := make([]error, 2*runs)
		var wg sync.WaitGroup
		for i := range results {
			wg.Add(1)
			go func() {
				defer wg.Done()
				r := request
				r.ParallelGroups = parallel
				// Half of the simulations run at the start round, half at the latest round.
				if i%2 == 0 {
					r.Round = start
				}
				results[i], errs[i] = simulation.MakeSimulator(env.Ledger, false).Simulate(r)
			}()
		}
		wg.Wait()

		for i, result := range results {
			require.NoError(t, errs[i])
			for g := range result.TxnGroups {
				requireGroupSucceeded(t, result, g)
			}
			if parallel {
				require.Zero(t, result.TxnGroups[1].Txns[0].FixedSigner)
			} else {
				require.Equal(t, b.Addr, result.TxnGroups[1].Txns[0].FixedSigner)
			}
			require.Equal(t, results[i%2].TxnGroups, result.TxnGroups, "simulation %d", i)
		}
		require.Equal(t, start, results[0].LastRound)
		require.Equal(t, start+1, results[1].LastRound)
		require.Equal(t, want, request.TxnGroups)
	}
}
