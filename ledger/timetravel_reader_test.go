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
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestHistoryReader checks that a HistoryReader over the store of a closed
// ledger, opened read-only, answers every lookup as the ledger did.
func TestHistoryReader(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival())
	l := dl.validator

	asa := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x", Manager: addrs[0]}}).ApplyData.ConfigAsset
	app := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: app.Address(), Amount: 10_000_000})
	for i := 0; i < 30; i++ {
		txs := []*txntest.Txn{
			{Type: "pay", Sender: addrs[3], Receiver: addrs[4], Amount: uint64(i + 1)},
			{Type: "appl", Sender: addrs[3], ApplicationID: app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i/3))},
				Boxes: []transactions.BoxRef{{Name: []byte("b")}}},
		}
		switch i {
		case 2:
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: asa})
		case 5:
			txs = append(txs, &txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: app, OnCompletion: transactions.OptInOC,
				ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i/3))}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}})
		case 9:
			txs = append(txs, &txntest.Txn{Type: "acfg", Sender: addrs[0], ConfigAsset: asa, AssetParams: basics.AssetParams{Manager: addrs[0], Reserve: addrs[1]}})
		case 20:
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[0], AssetCloseTo: addrs[0], XferAsset: asa})
		}
		if i%4 == 1 && i > 2 && i < 20 {
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[0], AssetReceiver: addrs[1], XferAsset: asa, AssetAmount: uint64(i)})
		}
		for j := range txs {
			txs[j].Note = []byte{byte(i), byte(j)}
		}
		dl.fullBlock(txs...)
	}
	commitRoundLookback(0, l)
	last, ok := l.history.store.Last()
	require.True(t, ok)
	require.Greater(t, last, uint64(20))
	box := apps.MakeBoxKey(uint64(app), "b")

	// everything the reader answers, as the ledger answers it
	type answers struct {
		latest  map[basics.Address]basics.AccountData
		without map[basics.Address]basics.MicroAlgos
		assets  any
		apps    any
		kvs     any
		totals  any
		hdr     any
	}
	full := VersionRange{Max: basics.Round(last), Limit: 1000, Values: true}
	type versionsCase struct {
		name   string
		ledger func() (VersionPage, error)
		reader func(*HistoryReader) (VersionPage, error)
	}
	vcases := []versionsCase{
		{"account", func() (VersionPage, error) { return l.AccountVersions(addrs[1], AccountVersionsExclude{}, full) },
			func(r *HistoryReader) (VersionPage, error) {
				return r.AccountVersions(addrs[1], AccountVersionsExclude{}, full)
			}},
		{"account base", func() (VersionPage, error) {
			return l.AccountVersions(addrs[4], AccountVersionsExclude{All: true}, full)
		},
			func(r *HistoryReader) (VersionPage, error) {
				return r.AccountVersions(addrs[4], AccountVersionsExclude{All: true}, full)
			}},
		{"holding", func() (VersionPage, error) { return l.AccountAssetVersions(addrs[1], asa, full) },
			func(r *HistoryReader) (VersionPage, error) { return r.AccountAssetVersions(addrs[1], asa, full) }},
		{"local state", func() (VersionPage, error) { return l.AccountAppVersions(addrs[2], app, full) },
			func(r *HistoryReader) (VersionPage, error) { return r.AccountAppVersions(addrs[2], app, full) }},
		{"asset", func() (VersionPage, error) { return l.AssetVersions(asa, full) },
			func(r *HistoryReader) (VersionPage, error) { return r.AssetVersions(asa, full) }},
		{"app", func() (VersionPage, error) { return l.AppVersions(app, full) },
			func(r *HistoryReader) (VersionPage, error) { return r.AppVersions(app, full) }},
		{"box", func() (VersionPage, error) { return l.KvVersions(box, full) },
			func(r *HistoryReader) (VersionPage, error) { return r.KvVersions(box, full) }},
	}

	want := map[basics.Round]answers{}
	for rnd := basics.Round(0); rnd <= basics.Round(last); rnd++ {
		a := answers{latest: map[basics.Address]basics.AccountData{}, without: map[basics.Address]basics.MicroAlgos{}}
		for _, addr := range addrs[:5] {
			ad, got, without, err := l.LookupLatestAt(rnd, addr)
			require.NoError(t, err)
			require.Equal(t, rnd, got)
			a.latest[addr], a.without[addr] = ad, without
		}
		var err error
		a.assets, _, err = l.LookupAssetsAt(rnd, addrs[1], 0, 10)
		require.NoError(t, err)
		a.apps, _, err = l.LookupApplicationsAt(rnd, addrs[2], 0, 10, true)
		require.NoError(t, err)
		a.kvs, _, _, err = l.LookupKvPairsByPrefix(rnd, apps.MakeBoxKey(uint64(app), ""), "", 10, 0, true)
		require.NoError(t, err)
		a.totals, err = l.Totals(rnd)
		require.NoError(t, err)
		a.hdr, err = l.BlockHdr(rnd)
		require.NoError(t, err)
		want[rnd] = a
	}
	wantVersions := map[string]VersionPage{}
	for _, c := range vcases {
		page, err := c.ledger()
		require.NoError(t, err)
		require.NotEmpty(t, page.Versions, c.name)
		wantVersions[c.name] = page
	}
	path := l.history.path
	dl.Close()

	s, err := timetravel.Open(path, timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer s.Close()
	r, err := NewHistoryReader(s)
	require.NoError(t, err)
	require.Zero(t, r.First())
	require.EqualValues(t, last, r.Last())

	for rnd := basics.Round(0); rnd <= basics.Round(last); rnd++ {
		w := want[rnd]
		for _, addr := range addrs[:5] {
			ad, got, without, err := r.LookupLatestAt(rnd, addr)
			require.NoError(t, err)
			require.Equal(t, rnd, got)
			require.Equal(t, w.latest[addr], ad, "round %d", rnd)
			require.Equal(t, w.without[addr], without, "round %d", rnd)
			base, baseWithout, err := r.LookupAccountAt(rnd, addr)
			require.NoError(t, err)
			require.Equal(t, w.without[addr], baseWithout)
			require.Equal(t, w.latest[addr].MicroAlgos, base.MicroAlgos)
		}
		assets, _, err := r.LookupAssetsAt(rnd, addrs[1], 0, 10)
		require.NoError(t, err)
		require.Equal(t, w.assets, assets, "round %d", rnd)
		appRes, _, err := r.LookupApplicationsAt(rnd, addrs[2], 0, 10, true)
		require.NoError(t, err)
		require.Equal(t, w.apps, appRes, "round %d", rnd)
		kvs, _, err := r.LookupKvPairsAt(rnd, apps.MakeBoxKey(uint64(app), ""), "", 10, 0, true)
		require.NoError(t, err)
		require.Equal(t, w.kvs, kvs, "round %d", rnd)
		totals, err := r.Totals(rnd)
		require.NoError(t, err)
		require.Equal(t, w.totals, totals, "round %d", rnd)
		hdr, err := r.BlockHdr(rnd)
		require.NoError(t, err)
		require.Equal(t, w.hdr, hdr, "round %d", rnd)
	}
	for _, c := range vcases {
		page, err := c.reader(r)
		require.NoError(t, err)
		require.Equal(t, wantVersions[c.name], page, c.name)
	}

	// rounds the store does not hold are errors, not the last round's answers
	past := basics.Round(last) + 1
	_, _, _, err = r.LookupLatestAt(past, addrs[0])
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, _, err = r.LookupAccountAt(past, addrs[0])
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, err = r.Totals(past)
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, err = r.BlockHdr(past)
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, _, err = r.LookupKvPairsAt(past, "", "", 10, 0, false)
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, err = r.AppVersions(app, VersionRange{Max: past, Limit: 10})
	require.ErrorIs(t, err, ErrRoundOutOfRange)
	_, err = r.AppVersions(app, VersionRange{Min: 5, Max: 3, Limit: 10})
	require.ErrorContains(t, err, "empty range")
}

// TestHistProto checks that a header of an unknown protocol is an error, not
// parameters whose zero RewardUnit divides by zero.
func TestHistProto(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	_, err := histProto(bookkeeping.BlockHeader{Round: 7, UpgradeState: bookkeeping.UpgradeState{CurrentProtocol: "nope"}})
	require.ErrorContains(t, err, "round 7 has an unknown consensus protocol nope")
	proto, err := histProto(bookkeeping.BlockHeader{UpgradeState: bookkeeping.UpgradeState{CurrentProtocol: protocol.ConsensusCurrentVersion}})
	require.NoError(t, err)
	require.NotZero(t, proto.RewardUnit)
}
