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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/committee"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

// counterApp counts its calls in global "n" and stores arg 0 in box "b".
var counterApp = main(`
	byte "n"; byte "n"; app_global_get; int 1; +; app_global_put
	byte "b"; txn ApplicationArgs 0; box_put
`)

type ttSnapshot struct {
	acct    ledgercore.AccountData
	rewards ledgercore.AccountData
	holding ledgercore.AssetResource
	app     ledgercore.AppResource
	box     []byte
	totals  ledgercore.AccountTotals
	hdr     bookkeeping.BlockHeader
	online  basics.OnlineAccountData
	circ    basics.MicroAlgos
}

func ttSnap(t require.TestingT, l *Ledger, r basics.Round, addrs []basics.Address, asa basics.AssetIndex, app basics.AppIndex) ttSnapshot {
	var s ttSnapshot
	var err error
	s.acct, _, err = l.LookupWithoutRewards(r, addrs[4])
	require.NoError(t, err)
	s.rewards, _, _, err = l.LookupAccount(r, addrs[4])
	require.NoError(t, err)
	s.holding, err = l.LookupAsset(r, addrs[1], asa)
	require.NoError(t, err)
	s.app, err = l.LookupApplication(r, addrs[2], app)
	require.NoError(t, err)
	s.box, err = l.LookupKv(r, apps.MakeBoxKey(uint64(app), "b"))
	require.NoError(t, err)
	s.totals, err = l.Totals(r)
	require.NoError(t, err)
	s.hdr, err = l.BlockHdr(r)
	require.NoError(t, err)
	s.online, err = l.LookupAgreement(r, addrs[0])
	require.NoError(t, err)
	// the vote round that has r as its balance round
	s.circ, err = l.OnlineCirculation(r, r+agreement.BalanceLookback(config.Consensus[s.hdr.CurrentProtocol]))
	require.NoError(t, err)
	return s
}

