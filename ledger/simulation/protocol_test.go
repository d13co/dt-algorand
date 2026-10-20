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

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// newerAVMApp is an app that needs AVM 11: it reads the proposer of the round before the txn's
// FirstValid, the sender's incentive eligibility and the online stake, and logs them.
const newerAVMApp = `#pragma version 11
txn FirstValid; int 1; -; block BlkProposer; log
txn Sender; acct_params_get AcctIncentiveEligible; assert; itob; log
online_stake; itob; log
int 1`

// newerAVMLsig is a logic signature that needs AVM 11 and reads the proposer of the round before
// the txn's FirstValid.
const newerAVMLsig = `#pragma version 11
txn FirstValid; int 1; -; block BlkProposer; global ZeroAddress; ==`

func assemble(t *testing.T, source string) []byte {
	t.Helper()
	ops, err := logic.AssembleString(source)
	require.NoError(t, err, ops.Errors)
	return ops.Program
}

// protocolOverrideGroups returns two groups: an app creation running newerAVMApp, and a payment
// from the newerAVMLsig escrow, funded first.
func protocolOverrideGroups(t *testing.T, env *simulationtesting.Environment) [][]transactions.SignedTxn {
	sender := env.Accounts[0]
	// A few blocks, so the round before FirstValid exists.
	for range 5 {
		env.TransferAlgos(sender.Addr, env.Accounts[1].Addr, 1000)
	}
	create := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   newerAVMApp,
		ClearStateProgram: "#pragma version 11\nint 1",
	}).SignedTxn()
	create = create.Txn.Sign(sender.Sk)

	lsig := assemble(t, newerAVMLsig)
	escrow := basics.Address(logic.HashProgram(lsig))
	fund := env.TxnInfo.NewTxn(txntest.Txn{
		Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: escrow, Amount: 1_000_000,
	})
	spend := env.TxnInfo.NewTxn(txntest.Txn{
		Type: protocol.PaymentTx, Sender: escrow, Receiver: sender.Addr, Amount: 1000,
	})
	txntest.Group(&fund, &spend)
	signedSpend := spend.SignedTxn()
	signedSpend.Lsig = transactions.LogicSig{Logic: lsig}
	return [][]transactions.SignedTxn{
		{create},
		{fund.Txn().Sign(sender.Sk), signedSpend},
	}
}

func noteCodes(po *simulation.ProtocolOverride) []string {
	codes := make([]string, len(po.Notes))
	for i, n := range po.Notes {
		codes[i] = n.Code
	}
	return codes
}

func findNote(t *testing.T, po *simulation.ProtocolOverride, code string) simulation.ProtocolNote {
	t.Helper()
	for _, n := range po.Notes {
		if n.Code == code {
			return n
		}
	}
	require.Failf(t, "note missing", "no note %s in %v", code, noteCodes(po))
	return simulation.ProtocolNote{}
}

// TestSimulateProtocolOverride runs programs that need AVM 11 on a ledger whose protocol (v35)
// only has AVM 7, under a later protocol.
func TestSimulateProtocolOverride(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	groups := protocolOverrideGroups(t, &env)
	s := simulation.MakeSimulator(env.Ledger, true)

	// Without the override the ledger's protocol rejects both programs.
	result, err := s.Simulate(simulation.Request{TxnGroups: groups, MaxGroups: 16})
	require.NoError(t, err)
	require.Nil(t, result.ProtocolOverride)
	require.Contains(t, result.TxnGroups[0].FailureMessage, "greater than protocol supported version 7")
	require.Contains(t, result.TxnGroups[1].FailureMessage, "LogicSig.Logic version too new")

	for _, parallel := range []bool{false, true} {
		for _, target := range []string{"v42", "future"} {
			result, err := s.Simulate(simulation.Request{TxnGroups: groups, MaxGroups: 16, Protocol: target, ParallelGroups: parallel})
			require.NoError(t, err)
			for g := range groups {
				require.Empty(t, result.TxnGroups[g].FailureMessage, "%s parallel=%v group %d", target, parallel, g)
			}

			po := result.ProtocolOverride
			require.NotNil(t, po)
			require.Equal(t, protocol.ConsensusV35, po.From)
			wantTo := protocol.ConsensusVersion(target)
			if target == "v42" {
				wantTo = protocol.ConsensusV42
			}
			require.Equal(t, wantTo, po.To)
			require.True(t, po.Applied)

			// The round before the txns ran v35, without incentives: the app and the logic
			// signature read a zero proposer, and nobody is eligible.
			logs := result.TxnGroups[0].Txns[0].Txn.EvalDelta.Logs
			require.Len(t, logs, 3)
			require.Equal(t, string(make([]byte, 32)), logs[0])
			require.Equal(t, string(make([]byte, 8)), logs[1])

			require.Equal(t, []string{"semantics-changed", "no-heartbeat-challenges", "no-load-history",
				"block-incentive-fields", "account-incentive-fields"}, noteCodes(po), target)
			require.Contains(t, findNote(t, po, "semantics-changed").Message, "LogicSigVersion: 7 → ")
			require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}, {Group: 1, Path: simulation.TxnPath{1}}},
				findNote(t, po, "block-incentive-fields").Hits)
			require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}},
				findNote(t, po, "account-incentive-fields").Hits)

			if !parallel {
				// The simulated block runs the target, as the first block after an upgrade.
				hdr := result.Block.Block().BlockHeader
				require.Equal(t, wantTo, hdr.CurrentProtocol)
				require.Equal(t, basics.MicroAlgos{Raw: config.Consensus[wantTo].Bonus.BaseAmount}, hdr.Bonus)
			}
		}
	}
}

