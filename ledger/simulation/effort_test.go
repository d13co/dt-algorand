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
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulateEffort checks that a simulation counts in the Effort its context carries the budget
// its programs consumed, as the result reports it, and its ledger reads.
func TestSimulateEffort(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	bothModes(t, func(t *testing.T, parallel bool) {
		env := simulationtesting.PrepareSimulatorTest(t)
		defer env.Close()

		request := loopRequest(loopGroups(t, env, 3), parallel)
		request.ExtraOpcodeBudget = 10_000
		e := ledgercore.NewEffort()
		result, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request.WithContext(ledgercore.ContextWithEffort(context.Background(), e)))
		require.NoError(t, err)

		var consumed uint64
		for _, g := range result.TxnGroups {
			for _, txn := range g.Txns {
				consumed += uint64(txn.AppBudgetConsumed + txn.LogicSigBudgetConsumed)
			}
		}
		require.Greater(t, consumed, uint64(3*10_000))
		c := e.Counts()
		require.Equal(t, consumed, c.Opcodes)
		require.Positive(t, c.Lookups)
		require.Zero(t, c.History, "no time travel")
		require.Zero(t, c.Scanned)
	})
}

// TestSimulateEffortAborted checks that an aborted simulation counts what its programs ran.
func TestSimulateEffortAborted(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()

	// build the groups first: creating the app adds a block, which mustn't eat into the time limit
	request := loopRequest(loopGroups(t, env, 1), false)
	e := ledgercore.NewEffort()
	ctx, cancel := context.WithTimeout(ledgercore.ContextWithEffort(context.Background(), e), 50*time.Millisecond)
	defer cancel()
	_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(request.WithContext(ctx))
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.Positive(t, e.Counts().Opcodes, "the loop ran until it was stopped")
}
