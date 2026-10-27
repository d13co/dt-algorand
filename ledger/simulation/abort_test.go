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
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// loopApp loops until it runs out of budget when called, and approves its creation.
const loopApp = `#pragma version 8
txn ApplicationID
bz ok
loop:
int 1
bnz loop
ok:
int 1`

// loopGroups returns n groups that each call a new loopApp.
func loopGroups(t *testing.T, env simulationtesting.Environment, n int) [][]transactions.SignedTxn {
	sender := env.Accounts[0]
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   loopApp,
		ClearStateProgram: "#pragma version 8\nint 1",
	})
	groups := make([][]transactions.SignedTxn, n)
	for g := range groups {
		groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
			Type:          protocol.ApplicationCallTx,
			Sender:        sender.Addr,
			ApplicationID: app,
			Note:          []byte{byte(g)},
		}).Txn().Sign(sender.Sk)}
	}
	return groups
}

// loopRequest returns a request whose groups would each loop for a long time: at the budget
// ceiling, minutes.
func loopRequest(groups [][]transactions.SignedTxn, parallel bool) simulation.Request {
	return simulation.Request{
		TxnGroups:            groups,
		ParallelGroups:       parallel,
		ExtraOpcodeBudget:    simulation.MaxExtraOpcodeBudgetCeiling,
		MaxExtraOpcodeBudget: simulation.MaxExtraOpcodeBudgetCeiling,
		MaxGroups:            uint64(len(groups)),
	}
}

// TestSimulateCanceledBeforeStart checks that a request whose context is already done simulates
// nothing.
func TestSimulateCanceledBeforeStart(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var groupsRun atomic.Int32
		request := simulation.WithAfterGroupHook(loopRequest(loopGroups(t, env, 2), parallel), func(int, error) {
			groupsRun.Add(1)
		}).WithContext(ctx)
		_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
		require.ErrorIs(t, err, context.Canceled)
		require.Zero(t, groupsRun.Load())
	})
}

// TestSimulateCanceledMidProgram checks that a canceled request stops in the middle of a long
// program, and that its later groups don't run.
func TestSimulateCanceledMidProgram(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, deadline := range []bool{false, true} {
		t.Run(fmt.Sprintf("deadline=%t", deadline), func(t *testing.T) {
			t.Parallel()
			testSimulateCanceledMidProgram(t, deadline)
		})
	}
}

func testSimulateCanceledMidProgram(t *testing.T, deadline bool) {
	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		var groupsRun atomic.Int32
		request := simulation.WithAfterGroupHook(loopRequest(loopGroups(t, env, 3), parallel), func(int, error) {
			groupsRun.Add(1)
		})

		const wait = 100 * time.Millisecond
		start := time.Now()
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), wait)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
			time.AfterFunc(wait, cancel)
		}
		defer cancel()
		request = request.WithContext(ctx)
		_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
		if deadline {
			require.ErrorIs(t, err, context.DeadlineExceeded)
		} else {
			require.ErrorIs(t, err, context.Canceled)
		}
		// It ran until canceled, then stopped soon after.
		require.GreaterOrEqual(t, time.Since(start), wait)
		require.Less(t, time.Since(start), 10*time.Second)
		if !parallel {
			// Only the first group started.
			require.Equal(t, int32(1), groupsRun.Load())
		}

		if parallel {
			waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Minute)
			defer waitCancel()
			require.NoError(t, simulation.AcquireAllParallelGroupSlots(waitCtx))
		}
	})
}

// TestSimulateFinishedAsCanceled checks that a simulation whose context ends after its last group
// ran keeps its result.
func TestSimulateFinishedAsCanceled(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		groups := make([][]transactions.SignedTxn, 2)
		if parallel {
			// one group, so that no other group is left to start once it ends
			groups = groups[:1]
		}
		sender := env.Accounts[0]
		for g := range groups {
			groups[g] = []transactions.SignedTxn{env.TxnInfo.NewTxn(txntest.Txn{
				Type:     protocol.PaymentTx,
				Sender:   sender.Addr,
				Receiver: env.Accounts[1].Addr,
				Amount:   1000,
				Note:     []byte{byte(g)},
			}).Txn().Sign(sender.Sk)}
		}
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		request := simulation.WithAfterGroupHook(loopRequest(groups, parallel), func(g int, _ error) {
			if g == len(groups)-1 || parallel {
				cancel()
			}
		}).WithContext(ctx)
		result, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request)
		require.NoError(t, err)
		require.Error(t, ctx.Err())
		require.Len(t, result.TxnGroups, len(groups))
		for g, group := range result.TxnGroups {
			require.Empty(t, group.FailureMessage, "group %d", g)
		}
		if !parallel {
			require.NotNil(t, result.Block)
		}
	})
}
