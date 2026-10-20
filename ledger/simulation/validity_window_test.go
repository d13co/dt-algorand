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
	"encoding/binary"
	"fmt"
	"slices"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// addEmptyBlocks adds n empty blocks to env's ledger. Their seeds are not zero, so heartbeats can
// use them.
func addEmptyBlocks(t *testing.T, env *simulationtesting.Environment, n int) {
	t.Helper()
	for range n {
		hdr := env.TxnInfo.LatestHeader
		blk := bookkeeping.MakeBlock(hdr)
		blk.TxnCounter = hdr.TxnCounter
		binary.BigEndian.PutUint64(blk.BlockHeader.Seed[:], uint64(blk.Round()))
		blk.BlockHeader.Seed[31] = 1
		require.NoError(t, env.Ledger.AddBlock(blk, agreement.Certificate{}))
		env.TxnInfo.LatestHeader = blk.BlockHeader
	}
}

// windowLoggerApp, run as an app create, logs txn FirstValid and txn LastValid.
const windowLoggerApp = `#pragma version 8
txn FirstValid
itob
log
txn LastValid
itob
log
int 1`

// blockReaderApp, run as an app create, reads the header of round FirstValid-1, with block and
// with txn FirstValidTime, and logs that round.
const blockReaderApp = `#pragma version 8
txn FirstValid
int 1
-
dup
block BlkTimestamp
pop
itob
log
txn FirstValidTime
pop
int 1`

// appCreate returns an app create from sender running approval, with window fv..lv.
func appCreate(env simulationtesting.Environment, sender basics.Address, approval string, fv, lv basics.Round, note byte) txntest.Txn {
	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender,
		ApprovalProgram:   approval,
		ClearStateProgram: "#pragma version 8\nint 1",
		Note:              []byte{note},
	})
	txn.FirstValid, txn.LastValid = fv, lv
	return txn
}

// payTxn returns a payment of amount from sender to receiver, with window fv..lv.
func payTxn(env simulationtesting.Environment, sender, receiver basics.Address, amount uint64, fv, lv basics.Round) txntest.Txn {
	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:     protocol.PaymentTx,
		Sender:   sender,
		Receiver: receiver,
		Amount:   amount,
	})
	txn.FirstValid, txn.LastValid = fv, lv
	return txn
}

// eachGroupMode runs f once with sequential groups and once with parallel groups, as subtests
// that share the caller's environment (bothModes' parallel subtests would outlive it).
func eachGroupMode(t *testing.T, f func(t *testing.T, parallel bool)) {
	t.Helper()
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%t", parallel), func(t *testing.T) { f(t, parallel) })
	}
}

// windowLogs returns the first valid and last valid that windowLoggerApp logged in txn i of
// group g.
func windowLogs(t *testing.T, result simulation.Result, g, i int) (basics.Round, basics.Round) {
	t.Helper()
	requireGroupSucceeded(t, result, g)
	logs := result.TxnGroups[g].Txns[i].Txn.EvalDelta.Logs
	require.Len(t, logs, 2)
	return basics.Round(binary.BigEndian.Uint64([]byte(logs[0]))), basics.Round(binary.BigEndian.Uint64([]byte(logs[1])))
}

func oneTxn(stxn transactions.SignedTxn) [][]transactions.SignedTxn {
	return [][]transactions.SignedTxn{{stxn}}
}

