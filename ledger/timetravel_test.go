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
	"encoding/binary"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
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

func ttSnap(t *testing.T, l *Ledger, r basics.Round, addrs []basics.Address, asa basics.AssetIndex, app basics.AppIndex) ttSnapshot {
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
	s.circ, err = l.OnlineCirculation(r, r)
	require.NoError(t, err)
	return s
}

func TestTimeTravelLookups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		n        uint64
		archival bool
	}{{1, false}, {7, false}, {1, true}} {
		t.Run(fmt.Sprintf("N=%d,archival=%v", tc.n, tc.archival), func(t *testing.T) {
			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = true
			cfg.TimeTravelCheckpointInterval = tc.n
			cfg.MaxAcctLookback = 4 // the window checks below count on it
			var opts []simpleLedgerOption
			if !tc.archival {
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
			if tc.archival {
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

// TestHistViewModel checks the merged reads (store plus in-memory rounds,
// including rounds in both) against a brute-force model.
func TestHistViewModel(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	type mval struct {
		v   []byte
		del bool
	}
	for _, n := range []uint64{1, 7} {
		for seed := int64(0); seed < 30; seed++ {
			t.Run(fmt.Sprintf("n=%d/seed=%d", n, seed), func(t *testing.T) {
				rng := rand.New(rand.NewSource(seed))
				s, err := timetravel.Open(t.TempDir(), timetravel.Options{CheckpointInterval: n})
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
					require.NoError(t, s.AddRound(uint64(r), rounds[r], nil))
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
}

// ttListings are the account listings of every address at one round.
type ttListings struct {
	accts  []basics.AccountData
	assets [][]ledgercore.AssetResourceWithIDs
	apps   [][]ledgercore.AppResourceWithIDs
}

// ttList records the listings at the latest round, from the trackers.
func ttList(t *testing.T, l *Ledger, addrs []basics.Address) ttListings {
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
func ttListAt(t *testing.T, l *Ledger, r basics.Round, addrs []basics.Address) ttListings {
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