func TestTimeTravelLookups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, archival := range []bool{false, true} {
		t.Run(fmt.Sprintf("archival=%v", archival), func(t *testing.T) {
			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = true
			cfg.MaxAcctLookback = 4 // the window checks below count on it
			var opts []simpleLedgerOption
			if !archival {
				opts = append(opts, simpleLedgerNotArchival())
			}
			dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, opts...)
			defer dl.Close()
			l := dl.validator

			asa := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x"}}).ApplyData.ConfigAsset
			dl.txn(&txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: asa})
			app := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
			dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: app.Address(), Amount: 10_000_000})

			snaps := map[basics.Round]ttSnapshot{}
			deltas := map[basics.Round]ledgercore.StateDelta{}
			first := l.Latest() + 1
			const rounds = 1100
			for i := 0; i < rounds; i++ {
				r := l.Latest() + 1
				dl.beginBlock()
				dl.txns(
					&txntest.Txn{Type: "pay", Sender: addrs[3], Receiver: addrs[4], Amount: uint64(r)},
					&txntest.Txn{Type: "axfer", Sender: addrs[0], AssetReceiver: addrs[1], XferAsset: asa, AssetAmount: uint64(r)},
					&txntest.Txn{Type: "appl", Sender: addrs[3], ApplicationID: app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))},
						Boxes: []transactions.BoxRef{{Name: []byte("b")}}},
				)
				dl.endBlock()
				snaps[r] = ttSnap(t, l, r, addrs, asa, app)
				sd, err := l.GetStateDeltaForRound(r)
				require.NoError(t, err)
				deltas[r] = sd
				if i%100 == 0 {
					commitRoundLookback(4, dl.generator)
					commitRoundLookback(4, l)
				}
			}
			commitRoundLookback(4, l)
			last, ok := l.history.store.Last()
			require.True(t, ok)
			require.GreaterOrEqual(t, basics.Round(last), l.trackers.getDbRound())

			_, err := l.Block(first)
			if archival {
				require.NoError(t, err, "archival keeps old blocks")
			} else {
				require.Error(t, err, "old blocks are pruned")
			}
			for r := first; r < l.trackers.getDbRound(); r++ {
				_, _, err := l.accts.LookupWithoutRewards(r, addrs[4])
				require.True(t, tooOld(err), "round %d must come from history", r)
				require.Equal(t, snaps[r], ttSnap(t, l, r, addrs, asa, app), "round %d", r)
			}

			// listings at an old round
			r := first + 10
			ad, rnd, _, err := l.LookupLatestAt(r, addrs[0])
			require.NoError(t, err)
			require.Equal(t, r, rnd)
			require.Contains(t, ad.AssetParams, asa)
			require.Contains(t, ad.Assets, asa)
			assets, _, err := l.LookupAssetsAt(r, addrs[1], 0, 10)
			require.NoError(t, err)
			require.Len(t, assets, 1)
			require.Equal(t, snaps[r].holding.AssetHolding, assets[0].AssetHolding)
			require.Equal(t, addrs[0], assets[0].Creator)
			appRes, _, err := l.LookupApplicationsAt(r, addrs[2], 0, 10, true)
			require.NoError(t, err)
			require.Len(t, appRes, 1)
			require.Equal(t, snaps[r].app.AppParams, appRes[0].AppParams)
			kvs, _, more, err := l.LookupKvPairsByPrefix(r, apps.MakeBoxKey(uint64(app), ""), "", 10, 0, true)
			require.NoError(t, err)
			require.False(t, more)
			require.Equal(t, []ledgercore.KvPairResult{{Key: apps.MakeBoxKey(uint64(app), "b"), Value: snaps[r].box}}, kvs)
			keys, err := l.LookupKeysByPrefix(r, apps.MakeBoxKey(uint64(app), ""), 0)
			require.NoError(t, err)
			require.Equal(t, []string{apps.MakeBoxKey(uint64(app), "b")}, keys)
			// limit 0 lists nothing, as it does in the trackers' window
			assets, _, err = l.LookupAssetsAt(r, addrs[1], 0, 0)
			require.NoError(t, err)
			require.Empty(t, assets)
			appRes, _, err = l.LookupApplicationsAt(r, addrs[2], 0, 0, true)
			require.NoError(t, err)
			require.Empty(t, appRes)
			kvs, _, more, err = l.LookupKvPairsByPrefix(r, apps.MakeBoxKey(uint64(app), ""), "", 0, 0, true)
			require.NoError(t, err)
			require.False(t, more)
			require.Empty(t, kvs)
			kvs, _, more, err = l.LookupKvPairsByPrefix(l.Latest(), apps.MakeBoxKey(uint64(app), ""), "", 0, 0, true)
			require.NoError(t, err)
			require.False(t, more)
			require.Empty(t, kvs)
			creator, ok, err := l.GetCreatorForRound(r, basics.CreatableIndex(app), basics.AppCreatable)
			require.NoError(t, err)
			require.True(t, ok)
			require.Equal(t, addrs[2], creator)
			_, ok, err = l.GetCreatorForRound(first-5, basics.CreatableIndex(app), basics.AppCreatable)
			require.NoError(t, err)
			require.False(t, ok, "app did not exist yet")

			// old deltas are re-evaluated from their block and the history
			reevaluated := 0
			for r := first; r < l.trackers.getDbRound(); r++ {
				sd, err := l.GetStateDeltaForRound(r)
				if _, berr := l.Block(r); berr != nil {
					require.ErrorContains(t, err, "block is not stored", "round %d", r)
					continue
				}
				require.NoError(t, err, "round %d", r)
				require.Equal(t, deltaJSON(t, deltas[r]), deltaJSON(t, sd), "round %d", r)
				reevaluated++
			}
			require.NotZero(t, reevaluated)

			// the check catches a delta that is not the round's
			r = first + 500
			h := l.historyAt(r)
			sd := deltas[r]
			require.NoError(t, verifyDelta(h, r, &sd))
			sd = deltas[r+1]
			require.ErrorIs(t, verifyDelta(h, r, &sd), ErrDeltaReevaluation)
			sd = deltas[r]
			sd.KvMods = nil // a write the evaluation missed
			require.ErrorIs(t, verifyDelta(h, r, &sd), ErrDeltaReevaluation)

			// rounds the store does not hold yet answer from the tracker's
			// in-memory rounds: a new asset, an opt in and a close out
			live := map[basics.Round]ttListings{}
			record := func() {
				r := l.Latest()
				live[r] = ttList(t, l, addrs)
				sd, err := l.GetStateDeltaForRound(r)
				require.NoError(t, err)
				deltas[r] = sd
			}
			asa2 := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[3], AssetParams: basics.AssetParams{Total: 10, UnitName: "y"}}).ApplyData.ConfigAsset
			record()
			dl.txn(&txntest.Txn{Type: "axfer", Sender: addrs[4], AssetReceiver: addrs[4], XferAsset: asa2})
			record()
			dl.txn(&txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[0], AssetCloseTo: addrs[0], XferAsset: asa})
			record()
			dl.fullBlock()
			record()
			checkWindow := func() {
				last, _ := l.history.store.Last()
				require.Less(t, basics.Round(last), l.Latest()-3, "the new rounds are not in the store")
				for r, want := range live {
					require.Equal(t, want, ttListAt(t, l, r, addrs), "round %d", r)
				}
			}
			checkWindow()

			// the history survives a reload
			dl.reloadLedgers()
			require.Equal(t, snaps[first], ttSnap(t, l, first, addrs, asa, app))
			checkWindow()

			// out of the trackers' window, the creation, opt in and close out
			// rounds re-evaluate to the same deltas
			for range 8 {
				dl.fullBlock()
			}
			commitRoundLookback(4, l)
			for r := range live {
				_, err := l.accts.lookupStateDelta(r)
				require.Error(t, err, "round %d left the trackers", r)
				sd, err := l.GetStateDeltaForRound(r)
				require.NoError(t, err, "round %d", r)
				require.Equal(t, deltaJSON(t, deltas[r]), deltaJSON(t, sd), "round %d", r)
			}
		})
	}
}

