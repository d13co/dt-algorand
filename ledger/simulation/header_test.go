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

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

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

// TestSimulateHeaderAfterStart checks that a simulation at a past round can't read a header after
// its start round, as on chain, even though the ledger has it: only a heartbeat can try, through
// the seed of its FirstValid.
func TestSimulateHeaderAfterStart(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	b, c := env.Accounts[1], env.Accounts[2]
	otss := keyedAccount(t, &env, c.Addr)
	addEmptyBlocks(t, &env, 30)
	start := env.TxnInfo.LatestRound() - 3

	t.Run("seed after the start round", func(t *testing.T) {
		// Valid in the block after start, but the seed of that round is refused.
		hb := heartbeat(t, env, b.Addr, c.Addr, otss, start+1, start+10).Txn().Sign(b.Sk)
		result := simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(hb)})
		require.Contains(t, result.TxnGroups[0].FailureMessage, fmt.Sprintf("round %d is after the simulation start round %d", start+1, start))
	})

	t.Run("seed of the start round", func(t *testing.T) {
		hb := heartbeat(t, env, b.Addr, c.Addr, otss, start, start+10).Txn().Sign(b.Sk)
		result := simulateGroups(t, env, simulation.Request{Round: start, TxnGroups: oneTxn(hb)})
		requireGroupSucceeded(t, result, 0)
	})
}
