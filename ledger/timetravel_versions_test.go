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
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

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

// ttObj is one object as the lookups at a round see it, for building the
// versions a lookup must return.
type ttObj struct {
	get func(r basics.Round) (val any, ok bool)
	// put adds the object's entry to the version of its round
	put func(v *Version, val any, ok, values bool)
}

// ttVersions is the oracle: the rounds at which any of objs differs from the
// round before, with the entries of the objects that do.
func ttVersions(t *testing.T, l *Ledger, objs []ttObj, values bool) []Version {
	var out []Version
	for r := basics.Round(0); r <= l.Latest(); r++ {
		var v *Version
		for _, o := range objs {
			val, ok := o.get(r)
			changed := ok
			if r > 0 {
				pv, pok := o.get(r - 1)
				changed = ok != pok || (ok && !reflect.DeepEqual(val, pv))
			}
			if !changed {
				continue
			}
			if v == nil {
				hdr, err := l.BlockHdr(r)
				require.NoError(t, err)
				out = append(out, Version{Round: r, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol})
				v = &out[len(out)-1]
			}
			o.put(v, val, ok, values)
		}
	}
	return out
}

func ttBase(t *testing.T, l *Ledger, addr basics.Address) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			ad, _, err := l.LookupWithoutRewards(r, addr)
			require.NoError(t, err)
			return ad, !ad.IsZero()
		},
		put: func(v *Version, _ any, ok, values bool) {
			v.Account = &AccountVersion{Deleted: !ok}
			if values && ok {
				ad, _, without, err := l.LookupAccount(v.Round, addr)
				require.NoError(t, err)
				v.Account.Data, v.Account.WithoutRewards = &ad, without
			}
		},
	}
}

func ttHolding(t *testing.T, l *Ledger, addr basics.Address, aidx basics.AssetIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			res, err := l.LookupAsset(r, addr, aidx)
			require.NoError(t, err)
			return res.AssetHolding, res.AssetHolding != nil
		},
		put: func(v *Version, val any, ok, values bool) {
			hv := HoldingVersion{Asset: aidx, Deleted: !ok}
			if values && ok {
				hv.Holding = val.(*basics.AssetHolding)
			}
			v.Holdings = append(v.Holdings, hv)
		},
	}
}

func ttLocal(t *testing.T, l *Ledger, addr basics.Address, app basics.AppIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			res, err := l.LookupApplication(r, addr, app)
			require.NoError(t, err)
			return res.AppLocalState, res.AppLocalState != nil
		},
		put: func(v *Version, val any, ok, values bool) {
			lv := LocalStateVersion{App: app, Deleted: !ok}
			if values && ok {
				lv.State = val.(*basics.AppLocalState)
			}
			v.LocalStates = append(v.LocalStates, lv)
		},
	}
}

func ttAssetParams(t *testing.T, l *Ledger, creator basics.Address, aidx basics.AssetIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			res, err := l.LookupAsset(r, creator, aidx)
			require.NoError(t, err)
			return res.AssetParams, res.AssetParams != nil
		},
		put: func(v *Version, val any, ok, values bool) {
			av := AssetVersion{Asset: aidx, Deleted: !ok}
			if values && ok {
				av.Creator, av.Params = creator, val.(*basics.AssetParams)
			}
			v.Assets = append(v.Assets, av)
		},
	}
}

// ttCreatedAsset is an asset as the creator index sees it: created or not.
func ttCreatedAsset(t *testing.T, l *Ledger, creator basics.Address, aidx basics.AssetIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			_, ok, err := l.GetCreatorForRound(r, basics.CreatableIndex(aidx), basics.AssetCreatable)
			require.NoError(t, err)
			return nil, ok
		},
		put: func(v *Version, _ any, ok, _ bool) {
			v.Assets = append(v.Assets, AssetVersion{Asset: aidx, Deleted: !ok, Creator: creator})
		},
	}
}

