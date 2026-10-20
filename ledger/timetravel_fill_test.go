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

package ledger

import (
	"fmt"
	"maps"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// fillSource serves recorded deltas, and headers from a ledger.
type fillSource struct {
	deltas map[basics.Round]ledgercore.StateDelta
	l      *Ledger
}

func (f fillSource) Delta(rnd basics.Round) (*ledgercore.StateDelta, error) {
	sd := f.deltas[rnd]
	return &sd, nil
}

func (f fillSource) Header(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	return f.l.BlockHdr(rnd)
}

// TestFillTimeTravelGap runs a time travel ledger's directory without time
// travel for a while, as an algod without it would, and checks that
// FillTimeTravelGap adds the rounds its store lacks, the same as a ledger that
// kept time travel on wrote them, after refusing deltas that do not reproduce
// the stored rounds.
func TestFillTimeTravelGap(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	proto := config.Consensus[protocol.ConsensusFuture]
	require.True(t, proto.ExcludeExpiredCirculation)
	// the generator keeps the history the validator's directory should end up with
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerOnDisk())
	defer dl.Close()
	genesis, err := dl.validator.Block(0)
	require.NoError(t, err)
	initState := ledgercore.InitState{Block: genesis, Accounts: genBalances.Balances, GenesisHash: dl.validator.GenesisHash()}
	dirs := dl.validator.dirsAndPrefix
	reopen := func(cfg config.Local) (*Ledger, error) {
		return OpenLedger(logging.TestingLog(t), dirs, false, initState, cfg)
	}

	accts := make([]basics.Address, 6)
	dl.beginBlock()
	for i := range accts {
		accts[i] = ledgertesting.RandomAddress()
		dl.txn(&txntest.Txn{Type: "pay", Sender: addrs[1], Receiver: accts[i], Amount: 1000_000_000})
	}
	dl.endBlock()
	deltas := map[basics.Round]ledgercore.StateDelta{}
	expiring := 0
	blocks := func(n int) {
		for i := 0; i < n; i++ {
			r := dl.generator.Latest() + 1
			dl.beginBlock()
			a := accts[i%len(accts)]
			dl.txn(&txntest.Txn{Type: "pay", Sender: addrs[1], Receiver: a, Amount: uint64(1000 + i)})
			switch i % 20 {
			case 0: // online with keys that expire 30 to 130 rounds from now
				dl.txn(&txntest.Txn{Type: "keyreg", Sender: a, VoteFirst: r, VoteLast: r + 30 + basics.Round(i*7%100),
					VotePK: crypto.OneTimeSignatureVerifier(a), SelectionPK: crypto.VRFVerifier(a), StateProofPK: merklesignature.Commitment{1}, VoteKeyDilution: 1024})
			case 10:
				dl.txn(&txntest.Txn{Type: "keyreg", Sender: accts[(i/20+3)%len(accts)]})
			case 15:
				dl.txn(&txntest.Txn{Type: "appl", Sender: addrs[2], ApprovalProgram: `byte "x"; int 1; app_global_put; int 1`,
					GlobalStateSchema: basics.StateSchema{NumUint: 1}})
			}
			dl.endBlock()
			sd, err := dl.generator.GetStateDeltaForRound(r)
			require.NoError(t, err)
			deltas[r] = sd
			if e, err := dl.generator.acctsOnline.expiredOnlineCirculation(r, r+320); err == nil && !e.IsZero() {
				expiring++
			}
		}
	}

	blocks(200)
	commitRoundLookback(4, dl.validator)
	last := dl.validator.LatestTrackerCommitted()
	dl.validator.Close()

	// without time travel, the account DB moves on alone
	off := cfg
	off.EnableTimeTravel = false
	dl.validator, err = reopen(off)
	require.NoError(t, err)
	blocks(150)
	commitRoundLookback(4, dl.validator)
	commitRoundLookback(4, dl.generator)
	to := dl.validator.LatestTrackerCommitted()
	require.Equal(t, last+150, to)
	require.Equal(t, to, dl.generator.LatestTrackerCommitted())
	dl.validator.Close()
	dl.validator = nil
	require.Greater(t, expiring, 50, "the test must exercise expiring keys")

	_, err = reopen(cfg)
	require.ErrorContains(t, err, "ends at round")

	// deltas that do not reproduce a stored round add nothing
	src := fillSource{deltas: deltas, l: dl.generator}
	bad := fillSource{deltas: map[basics.Round]ledgercore.StateDelta{}, l: dl.generator}
	maps.Copy(bad.deltas, deltas)
	sd := bad.deltas[last-3]
	sd.Totals.RewardsLevel++
	bad.deltas[last-3] = sd
	_, _, err = FillTimeTravelGap(dirs, cfg, bad, logging.TestingLog(t), nil)
	require.ErrorContains(t, err, fmt.Sprintf("round %d: the changes from the delta differ", last-3))
	_, err = reopen(cfg)
	require.ErrorContains(t, err, fmt.Sprintf("ends at round %d ", last))

	from, got, err := FillTimeTravelGap(dirs, cfg, src, logging.TestingLog(t), nil)
	require.NoError(t, err)
	require.Equal(t, last+1, from)
	require.Equal(t, to, got)
	from, got, err = FillTimeTravelGap(dirs, cfg, src, logging.TestingLog(t), nil)
	require.NoError(t, err)
	require.Greater(t, from, got)

	dl.validator, err = reopen(cfg)
	require.NoError(t, err)
	want, have := dl.generator.history.store, dl.validator.history.store
	l, _ := have.Last()
	require.Equal(t, uint64(to), l)
	for r := uint64(0); r <= uint64(to); r++ {
		d1, ok1, err1 := want.Digest(r)
		d2, ok2, err2 := have.Digest(r)
		require.NoError(t, err1)
		require.NoError(t, err2)
		require.True(t, ok1 && ok2, "round %d", r)
		require.Equal(t, d1, d2, "round %d", r)
		h1, _, _ := want.Header(r)
		h2, _, _ := have.Header(r)
		require.Equal(t, h1, h2, "round %d", r)
		e1, ok1, _ := want.ExpiredStake(r)
		e2, ok2, _ := have.ExpiredStake(r)
		require.Equal(t, ok1, ok2, "round %d", r)
		require.Equal(t, e1, e2, "round %d", r)
	}
	// the filled rounds read like the others, from the history
	for r := last + 1; r+4 < to; r += 13 {
		for _, a := range accts {
			w, _, err := dl.generator.LookupWithoutRewards(r, a)
			require.NoError(t, err)
			h, _, err := dl.validator.LookupWithoutRewards(r, a)
			require.NoError(t, err)
			require.Equal(t, w, h, "round %d", r)
		}
		w, err := dl.generator.OnlineCirculation(r, r+320)
		require.NoError(t, err)
		h, err := dl.validator.OnlineCirculation(r, r+320)
		require.NoError(t, err)
		require.Equal(t, w, h, "round %d", r)
	}
}
