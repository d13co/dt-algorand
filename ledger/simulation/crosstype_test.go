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

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulatePastRoundCrossType checks that the refusal of a txn that looks up an app as an asset
// (a policy for new blocks since 5.1.1, like the close check of TestSimulatePastRoundClose) holds at
// the latest round only. At a past round, the consensus rules alone apply, and they accept it.
func TestSimulatePastRoundCrossType(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV42)
	defer env.Close()
	creator, holder := env.Accounts[0], env.Accounts[1]
	const clear = "#pragma version 10\nint 1"

	// holder opts into target, then asks about its holding of an asset with target's ID.
	target := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 10\nint 1",
		ClearStateProgram: clear,
	})
	env.OptIntoApp(holder.Addr, target)
	asker := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 10\ntxn ApplicationID; bz end\ntxn Sender; txna Assets 0; asset_holding_get AssetBalance; pop; pop\nend:\nint 1",
		ClearStateProgram: clear,
	})

	addEmptyBlocks(t, &env, 2)
	latest := env.TxnInfo.LatestRound()
	past := latest - 1

	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:          protocol.ApplicationCallTx,
		Sender:        holder.Addr,
		ApplicationID: asker,
		ForeignAssets: []basics.AssetIndex{basics.AssetIndex(target)},
	})
	txn.FirstValid, txn.LastValid = past+1, latest+10
	stxn := txn.Txn().Sign(holder.Sk)
	groups := [][]transactions.SignedTxn{{stxn}}

	for _, rnd := range []basics.Round{0, latest} {
		result := simulateGroups(t, env, simulation.Request{Round: rnd, TxnGroups: groups})
		require.Contains(t, result.TxnGroups[0].FailureMessage, "as an asset", "round %d", rnd)
	}
	result := simulateGroups(t, env, simulation.Request{Round: past, TxnGroups: groups})
	requireGroupSucceeded(t, result, 0)
}