// TestTimeTravelOnlineCirculation checks the expired stake the history records
// for every round against onlineAccounts while the round is in its window,
// then the online stake served from the history once it left the window,
// directly and to the online_stake opcode of re-evaluated blocks. Before
// ExcludeExpiredCirculation nothing is recorded and the online stake is the
// online money. That case runs on v33: with the state proofs of v34-v37, the
// two ledgers of a DoubleLedger sometimes disagree on StateProofOnlineTotalWeight
// when the validator is read between blocks, history or not.
func TestTimeTravelOnlineCirculation(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		name string
		cv   protocol.ConsensusVersion
	}{{"v33", protocol.ConsensusV33}, {"future", protocol.ConsensusFuture}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			proto := config.Consensus[tc.cv]
			exclude := proto.ExcludeExpiredCirculation
			require.Equal(t, tc.cv == protocol.ConsensusFuture, exclude)

			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			// a genesis account whose keys expire during the test: the online set
			// loaded at genesis counts
			gen := genBalances.Balances[addrs[0]]
			gen.Status, gen.VoteLastValid = basics.Online, 400
			gen.VoteID, gen.SelectionID, gen.StateProofID = crypto.OneTimeSignatureVerifier{1}, crypto.VRFVerifier{1}, merklesignature.Commitment{1}
			genBalances.Balances[addrs[0]] = gen

			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = true
			cfg.MaxAcctLookback = 4
			dl := NewDoubleLedger(t, genBalances, tc.cv, cfg) // archival: old blocks are re-evaluated
			defer dl.Close()
			l := dl.validator
			lookback := agreement.BalanceLookback(proto)

			// stores online_stake (AVM 11) every round
			var app basics.AppIndex
			if proto.LogicSigVersion >= 11 {
				app = dl.createApp(addrs[2], main(`byte "s"; online_stake; app_global_put`), basics.StateSchema{NumUint: 1})
			}
			accts := make([]basics.Address, 6)
			dl.beginBlock()
			for i := range accts {
				accts[i] = ledgertesting.RandomAddress()
				dl.txn(&txntest.Txn{Type: "pay", Sender: addrs[1], Receiver: accts[i], Amount: 1000_000_000})
			}
			dl.endBlock()

			want := map[basics.Round]basics.MicroAlgos{}
			deltas := map[basics.Round]ledgercore.StateDelta{}
			expiring := map[basics.Round]bool{}
			for i := 0; i < 900; i++ {
				r := l.Latest() + 1
				dl.beginBlock()
				a := accts[i%len(accts)]
				dl.txn(&txntest.Txn{Type: "pay", Sender: addrs[1], Receiver: a, Amount: uint64(1000 + i)})
				switch i % 40 {
				case 0: // online with keys that expire 100 to 590 rounds from now
					a = accts[(i/40)%len(accts)]
					dl.txn(&txntest.Txn{Type: "keyreg", Sender: a, VoteFirst: r, VoteLast: r + 100 + basics.Round(i*7%500),
						VotePK: crypto.OneTimeSignatureVerifier(a), SelectionPK: crypto.VRFVerifier(a), StateProofPK: merklesignature.Commitment{1}, VoteKeyDilution: 1024})
				case 20:
					dl.txn(&txntest.Txn{Type: "keyreg", Sender: accts[(i/40+3)%len(accts)]})
				}
				if app != 0 {
					dl.txn(&txntest.Txn{Type: "appl", Sender: addrs[2], ApplicationID: app})
				}
				dl.endBlock()

				expired, err := l.acctsOnline.expiredOnlineCirculation(r, r+lookback)
				require.NoError(t, err)
				got, ok, err := l.historyAt(r).ExpiredStake(uint64(r))
				require.NoError(t, err)
				require.Equal(t, exclude, ok, "round %d", r)
				if exclude {
					require.Equal(t, expired.Raw, got, "round %d", r)
				}
				expiring[r] = !expired.IsZero()
				want[r], err = l.acctsOnline.onlineCirculation(r, r+lookback)
				require.NoError(t, err)
				deltas[r], err = l.GetStateDeltaForRound(r)
				require.NoError(t, err)

				if i%100 == 0 {
					commitRoundLookback(4, dl.generator)
					commitRoundLookback(4, l)
				}
				if i == 450 {
					dl.reloadLedgers() // the online set is loaded again, from the tracker DB
				}
			}
			n := 0
			for _, e := range expiring {
				if e {
					n++
				}
			}
			require.Greater(t, n, 300, "the test must exercise expiring keys")

			commitRoundLookback(4, l)
			old := 0
			for r, c := range want {
				if _, _, err := l.acctsOnline.onlineTotals(r); !tooOld(err) {
					continue
				}
				old++
				got, err := l.OnlineCirculation(r, r+lookback)
				require.NoError(t, err, "round %d", r)
				require.Equal(t, c, got, "round %d", r)
				totals, err := l.Totals(r)
				require.NoError(t, err)
				if !exclude {
					require.Equal(t, totals.Online.Money, got, "round %d", r)
					continue
				}

				// agreement never pairs these rounds: the history cannot answer, but
				// re-evaluating a delta falls back to the online money
				_, err = l.OnlineCirculation(r, r)
				require.ErrorIs(t, err, ErrOnlineStakeUnavailable)
				up, err := pastLedger{l, r}.OnlineCirculation(r, r)
				require.NoError(t, err)
				require.Equal(t, totals.Online.Money, up)
			}
			require.Greater(t, old, 400)

			// the online stake of balance round 0 (vote rounds up to 320) is the online money
			totals, err := l.Totals(0)
			require.NoError(t, err)
			c, err := l.OnlineCirculation(0, lookback)
			require.NoError(t, err)
			require.Equal(t, totals.Online.Money, c)

			// old blocks re-evaluate to the same deltas, with online_stake read from the history
			exact := 0
			for r, sd := range deltas {
				if _, err := l.accts.lookupStateDelta(r); err == nil {
					continue
				}
				got, err := l.GetStateDeltaForRound(r)
				require.NoError(t, err, "round %d", r)
				require.Equal(t, deltaJSON(t, sd), deltaJSON(t, got), "round %d", r)
				if r > lookback && expiring[r-lookback] {
					exact++
				}
			}
			if app != 0 {
				require.Greater(t, exact, 100, "online_stake must exclude expired stake in re-evaluated blocks")
			}
		})
	}
}