func TestSimulateProtocolOverrideRejected(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	sender := env.Accounts[0]
	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr}).Txn().Sign(sender.Sk)
	groups := [][]transactions.SignedTxn{{pay}}

	// An earlier protocol of the chain
	_, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{TxnGroups: groups, Protocol: "v34"})
	require.ErrorAs(t, err, &simulation.InvalidRequestError{})
	require.ErrorContains(t, err, "is not a later protocol")

	// Another chain's protocol
	_, err = simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{TxnGroups: groups, Protocol: "fnet1"})
	require.ErrorAs(t, err, &simulation.InvalidRequestError{})

	// vFuture needs the developer API.
	_, err = simulation.MakeSimulator(env.Ledger, false).Simulate(simulation.Request{TxnGroups: groups, Protocol: "future"})
	require.ErrorAs(t, err, &simulation.InvalidRequestError{})

	_, err = simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{TxnGroups: groups, Protocol: "nope"})
	require.ErrorAs(t, err, &simulation.InvalidRequestError{})
}

// TestSimulateProtocolOverrideNoop asks for the protocol the round already runs: the simulation is
// the one without the override, reported as not applied.
func TestSimulateProtocolOverrideNoop(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, proto := range []protocol.ConsensusVersion{protocol.ConsensusV35, protocol.ConsensusFuture} {
		env := simulationtesting.PrepareSimulatorTestAt(t, proto)
		sender := env.Accounts[0]
		pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr}).Txn().Sign(sender.Sk)
		groups := [][]transactions.SignedTxn{{pay}}

		for _, name := range []string{simulation.ProtocolLatest, string(proto)} {
			result, err := simulation.MakeSimulator(env.Ledger, true).Simulate(simulation.Request{TxnGroups: groups, Protocol: name})
			require.NoError(t, err)
			require.Empty(t, result.TxnGroups[0].FailureMessage)
			require.Equal(t, &simulation.ProtocolOverride{From: proto, To: proto}, result.ProtocolOverride, name)
			require.Equal(t, proto, result.Block.Block().CurrentProtocol)
		}
		env.Close()
	}
}

// TestSimulateProtocolOverrideTimeTravel runs a program that needs AVM 11 at a round whose blocks
// and tracker state are long gone, on a ledger that only has AVM 7.
func TestSimulateProtocolOverrideTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender, other := env.Accounts[0], env.Accounts[1]

	first := env.TxnInfo.LatestRound() + 1
	for i := 0; i < 1100; i++ {
		env.TransferAlgos(sender.Addr, other.Addr, uint64(1000+i))
	}
	_, err := env.Ledger.Block(first)
	require.Error(t, err, "block %d should be pruned", first)

	r := first + 10
	hdr, err := env.Ledger.BlockHdr(r)
	require.NoError(t, err)
	create := txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   newerAVMApp,
		ClearStateProgram: "#pragma version 11\nint 1",
		FirstValid:        r,
		LastValid:         r + 10,
		GenesisHash:       hdr.GenesisHash,
	}
	create.FillDefaults(config.Consensus[protocol.ConsensusV35])
	groups := [][]transactions.SignedTxn{{create.Txn().Sign(sender.Sk)}}

	s := simulation.MakeSimulator(env.Ledger, false)
	result, err := s.Simulate(simulation.Request{Round: r, TxnGroups: groups})
	require.NoError(t, err)
	require.Contains(t, result.TxnGroups[0].FailureMessage, "greater than protocol supported version 7")

	result, err = s.Simulate(simulation.Request{Round: r, TxnGroups: groups, Protocol: "v42"})
	require.NoError(t, err)
	require.Empty(t, result.TxnGroups[0].FailureMessage)
	require.Equal(t, r, result.LastRound)
	require.Equal(t, protocol.ConsensusV42, result.Block.Block().CurrentProtocol)
	require.True(t, result.ProtocolOverride.Applied)
	require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}},
		findNote(t, result.ProtocolOverride, "block-incentive-fields").Hits)

	// The block opcode read the real header of r-1 from history, unchanged by the override.
	prev, err := env.Ledger.BlockHdr(r - 1)
	require.NoError(t, err)
	require.Equal(t, crypto.Digest{}, crypto.Digest(prev.Proposer))
}