func ttAppParams(t *testing.T, l *Ledger, creator basics.Address, app basics.AppIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			res, err := l.LookupApplication(r, creator, app)
			require.NoError(t, err)
			return res.AppParams, res.AppParams != nil
		},
		put: func(v *Version, val any, ok, values bool) {
			av := AppVersion{App: app, Deleted: !ok}
			if values && ok {
				av.Creator, av.Params = creator, val.(*basics.AppParams)
			}
			v.Apps = append(v.Apps, av)
		},
	}
}

func ttBox(t *testing.T, l *Ledger, key string) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			b, err := l.LookupKv(r, key)
			require.NoError(t, err)
			return b, b != nil
		},
		put: func(v *Version, val any, ok, values bool) {
			v.Box = &BoxVersion{Deleted: !ok}
			if values && ok {
				v.Box.Value = val.([]byte)
			}
		},
	}
}

// ttPages pages through a lookup and returns every version, in page order.
func ttPages(t *testing.T, lookup func(VersionRange) (VersionPage, error), r VersionRange) []Version {
	var all []Version
	for {
		p, err := lookup(r)
		require.NoError(t, err)
		require.LessOrEqual(t, len(p.Versions), int(r.Limit))
		all = append(all, p.Versions...)
		if !p.More {
			return all
		}
		require.NotEmpty(t, p.Versions)
		if r.Desc {
			require.Less(t, p.Next, p.Versions[len(p.Versions)-1].Round)
			r.Max = p.Next
		} else {
			require.Greater(t, p.Next, p.Versions[len(p.Versions)-1].Round)
			r.Min = p.Next
		}
	}
}