// TestHistOnlineCirculation covers the history's online stake for each kind of
// round, including one a store wrote before it recorded the expired stake.
func TestHistOnlineCirculation(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := timetravel.Open(t.TempDir(), timetravel.Options{})
	require.NoError(t, err)
	defer s.Close()
	totals := ledgercore.AccountTotals{Online: ledgercore.AlgoCount{Money: basics.MicroAlgos{Raw: 1000}}}
	expired := uint64(300)
	for r, rd := range []struct {
		cv      protocol.ConsensusVersion
		expired *uint64
	}{
		{protocol.ConsensusFuture, nil},      // genesis
		{protocol.ConsensusV37, nil},         // before ExcludeExpiredCirculation
		{protocol.ConsensusFuture, nil},      // written before the history recorded expired stake
		{protocol.ConsensusFuture, &expired}, // recorded
	} {
		hdr := bookkeeping.BlockHeader{Round: basics.Round(r), UpgradeState: bookkeeping.UpgradeState{CurrentProtocol: rd.cv}}
		require.NoError(t, s.AddRound(uint64(r), []timetravel.Change{{Key: timetravel.TotalsKey, Value: protocol.Encode(&totals)}}, protocol.Encode(&hdr), rd.expired))
	}
	h := &histView{store: s, last: 3}
	lookback := agreement.BalanceLookback(config.Consensus[protocol.ConsensusFuture])
	for _, tc := range []struct {
		rnd, voteRnd basics.Round
		want         uint64
		err          error
	}{
		{0, 7, 1000, nil},
		{1, 1 + lookback, 1000, nil},
		{2, 2 + lookback, 0, ErrOnlineStakeUnavailable},
		{3, 3 + lookback, 700, nil},
		{3, 3, 0, ErrOnlineStakeUnavailable}, // a pair agreement does not use
	} {
		c, err := histOnlineCirculation(h, tc.rnd, tc.voteRnd)
		if tc.err != nil {
			require.ErrorIs(t, err, tc.err, "%+v", tc)
			continue
		}
		require.NoError(t, err, "%+v", tc)
		require.Equal(t, tc.want, c.Raw, "%+v", tc)
	}
}

// TestTimeTravelOpenFailure checks that a store that cannot be opened fails
// OpenLedger cleanly.
func TestTimeTravelOpenFailure(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 10)
	prefix := filepath.Join(t.TempDir(), "ledger")
	require.NoError(t, os.WriteFile(prefix+".timetravel", []byte("x"), 0o644)) // a file where the store directory goes
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	var err error
	require.NotPanics(t, func() {
		_, err = OpenLedger(logging.TestingLog(t), prefix, true, genesisInitState, cfg)
	})
	require.Error(t, err)
}

// TestTimeTravelDBDir checks where the store goes, and the error when the
// config points somewhere the store is not.
func TestTimeTravelDBDir(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 10)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.BlockDBDir = t.TempDir()
	root := t.TempDir()
	open := func() (*Ledger, error) {
		dirs, err := cfg.EnsureAndResolveGenesisDirs(root, "gid", logging.TestingLog(t))
		require.NoError(t, err)
		return OpenLedger(logging.TestingLog(t), DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs}, false, genesisInitState, cfg)
	}
	inBlocks := filepath.Join(cfg.BlockDBDir, "gid", "ledger.timetravel")

	// by default the store follows BlockDBDir
	l, err := open()
	require.NoError(t, err)
	for range 20 {
		addEmptyValidatedBlock(t, l, genesisInitState.Accounts)
	}
	commitRoundLookback(0, l)
	l.Close()
	require.DirExists(t, inBlocks)
	l, err = open()
	require.NoError(t, err)
	l.Close()

	// pointing TimeTravelDBDir elsewhere without moving the store fails, naming the path
	cfg.TimeTravelDBDir = t.TempDir()
	moved := filepath.Join(cfg.TimeTravelDBDir, "gid", "ledger.timetravel")
	_, err = open()
	require.ErrorContains(t, err, moved+" is empty")
	require.ErrorContains(t, err, "if the store was moved")

	// moving it there works
	require.NoError(t, os.RemoveAll(moved))
	require.NoError(t, os.Rename(inBlocks, moved))
	l, err = open()
	require.NoError(t, err)
	l.Close()
}

