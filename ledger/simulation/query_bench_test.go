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
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
)

// balanceGhost reads the balance, minimum balance and asset count of each of accounts, 127 being
// the most one simulate txn may read without naming them, and logs one entry per account, as
// ghostkit's acctBalanceData does.
func balanceGhost(accounts []basics.Address) string {
	var b strings.Builder
	b.WriteString("#pragma version 10\n")
	for _, addr := range accounts {
		fmt.Fprintf(&b, "addr %[1]s; balance; itob; addr %[1]s; min_balance; itob; concat; addr %[1]s; acct_params_get AcctTotalAssets; pop; itob; concat; log\n", addr)
	}
	b.WriteString("int 1\n")
	return b.String()
}

// BenchmarkQueryVsSimulate runs 16 balanceGhost calls per op: through Simulate, as clients do (an
// unsigned app create from the fee sink per call, with unnamed resources, more logging and extra
// budget), and through Query.
func BenchmarkQueryVsSimulate(b *testing.B) {
	env := simulationtesting.PrepareSimulatorTest(b)
	defer env.Close()
	// 127 funded accounts, distinct so that every read is one.
	accounts := make([]basics.Address, 127)
	for i := range accounts {
		crypto.RandBytes(accounts[i][:])
		env.TransferAlgos(env.Accounts[i%len(env.Accounts)].Addr, accounts[i], 100_000)
	}
	const calls = 16
	source := balanceGhost(accounts)
	program, err := logic.AssembleString(source)
	require.NoError(b, err)
	s := simulation.MakeSimulator(env.Ledger, false)

	txn := func(i int) txntest.Txn {
		return env.TxnInfo.NewTxn(txntest.Txn{
			Type:              protocol.ApplicationCallTx,
			Sender:            env.FeeSinkAccount.Addr,
			Fee:               1_000_000,
			ApprovalProgram:   source,
			ClearStateProgram: "#pragma version 10\nint 1",
			ExtraProgramPages: 3,
			ApplicationArgs:   [][]byte{{byte(i)}},
		})
	}
	simRequest := func(parallel bool) simulation.Request {
		r := simulation.Request{
			AllowEmptySignatures:  true,
			AllowUnnamedResources: true,
			AllowMoreLogging:      true,
			ExtraOpcodeBudget:     20_000 * calls,
			ParallelGroups:        parallel,
			MaxGroups:             calls,
		}
		if parallel {
			for i := range calls {
				t := txn(i)
				r.TxnGroups = append(r.TxnGroups, []transactions.SignedTxn{t.SignedTxn()})
			}
		} else {
			txns := make([]*txntest.Txn, calls)
			for i := range txns {
				t := txn(i)
				txns[i] = &t
			}
			txntest.Group(txns...)
			group := make([]transactions.SignedTxn, calls)
			for i, t := range txns {
				group[i] = t.SignedTxn()
			}
			r.TxnGroups = [][]transactions.SignedTxn{group}
		}
		return r
	}
	queryRequest := func(parallelism int) simulation.QueryRequest {
		r := simulation.QueryRequest{
			Program: program.Program,
			Limits:  simulation.QueryLimitsFromConfig(env.Config),
		}
		r.Limits.Parallelism = parallelism
		for i := range calls {
			r.Calls = append(r.Calls, simulation.QueryCall{Args: [][]byte{{byte(i)}}})
		}
		return r
	}

	// Both return the same logs.
	simResult, err := s.Simulate(simRequest(true))
	require.NoError(b, err)
	queryResult, err := s.Query(queryRequest(1))
	require.NoError(b, err)
	for i := range calls {
		require.Empty(b, simResult.TxnGroups[i].FailureMessage)
		require.Empty(b, queryResult.Rounds[0].Calls[i].Error)
		require.Equal(b, simResult.TxnGroups[i].Txns[0].Txn.ApplyData.EvalDelta.Logs, queryResult.Rounds[0].Calls[i].Logs)
	}

	for _, parallel := range []bool{false, true} {
		name := "simulate/one-group"
		if parallel {
			name = "simulate/parallel-groups"
		}
		request := simRequest(parallel)
		b.Run(name, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				result, err := s.Simulate(request)
				if err != nil || result.TxnGroups[len(result.TxnGroups)-1].FailureMessage != "" {
					b.Fatal(err, result.TxnGroups[0].FailureMessage)
				}
			}
		})
	}
	for _, parallelism := range []int{1, 4} {
		request := queryRequest(parallelism)
		b.Run(fmt.Sprintf("query/parallelism-%d", parallelism), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				result, err := s.Query(request)
				if err != nil || result.Rounds[0].Calls[calls-1].Error != "" {
					b.Fatal(err)
				}
			}
		})
	}
}
