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
	"testing"

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

// sponsorCloserApp sponsors the size of app Applications 1 with an inner update on "update", and
// closes its own account to Accounts 1 with an inner payment on "close".
const sponsorCloserApp = `#pragma version 10
txn ApplicationID; bz end
txna ApplicationArgs 0; byte "close"; ==; bnz close
itxn_begin
int appl;              itxn_field TypeEnum
txna Applications 1;   itxn_field ApplicationID
int UpdateApplication; itxn_field OnCompletion
txna Applications 1; app_params_get AppApprovalProgram;   assert; itxn_field ApprovalProgram
txna Applications 1; app_params_get AppClearStateProgram; assert; itxn_field ClearStateProgram
int 1;                 itxn_field ExtraProgramPages
itxn_submit
b end
close:
itxn_begin
int pay;               itxn_field TypeEnum
txna Accounts 1;       itxn_field Receiver
txna Accounts 1;       itxn_field CloseRemainderTo
itxn_submit
end:
int 1`

// TestSimulatePastRoundClose checks that the node's refusal to close an account whose minimum
// balance is still above the base (a policy for new blocks since 5.0.4) holds at the latest round
// only. At a past round, simulate and query apply the consensus rules alone, as the chain did: v42
// accepts the close of an app size sponsor, while CloseRequiresBaseMinBalance (v43) refuses it.
func TestSimulatePastRoundClose(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		name string
		cv   protocol.ConsensusVersion
	}{{"v42", protocol.ConsensusV42}, {"future", protocol.ConsensusFuture}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			testSimulatePastRoundClose(t, tc.cv)
		})
	}
}

func testSimulatePastRoundClose(t *testing.T, cv protocol.ConsensusVersion) {
	env := simulationtesting.PrepareSimulatorTestAt(t, cv)
	defer env.Close()
	creator, sponsor, sink := env.Accounts[0], env.Accounts[1], env.Accounts[2]
	const clear = "#pragma version 10\nint 1"

	target := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 10\nint 1",
		ClearStateProgram: clear,
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})
	// sponsor takes over the target's size, closer then takes it over for a second target.
	env.Txn(env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sponsor.Addr,
		ApplicationID:     target,
		OnCompletion:      transactions.UpdateApplicationOC,
		ApprovalProgram:   "#pragma version 10\nint 1",
		ClearStateProgram: clear,
		ExtraProgramPages: 1,
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	}).Txn().Sign(sponsor.Sk))
	target2 := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 10\nint 1",
		ClearStateProgram: clear,
	})
	closer := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram:   sponsorCloserApp,
		ClearStateProgram: clear,
	})
	env.TransferAlgos(creator.Addr, closer.Address(), 3_000_000)
	env.Txn(env.TxnInfo.NewTxn(txntest.Txn{
		Type:            protocol.ApplicationCallTx,
		Sender:          creator.Addr,
		ApplicationID:   closer,
		ApplicationArgs: [][]byte{[]byte("update")},
		ForeignApps:     []basics.AppIndex{target2},
	}).Txn().Sign(creator.Sk))

	addEmptyBlocks(t, &env, 2)
	latest := env.TxnInfo.LatestRound()
	past := latest - 1

	// Valid in the blocks after past and after latest.
	window := func(txn txntest.Txn) txntest.Txn {
		txn.FirstValid, txn.LastValid = past+1, latest+10
		return txn
	}
	pay := window(env.TxnInfo.NewTxn(txntest.Txn{
		Type:             protocol.PaymentTx,
		Sender:           sponsor.Addr,
		Receiver:         sink.Addr,
		CloseRemainderTo: sink.Addr,
	})).Txn().Sign(sponsor.Sk)
	innerPay := window(env.TxnInfo.NewTxn(txntest.Txn{
		Type:            protocol.ApplicationCallTx,
		Sender:          creator.Addr,
		ApplicationID:   closer,
		ApplicationArgs: [][]byte{[]byte("close")},
		Accounts:        []basics.Address{sink.Addr},
		Fee:             2_000,
	})).Txn().Sign(creator.Sk)

	// Where the consensus rule holds, it refuses first, at any round. Otherwise the policy refuses
	// at the latest round.
	consensus := config.Consensus[cv].CloseRequiresBaseMinBalance
	latestErr, pastErr := "cannot close: account", ""
	if consensus {
		latestErr, pastErr = "cannot close: min balance", "cannot close: min balance"
	}

	for _, tc := range []struct {
		name string
		txn  transactions.SignedTxn
	}{{"pay", pay}, {"inner pay", innerPay}} {
		t.Run(tc.name, func(t *testing.T) {
			eachGroupMode(t, func(t *testing.T, parallel bool) {
				for _, rnd := range []basics.Round{0, latest} {
					result := simulateGroups(t, env, simulation.Request{Round: rnd, TxnGroups: oneTxn(tc.txn), ParallelGroups: parallel})
					require.Contains(t, result.TxnGroups[0].FailureMessage, latestErr, "round %d", rnd)
				}
				result := simulateGroups(t, env, simulation.Request{Round: past, TxnGroups: oneTxn(tc.txn), ParallelGroups: parallel})
				if pastErr != "" {
					require.Contains(t, result.TxnGroups[0].FailureMessage, pastErr)
				} else {
					requireGroupSucceeded(t, result, 0)
				}
			})
		})
	}

	t.Run("query", func(t *testing.T) {
		result, err := simulation.MakeSimulator(env.Ledger, false).Query(simulation.QueryRequest{
			AppID:  closer,
			Calls:  []simulation.QueryCall{{Args: [][]byte{[]byte("close")}, Accounts: []basics.Address{sink.Addr}}},
			Rounds: []basics.Round{past, 0, latest},
			Limits: simulation.QueryLimitsFromConfig(env.Config),
		})
		require.NoError(t, err)
		require.Len(t, result.Rounds, 3)
		for i, rr := range result.Rounds {
			require.Empty(t, rr.Error, "round %d", rr.Round)
			require.Len(t, rr.Calls, 1)
			msg := fmt.Sprintf("round %d", rr.Round)
			want := pastErr
			if i > 0 {
				require.Equal(t, latest, rr.Round)
				want = latestErr
			}
			if want != "" {
				require.Contains(t, rr.Calls[0].Error, want, msg)
			} else {
				require.Empty(t, rr.Calls[0].Error, msg)
			}
		}
	})
}