// TestTimeTravelAccountsAhead checks that a time travel ledger refuses to open
// when its account DB is ahead of its block DB, instead of resetting the accounts.
func TestTimeTravelAccountsAhead(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 10)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	prefix := filepath.Join(t.TempDir(), "ledger")
	l, err := OpenLedger(logging.TestingLog(t), prefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	for range 20 {
		addEmptyValidatedBlock(t, l, genesisInitState.Accounts)
	}
	commitRoundLookback(0, l)
	require.Equal(t, basics.Round(20), l.LatestTrackerCommitted())
	l.Close()

	blocks, err := db.MakeAccessor(prefix+".block.sqlite", false, false)
	require.NoError(t, err)
	_, err = blocks.Handle.Exec("DELETE FROM blocks WHERE rnd > 10")
	require.NoError(t, err)
	blocks.Close()

	_, err = OpenLedger(logging.TestingLog(t), prefix, false, genesisInitState, cfg)
	require.ErrorContains(t, err, "the account DB is at round 20 but the block DB ends at round 10")
	accounts, err := db.MakeAccessor(prefix+".tracker.sqlite", false, false)
	require.NoError(t, err)
	defer accounts.Close()
	var rnd basics.Round
	require.NoError(t, accounts.Handle.QueryRow("SELECT rnd FROM acctrounds WHERE id = 'acctbase'").Scan(&rnd))
	require.Equal(t, basics.Round(20), rnd)
}

// TestTimeTravelReplayFlushes checks that a time travel ledger's replay
// flushes rounds that change little every timeTravelReplayMaxFlushRounds,
// busier ones every initializeCachesRoundFlushInterval, and records the same
// history either way.
func TestTimeTravelReplayFlushes(t *testing.T) {
	partitiontest.PartitionTest(t)
	// not parallel: it sets the flush variables and counts commits

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 1000)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.Archival = true // keep every block for the replays
	prefix := filepath.Join(t.TempDir(), "ledger")
	l, err := OpenLedger(logging.TestingLog(t), prefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	var addrs []basics.Address
	for addr := range genesisInitState.Accounts {
		if addr != testPoolAddr && addr != testSinkAddr {
			addrs = append(addrs, addr)
		}
	}
	// a payment a round, for changes the flushes count; the evaluator fills in its
	// rewards, and nothing checks the signature
	const rounds = 5000
	for range rounds {
		hdr, err := l.BlockHdr(l.Latest())
		require.NoError(t, err)
		eval, err := l.StartEvaluator(bookkeeping.MakeBlock(hdr).BlockHeader, 1, 0, nil)
		require.NoError(t, err)
		pay := txntest.Txn{Type: protocol.PaymentTx, Sender: addrs[0], Receiver: addrs[1], Amount: 1,
			FirstValid: l.Latest() + 1, GenesisHash: genesisInitState.GenesisHash}
		pay.FillDefaults(config.Consensus[hdr.CurrentProtocol])
		require.NoError(t, eval.TransactionGroup(pay.SignedTxn().WithAD()))
		ub, err := eval.GenerateBlock(nil)
		require.NoError(t, err)
		require.NoError(t, l.AddBlock(ub.FinishBlock(committee.Seed{1}, basics.Address{1}, false), agreement.Certificate{}))
	}
	// commit up to the round a replay ends at
	l.WaitForCommit(l.Latest())
	l.trackers.waitAccountsWriting()
	triggerTrackerFlush(t, l)
	digests := func(l *Ledger) [][]byte {
		require.Equal(t, basics.Round(rounds-cfg.MaxAcctLookback), l.LatestTrackerCommitted())
		var ds [][]byte
		for r := basics.Round(1); r <= l.LatestTrackerCommitted(); r++ {
			d, ok, err := l.historyAt(r).Digest(uint64(r))
			require.NoError(t, err)
			require.True(t, ok, "round %d", r)
			ds = append(ds, d)
		}
		return ds
	}
	want := digests(l)
	l.Close()

	defer func(r basics.Round, c, m int) {
		timeTravelReplayMaxFlushRounds, timeTravelReplayFlushChanges, timeTravelReplayFlushMaxChanges = r, c, m
	}(timeTravelReplayMaxFlushRounds, timeTravelReplayFlushChanges, timeTravelReplayFlushMaxChanges)
	timeTravelReplayMaxFlushRounds = 2000
	// little change: flushes after rounds 2001, 4002 and the last; much: every
	// 1001 rounds and the last; more than maxChanges (6 a round): every 401 rounds
	// and the last, or every 11 rounds, so that commits overlap the replay
	// throughout
	for _, tc := range []struct {
		changes, maxChanges int
		commits             uint64
	}{{math.MaxInt, math.MaxInt, 3}, {-1, math.MaxInt, 5}, {-1, 2400, 13}, {-1, 60, 455}} {
		// without its account DB and history, the ledger replays every block
		require.DirExists(t, prefix+".timetravel")
		require.NoError(t, os.RemoveAll(prefix+".timetravel"))
		for _, suffix := range []string{"", "-wal", "-shm"} {
			require.NoError(t, os.RemoveAll(prefix+".tracker.sqlite"+suffix))
		}
		timeTravelReplayFlushChanges, timeTravelReplayFlushMaxChanges = tc.changes, tc.maxChanges
		commits := ledgerCommitroundCount.GetUint64Value()
		l, err = OpenLedger(logging.TestingLog(t), prefix, false, genesisInitState, cfg)
		require.NoError(t, err)
		require.Equal(t, tc.commits, ledgerCommitroundCount.GetUint64Value()-commits, "changes %d, max %d", tc.changes, tc.maxChanges)
		require.Equal(t, want, digests(l))
		l.Close()
	}
}