// TestSkipValidityWindow checks skip mode: txns outside their window run as sent, and the checks
// other than the round range still apply.
func TestSkipValidityWindow(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	addEmptyBlocks(t, &env, 30)
	latest := env.TxnInfo.LatestRound()

	old := appCreate(env, a.Addr, windowLoggerApp, 1, 11, 1).Txn().Sign(a.Sk)

	t.Run("dead without the flag", func(t *testing.T) {
		result := simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(old)})
		require.Contains(t, result.TxnGroups[0].FailureMessage, "txn dead")
	})

	eachGroupMode(t, func(t *testing.T, parallel bool) {
		result := simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(old), SkipValidityWindow: true, ParallelGroups: parallel})
		fv, lv := windowLogs(t, result, 0, 0)
		require.Equal(t, basics.Round(1), fv)
		require.Equal(t, basics.Round(11), lv)
		require.True(t, result.EvalOverrides.SkipValidityWindow)
		require.Equal(t, old, result.TxnGroups[0].Txns[0].Txn.SignedTxn)
	})

	t.Run("wrong genesis hash", func(t *testing.T) {
		txn := appCreate(env, a.Addr, windowLoggerApp, 1, 11, 2)
		txn.GenesisHash[0] ^= 1
		result := simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(txn.Txn().Sign(a.Sk)), SkipValidityWindow: true})
		require.Contains(t, result.TxnGroups[0].FailureMessage, "does not match expected")
	})

	t.Run("in-block duplicates", func(t *testing.T) {
		eachGroupMode(t, func(t *testing.T, parallel bool) {
			result := simulateGroups(t, env, simulation.Request{TxnGroups: [][]transactions.SignedTxn{{old}, {old}}, SkipValidityWindow: true, ParallelGroups: parallel})
			requireGroupSucceeded(t, result, 0)
			if parallel {
				requireGroupSucceeded(t, result, 1)
			} else {
				require.Contains(t, result.TxnGroups[1].FailureMessage, "already in ledger")
			}
		})
	})

	t.Run("in-block lease with an old last valid", func(t *testing.T) {
		lease := [32]byte{7}
		first := payTxn(env, a.Addr, b.Addr, 1, 1, 11)
		first.Lease = lease
		second := payTxn(env, a.Addr, b.Addr, 2, 1, 11)
		second.Lease = lease
		result := simulateGroups(t, env, simulation.Request{
			TxnGroups:          [][]transactions.SignedTxn{{first.Txn().Sign(a.Sk)}, {second.Txn().Sign(a.Sk)}},
			SkipValidityWindow: true,
		})
		requireGroupSucceeded(t, result, 0)
		require.Contains(t, result.TxnGroups[1].FailureMessage, "lease")
	})

	t.Run("future window", func(t *testing.T) {
		// The window lies after the start round, which the ledger has: its headers are refused.
		start := latest - 3
		txn := appCreate(env, a.Addr, blockReaderApp, latest, latest+10, 3).Txn().Sign(a.Sk)
		result := simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(txn)})
		require.Contains(t, result.TxnGroups[0].FailureMessage, "txn dead")
		result = simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(txn), SkipValidityWindow: true})
		require.Contains(t, result.TxnGroups[0].FailureMessage, fmt.Sprintf("round %d is after the simulation start round %d", latest-1, start))
	})
}

// TestSkipValidityWindowLedgerDups checks skip mode against the txns the ledger has confirmed: a
// txid is no longer a duplicate, a live lease still is.
func TestSkipValidityWindowLedgerDups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	addEmptyBlocks(t, &env, 5)
	latest := env.TxnInfo.LatestRound()

	confirmed := payTxn(env, a.Addr, b.Addr, 1, latest, latest+10).Txn().Sign(a.Sk)
	env.Txn(confirmed)
	leased := payTxn(env, a.Addr, b.Addr, 2, latest, latest+10)
	leased.Lease = [32]byte{9}
	env.Txn(leased.Txn().Sign(a.Sk))

	result := simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(confirmed)})
	require.Contains(t, result.TxnGroups[0].FailureMessage, "already in ledger")
	result = simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(confirmed), SkipValidityWindow: true})
	requireGroupSucceeded(t, result, 0)

	// Another txn with the confirmed lease, and an old window, still fails.
	other := payTxn(env, a.Addr, b.Addr, 3, 1, 11)
	other.Lease = leased.Lease
	result = simulateGroups(t, env, simulation.Request{TxnGroups: oneTxn(other.Txn().Sign(a.Sk)), SkipValidityWindow: true})
	require.Contains(t, result.TxnGroups[0].FailureMessage, "lease")
}