// TestSimulateProtocolOverrideHits checks the notes of every kind of read, in an app call and in
// an inner one, on a v35 ledger old enough for online_stake to look 320 rounds back.
func TestSimulateProtocolOverrideHits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	sender := env.Accounts[0]
	for range 330 {
		env.TransferAlgos(sender.Addr, env.Accounts[1].Addr, 1000)
	}

	// Group index 0 creates the reader app, which reads its caller's incentive eligibility; group
	// index 1 creates an app that reads the other fields and calls the reader in an inner txn.
	reader := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   "#pragma version 13\ntxn Sender; acct_params_get AcctIncentiveEligible; pop; pop; int 1",
		ClearStateProgram: "#pragma version 13\nint 1",
	})
	caller := env.TxnInfo.NewTxn(txntest.Txn{
		Type:   protocol.ApplicationCallTx,
		Sender: sender.Addr,
		ApprovalProgram: `#pragma version 13
online_stake; pop
txn FirstValid; int 1; -; block BlkBranch512; pop
gtxn 0 CreatedApplicationID; app_params_get AppVersion; assert; pop
itxn_begin
int appl; itxn_field TypeEnum
gtxn 0 CreatedApplicationID; itxn_field ApplicationID
int 0; itxn_field Fee
itxn_submit
int 1`,
		ClearStateProgram: "#pragma version 13\nint 1",
		Fee:               3000,
	})
	group := txntest.Group(&reader, &caller)
	for i := range group {
		group[i] = group[i].Txn.Sign(sender.Sk)
	}

	s := simulation.MakeSimulator(env.Ledger, true)
	result, err := s.Simulate(simulation.Request{TxnGroups: [][]transactions.SignedTxn{group}, Protocol: "v42"})
	require.NoError(t, err)
	require.Empty(t, result.TxnGroups[0].FailureMessage)

	po := result.ProtocolOverride
	require.Equal(t, []string{"semantics-changed", "no-heartbeat-challenges", "no-load-history",
		"block-hash-fields", "online-stake-includes-expired", "account-incentive-fields", "app-version-starts-at-upgrade"}, noteCodes(po))
	hit := func(path ...int) simulation.NoteHit { return simulation.NoteHit{Group: 0, Path: path} }
	require.Equal(t, []simulation.NoteHit{hit(1)}, findNote(t, po, "block-hash-fields").Hits)
	require.Equal(t, []simulation.NoteHit{hit(1)}, findNote(t, po, "online-stake-includes-expired").Hits)
	require.Equal(t, []simulation.NoteHit{hit(0), hit(1, 0)}, findNote(t, po, "account-incentive-fields").Hits)
	require.Equal(t, []simulation.NoteHit{hit(1)}, findNote(t, po, "app-version-starts-at-upgrade").Hits)
}

// TestSimulateProtocolOverrideRewriteWindow combines the override with rewrite-validity-window: a
// txn built for a later round runs at an earlier one, under a later protocol, and reads the header
// of the start round.
func TestSimulateProtocolOverrideRewriteWindow(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTestAt(t, protocol.ConsensusV35)
	defer env.Close()
	sender := env.Accounts[0]
	for range 10 {
		env.TransferAlgos(sender.Addr, env.Accounts[1].Addr, 1000)
	}
	start := env.TxnInfo.LatestRound() - 8

	// Built at the latest round, so its window starts after start+1.
	create := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   newerAVMApp,
		ClearStateProgram: "#pragma version 11\nint 1",
	})
	require.Greater(t, create.FirstValid, start+1)
	groups := [][]transactions.SignedTxn{{create.SignedTxn()}}

	s := simulation.MakeSimulator(env.Ledger, true)
	result, err := s.Simulate(simulation.Request{
		Round: start, TxnGroups: groups, Protocol: "v42",
		RewriteValidityWindow: true, AllowEmptySignatures: true,
	})
	require.NoError(t, err)
	require.Empty(t, result.TxnGroups[0].FailureMessage)
	require.Equal(t, start+1, result.TxnGroups[0].Txns[0].Txn.Txn.FirstValid)
	require.Equal(t, protocol.ConsensusV42, result.Block.Block().CurrentProtocol)
	require.True(t, result.ProtocolOverride.Applied)
	require.Equal(t, []simulation.NoteHit{{Group: 0, Path: simulation.TxnPath{0}}},
		findNote(t, result.ProtocolOverride, "block-incentive-fields").Hits)
}