// TestHistoryTrackerWriteErrors checks that the history write prepareCommit
// starts fails commitRound, also when called again as a retried transaction
// does, and that the error handler leaves no write behind and the rounds for
// the next commit.
func TestHistoryTrackerWriteErrors(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dirs := DirsAndPrefix{DBFilePrefix: "ledger"}
	dirs.TimeTravelGenesisDir = t.TempDir()
	ht, err := openHistoryTracker(dirs, config.GetDefaultLocal(), logging.TestingLog(t))
	require.NoError(t, err)
	defer ht.store.Close()
	hdr := func(r basics.Round) []byte { return protocol.Encode(&bookkeeping.BlockHeader{Round: r}) }

	// round 3 does not follow the stored round 1
	require.NoError(t, ht.store.AddRound(1, nil, hdr(1), nil))
	ht.pending = []historyRound{{rnd: 3, hdr: hdr(3)}}
	dcc := &deferredCommitContext{deferredCommitRange: deferredCommitRange{oldBase: 2, offset: 1}}
	require.NoError(t, ht.prepareCommit(dcc))
	for range 2 {
		require.ErrorContains(t, ht.commitRound(context.Background(), nil, dcc), "does not follow stored round 1")
	}
	ht.handleCommitError(dcc)
	require.Nil(t, ht.write)
	require.Len(t, ht.pending, 1)

	// once round 2 is stored, the next commit writes round 3
	require.NoError(t, ht.store.AddRound(2, nil, hdr(2), nil))
	require.NoError(t, ht.prepareCommit(dcc))
	require.NoError(t, ht.commitRound(context.Background(), nil, dcc))
	ht.postCommit(context.Background(), dcc)
	require.Nil(t, ht.write)
	require.Empty(t, ht.pending)
	last, ok := ht.store.Last()
	require.True(t, ok)
	require.EqualValues(t, 3, last)
}

// TestHistViewModel checks the merged reads (store plus in-memory rounds,
// including rounds in both) against a brute-force model.
func TestHistViewModel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	type mval struct {
		v   []byte
		del bool
	}
	for seed := int64(0); seed < 30; seed++ {
		t.Run(fmt.Sprintf("seed=%d", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			s, err := timetravel.Open(t.TempDir(), timetravel.Options{})
			require.NoError(t, err)
			defer s.Close()
			keys := make([][]byte, 12)
			for i := range keys {
				keys[i] = []byte{'h', byte(i / 4), byte(i % 4)}
			}
			var rounds [][]timetravel.Change
			total := 20 + rng.Intn(20)
			for r := 0; r < total; r++ {
				var chs []timetravel.Change
				seen := map[int]bool{}
				for j := rng.Intn(4); j > 0; j-- {
					k := rng.Intn(len(keys))
					if seen[k] {
						continue
					}
					seen[k] = true
					if rng.Intn(3) == 0 {
						chs = append(chs, timetravel.Change{Key: keys[k], Deleted: true})
					} else {
						chs = append(chs, timetravel.Change{Key: keys[k], Value: []byte{byte(r), byte(k), byte(rng.Intn(256))}})
					}
				}
				rounds = append(rounds, chs)
			}
			last := uint64(total/2 + rng.Intn(total/2))
			for r := 0; r <= int(last); r++ {
				require.NoError(t, s.AddRound(uint64(r), rounds[r], nil, nil))
			}
			// pending: some rounds <= last still present, plus (last, total)
			var pending []historyRound
			for r := max(int(last)-rng.Intn(3), 1); r < total; r++ {
				hr := historyRound{rnd: basics.Round(r), changes: rounds[r], idx: map[string]int{}}
				for i, ch := range rounds[r] {
					hr.idx[string(ch.Key)] = i
				}
				pending = append(pending, hr)
			}
			v := &histView{store: s, pending: pending, last: basics.Round(last)}
			state := func(rnd int) map[string]mval {
				m := map[string]mval{}
				for r := 0; r <= rnd; r++ {
					for _, ch := range rounds[r] {
						m[string(ch.Key)] = mval{ch.Value, ch.Deleted}
					}
				}
				return m
			}
			for rnd := 0; rnd < total; rnd++ {
				m := state(rnd)
				for _, k := range keys {
					got, ok, err := v.Get(k, uint64(rnd))
					require.NoError(t, err)
					want, wok := m[string(k)]
					require.Equal(t, wok && !want.del, ok, "get %x@%d", k, rnd)
					if ok {
						require.Equal(t, want.v, got, "get %x@%d", k, rnd)
					}
				}
				digest, ok, err := v.Digest(uint64(rnd))
				require.NoError(t, err)
				require.True(t, ok)
				require.Equal(t, timetravel.ChangesDigest(rounds[rnd]), digest)
				for _, prefix := range [][]byte{{'h'}, {'h', 1}, {'h', 2}} {
					for _, after := range [][]byte{nil, {'h', 0, 1}, {'h', 1, 2}, {'h', 2, 0}} {
						var exp []string
						for k, mv := range m {
							if !mv.del && bytes.HasPrefix([]byte(k), prefix) && (after == nil || k > string(after)) {
								exp = append(exp, k)
							}
						}
						sort.Strings(exp)
						for stop := 1; stop <= len(exp)+1; stop++ {
							var got []string
							err := v.Scan(prefix, after, uint64(rnd), func(obj, val []byte) bool {
								require.Equal(t, m[string(obj)].v, val, "scan val %x@%d", obj, rnd)
								got = append(got, string(obj))
								return len(got) < stop
							})
							require.NoError(t, err)
							want := exp[:min(stop, len(exp))]
							if len(want) == 0 {
								want = nil
							}
							require.Equal(t, want, got, "scan prefix %x after %x rnd %d stop %d last %d", prefix, after, rnd, stop, last)
						}
					}
				}
			}
		})
	}
}