func TestTimeTravelVersions(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival())
	defer dl.Close()
	l := dl.validator

	asa := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x", Manager: addrs[0], Reserve: addrs[0]}}).ApplyData.ConfigAsset
	asa2 := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 5, UnitName: "y", Manager: addrs[0]}}).ApplyData.ConfigAsset
	app := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: app.Address(), Amount: 10_000_000})
	call := func(sender basics.Address, i int, oc transactions.OnCompletion) *txntest.Txn {
		// the box value changes every 10 rounds: the writes in between are no-ops
		return &txntest.Txn{Type: "appl", Sender: sender, ApplicationID: app, OnCompletion: oc,
			ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i/10))}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}}
	}
	optIn := &txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: asa}
	for i := 0; i < 32; i++ {
		var txs []*txntest.Txn
		holds := (i > 2 && i < 10) || i > 15
		switch i {
		case 2, 15:
			txs = append(txs, optIn)
		case 4:
			txs = append(txs, call(addrs[1], i, transactions.OptInOC))
		case 6, 25:
			txs = append(txs, &txntest.Txn{Type: "acfg", Sender: addrs[0], ConfigAsset: asa, AssetParams: basics.AssetParams{Manager: addrs[0], Reserve: addrs[i%4]}})
		case 10:
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[0], AssetCloseTo: addrs[0], XferAsset: asa})
		case 12:
			txs = append(txs, &txntest.Txn{Type: "acfg", Sender: addrs[0], ConfigAsset: asa2})
		case 20:
			txs = append(txs, &txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: app, OnCompletion: transactions.ClearStateOC})
		}
		if i%3 == 0 {
			txs = append(txs, &txntest.Txn{Type: "pay", Sender: addrs[3], Receiver: addrs[4], Amount: uint64(i + 1)})
		}
		if holds && i%4 == 1 {
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[0], AssetReceiver: addrs[1], XferAsset: asa, AssetAmount: uint64(i)})
		}
		if holds && i%7 == 0 { // rewrites the holding unchanged
			txs = append(txs, &txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: asa})
		}
		if i%5 == 0 {
			txs = append(txs, call(addrs[3], i, transactions.NoOpOC))
		}
		for j := range txs { // identical transactions recur
			tx := *txs[j]
			tx.Note = []byte{byte(i), byte(j)}
			txs[j] = &tx
		}
		dl.fullBlock(txs...)
		if i == 16 {
			commitRoundLookback(4, l)
		}
	}
	box := apps.MakeBoxKey(uint64(app), "b")

	type lookupCase struct {
		name   string
		objs   []ttObj
		lookup func(VersionRange) (VersionPage, error)
	}
	check := func() {
		cases := []lookupCase{
			{"asset params", []ttObj{ttAssetParams(t, l, addrs[0], asa)},
				func(r VersionRange) (VersionPage, error) { return l.AssetVersions(asa, r) }},
			{"destroyed asset params", []ttObj{ttAssetParams(t, l, addrs[0], asa2)},
				func(r VersionRange) (VersionPage, error) { return l.AssetVersions(asa2, r) }},
			{"app params", []ttObj{ttAppParams(t, l, addrs[2], app)},
				func(r VersionRange) (VersionPage, error) { return l.AppVersions(app, r) }},
			{"box", []ttObj{ttBox(t, l, box)},
				func(r VersionRange) (VersionPage, error) { return l.KvVersions(box, r) }},
			{"holding", []ttObj{ttHolding(t, l, addrs[1], asa)},
				func(r VersionRange) (VersionPage, error) { return l.AccountAssetVersions(addrs[1], asa, r) }},
			{"creator's holding and params", []ttObj{ttHolding(t, l, addrs[0], asa), ttAssetParams(t, l, addrs[0], asa)},
				func(r VersionRange) (VersionPage, error) { return l.AccountAssetVersions(addrs[0], asa, r) }},
			{"local state", []ttObj{ttLocal(t, l, addrs[1], app)},
				func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[1], app, r) }},
			{"creator's app", []ttObj{ttLocal(t, l, addrs[2], app), ttAppParams(t, l, addrs[2], app)},
				func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[2], app, r) }},
			{"account", []ttObj{ttBase(t, l, addrs[1]), ttHolding(t, l, addrs[1], asa), ttLocal(t, l, addrs[1], app)},
				func(r VersionRange) (VersionPage, error) {
					return l.AccountVersions(addrs[1], AccountVersionsExclude{}, r)
				}},
			{"account base", []ttObj{ttBase(t, l, addrs[1])},
				func(r VersionRange) (VersionPage, error) {
					return l.AccountVersions(addrs[1], AccountVersionsExclude{All: true}, r)
				}},
			{"creator account", []ttObj{ttBase(t, l, addrs[0]), ttHolding(t, l, addrs[0], asa), ttHolding(t, l, addrs[0], asa2),
				ttAssetParams(t, l, addrs[0], asa), ttAssetParams(t, l, addrs[0], asa2)},
				func(r VersionRange) (VersionPage, error) {
					return l.AccountVersions(addrs[0], AccountVersionsExclude{}, r)
				}},
			{"creator account without params", []ttObj{ttBase(t, l, addrs[0]), ttHolding(t, l, addrs[0], asa), ttHolding(t, l, addrs[0], asa2),
				ttCreatedAsset(t, l, addrs[0], asa), ttCreatedAsset(t, l, addrs[0], asa2)},
				func(r VersionRange) (VersionPage, error) {
					return l.AccountVersions(addrs[0], AccountVersionsExclude{CreatedAssetsParams: true}, r)
				}},
			{"app creator account", []ttObj{ttBase(t, l, addrs[2]), ttAppParams(t, l, addrs[2], app)},
				func(r VersionRange) (VersionPage, error) {
					return l.AccountVersions(addrs[2], AccountVersionsExclude{}, r)
				}},
		}
		latest := l.Latest()
		for _, c := range cases {
			for _, values := range []bool{false, true} {
				want := ttVersions(t, l, c.objs, values)
				require.NotEmpty(t, want, c.name)
				all := VersionRange{Max: latest, Limit: 1000, Values: values}
				p, err := c.lookup(all)
				require.NoError(t, err)
				require.False(t, p.More)
				require.Equal(t, want, p.Versions, "%s values=%v", c.name, values)

				desc := slices.Clone(want)
				slices.Reverse(desc)
				var mid []Version // a range that cuts versions off both ends
				lo, hi := want[0].Round+1, want[len(want)-1].Round-1
				for _, v := range want {
					if v.Round >= lo && v.Round <= hi {
						mid = append(mid, v)
					}
				}
				for _, limit := range []uint64{1, 2, 3} {
					r := all
					r.Limit = limit
					require.Equal(t, want, ttPages(t, c.lookup, r), "%s limit %d", c.name, limit)
					r.Desc = true
					require.Equal(t, desc, ttPages(t, c.lookup, r), "%s limit %d desc", c.name, limit)
					r.Desc, r.Min, r.Max = false, lo, hi
					require.Equal(t, mid, ttPages(t, c.lookup, r), "%s limit %d mid", c.name, limit)
				}
				if values { // a 1-byte budget ends a page at every version with values
					r := all
					r.MaxBytes = 1
					require.Equal(t, want, ttPages(t, c.lookup, r), "%s byte budget", c.name)
				}
			}
		}
		// two holdings and two created assets, one destroyed: they all count
		_, err := l.AccountVersions(addrs[0], AccountVersionsExclude{}, VersionRange{Max: latest, Limit: 10, MaxObjects: 3})
		require.ErrorIs(t, err, ErrTooManyVersionObjects)
		_, err = l.AccountVersions(addrs[0], AccountVersionsExclude{}, VersionRange{Max: latest, Limit: 10, MaxObjects: 4})
		require.NoError(t, err)
		_, err = l.AssetVersions(asa, VersionRange{Max: latest + 1, Limit: 10})
		require.ErrorIs(t, err, errHistoryUnavailable)
	}

	last, _ := l.history.store.Last()
	require.Less(t, basics.Round(last), l.Latest()-10, "the last rounds are in memory only")
	check()

	for range 8 {
		dl.fullBlock()
	}
	commitRoundLookback(4, l)
	last, _ = l.history.store.Last()
	require.Greater(t, basics.Round(last), l.Latest()-10, "the rounds reached the store")
	check()
}

