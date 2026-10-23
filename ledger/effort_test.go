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
	"context"
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestCountedLedger checks the counts of CountedLedger: one lookup per call, history where the
// trackers no longer held the round, and the entries history scans read.
func TestCountedLedger(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
	defer dl.Close()
	l := dl.validator

	asa := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x"}}).ApplyData.ConfigAsset
	dl.txn(&txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: asa})
	app := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: app.Address(), Amount: 10_000_000})
	old := l.Latest() + 1
	for range 20 {
		r := l.Latest() + 1
		dl.fullBlock(
			&txntest.Txn{Type: "pay", Sender: addrs[3], Receiver: addrs[4], Amount: uint64(r)},
			&txntest.Txn{Type: "appl", Sender: addrs[3], ApplicationID: app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))},
				Boxes: []transactions.BoxRef{{Name: []byte("b")}}},
		)
	}
	commitRoundLookback(4, l)
	require.Less(t, old, l.trackers.getDbRound())
	_, _, err := l.accts.LookupWithoutRewards(old, addrs[4])
	require.True(t, tooOld(err), "round %d must come from history", old)

	count := func(f func(c CountedLedger)) ledgercore.EffortCounts {
		e := ledgercore.NewEffort()
		f(l.Counted(e))
		return e.Counts()
	}
	type counts struct{ lookups, history, scanned uint64 }
	of := func(c ledgercore.EffortCounts) counts { return counts{c.Lookups, c.History, c.Scanned} }

	// point lookups: history only below the trackers' window
	for _, tc := range []struct {
		rnd     basics.Round
		history uint64
	}{{old, 1}, {l.Latest(), 0}} {
		c := count(func(c CountedLedger) {
			_, _, err := c.LookupWithoutRewards(tc.rnd, addrs[4])
			require.NoError(t, err)
		})
		require.Equal(t, counts{1, tc.history, 0}, of(c), "round %d", tc.rnd)
		c = count(func(c CountedLedger) {
			_, err := c.LookupKv(tc.rnd, apps.MakeBoxKey(uint64(app), "b"))
			require.NoError(t, err)
			_, err = c.LookupApplication(tc.rnd, addrs[2], app)
			require.NoError(t, err)
			_, err = c.Totals(tc.rnd)
			require.NoError(t, err)
		})
		require.Equal(t, counts{3, 3 * tc.history, 0}, of(c), "round %d", tc.rnd)
	}

	// scans read entries
	c := count(func(c CountedLedger) {
		assets, _, err := c.LookupAssetsAt(old, addrs[1], 0, 10)
		require.NoError(t, err)
		require.Len(t, assets, 1)
	})
	require.Equal(t, uint64(2), c.Lookups, "the holding's account, and the asset's params")
	require.Equal(t, uint64(2), c.History)
	require.Positive(t, c.Scanned)
	c = count(func(c CountedLedger) {
		kvs, _, _, err := c.LookupKvPairsByPrefix(old, apps.MakeBoxKey(uint64(app), ""), "", 10, 0, true)
		require.NoError(t, err)
		require.Len(t, kvs, 1)
	})
	require.Equal(t, counts{1, 1, 1}, of(c))

	// nil counts nothing and answers as the ledger does
	_, _, err = l.Counted(nil).LookupWithoutRewards(old, addrs[4])
	require.NoError(t, err)

	// versions count the entries they read, not lookups
	e := ledgercore.NewEffort()
	page, err := l.AccountAssetVersions(addrs[1], asa, VersionRange{Max: l.Latest(), Limit: 100, Ctx: ledgercore.ContextWithEffort(context.Background(), e)})
	require.NoError(t, err)
	require.NotEmpty(t, page.Versions)
	require.Zero(t, e.Counts().Lookups)
	require.GreaterOrEqual(t, e.Counts().Scanned, uint64(len(page.Versions)))

	// a rebuilt delta counts the block and the reads of its evaluation
	e = ledgercore.NewEffort()
	_, err = l.GetStateDeltaForRoundContext(ledgercore.ContextWithEffort(context.Background(), e), old)
	require.NoError(t, err)
	require.Greater(t, e.Counts().Lookups, uint64(1))
	require.Positive(t, e.Counts().History)
	require.LessOrEqual(t, e.Counts().History, e.Counts().Lookups)
	// a recent one only its delta
	e = ledgercore.NewEffort()
	_, err = l.GetStateDeltaForRoundContext(ledgercore.ContextWithEffort(context.Background(), e), l.Latest())
	require.NoError(t, err)
	require.Equal(t, counts{1, 0, 0}, of(e.Counts()))
}