// TestRewriteValidityWindow checks rewrite mode: windows move to the simulated block, signatures
// over the txn are dropped, and the normal checks apply to the rewritten txns.
func TestRewriteValidityWindow(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	addEmptyBlocks(t, &env, 30)
	latest := env.TxnInfo.LatestRound()
	next := latest + 1
	rewrite := func(groups [][]transactions.SignedTxn) simulation.Request {
		return simulation.Request{TxnGroups: groups, RewriteValidityWindow: true, AllowEmptySignatures: true}
	}

	eachGroupMode(t, func(t *testing.T, parallel bool) {
		stxn := appCreate(env, a.Addr, windowLoggerApp, 1, 11, 1).Txn().Sign(a.Sk)
		request := rewrite(oneTxn(stxn))
		request.ParallelGroups = parallel
		result := simulateGroups(t, env, request)
		fv, lv := windowLogs(t, result, 0, 0)
		require.Equal(t, next, fv)
		require.Equal(t, next+10, lv)
		require.True(t, result.EvalOverrides.RewriteValidityWindow)
		echoed := result.TxnGroups[0].Txns[0].Txn.SignedTxn
		require.True(t, echoed.Sig.Blank())
		require.Equal(t, next, echoed.Txn.FirstValid)
		require.Equal(t, next+10, echoed.Txn.LastValid)
	})

	t.Run("block reads before the simulated round", func(t *testing.T) {
		stxn := appCreate(env, a.Addr, blockReaderApp, 1, 11, 2).Txn().Sign(a.Sk)
		result := simulateGroups(t, env, rewrite(oneTxn(stxn)))
		require.Equal(t, latest, basics.Round(uintLog(t, result, 0)))
	})

	t.Run("already at the simulated round", func(t *testing.T) {
		// The window does not move, so the txn and its signature are kept.
		stxn := payTxn(env, a.Addr, b.Addr, 1, next, next+10).Txn().Sign(a.Sk)
		result := simulateGroups(t, env, rewrite(oneTxn(stxn)))
		requireGroupSucceeded(t, result, 0)
		require.Equal(t, stxn, result.TxnGroups[0].Txns[0].Txn.SignedTxn)
	})

	t.Run("group", func(t *testing.T) {
		call := appCreate(env, a.Addr, windowLoggerApp, 1, 11, 3)
		pay := payTxn(env, b.Addr, a.Addr, 1, 2, 20)
		group := txntest.Group(&call, &pay)
		oldGroup := group[0].Txn.Group
		group[0] = group[0].Txn.Sign(a.Sk) // pay stays unsigned
		result := simulateGroups(t, env, rewrite([][]transactions.SignedTxn{group}))
		requireGroupSucceeded(t, result, 0)
		txns := result.TxnGroups[0].Txns
		newGroup := txns[0].Txn.Txn.Group
		require.NotEqual(t, oldGroup, newGroup)
		require.Equal(t, newGroup, txns[1].Txn.Txn.Group)
		require.Equal(t, next+18, txns[1].Txn.Txn.LastValid)

		// A group whose ID does not match its txns still fails.
		// A lone group with such an error fails the request.
		bad := slices.Clone(group)
		bad[1].Txn.Amount.Raw++
		_, err := simulation.MakeSimulator(env.Ledger, false).Simulate(rewrite([][]transactions.SignedTxn{bad}))
		require.ErrorContains(t, err, "incomplete group")
	})

	t.Run("malformed window", func(t *testing.T) {
		stxn := payTxn(env, a.Addr, b.Addr, 1, 20, 10).SignedTxn()
		result := simulateGroups(t, env, rewrite(oneTxn(stxn)))
		require.Contains(t, result.TxnGroups[0].FailureMessage, "transaction invalid range")
		require.Equal(t, basics.Round(20), result.TxnGroups[0].Txns[0].Txn.Txn.FirstValid)
	})

	t.Run("logic sigs", func(t *testing.T) {
		program := logic.MustAssemble("#pragma version 8\nint 1")
		escrow := basics.Address(logic.HashProgram(program))
		env.TransferAlgos(a.Addr, escrow, 1_000_000)

		// a 1-of-1 multisig account that delegates to program with LMsig
		msigAddr, err := crypto.MultisigAddrGen(1, 1, []crypto.PublicKey{crypto.PublicKey(a.Sk.SignatureVerifier)})
		require.NoError(t, err)
		lmsig, err := crypto.MultisigSign(logic.MultisigProgram{Addr: msigAddr, Program: program}, msigAddr, 1, 1, []crypto.PublicKey{crypto.PublicKey(a.Sk.SignatureVerifier)}, *a.Sk)
		require.NoError(t, err)
		env.TransferAlgos(a.Addr, basics.Address(msigAddr), 1_000_000)
		latest := env.TxnInfo.LatestRound()

		escrowPay := payTxn(env, escrow, b.Addr, 1, 1, 11)
		delegated := payTxn(env, basics.Address(msigAddr), b.Addr, 1, 1, 11)
		// and a txn signed by the multisig itself, whose msig is dropped
		msigPay := payTxn(env, basics.Address(msigAddr), b.Addr, 2, 1, 11)
		msig, err := crypto.MultisigSign(msigPay.Txn(), msigAddr, 1, 1, []crypto.PublicKey{crypto.PublicKey(a.Sk.SignatureVerifier)}, *a.Sk)
		require.NoError(t, err)
		groups := [][]transactions.SignedTxn{
			{{Txn: escrowPay.Txn(), Lsig: transactions.LogicSig{Logic: program}}},
			{{Txn: delegated.Txn(), Lsig: transactions.LogicSig{Logic: program, LMsig: lmsig}}},
			{{Txn: msigPay.Txn(), Msig: msig}},
		}
		request := rewrite(groups)
		request.MaxGroups = 3
		result := simulateGroups(t, env, request)
		for g := range groups {
			requireGroupSucceeded(t, result, g)
			require.Equal(t, groups[g][0].Lsig, result.TxnGroups[g].Txns[0].Txn.Lsig)
			require.Equal(t, latest+1, result.TxnGroups[g].Txns[0].Txn.Txn.FirstValid)
		}
		require.True(t, result.TxnGroups[2].Txns[0].Txn.Msig.Blank())
	})

	t.Run("fix signers", func(t *testing.T) {
		// a's second txn is signed by a, but group 0 rekeys a to b: FixSigners replaces the signer.
		fresh, freshSk := newTestAccount(0x31)
		env.TransferAlgos(a.Addr, fresh, 10_000_000)
		rekey := payTxn(env, fresh, fresh, 0, 1, 11)
		rekey.RekeyTo = b.Addr
		spend := payTxn(env, fresh, a.Addr, 1, 1, 11)
		request := rewrite([][]transactions.SignedTxn{{rekey.Txn().Sign(freshSk)}, {spend.Txn().Sign(freshSk)}})
		request.FixSigners = true
		result := simulateGroups(t, env, request)
		requireGroupSucceeded(t, result, 0)
		requireGroupSucceeded(t, result, 1)
		require.Equal(t, b.Addr, result.TxnGroups[1].Txns[0].FixedSigner)
	})
}