// boxChurnApp puts arg 1 in box arg 0 and, given arg 2, deletes that box.
var boxChurnApp = main(`
	txna ApplicationArgs 0; txna ApplicationArgs 1; box_put
	txn NumAppArgs; int 3; <; bnz end
	txna ApplicationArgs 2; box_del; pop
`)

// TestTimeTravelAppBoxVersions checks the changes of an app's boxes against
// the boxes at every round: boxes created and deleted in adjacent rounds and
// within a round, and rewritten unchanged.
func TestTimeTravelAppBoxVersions(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival())
	defer dl.Close()
	l := dl.validator

	app := dl.createApp(addrs[2], boxChurnApp)
	other := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	for _, a := range []basics.AppIndex{app, other} {
		dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: a.Address(), Amount: 10_000_000})
	}
	names := map[string]bool{}
	put := func(i int, name string, val uint64, del string) *txntest.Txn {
		names[name] = true
		tx := &txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: app,
			ApplicationArgs: [][]byte{[]byte(name), binary.BigEndian.AppendUint64(nil, val)}, Boxes: []transactions.BoxRef{{Name: []byte(name)}}}
		if del != "" {
			names[del] = true
			tx.ApplicationArgs = append(tx.ApplicationArgs, []byte(del))
			tx.Boxes = append(tx.Boxes, transactions.BoxRef{Name: []byte(del)})
		}
		return tx
	}
	for i := 0; i < 32; i++ {
		// "s" changes every 10 rounds: the writes in between are no-ops
		txs := []*txntest.Txn{put(i, "s", uint64(i/10), "")}
		if i%2 == 0 { // a new box every other round, deleted two rounds later
			txs = append(txs, put(i, fmt.Sprintf("c%02d", i), uint64(i), fmt.Sprintf("c%02d", i-2)))
		}
		if i%5 == 0 { // created and deleted within the round
			tn := fmt.Sprintf("t%02d", i)
			txs = append(txs, put(i, tn, 1, tn))
		}
		if i == 9 { // "s" deleted, recreated next round
			txs = append(txs, put(i, "x", 9, "s"))
		}
		if i%4 == 0 {
			txs = append(txs, &txntest.Txn{Type: "appl", Sender: addrs[3], ApplicationID: other,
				ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i))}, Boxes: []transactions.BoxRef{{Name: []byte("b")}}})
		}
		for j := range txs { // identical transactions recur
			tx := *txs[j]
			tx.Note = []byte{byte(i), byte(j)}
			txs[j] = &tx
		}
		dl.fullBlock(txs...)
		if i == 16 {
			commitRoundLookback(4, l)
		}
	}
	sorted := slices.Sorted(maps.Keys(names))

	// the oracle: the rounds at which any of the app's boxes differs from the round before
	oracle := func(values bool) []Version {
		var out []Version
		prev := map[string][]byte{}
		for r := basics.Round(0); r <= l.Latest(); r++ {
			var v *Version
			for _, name := range sorted {
				b, err := l.LookupKv(r, apps.MakeBoxKey(uint64(app), name))
				require.NoError(t, err)
				p, had := prev[name]
				if (b != nil) == had && (b == nil || slices.Equal(b, p)) {
					continue
				}
				if b == nil {
					delete(prev, name)
				} else {
					prev[name] = b
				}
				if v == nil {
					hdr, err := l.BlockHdr(r)
					require.NoError(t, err)
					out = append(out, Version{Round: r, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol})
					v = &out[len(out)-1]
				}
				bc := BoxChange{Name: []byte(name), Deleted: b == nil}
				if values {
					bc.Value = b
				}
				v.Boxes = append(v.Boxes, bc)
			}
		}
		return out
	}
	lookup := func(r VersionRange) (VersionPage, error) { return l.AppBoxVersions(app, r) }

	check := func() {
		latest := l.Latest()
		for _, values := range []bool{false, true} {
			want := oracle(values)
			require.Greater(t, len(want), 15)
			var multi bool
			for _, v := range want {
				multi = multi || len(v.Boxes) > 2
			}
			require.True(t, multi, "some rounds change several boxes")
			all := VersionRange{Max: latest, Limit: 1000, Values: values}
			p, err := lookup(all)
			require.NoError(t, err)
			require.False(t, p.More)
			require.Equal(t, want, p.Versions, "values=%v", values)

			desc := slices.Clone(want)
			slices.Reverse(desc)
			var mid []Version
			lo, hi := want[0].Round+1, want[len(want)-1].Round-1
			for _, v := range want {
				if v.Round >= lo && v.Round <= hi {
					mid = append(mid, v)
				}
			}
			for _, limit := range []uint64{1, 2, 3} {
				r := all
				r.Limit = limit
				require.Equal(t, want, ttPages(t, lookup, r), "limit %d", limit)
				r.Desc = true
				require.Equal(t, desc, ttPages(t, lookup, r), "limit %d desc", limit)
				r.Desc, r.Min, r.Max = false, lo, hi
				require.Equal(t, mid, ttPages(t, lookup, r), "limit %d mid", limit)
			}
			if values {
				r := all
				r.MaxBytes = 1
				require.Equal(t, want, ttPages(t, lookup, r), "byte budget")
				p, err := lookup(r)
				require.NoError(t, err)
				require.Len(t, p.Versions, 1)
			}
		}
		p, err := l.AppBoxVersions(app+100, VersionRange{Max: latest, Limit: 10})
		require.NoError(t, err)
		require.Empty(t, p.Versions, "an app without boxes")
		_, err = l.AppBoxVersions(app, VersionRange{Max: latest + 1, Limit: 10})
		require.ErrorIs(t, err, errHistoryUnavailable)
	}

	last, _ := l.history.store.Last()
	require.Less(t, basics.Round(last), l.Latest()-10, "the last rounds are in memory only")
	check()

	for range 8 {
		dl.fullBlock()
	}
	commitRoundLookback(4, l)
	last, _ = l.history.store.Last()
	require.Greater(t, basics.Round(last), l.Latest()-10, "the rounds reached the store")
	check()
}