// ttListings are the account listings of every address at one round.
type ttListings struct {
	accts  []basics.AccountData
	assets [][]ledgercore.AssetResourceWithIDs
	apps   [][]ledgercore.AppResourceWithIDs
}

// ttList records the listings at the latest round, from the trackers.
func ttList(t require.TestingT, l *Ledger, addrs []basics.Address) ttListings {
	var ls ttListings
	for _, a := range addrs[:5] {
		ad, _, _, err := l.LookupLatest(a)
		require.NoError(t, err)
		assets, _, err := l.LookupAssets(a, 0, 100)
		require.NoError(t, err)
		appRes, _, err := l.LookupApplications(a, 0, 100, true)
		require.NoError(t, err)
		ls.accts, ls.assets, ls.apps = append(ls.accts, ad), append(ls.assets, assets), append(ls.apps, appRes)
	}
	return ls
}

// ttListAt is ttList at round r, through the time travel lookups.
func ttListAt(t require.TestingT, l *Ledger, r basics.Round, addrs []basics.Address) ttListings {
	var ls ttListings
	for _, a := range addrs[:5] {
		ad, rnd, _, err := l.LookupLatestAt(r, a)
		require.NoError(t, err)
		require.Equal(t, r, rnd)
		assets, _, err := l.LookupAssetsAt(r, a, 0, 100)
		require.NoError(t, err)
		appRes, _, err := l.LookupApplicationsAt(r, a, 0, 100, true)
		require.NoError(t, err)
		ls.accts, ls.assets, ls.apps = append(ls.accts, ad), append(ls.assets, assets), append(ls.apps, appRes)
	}
	return ls
}

// deltaJSON is the /v2/deltas encoding of a delta, without its unexported caches.
func deltaJSON(t *testing.T, sd ledgercore.StateDelta) string {
	sd.Txleases = nil // not representable in JSON
	return string(protocol.EncodeJSONStrict(sd))
}

// TestCheckLeaseAt checks CheckLeaseAt, simulate's CheckDup in skip-validity-window mode: only
// the leases of rounds before current count, a confirmed txid is not an error, and neither is a
// lastValid below the txtail's low watermark, with or without time travel.
func TestCheckLeaseAt(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, timeTravel := range []bool{false, true} {
		t.Run(fmt.Sprintf("timeTravel=%t", timeTravel), func(t *testing.T) {
			t.Parallel()
			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = timeTravel
			dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
			defer dl.Close()
			l := dl.validator
			proto := config.Consensus[protocol.ConsensusFuture]

			lease := ledgercore.Txlease{Sender: addrs[0], Lease: [32]byte{1}}
			leased := txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: addrs[1], Amount: 1, Lease: lease.Lease}
			dl.txn(&leased)
			leasedRnd, leasedID := l.Latest(), leased.Txn().ID()
			for range 10 {
				dl.fullBlock()
			}
			// drop the txids of the rounds written so far, as notifyCommit does
			l.txTail.committedUpTo(l.Latest())

			var leaseErr *ledgercore.LeaseInLedgerError
			for _, current := range []basics.Round{leasedRnd + 1, l.Latest() + 1} {
				// the lease is found, with any window
				require.ErrorAs(t, l.CheckLeaseAt(proto, current, 1, 2, transactions.Txid{9}, lease), &leaseErr, "round %d", current)
				// the txid is no duplicate
				require.NoError(t, l.CheckLeaseAt(proto, current, leased.FirstValid, leased.LastValid, leasedID, ledgercore.Txlease{}), "round %d", current)
			}
			// before FixTransactionLeases too, whatever the window
			oldProto := proto
			oldProto.FixTransactionLeases = false
			require.ErrorAs(t, l.CheckLeaseAt(oldProto, leasedRnd+1, 500, 600, transactions.Txid{9}, lease), &leaseErr)
			l.txTail.tailMu.RLock()
			narrow := l.txTail.checkLeasePastLocked(oldProto, leasedRnd+1, 500, 600, transactions.Txid{9}, lease, false)
			l.txTail.tailMu.RUnlock()
			require.NoError(t, narrow, "the window's rounds only")
			// at leased's own round and before, its lease comes later
			require.NoError(t, l.CheckLeaseAt(proto, leasedRnd, 1, 2, transactions.Txid{9}, lease))
			require.NoError(t, l.CheckLeaseAt(proto, leasedRnd-1, 1, 2, transactions.Txid{9}, lease))
			// a lastValid below the low watermark: CheckDup's error, not CheckLeaseAt's
			var missing *errTxTailMissingRound
			require.ErrorAs(t, l.CheckDup(proto, l.Latest()+1, 1, 2, transactions.Txid{9}, ledgercore.Txlease{}), &missing)
			require.NoError(t, l.CheckLeaseAt(proto, l.Latest()+1, 1, 2, transactions.Txid{9}, ledgercore.Txlease{Sender: addrs[0], Lease: [32]byte{2}}))
		})
	}
}