// TestRewriteValidityWindowPQ checks that a rewritten PQ-signed txn keeps its PQ fee contribution.
func TestRewriteValidityWindowPQ(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]

	var seed crypto.FalconSeed
	seed[0] = 0x42
	signer, err := crypto.GenerateFalconSigner(seed)
	require.NoError(t, err)
	publicKey := slices.Clone(signer.PublicKey[:])
	salt, pqAddr, err := basics.CanonicalPQAddressSalt(protocol.PQSchemeFalcon1024, publicKey)
	require.NoError(t, err)
	env.TransferAlgos(a.Addr, pqAddr, 10_000_000)
	addEmptyBlocks(t, &env, 20)
	proto := env.TxnInfo.CurrentProtocolParams()

	pqSigned := func(fee uint64) transactions.SignedTxn {
		txn := payTxn(env, pqAddr, b.Addr, 1, 1, 11)
		txn.Fee = basics.MicroAlgos{Raw: fee}
		sig, err := signer.Sign(txn.Txn())
		require.NoError(t, err)
		return transactions.SignedTxn{Txn: txn.Txn(), PQsig: transactions.PQSig{Scheme: protocol.PQSchemeFalcon1024, Salt: salt, PublicKey: publicKey, Signature: sig}}
	}
	request := func(stxn transactions.SignedTxn) simulation.Request {
		return simulation.Request{TxnGroups: oneTxn(stxn), RewriteValidityWindow: true, AllowEmptySignatures: true}
	}
	contribution := proto.PQSchemeFeeContribution(protocol.PQSchemeFalcon1024)
	require.NotZero(t, contribution)
	usage := basics.AddSaturate(basics.Micros(1e6), contribution)
	paid := proto.MinTxnFee * uint64(usage) / 1e6

	result := simulateGroups(t, env, request(pqSigned(paid)))
	requireGroupSucceeded(t, result, 0)
	require.Equal(t, usage, result.TxnGroups[0].GroupUsage)
	echoed := result.TxnGroups[0].Txns[0].Txn.PQsig
	require.Empty(t, echoed.Signature)
	require.Equal(t, publicKey, echoed.PublicKey)

	result = simulateGroups(t, env, request(pqSigned(proto.MinTxnFee)))
	require.Contains(t, result.TxnGroups[0].FailureMessage, "fee")
}

