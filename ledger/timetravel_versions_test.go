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
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
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
	// put adds the object's entry to the version of its round; prev is the
	// object at the round before, nil if it had none
	put func(v *Version, val, prev any, ok bool, inc ttInclude)
}

// ttInclude is what a versions lookup includes.
type ttInclude struct{ values, keys bool }

// ttVersions is the oracle: the rounds at which any of objs differs from the
// round before, with the entries of the objects that do.
func ttVersions(t *testing.T, l *Ledger, objs []ttObj, inc ttInclude) []Version {
	var out []Version
	for r := basics.Round(0); r <= l.Latest(); r++ {
		var v *Version
		for _, o := range objs {
			val, ok := o.get(r)
			changed := ok
			var prev any
			if r > 0 {
				pv, pok := o.get(r - 1)
				changed = ok != pok || (ok && !reflect.DeepEqual(val, pv))
				if pok {
					prev = pv
				}
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
			o.put(v, val, prev, ok, inc)
		}
	}
	return out
}

// ttChangedKeys is the oracle's changedKeys: the keys of either state whose
// values differ, absent ones included, sorted.
func ttChangedKeys(prev, cur basics.TealKeyValue) [][]byte {
	union := maps.Clone(prev)
	if union == nil {
		union = basics.TealKeyValue{}
	}
	maps.Copy(union, cur)
	all := slices.Sorted(maps.Keys(union))
	out := [][]byte{}
	for _, k := range all {
		pv, pok := prev[k]
		cv, cok := cur[k]
		if pok != cok || !reflect.DeepEqual(pv, cv) {
			out = append(out, []byte(k))
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
		put: func(v *Version, _, _ any, ok bool, inc ttInclude) {
			v.Account = &AccountVersion{Deleted: !ok}
			if inc.values && ok {
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
		put: func(v *Version, val, _ any, ok bool, inc ttInclude) {
			hv := HoldingVersion{Asset: aidx, Deleted: !ok}
			if inc.values && ok {
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
		put: func(v *Version, val, prev any, ok bool, inc ttInclude) {
			lv := LocalStateVersion{App: app, Deleted: !ok}
			if inc.values && ok {
				lv.State = val.(*basics.AppLocalState)
			}
			if inc.keys && ok {
				var before basics.TealKeyValue
				if prev != nil {
					before = prev.(*basics.AppLocalState).KeyValue
				}
				lv.ChangedKeys = ttChangedKeys(before, val.(*basics.AppLocalState).KeyValue)
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
		put: func(v *Version, val, _ any, ok bool, inc ttInclude) {
			av := AssetVersion{Asset: aidx, Deleted: !ok}
			if inc.values && ok {
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
		put: func(v *Version, _, _ any, ok bool, _ ttInclude) {
			v.Assets = append(v.Assets, AssetVersion{Asset: aidx, Deleted: !ok, Creator: creator})
		},
	}
}

// ttCreatedApp is an app as the creator index sees it: no params, so no keys.
func ttCreatedApp(t *testing.T, l *Ledger, creator basics.Address, app basics.AppIndex) ttObj {
	return ttObj{
		get: func(r basics.Round) (any, bool) {
			_, ok, err := l.GetCreatorForRound(r, basics.CreatableIndex(app), basics.AppCreatable)
			require.NoError(t, err)
			return nil, ok
		},
		put: func(v *Version, _, _ any, ok bool, _ ttInclude) {
			v.Apps = append(v.Apps, AppVersion{App: app, Deleted: !ok, Creator: creator})
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
		put: func(v *Version, val, prev any, ok bool, inc ttInclude) {
			av := AppVersion{App: app, Deleted: !ok}
			if inc.values && ok {
				av.Creator, av.Params = creator, val.(*basics.AppParams)
			}
			if inc.keys && ok {
				var before basics.TealKeyValue
				if prev != nil {
					before = prev.(*basics.AppParams).GlobalState
				}
				av.ChangedKeys = ttChangedKeys(before, val.(*basics.AppParams).GlobalState)
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
		put: func(v *Version, val, _ any, ok bool, inc ttInclude) {
			v.Box = &BoxVersion{Deleted: !ok}
			if inc.values && ok {
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

// ttChanges lists, for each of objs, the rounds at which it differs from the
// round before (round 0 if it exists then).
func ttChanges(l *Ledger, objs []ttObj) [][]basics.Round {
	out := make([][]basics.Round, len(objs))
	for i, o := range objs {
		for r := basics.Round(0); r <= l.Latest(); r++ {
			val, ok := o.get(r)
			changed := ok
			if r > 0 {
				pv, pok := o.get(r - 1)
				changed = ok != pok || (ok && !reflect.DeepEqual(val, pv))
			}
			if changed {
				out[i] = append(out[i], r)
			}
		}
	}
	return out
}

// ttVersionsAt is the oracle of a Rounds lookup: per round, each object's
// entry from its latest change at or before the round (chg, from ttChanges),
// with that change's round as Changed. Objects that do not exist at a round
// are left out, except objs[0] if first is set, which is then deleted.
func ttVersionsAt(t *testing.T, l *Ledger, objs []ttObj, chg [][]basics.Round, inc ttInclude, rounds []basics.Round, first bool) []Version {
	var out []Version
	for _, R := range rounds {
		hdr, err := l.BlockHdr(R)
		require.NoError(t, err)
		v := Version{Round: R, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol}
		for j, o := range objs {
			always := j == 0 && first
			i := sort.Search(len(chg[j]), func(i int) bool { return chg[j][i] > R }) - 1
			if i < 0 {
				if always {
					o.put(&v, nil, nil, false, inc)
				}
				continue
			}
			at := chg[j][i]
			val, ok := o.get(at)
			if !ok && !always {
				continue
			}
			var prev any
			if at > 0 {
				if pv, pok := o.get(at - 1); pok {
					prev = pv
				}
			}
			acct, box := v.Account, v.Box
			nh, nl, nas, nap := len(v.Holdings), len(v.LocalStates), len(v.Assets), len(v.Apps)
			o.put(&v, val, prev, ok, inc)
			switch {
			case v.Account != acct:
				v.Account.Changed = &at
			case v.Box != box:
				v.Box.Changed = &at
			case len(v.Holdings) > nh:
				v.Holdings[nh].Changed = &at
			case len(v.LocalStates) > nl:
				v.LocalStates[nl].Changed = &at
			case len(v.Assets) > nas:
				v.Assets[nas].Changed = &at
			case len(v.Apps) > nap:
				v.Apps[nap].Changed = &at
			default:
				require.Fail(t, "put added nothing")
			}
		}
		out = append(out, v)
	}
	return out
}

// ttRoundPages pages through a Rounds lookup and returns every entry.
func ttRoundPages(t *testing.T, lookup func(VersionRange) (VersionPage, error), r VersionRange) []Version {
	var all []Version
	for {
		r.Min, r.Max = min(r.Rounds[0], r.Rounds[len(r.Rounds)-1]), max(r.Rounds[0], r.Rounds[len(r.Rounds)-1])
		p, err := lookup(r)
		require.NoError(t, err)
		require.LessOrEqual(t, len(p.Versions), int(r.Limit))
		all = append(all, p.Versions...)
		if !p.More {
			require.Len(t, p.Versions, len(r.Rounds))
			return all
		}
		require.NotEmpty(t, p.Versions)
		require.Less(t, len(p.Versions), len(r.Rounds))
		require.Equal(t, r.Rounds[len(p.Versions)], p.Next)
		r.Rounds = r.Rounds[len(p.Versions):]
	}
}

// kvApp creates global keys c1 (uint) and c2, then edits the state as its
// arguments say: "g" key value and "u" key uint put a global key, "d" key
// deletes one, "l" key value puts a local key of the sender, "x" key deletes
// one. Calls without arguments only approve.
const kvApp = `#pragma version 10
	txn ApplicationID
	bnz run
	byte "c1"; int 1; app_global_put
	byte "c2"; byte "x"; app_global_put
	b end
run:
	txn NumAppArgs
	bz end
	txna ApplicationArgs 0; byte "g"; ==; bz notg
	txna ApplicationArgs 1; txna ApplicationArgs 2; app_global_put; b end
notg:
	txna ApplicationArgs 0; byte "u"; ==; bz notu
	txna ApplicationArgs 1; txna ApplicationArgs 2; btoi; app_global_put; b end
notu:
	txna ApplicationArgs 0; byte "d"; ==; bz notd
	txna ApplicationArgs 1; app_global_del; b end
notd:
	txna ApplicationArgs 0; byte "l"; ==; bz notl
	txn Sender; txna ApplicationArgs 1; txna ApplicationArgs 2; app_local_put; b end
notl:
	txn Sender; txna ApplicationArgs 1; app_local_del
end:
	int 1
`

// genesisApp is the id of an app the genesis of TestTimeTravelVersions
// creates, with global and local state. It is never called: the ledger does
// not index the creators of genesis apps.
const genesisApp = basics.AppIndex(5)

func TestTimeTravelVersions(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	kvProg, err := logic.AssembleString(kvApp)
	require.NoError(t, err)
	clearProg, err := logic.AssembleString("#pragma version 10\nint 1")
	require.NoError(t, err)
	kvGlobal := basics.StateSchema{NumUint: 4, NumByteSlice: 8}
	kvLocal := basics.StateSchema{NumByteSlice: 4}

	for _, n := range []uint64{1, 3} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			// a genesis app with state, and a local state of it
			gc, gl := genBalances.Balances[addrs[5]], genBalances.Balances[addrs[6]]
			gc.AppParams = map[basics.AppIndex]basics.AppParams{genesisApp: {
				ApprovalProgram: kvProg.Program, ClearStateProgram: clearProg.Program,
				GlobalState:  basics.TealKeyValue{"g1": {Type: basics.TealUintType, Uint: 1}, "g2": {Type: basics.TealBytesType, Bytes: "two"}},
				StateSchemas: basics.StateSchemas{GlobalStateSchema: kvGlobal, LocalStateSchema: kvLocal},
			}}
			gc.TotalAppSchema = kvGlobal
			gl.AppLocalStates = map[basics.AppIndex]basics.AppLocalState{genesisApp: {
				Schema: kvLocal, KeyValue: basics.TealKeyValue{"l1": {Type: basics.TealBytesType, Bytes: "one"}},
			}}
			gl.TotalAppSchema = kvLocal
			genBalances.Balances[addrs[5]], genBalances.Balances[addrs[6]] = gc, gl
			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = true
			cfg.TimeTravelCheckpointInterval = n
			cfg.MaxAcctLookback = 4
			dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerNotArchival())
			defer dl.Close()
			l := dl.validator

			asa := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x", Manager: addrs[0], Reserve: addrs[0]}}).ApplyData.ConfigAsset
			asa2 := dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 5, UnitName: "y", Manager: addrs[0]}}).ApplyData.ConfigAsset
			app := dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
			dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: app.Address(), Amount: 10_000_000})
			kv := dl.createApp(addrs[2], kvApp, kvGlobal, kvLocal)
			kvCreated := l.Latest()
			// kvCall calls an app kvApp; oc is NoOpOC unless given
			kvCall := func(app basics.AppIndex, sender basics.Address, oc transactions.OnCompletion, args ...string) *txntest.Txn {
				tx := &txntest.Txn{Type: "appl", Sender: sender, ApplicationID: app, OnCompletion: oc}
				for _, a := range args {
					tx.ApplicationArgs = append(tx.ApplicationArgs, []byte(a))
				}
				return tx
			}
			g := func(args ...string) *txntest.Txn { return kvCall(kv, addrs[3], transactions.NoOpOC, args...) }
			loc := func(args ...string) *txntest.Txn { return kvCall(kv, addrs[1], transactions.NoOpOC, args...) }
			u64 := func(n uint64) string { return string(binary.BigEndian.AppendUint64(nil, n)) }
			// the keys kv's params and addrs[1]'s local state of it change, by
			// round; deleted is nil
			wantGlobal := map[basics.Round][]string{kvCreated: {"c1", "c2"}}
			wantLocal := map[basics.Round][]string{}
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
				// the keys this round changes; the global state and the local
				// one change in different rounds, as a validator orders their
				// records in a round's delta differently
				var global, local []string
				switch i {
				case 1:
					txs = append(txs, g("g", "a", "v1"), g("u", "n", u64(5)))
					global = []string{"a", "n"}
				case 2: // opted in with a key
					txs = append(txs, kvCall(kv, addrs[1], transactions.OptInOC, "l", "x", "1"))
					local = []string{"x"}
				case 3:
					txs = append(txs, g("u", "n", u64(6)))
					global = []string{"n"}
				case 6: // t set and deleted, a set back: neither changed
					txs = append(txs, g("g", "t", "1"), g("d", "t"), g("g", "a", "v2"), g("g", "a", "v1"), g("u", "m", u64(1)))
					global = []string{"m"}
				case 7: // a uint becomes bytes
					txs = append(txs, g("g", "n", "five"))
					global = []string{"n"}
				case 8:
					txs = append(txs, loc("l", "y", "2"))
					local = []string{"y"}
				case 9:
					txs = append(txs, g("d", "c1"))
					global = []string{"c1"}
				case 12: // rewritten unchanged: not a version
					txs = append(txs, loc("l", "x", "1"))
				case 11: // the program only
					txs = append(txs, &txntest.Txn{Type: "appl", Sender: addrs[2], ApplicationID: kv, OnCompletion: transactions.UpdateApplicationOC,
						ApprovalProgram: kvApp + "int 1; pop", ClearStateProgram: "#pragma version 10\nint 1"})
					global = []string{}
				case 13:
					txs = append(txs, loc("x", "x"))
					local = []string{"x"}
				case 14: // cleared and opted in again: {y} becomes {x}
					txs = append(txs, kvCall(kv, addrs[1], transactions.ClearStateOC), kvCall(kv, addrs[1], transactions.OptInOC, "l", "x", "9"))
					local = []string{"x", "y"}
				case 18:
					txs = append(txs, kvCall(kv, addrs[1], transactions.CloseOutOC))
				case 19: // opted in empty
					txs = append(txs, kvCall(kv, addrs[1], transactions.OptInOC))
					local = []string{}
				case 22:
					txs = append(txs, g("g", "z", "1"))
					global = []string{"z"}
				case 23:
					txs = append(txs, loc("l", "z", "1"))
					local = []string{"z"}
				case 27:
					txs = append(txs, kvCall(kv, addrs[2], transactions.DeleteApplicationOC))
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
				switch i {
				case 18, 27:
					global, local = nil, nil
					if i == 18 {
						wantLocal[l.Latest()] = nil
					} else {
						wantGlobal[l.Latest()] = nil
					}
				}
				if global != nil {
					wantGlobal[l.Latest()] = global
				}
				if local != nil {
					wantLocal[l.Latest()] = local
				}
				if i == 16 {
					commitRoundLookback(4, l)
				}
			}
			box := apps.MakeBoxKey(uint64(app), "b")
			// keysOf maps the versions' keys by round, nil for deletions
			keysOf := func(vs []Version, local bool) map[basics.Round][]string {
				out := map[basics.Round][]string{}
				for _, v := range vs {
					var ks [][]byte
					var deleted bool
					if local {
						require.Len(t, v.LocalStates, 1)
						ks, deleted = v.LocalStates[0].ChangedKeys, v.LocalStates[0].Deleted
					} else {
						require.Len(t, v.Apps, 1)
						ks, deleted = v.Apps[0].ChangedKeys, v.Apps[0].Deleted
					}
					if deleted {
						require.Nil(t, ks, "round %d", v.Round)
						out[v.Round] = nil
						continue
					}
					require.NotNil(t, ks, "round %d", v.Round)
					out[v.Round] = []string{}
					for _, k := range ks {
						out[v.Round] = append(out[v.Round], string(k))
					}
				}
				return out
			}

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
					{"account", []ttObj{ttBase(t, l, addrs[1]), ttHolding(t, l, addrs[1], asa), ttLocal(t, l, addrs[1], app), ttLocal(t, l, addrs[1], kv)},
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
					{"app creator account", []ttObj{ttBase(t, l, addrs[2]), ttAppParams(t, l, addrs[2], app), ttAppParams(t, l, addrs[2], kv)},
						func(r VersionRange) (VersionPage, error) {
							return l.AccountVersions(addrs[2], AccountVersionsExclude{}, r)
						}},
					{"app creator account without params", []ttObj{ttBase(t, l, addrs[2]), ttCreatedApp(t, l, addrs[2], app), ttCreatedApp(t, l, addrs[2], kv)},
						func(r VersionRange) (VersionPage, error) {
							return l.AccountVersions(addrs[2], AccountVersionsExclude{CreatedAppsParams: true}, r)
						}},
					{"kv params", []ttObj{ttAppParams(t, l, addrs[2], kv)},
						func(r VersionRange) (VersionPage, error) { return l.AppVersions(kv, r) }},
					{"kv local state", []ttObj{ttLocal(t, l, addrs[1], kv)},
						func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[1], kv, r) }},
					{"kv creator", []ttObj{ttLocal(t, l, addrs[2], kv), ttAppParams(t, l, addrs[2], kv)},
						func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[2], kv, r) }},
					{"genesis app", []ttObj{ttAppParams(t, l, addrs[5], genesisApp)},
						func(r VersionRange) (VersionPage, error) { return l.AppVersions(genesisApp, r) }},
					{"genesis local state", []ttObj{ttLocal(t, l, addrs[6], genesisApp)},
						func(r VersionRange) (VersionPage, error) { return l.AccountAppVersions(addrs[6], genesisApp, r) }},
				}
				latest := l.Latest()
				for _, c := range cases {
					for _, inc := range []ttInclude{{}, {values: true}, {keys: true}, {values: true, keys: true}} {
						want := ttVersions(t, l, c.objs, inc)
						require.NotEmpty(t, want, c.name)
						all := VersionRange{Max: latest, Limit: 1000, Values: inc.values, Keys: inc.keys}
						p, err := c.lookup(all)
						require.NoError(t, err)
						require.False(t, p.More)
						require.Equal(t, want, p.Versions, "%s %+v", c.name, inc)

						// a page, by range or by rounds, stops when its
						// context is done or past its deadline
						canceled, cancel := context.WithCancel(context.Background())
						cancel()
						for _, rounds := range [][]basics.Round{nil, {latest}} {
							r := all
							r.Rounds = rounds
							r.Ctx = canceled
							_, err = c.lookup(r)
							require.ErrorIs(t, err, context.Canceled, c.name)
							r.Ctx, r.Deadline = nil, time.Now().Add(-time.Second)
							_, err = c.lookup(r)
							require.ErrorIs(t, err, context.DeadlineExceeded, c.name)
						}

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
							if lo <= hi && hi < want[len(want)-1].Round { // a range between two versions; hi wraps when the only version is round 0
								r.Desc, r.Min, r.Max = false, lo, hi
								require.Equal(t, mid, ttPages(t, c.lookup, r), "%s limit %d mid", c.name, limit)
							}
						}
						if inc.values || inc.keys { // a 1-byte budget ends a page at every version with values
							r := all
							r.MaxBytes = 1
							require.Equal(t, want, ttPages(t, c.lookup, r), "%s %+v byte budget", c.name, inc)
						}
					}
				}
				// the keys by hand, against the oracle's
				p, err := l.AppVersions(kv, VersionRange{Max: latest, Limit: 1000, Keys: true})
				require.NoError(t, err)
				require.Equal(t, wantGlobal, keysOf(p.Versions, false))
				p, err = l.AccountAppVersions(addrs[1], kv, VersionRange{Max: latest, Limit: 1000, Keys: true})
				require.NoError(t, err)
				require.Equal(t, wantLocal, keysOf(p.Versions, true))
				p, err = l.AppVersions(genesisApp, VersionRange{Max: latest, Limit: 1000, Keys: true})
				require.NoError(t, err)
				require.Equal(t, map[basics.Round][]string{0: {"g1", "g2"}}, keysOf(p.Versions, false))
				p, err = l.AccountAppVersions(addrs[6], genesisApp, VersionRange{Max: latest, Limit: 1000, Keys: true})
				require.NoError(t, err)
				require.Equal(t, map[basics.Round][]string{0: {"l1"}}, keysOf(p.Versions, true))
				// a page of keys counts the previous values too: a budget that
				// holds two versions' values holds one version with their keys
				p, err = l.AppVersions(kv, VersionRange{Max: latest, Limit: 1000})
				require.NoError(t, err)
				vs := p.Versions
				h := l.historyNow(latest)
				var budget uint64
				for _, v := range vs[1:3] {
					val, ok, err := h.Get(timetravel.AppParamsKey(kv), uint64(v.Round))
					require.NoError(t, err)
					require.True(t, ok)
					budget += uint64(len(val))
				}
				for _, inc := range []ttInclude{{values: true}, {keys: true}, {values: true, keys: true}} {
					p, err := l.AppVersions(kv, VersionRange{Min: vs[1].Round, Max: latest, Limit: 1000, Values: inc.values, Keys: inc.keys, MaxBytes: budget})
					require.NoError(t, err)
					require.True(t, p.More)
					n := 1
					if !inc.keys {
						n = 2
					}
					require.Len(t, p.Versions, n, "%+v", inc)
				}

				// as of rounds: every round, both ways, paged, and a sparse subset
				var every, sparse []basics.Round
				for R := basics.Round(0); R <= latest; R++ {
					every = append(every, R)
					if R%7 == 3 {
						sparse = append(sparse, R)
					}
				}
				// the lookups that report their object at every round, existing or not
				notFirst := map[string]bool{"holding": true, "creator's holding and params": true, "local state": true,
					"creator's app": true, "kv local state": true, "kv creator": true, "genesis local state": true}
				for _, c := range cases {
					chg := ttChanges(l, c.objs)
					for _, inc := range []ttInclude{{}, {values: true}, {keys: true}, {values: true, keys: true}} {
						for _, rounds := range [][]basics.Round{every, sparse} {
							want := ttVersionsAt(t, l, c.objs, chg, inc, rounds, !notFirst[c.name])
							r := VersionRange{Min: rounds[0], Max: rounds[len(rounds)-1], Limit: 1000, Values: inc.values, Keys: inc.keys, Rounds: rounds}
							p, err := c.lookup(r)
							require.NoError(t, err)
							require.False(t, p.More)
							require.Equal(t, want, p.Versions, "%s %+v rounds", c.name, inc)

							desc := slices.Clone(want)
							slices.Reverse(desc)
							r.Rounds, r.Desc = slices.Clone(rounds), true
							slices.Reverse(r.Rounds)
							require.Equal(t, desc, ttRoundPages(t, c.lookup, r), "%s %+v rounds desc", c.name, inc)
							r.Rounds, r.Desc = rounds, false
							for _, limit := range []uint64{1, 3} {
								r.Limit = limit
								require.Equal(t, want, ttRoundPages(t, c.lookup, r), "%s %+v rounds limit %d", c.name, inc, limit)
							}
							// a seek budget below one round's ends every page after one round
							r.Limit, r.MaxSeeks = 1000, 1
							p, err = c.lookup(r)
							require.NoError(t, err)
							require.Len(t, p.Versions, 1)
							require.Equal(t, want, ttRoundPages(t, c.lookup, r), "%s %+v rounds seek budget", c.name, inc)
							r.MaxSeeks = uint64(2 * len(c.objs)) // two rounds a page
							p, err = c.lookup(r)
							require.NoError(t, err)
							require.Len(t, p.Versions, min(2, len(rounds)))
							r.MaxSeeks = 0
							if inc.values || inc.keys {
								r.Limit, r.MaxBytes = 1000, 1
								require.Equal(t, want, ttRoundPages(t, c.lookup, r), "%s %+v rounds byte budget", c.name, inc)
							}
						}
					}
				}
				// by hand: asa2 before its creation, while it exists, after its destruction
				created, destroyed := ttChanges(l, []ttObj{ttAssetParams(t, l, addrs[0], asa2)})[0][0], ttChanges(l, []ttObj{ttAssetParams(t, l, addrs[0], asa2)})[0][1]
				p, err = l.AssetVersions(asa2, VersionRange{Max: latest, Limit: 10, Rounds: []basics.Round{0, created, destroyed - 1, destroyed, latest}})
				require.NoError(t, err)
				require.Len(t, p.Versions, 5)
				for i, w := range []struct {
					deleted bool
					changed *basics.Round
				}{{true, nil}, {false, &created}, {false, &created}, {true, &destroyed}, {true, &destroyed}} {
					require.Len(t, p.Versions[i].Assets, 1)
					a := p.Versions[i].Assets[0]
					require.Equal(t, w.deleted, a.Deleted, "entry %d", i)
					require.Equal(t, w.changed, a.Changed, "entry %d", i)
				}

				// two holdings and two created assets, one destroyed: they all count
				_, err = l.AccountVersions(addrs[0], AccountVersionsExclude{}, VersionRange{Max: latest, Limit: 10, MaxObjects: 3})
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
		})
	}
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

	for _, n := range []uint64{1, 3} {
		t.Run(fmt.Sprintf("N=%d", n), func(t *testing.T) {
			genBalances, addrs, _ := ledgertesting.NewTestGenesis()
			cfg := config.GetDefaultLocal()
			cfg.EnableTimeTravel = true
			cfg.TimeTravelCheckpointInterval = n
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
				// as of rounds: the boxes that exist then, each with its last change
				boxChanges := map[string][]basics.Round{}
				for _, name := range sorted {
					var prev []byte
					for r := basics.Round(0); r <= latest; r++ {
						b, err := l.LookupKv(r, apps.MakeBoxKey(uint64(app), name))
						require.NoError(t, err)
						if (b != nil) != (prev != nil) || (b != nil && !slices.Equal(b, prev)) {
							boxChanges[name] = append(boxChanges[name], r)
						}
						prev = b
					}
				}
				var every []basics.Round
				for R := basics.Round(0); R <= latest; R++ {
					every = append(every, R)
				}
				for _, values := range []bool{false, true} {
					var want []Version
					var most int
					for _, R := range every {
						hdr, err := l.BlockHdr(R)
						require.NoError(t, err)
						v := Version{Round: R, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol, Boxes: []BoxChange{}}
						for _, name := range sorted {
							b, err := l.LookupKv(R, apps.MakeBoxKey(uint64(app), name))
							require.NoError(t, err)
							if b == nil {
								continue
							}
							chg := boxChanges[name]
							at := chg[sort.Search(len(chg), func(i int) bool { return chg[i] > R })-1]
							bc := BoxChange{Name: []byte(name), Changed: &at}
							if values {
								bc.Value = b
							}
							v.Boxes = append(v.Boxes, bc)
						}
						most = max(most, len(v.Boxes))
						want = append(want, v)
					}
					require.Greater(t, most, 2, "some rounds have several boxes")
					r := VersionRange{Max: latest, Limit: 1000, Values: values, Rounds: every}
					p, err := lookup(r)
					require.NoError(t, err)
					require.False(t, p.More)
					require.Equal(t, want, p.Versions, "values=%v rounds", values)
					for _, limit := range []uint64{1, 3} {
						r.Limit = limit
						require.Equal(t, want, ttRoundPages(t, lookup, r), "rounds limit %d", limit)
					}
					r.Limit = 1000
					desc := slices.Clone(want)
					slices.Reverse(desc)
					r.Rounds, r.Desc = slices.Clone(every), true
					slices.Reverse(r.Rounds)
					require.Equal(t, desc, ttRoundPages(t, lookup, r), "rounds desc")
					r.Rounds, r.Desc = every, false
					// a page holds whole rounds: budgets end pages early, and a
					// round larger than one fails
					var mostBytes uint64
					var biggest, heaviest int // the rounds with the most boxes and values
					for i, v := range want {
						var n uint64
						for _, b := range v.Boxes {
							n += uint64(len(b.Value))
						}
						if n > mostBytes {
							mostBytes, heaviest = n, i
						}
						if len(v.Boxes) == most && biggest == 0 {
							biggest = i
						}
					}
					r.MaxEntries = uint64(most)
					require.Equal(t, want, ttRoundPages(t, lookup, r), "rounds entry budget")
					r.MaxEntries = uint64(most) - 1
					p, err = lookup(r)
					require.NoError(t, err, "the page ends before the round too large for it")
					require.True(t, p.More)
					r.Rounds = every[biggest:]
					_, err = lookup(r)
					require.ErrorIs(t, err, ErrRoundTooLarge, "a page cannot hold it")
					r.Rounds, r.MaxEntries = every, 0
					if values {
						r.MaxBytes = mostBytes
						require.Equal(t, want, ttRoundPages(t, lookup, r), "rounds byte budget")
						r.MaxBytes, r.Rounds = mostBytes-1, every[heaviest:]
						_, err = lookup(r)
						require.ErrorIs(t, err, ErrRoundTooLarge)
						r.MaxBytes, r.Rounds = 0, every
					}
					// the seek budget counts every box, existing or not
					r.MaxSeeks = 1
					require.Equal(t, want, ttRoundPages(t, lookup, r), "rounds seek budget")
					p, err = lookup(r)
					require.NoError(t, err)
					require.Len(t, p.Versions, 1)
					r.MaxSeeks = 0
					// every box the app ever had a record of counts
					var recorded uint64
					require.NoError(t, l.historyNow(latest).Objects(timetravel.BoxKvPrefix(uint64(app)), func([]byte) bool { recorded++; return true }))
					require.GreaterOrEqual(t, recorded, uint64(len(boxChanges)))
					r.MaxObjects = recorded - 1
					_, err = lookup(r)
					require.ErrorIs(t, err, ErrTooManyVersionObjects)
					r.MaxObjects = recorded
					_, err = lookup(r)
					require.NoError(t, err)
				}

				p, err := l.AppBoxVersions(app+100, VersionRange{Max: latest, Limit: 10})
				require.NoError(t, err)
				require.Empty(t, p.Versions, "an app without boxes")

				// a page, by range or by rounds, stops when its context is
				// done or past its deadline
				canceled, cancel := context.WithCancel(context.Background())
				cancel()
				for _, rounds := range [][]basics.Round{nil, {latest}} {
					r := VersionRange{Max: latest, Limit: 10, Rounds: rounds, Ctx: canceled}
					_, err = l.AppBoxVersions(app, r)
					require.ErrorIs(t, err, context.Canceled)
					r.Ctx, r.Deadline = nil, time.Now().Add(-time.Second)
					_, err = l.AppBoxVersions(app, r)
					require.ErrorIs(t, err, context.DeadlineExceeded)
				}
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
		})
	}
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