// TestTimeTravelCheckDupPast checks CheckDupAt, simulate's CheckDup, at rounds
// the ledger already has: only the leases and confirmations of earlier rounds
// count, and a transaction whose txid the txtail dropped is not an error. At
// the next round it is CheckDup, which keeps its checks at every round.
func TestTimeTravelCheckDupPast(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
	defer dl.Close()
	l := dl.validator
	proto := config.Consensus[protocol.ConsensusFuture]

	lease := ledgercore.Txlease{Sender: addrs[0], Lease: [32]byte{1}}
	leased := txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: addrs[1], Amount: 1, Lease: lease.Lease}
	dl.txn(&leased)
	leasedRnd, leasedID := l.Latest(), leased.Txn().ID()
	short := txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: addrs[1], Amount: 2, FirstValid: l.Latest() + 1, LastValid: l.Latest() + 3}
	dl.txn(&short)
	shortRnd, shortID := l.Latest(), short.Txn().ID()
	for range 10 {
		dl.fullBlock()
	}
	// drop short's txid, as notifyCommit does once the blocks are written
	l.txTail.committedUpTo(l.Latest())

	var leaseErr *ledgercore.LeaseInLedgerError
	var dupErr *ledgercore.TransactionInLedgerError
	var missing *errTxTailMissingRound
	other := transactions.Txid{9}
	for _, current := range []basics.Round{leasedRnd + 1, l.Latest() + 1} {
		require.ErrorAs(t, l.CheckDupAt(proto, current, leased.FirstValid, leased.LastValid, other, lease), &leaseErr, "round %d", current)
		require.ErrorAs(t, l.CheckDupAt(proto, current, leased.FirstValid, leased.LastValid, leasedID, ledgercore.Txlease{}), &dupErr, "round %d", current)
	}
	// leased's own block: its lease and txid come later
	require.NoError(t, l.CheckDupAt(proto, leasedRnd, leased.FirstValid, leased.LastValid, other, lease))
	require.NoError(t, l.CheckDupAt(proto, leasedRnd, leased.FirstValid, leased.LastValid, leasedID, ledgercore.Txlease{}))
	// a duplicate the txtail no longer knows of goes undetected in the past
	require.NoError(t, l.CheckDupAt(proto, shortRnd+1, short.FirstValid, short.LastValid, shortID, ledgercore.Txlease{}))
	require.ErrorAs(t, l.CheckDupAt(proto, l.Latest()+1, short.FirstValid, short.LastValid, shortID, ledgercore.Txlease{}), &missing)

	// CheckDup, the transaction pool's, still sees every round it holds
	require.ErrorAs(t, l.CheckDup(proto, leasedRnd, leased.FirstValid, leased.LastValid, other, lease), &leaseErr)
	require.ErrorAs(t, l.CheckDup(proto, shortRnd+1, short.FirstValid, short.LastValid, shortID, ledgercore.Txlease{}), &missing)
}

// TestTimeTravelSkipsNoops checks that the history stores no record that
// repeats the one before: blocks write the totals and the rewards pool every
// round, changed or not.
func TestTimeTravelSkipsNoops(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 10)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	l, err := OpenLedger(logging.TestingLog(t), filepath.Join(t.TempDir(), "ledger"), false, genesisInitState, cfg)
	require.NoError(t, err)
	defer l.Close()
	const rounds = 20
	for range rounds {
		addEmptyValidatedBlock(t, l, genesisInitState.Accounts)
	}
	commitRoundLookback(0, l)
	require.Equal(t, basics.Round(rounds), l.LatestTrackerCommitted())

	hdr, err := l.BlockHdr(0)
	require.NoError(t, err)
	s := l.history.store
	for _, obj := range [][]byte{timetravel.TotalsKey, timetravel.AccountKey(hdr.RewardsPool)} {
		var records, versions int
		lo := append([]byte{'c'}, obj...)
		it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: append(lo, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff)})
		require.NoError(t, err)
		for it.First(); it.Valid(); it.Next() {
			records++
		}
		require.NoError(t, it.Close())
		require.NoError(t, s.Versions(obj, 0, rounds, false, func(uint64, []byte, bool) bool {
			versions++
			return true
		}))
		require.Equal(t, versions, records, "%x", obj)
		require.Less(t, records, rounds, "%x changes in some rounds only", obj)
	}
}