// TestTimeTravelAppBoxVersionsClose checks that closing the store while box
// pages are read does not hang: a page reads headers only once the store's
// scan is done, since a reader that takes the store's lock again behind a
// waiting Close deadlocks.
func TestTimeTravelAppBoxVersionsClose(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := timetravel.Open(t.TempDir(), timetravel.Options{})
	require.NoError(t, err)
	const rounds = 2000
	for r := uint64(0); r < rounds; r++ {
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r), TimeStamp: int64(r)})
		var chs []timetravel.Change
		for _, name := range []string{"a", fmt.Sprint(r)} {
			chs = append(chs, timetravel.Change{Key: timetravel.KvKey(apps.MakeBoxKey(1, name)), Value: binary.BigEndian.AppendUint64(nil, r)})
		}
		require.NoError(t, s.AddRound(r, chs, hdr, nil))
	}
	h := &histView{store: s, last: rounds - 1}
	p, err := histAppBoxVersions(h, 1, VersionRange{Max: rounds - 1, Limit: 10})
	require.NoError(t, err)
	require.Len(t, p.Versions, 10)
	require.Len(t, p.Versions[1].Boxes, 2)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if _, err := histAppBoxVersions(h, 1, VersionRange{Max: rounds - 1, Limit: 1000, Desc: true}); err != nil {
					require.ErrorIs(t, err, timetravel.ErrClosed)
					return
				}
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	closed := make(chan error)
	go func() {
		closed <- s.Close()
		wg.Wait()
		close(closed)
	}()
	select {
	case err := <-closed:
		require.NoError(t, err)
		<-closed
	case <-time.After(20 * time.Second):
		t.Fatal("Close hangs while box pages are read")
	}
}

