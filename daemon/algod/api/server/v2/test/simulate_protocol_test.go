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

package test

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/txntest"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// proposerApp needs AVM 11: it logs the proposer of the round before the txn's FirstValid.
const proposerApp = `#pragma version 11
txn FirstValid; int 1; -; block BlkProposer; log
int 1`

// TestSimulateProtocol simulates, on a ledger whose protocol (v35) only has AVM 7, an app that
// needs AVM 11, under a later protocol.
func TestSimulateProtocol(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	sender := env.Accounts[0]
	for range 5 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	create := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   proposerApp,
		ClearStateProgram: "#pragma version 11\nint 1",
	}).Txn().Sign(sender.Sk)
	router := newRoundsRouter(t, newRoundsNode(t, env))

	wantOverride := func(t *testing.T, po *model.SimulateProtocolOverride) {
		t.Helper()
		require.NotNil(t, po)
		require.Equal(t, string(protocol.ConsensusV35), po.From)
		require.Equal(t, string(protocol.ConsensusV42), po.To)
		require.True(t, po.Applied)
		require.NotNil(t, po.Notes)
		var hits *[]model.SimulateProtocolNoteHit
		for _, n := range *po.Notes {
			require.NotEmpty(t, n.Message)
			if n.Code == "block-incentive-fields" {
				hits = n.Hits
			}
		}
		require.Equal(t, &[]model.SimulateProtocolNoteHit{{Group: 0, Path: []int{0}}}, hits)
	}

	for _, format := range []string{"json", "msgpack"} {
		request := oneGroupRequest(create)
		response := simulateOK(t, router, request, format)
		require.Contains(t, *response.TxnGroups[0].FailureMessage, "greater than protocol supported version 7")
		require.Nil(t, response.ProtocolOverride)

		request.Protocol = "v42"
		response = simulateOK(t, router, request, format)
		require.Nil(t, response.TxnGroups[0].FailureMessage, format)
		require.Equal(t, [][]byte{make([]byte, 32)}, *response.TxnGroups[0].Txns[0].Txn.Logs)
		wantOverride(t, response.ProtocolOverride)

		// Every round of rounds gets its own report.
		request.Rounds = []basics.Round{latest - 1, latest}
		response = simulateOK(t, router, request, format)
		requireRoundResultsMatch(t, router, request, response, latest-1, latest)
		require.Nil(t, response.ProtocolOverride)
		for _, result := range *response.RoundResults {
			wantOverride(t, result.ProtocolOverride)
		}
	}
}

func TestSimulateProtocolErrors(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr}).Txn().Sign(sender.Sk)
	router := newRoundsRouter(t, newRoundsNode(t, env))

	post := func(protocolName string, rounds ...basics.Round) *http.Response {
		request := oneGroupRequest(pay)
		request.Protocol = protocolName
		request.Rounds = rounds
		return postSimulate(t, router, encodeSimulateRequest(t, request, "json"), "json").Result()
	}

	// An earlier protocol of the chain is an invalid request for a single round, and that round's
	// error with rounds.
	require.Equal(t, http.StatusBadRequest, post("v34").StatusCode)
	request := oneGroupRequest(pay)
	request.Protocol = "v34"
	request.Rounds = []basics.Round{latest - 1, latest}
	response := simulateOK(t, router, request, "json")
	for _, result := range *response.RoundResults {
		require.NotNil(t, result.Error)
		require.Contains(t, *result.Error, "is not a later protocol")
	}

	// An unknown name, or vFuture without the developer API, fails the whole request.
	for _, name := range []string{"bogus", "future"} {
		require.Equal(t, http.StatusBadRequest, post(name).StatusCode, name)
		require.Equal(t, http.StatusBadRequest, post(name, latest).StatusCode, name)
	}

	// With the developer API, vFuture is allowed.
	devRouter := newRoundsRouter(t, newRoundsNode(t, env, func(cfg *config.Local) { cfg.EnableDeveloperAPI = true }))
	request = oneGroupRequest(pay)
	request.Protocol = "future"
	response = simulateOK(t, devRouter, request, "json")
	require.Equal(t, string(protocol.ConsensusFuture), response.ProtocolOverride.To)
	require.True(t, response.ProtocolOverride.Applied)

	// The protocol the round already runs is no override.
	request.Protocol = "latest"
	response = simulateOK(t, router, request, "json")
	require.Equal(t, &model.SimulateProtocolOverride{From: string(protocol.ConsensusV35), To: string(protocol.ConsensusV35)}, response.ProtocolOverride)
}
