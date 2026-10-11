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
	"testing"

	"github.com/algorand/avm-abi/apps"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulateTimeTravel simulates at a round whose blocks and tracker state
// are long gone, touching an account and a box the transaction does not
// reference (unnamed resources).
func TestSimulateTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender, other := env.Accounts[0], env.Accounts[1]

	// logs global n, box b and other's balance, then bumps n and stores arg 0 in b
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: fmt.Sprintf(`#pragma version 10
txn ApplicationID
bz done
byte "n"; app_global_get; itob; log
byte "b"; box_get; pop; log
addr %s; balance; itob; log
byte "n"; byte "n"; app_global_get; int 1; +; app_global_put
byte "b"; txn ApplicationArgs 0; box_put
done: int 1`, other.Addr),
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})
	env.TransferAlgos(sender.Addr, app.Address(), 10_000_000)

	type state struct{ n, box, bal []byte }
	states := map[basics.Round]state{}
	first := env.TxnInfo.LatestRound() + 1
	for i := 0; i < 1300; i++ {
		if i%2 == 0 {
			r := env.TxnInfo.LatestRound() + 1
			txn := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
				ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))},
				Accounts:        []basics.Address{other.Addr}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}})
			env.Txn(txn.SignedTxn())
		} else {
			env.TransferAlgos(sender.Addr, other.Addr, uint64(1000+i))
		}
		r := env.TxnInfo.LatestRound()
		res, err := env.Ledger.LookupApplication(r, sender.Addr, app)
		require.NoError(t, err)
		box, err := env.Ledger.LookupKv(r, apps.MakeBoxKey(uint64(app), "b"))
		require.NoError(t, err)
		acct, _, _, err := env.Ledger.LookupAccount(r, other.Addr)
		require.NoError(t, err)
		states[r] = state{
			n:   binary.BigEndian.AppendUint64(nil, res.AppParams.GlobalState["n"].Uint),
			box: box,
			bal: binary.BigEndian.AppendUint64(nil, acct.MicroAlgos.Raw),
		}
	}

	s := simulation.MakeSimulator(env.Ledger, false)
	for _, r := range []basics.Round{first, first + 1, first + 150, env.TxnInfo.LatestRound()} {
		if r < env.TxnInfo.LatestRound() {
			_, err := env.Ledger.Block(r)
			require.Error(t, err, "block %d should be pruned", r)
		}
		txn := txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
			ApplicationArgs: [][]byte{{1, 2, 3, 4, 5, 6, 7, 8}},
			FirstValid:      r, LastValid: r + 10, GenesisHash: env.TxnInfo.LatestHeader.GenesisHash}
		txn.FillDefaults(env.TxnInfo.CurrentProtocolParams())
		result, err := s.Simulate(simulation.Request{
			Round:                 r,
			TxnGroups:             [][]transactions.SignedTxn{{txn.Txn().Sign(sender.Sk)}},
			AllowUnnamedResources: true,
		})
		require.NoError(t, err)
		require.Empty(t, result.TxnGroups[0].FailureMessage, "round %d", r)
		require.Equal(t, r, result.Block.Block().Round()-1)
		want := states[r]
		require.Equal(t, []string{string(want.n), string(want.box), string(want.bal)}, result.TxnGroups[0].Txns[0].Txn.ApplyData.EvalDelta.Logs, "round %d", r)
		require.NotNil(t, result.TxnGroups[0].UnnamedResourcesAccessed, "round %d", r)
	}
}

// TestSimulateTimeTravelOnlineStake runs online_stake at rounds the trackers no
// longer hold: it must see the online stake they gave while they held it,
// which excludes the stake of keys that expire before the vote round.
func TestSimulateTimeTravelOnlineStake(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender, other := env.Accounts[0], env.Accounts[1]
	proto := env.TxnInfo.CurrentProtocolParams()

	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   "#pragma version 11\ntxn ApplicationID\nbz done\nonline_stake\nitob\nlog\ndone: int 1",
		ClearStateProgram: "#pragma version 11\nint 1",
	})
	// other's keys expire at first+300, so vote rounds after that whose
	// balance round has other online exclude its stake
	first := env.TxnInfo.LatestRound() + 1
	keyreg := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.KeyRegistrationTx, Sender: other.Addr, VoteFirst: first, VoteLast: first + 300,
		VotePK: crypto.OneTimeSignatureVerifier(other.Addr), SelectionPK: crypto.VRFVerifier(other.Addr), StateProofPK: merklesignature.Commitment{1}, VoteKeyDilution: 1024})
	env.Txn(keyreg.SignedTxn())

	// the online stake the block after r reads, from the trackers
	stakes := map[basics.Round]basics.MicroAlgos{}
	for i := 0; i < 1400; i++ {
		env.TransferAlgos(sender.Addr, other.Addr, uint64(1000+i))
		r := env.TxnInfo.LatestRound()
		stake, err := env.Ledger.OnlineCirculation(agreement.BalanceRound(r+1, proto), r+1)
		require.NoError(t, err)
		stakes[r] = stake
	}

	// first+200's balance round is 0; the last round is within MaxTxnLife of
	// the tip, with a LastValid whose txids the txtail has already dropped
	s := simulation.MakeSimulator(env.Ledger, false)
	for _, r := range []basics.Round{first + 200, first + 350, env.TxnInfo.LatestRound() - 20} {
		txn := txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
			FirstValid: r, LastValid: r + 10, GenesisHash: env.TxnInfo.LatestHeader.GenesisHash}
		txn.FillDefaults(proto)
		result, err := s.Simulate(simulation.Request{
			Round:     r,
			TxnGroups: [][]transactions.SignedTxn{{txn.Txn().Sign(sender.Sk)}},
		})
		require.NoError(t, err)
		require.Empty(t, result.TxnGroups[0].FailureMessage, "round %d", r)
		require.Equal(t, []string{string(binary.BigEndian.AppendUint64(nil, stakes[r].Raw))}, result.TxnGroups[0].Txns[0].Txn.ApplyData.EvalDelta.Logs, "round %d", r)
	}
	totals, err := env.Ledger.Totals(agreement.BalanceRound(first+351, proto))
	require.NoError(t, err)
	require.Less(t, stakes[first+350].Raw, totals.Online.Money.Raw, "other's expiring stake must be excluded")
}