// keyedAccount registers fresh participation keys for acct, so that heartbeats can be made for it.
func keyedAccount(t *testing.T, env *simulationtesting.Environment, acct basics.Address) *crypto.OneTimeSignatureSecrets {
	t.Helper()
	otss := crypto.GenerateOneTimeSignatureSecrets(0, 2)
	keyreg := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.KeyRegistrationTx, Sender: acct, VoteFirst: 0, VoteLast: 100_000,
		VotePK: otss.OneTimeSignatureVerifier, SelectionPK: crypto.VRFVerifier{1}, StateProofPK: merklesignature.Commitment{1}, VoteKeyDilution: heartbeatKeyDilution})
	env.Txn(keyreg.SignedTxn())
	return otss
}

const heartbeatKeyDilution = 1000

// heartbeat returns a heartbeat for acct, sent by sender, with window fv..lv. It reads round fv's
// seed from the ledger, which must have it.
func heartbeat(t *testing.T, env simulationtesting.Environment, sender, acct basics.Address, otss *crypto.OneTimeSignatureSecrets, fv, lv basics.Round) txntest.Txn {
	t.Helper()
	hdr, err := env.Ledger.BlockHdr(fv)
	require.NoError(t, err)
	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:          protocol.HeartbeatTx,
		Sender:        sender,
		HbAddress:     acct,
		HbProof:       otss.Sign(basics.OneTimeIDForRound(lv, heartbeatKeyDilution), hdr.Seed).ToHeartbeatProof(),
		HbSeed:        hdr.Seed,
		HbVoteID:      otss.OneTimeSignatureVerifier,
		HbKeyDilution: heartbeatKeyDilution,
	})
	txn.FirstValid, txn.LastValid = fv, lv
	return txn
}