// TestTimeTravelAppBoxVersionsPartialIndex checks box pages on a store whose
// box index starts after its first round: descending pages list the changes
// since, then fail; ascending ones fail at once.
func TestTimeTravelAppBoxVersionsPartialIndex(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dir := t.TempDir()
	s, err := timetravel.Open(dir, timetravel.Options{})
	require.NoError(t, err)
	round := func(r uint64) {
		hdr := protocol.Encode(&bookkeeping.BlockHeader{Round: basics.Round(r), TimeStamp: int64(r)})
		ch := timetravel.Change{Key: timetravel.KvKey(apps.MakeBoxKey(1, "a")), Value: binary.BigEndian.AppendUint64(nil, r)}
		require.NoError(t, s.AddRound(r, []timetravel.Change{ch}, hdr, nil))
	}
	for r := uint64(0); r < 10; r++ {
		round(r)
	}
	// as a build without the index leaves the store
	require.NoError(t, s.DB().DeleteRange([]byte{'b'}, []byte{'c'}, nil))
	require.NoError(t, s.DB().Delete([]byte("mb"), nil))
	require.NoError(t, s.Close())
	s, err = timetravel.Open(dir, timetravel.Options{})
	require.NoError(t, err)
	defer s.Close()
	from, _ := s.BoxIndexFrom()
	require.EqualValues(t, 10, from)
	for r := uint64(10); r < 20; r++ {
		round(r)
	}
	h := &histView{store: s, last: 19}

	r := VersionRange{Max: 19, Limit: 4, Desc: true}
	var got []basics.Round
	for {
		p, err := histAppBoxVersions(h, 1, r)
		if err != nil {
			require.ErrorIs(t, err, ErrNoBoxIndex)
			break
		}
		for _, v := range p.Versions {
			got = append(got, v.Round)
		}
		require.True(t, p.More, "rounds before the index may hold changes")
		r.Max = p.Next
	}
	require.Equal(t, []basics.Round{19, 18, 17, 16, 15, 14, 13, 12, 11, 10}, got)
	_, err = histAppBoxVersions(h, 1, VersionRange{Max: 19, Limit: 4})
	require.ErrorIs(t, err, ErrNoBoxIndex)
	p, err := histAppBoxVersions(h, 1, VersionRange{Min: 10, Max: 19, Limit: 100})
	require.NoError(t, err)
	require.Len(t, p.Versions, 10)
}