// TestRewriteValidityWindowHeartbeat checks that heartbeats keep their window, and that their
// seed lookup can't read past the start round.
func TestRewriteValidityWindowHeartbeat(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b, c := env.Accounts[0], env.Accounts[1], env.Accounts[2]
	otss := keyedAccount(t, &env, c.Addr)
	addEmptyBlocks(t, &env, 30)
	latest := env.TxnInfo.LatestRound()

	t.Run("grouped with a payment", func(t *testing.T) {
		hb := heartbeat(t, env, b.Addr, c.Addr, otss, latest-1, latest+5)
		pay := payTxn(env, a.Addr, b.Addr, 1, 1, 11)
		group := txntest.Group(&hb, &pay)
		oldGroup := group[0].Txn.Group
		group[0] = group[0].Txn.Sign(b.Sk)
		group[1] = group[1].Txn.Sign(a.Sk)
		result := simulateGroups(t, env, simulation.Request{TxnGroups: [][]transactions.SignedTxn{group}, RewriteValidityWindow: true, AllowEmptySignatures: true})
		requireGroupSucceeded(t, result, 0)
		txns := result.TxnGroups[0].Txns
		require.Equal(t, latest-1, txns[0].Txn.Txn.FirstValid)
		require.Equal(t, latest+5, txns[0].Txn.Txn.LastValid)
		require.Equal(t, latest+1, txns[1].Txn.Txn.FirstValid)
		require.NotEqual(t, oldGroup, txns[0].Txn.Txn.Group)
		require.Equal(t, txns[0].Txn.Txn.Group, txns[1].Txn.Txn.Group)
		require.True(t, txns[0].Txn.Sig.Blank(), "the heartbeat's signature covered its old group")
	})

	t.Run("seed after the start round", func(t *testing.T) {
		// A heartbeat is valid in the block after start, but the seed of that round is refused.
		start := latest - 3
		hb := heartbeat(t, env, b.Addr, c.Addr, otss, start+1, start+10).Txn().Sign(b.Sk)
		result := simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(hb)})
		requireGroupSucceeded(t, result, 0)
		result = simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(hb), RewriteValidityWindow: true, AllowEmptySignatures: true})
		require.Contains(t, result.TxnGroups[0].FailureMessage, fmt.Sprintf("round %d is after the simulation start round %d", start+1, start))
		// A lone heartbeat is left unchanged, signature included.
		require.Equal(t, hb, result.TxnGroups[0].Txns[0].Txn.SignedTxn)
	})
}

// TestValidityWindowConcurrent simulates one request at several rounds at once, as a REST request
// with rounds does, in both modes and both group modes: the request is left unchanged, and every
// simulation of a round gives the same result. Run it with -race.
func TestValidityWindowConcurrent(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	addEmptyBlocks(t, &env, 30)
	latest := env.TxnInfo.LatestRound()

	call := appCreate(env, a.Addr, windowLoggerApp, 1, 11, 1)
	pay := payTxn(env, b.Addr, a.Addr, 1, 2, 20)
	pay.Note = []byte("note")
	group := txntest.Group(&call, &pay)
	group[0] = group[0].Txn.Sign(a.Sk)
	group[1] = group[1].Txn.Sign(b.Sk)
	lone := payTxn(env, a.Addr, b.Addr, 2, 3, 13).Txn().Sign(a.Sk)
	groups := [][]transactions.SignedTxn{group, {lone}}
	encode := func(groups [][]transactions.SignedTxn) [][]byte {
		var out [][]byte
		for _, g := range groups {
			for i := range g {
				out = append(out, protocol.Encode(&g[i]))
			}
		}
		return out
	}
	want := encode(groups)

	rounds := []basics.Round{latest, latest - 1, latest - 2}
	for _, skip := range []bool{false, true} {
		for _, parallel := range []bool{false, true} {
			t.Run(fmt.Sprintf("skip=%t/parallel=%t", skip, parallel), func(t *testing.T) {
				request := simulation.Request{TxnGroups: groups, ParallelGroups: parallel, MaxGroups: 2, AllowEmptySignatures: true,
					SkipValidityWindow: skip, RewriteValidityWindow: !skip}
				const runs = 2
				results := make([]simulation.Result, runs*len(rounds))
				errs := make([]error, len(results))
				var wg sync.WaitGroup
				for i := range results {
					wg.Add(1)
					go func() {
						defer wg.Done()
						r := request
						r.Round = rounds[i%len(rounds)]
						results[i], errs[i] = simulation.MakeSimulator(env.Ledger, false).Simulate(r)
					}()
				}
				wg.Wait()
				for i, result := range results {
					require.NoError(t, errs[i])
					for g := range result.TxnGroups {
						requireGroupSucceeded(t, result, g)
					}
					rnd := rounds[i%len(rounds)]
					fv, _ := windowLogs(t, result, 0, 0)
					if skip {
						require.Equal(t, basics.Round(1), fv)
					} else {
						require.Equal(t, rnd+1, fv)
					}
					require.Equal(t, results[i%len(rounds)].TxnGroups, result.TxnGroups, "simulation %d", i)
				}
				require.Equal(t, want, encode(groups))
			})
		}
	}
}
