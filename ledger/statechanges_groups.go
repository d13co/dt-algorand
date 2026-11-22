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

// What each top-level group of a past round changed besides app state
// (StateChangesFilter.Include): accounts, asset holdings and params, app params
// and local state opt-ins. The evaluator hands the tracer each group's delta
// (AfterTxnGroup), whose records are the values after the group; the value
// before is the latest earlier group's after, else the state at the round
// before, read in the post-pass.

import (
	"bytes"
	"cmp"
	"context"
	"fmt"
	"slices"
	"unsafe"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// StateChangesInclude selects what StateChangesForRound reports besides app
// state changes.
//
//msgp:ignore StateChangesInclude
type StateChangesInclude uint8

const (
	// IncludeAccounts reports, per group, accounts' own fields (balance, status, counts).
	IncludeAccounts StateChangesInclude = 1 << iota
	// IncludeAssets reports, per group, asset holdings and asset params.
	IncludeAssets
	// IncludeApps reports, per group, app params (not global state) and local state opt-ins and opt-outs.
	IncludeApps
	// IncludeBudget reports the opcode budget of each program run (statechanges_dev.go).
	IncludeBudget
	// IncludeReads reports the app state each transaction read.
	IncludeReads
	// IncludeTrace reports the execution traces of the group of the one txid,
	// recorded by StateChangesFilter.Trace.
	IncludeTrace
)

// includeGroups are the includes reported per group.
const includeGroups = IncludeAccounts | IncludeAssets | IncludeApps

// AccountChange is an account a group changed. A nil Before: it did not exist
// before the group; a nil After: the group closed it.
//
//msgp:ignore AccountChange
type AccountChange struct {
	Addr          basics.Address
	Before, After *ledgercore.AccountData
}

// HoldingChange is an asset holding a group changed, opted into (nil Before)
// or out of (nil After).
//
//msgp:ignore HoldingChange
type HoldingChange struct {
	Addr          basics.Address
	Asset         basics.AssetIndex
	Before, After *basics.AssetHolding
}

// AssetParamsChange is an asset's params a group changed, created (nil Before)
// or destroyed (nil After).
//
//msgp:ignore AssetParamsChange
type AssetParamsChange struct {
	Asset         basics.AssetIndex
	Creator       basics.Address
	Before, After *basics.AssetParams
}

// AppParamsChange is an app's params a group changed other than its global
// state (left out of Before and After), created (nil Before) or deleted (nil
// After).
//
//msgp:ignore AppParamsChange
type AppParamsChange struct {
	App           basics.AppIndex
	Creator       basics.Address
	Before, After *basics.AppParams
}

// LocalStateChange is a local state a group opted into (nil Before) or out of
// (nil After); the values are its schema. Its keys are in the transactions'
// StateChanges.
//
//msgp:ignore LocalStateChange
type LocalStateChange struct {
	Addr          basics.Address
	App           basics.AppIndex
	Before, After *basics.StateSchema
}

// GroupStateChanges is what a top-level group changed besides app state, each
// with its value right before and right after the group. Only what changed is
// listed; each list is sorted by address, then id (assets' and apps' params by
// id). What the block changes outside its groups (the proposer payout from the
// fee sink, the rewards pool) is in no group.
//
//msgp:ignore GroupStateChanges
type GroupStateChanges struct {
	First, Count int           // payset indexes First to First+Count-1
	GroupID      crypto.Digest // zero for a transaction outside a group
	TxIDs        []transactions.Txid
	Accounts     []AccountChange
	Holdings     []HoldingChange
	Assets       []AssetParamsChange
	Apps         []AppParamsChange
	LocalStates  []LocalStateChange
}

// gKind is the kind of a record in a group's delta.
//
//msgp:ignore gKind
type gKind uint8

const (
	gAcct gKind = iota
	gHolding
	gAsset
	gApp
	gLocal
)

// gKey identifies a record: an account (addr), a holding or local state
// (addr, id), or an asset's or app's params (id).
type gKey struct {
	kind gKind
	addr basics.Address
	id   uint64
}

// gVal is a record's value; ok is false when it does not exist.
type gVal struct {
	ok      bool
	creator basics.Address // gAsset, gApp, even when !ok
	acct    ledgercore.AccountData
	holding basics.AssetHolding
	asset   basics.AssetParams
	app     basics.AppParams // without GlobalState
	local   basics.StateSchema
}

// gValSize is a gVal's share of the tracer's bytes (programs are shared with
// the evaluation, not copied).
const gValSize = int64(unsafe.Sizeof(gVal{}) + unsafe.Sizeof(gKey{}))

// scGroup is a group whose changes are reported.
type scGroup struct {
	first, count int
	id           crypto.Digest
	keys         []gKey
	before       map[gKey]gVal // the latest earlier group's value; absent: read at the round before
	after        map[gKey]gVal
}

// BeforeBlock remembers the block's fee sink.
func (t *stateChangeTracer) BeforeBlock(hdr *bookkeeping.BlockHeader) {
	t.feeSink = hdr.FeeSink
	t.proto = hdr.CurrentProtocol
}

// groupDelta records the values a top-level group left. Inner groups have no
// delta of their own.
func (t *stateChangeTracer) groupDelta(ep *logic.EvalParams, deltas *ledgercore.StateDelta, evalError error) {
	if t.include&includeGroups == 0 || deltas == nil || evalError != nil || t.tooLarge {
		return
	}
	n := len(ep.TxnGroup)
	first := len(t.tops) - n
	if first < 0 || len(t.frames) != 0 {
		t.fail(fmt.Errorf("a group of %d ended with %d top-level transactions begun and %d open", n, len(t.tops), len(t.frames)))
		return
	}
	if t.lastTop >= 0 && first > t.lastTop {
		return // nothing after the last filtered transaction is reported or needed
	}
	var g *scGroup
	if t.groupReported(first, n) {
		g = &scGroup{first: first, count: n, id: ep.TxnGroup[0].Txn.Group,
			before: make(map[gKey]gVal), after: make(map[gKey]gVal)}
		t.groups = append(t.groups, g)
	}
	track := func(k gKey, v gVal) {
		if t.tooLarge {
			return
		}
		prev, seen := t.latest[k]
		if !seen {
			t.grow(gValSize)
			if t.tooLarge {
				return
			}
		}
		t.latest[k] = v
		if g == nil {
			return
		}
		if _, dup := g.after[k]; !dup {
			g.keys = append(g.keys, k)
			if seen {
				g.before[k] = prev
			}
		}
		g.after[k] = v
	}
	if t.include&IncludeAccounts != 0 {
		for i := range deltas.Accts.Accts {
			br := &deltas.Accts.Accts[i]
			track(gKey{kind: gAcct, addr: br.Addr}, gVal{ok: !br.AccountData.IsZero(), acct: br.AccountData})
		}
	}
	if t.include&IncludeAssets != 0 {
		for i := range deltas.Accts.AssetResources {
			r := &deltas.Accts.AssetResources[i]
			if r.AffectsHolding() {
				v := gVal{ok: !r.Holding.Deleted && r.Holding.Holding != nil}
				if v.ok {
					v.holding = *r.Holding.Holding
				}
				track(gKey{kind: gHolding, addr: r.Addr, id: uint64(r.Aidx)}, v)
			}
			if r.AffectsParams() {
				v := gVal{ok: !r.Params.Deleted && r.Params.Params != nil, creator: r.Addr}
				if v.ok {
					v.asset = *r.Params.Params
				}
				track(gKey{kind: gAsset, id: uint64(r.Aidx)}, v)
			}
		}
	}
	if t.include&IncludeApps != 0 {
		for i := range deltas.Accts.AppResources {
			r := &deltas.Accts.AppResources[i]
			if r.AffectsParams() {
				v := gVal{ok: !r.Params.Deleted && r.Params.Params != nil, creator: r.Addr}
				if v.ok {
					v.app = withoutGlobals(r.Params.Params)
				}
				track(gKey{kind: gApp, id: uint64(r.Aidx)}, v)
			}
			if r.AffectsLocals() {
				v := gVal{ok: !r.State.Deleted && r.State.LocalState != nil}
				if v.ok {
					v.local = r.State.LocalState.Schema
				}
				track(gKey{kind: gLocal, addr: r.Addr, id: uint64(r.Aidx)}, v)
			}
		}
	}
}

// groupReported reports whether the group at payset indexes first to
// first+n-1 is in the answer.
func (t *stateChangeTracer) groupReported(first, n int) bool {
	if t.only == nil {
		return true
	}
	for i := first; i < first+n; i++ {
		if t.only[i] {
			return true
		}
	}
	return false
}

// withoutGlobals returns params without their global state; the programs are
// shared, which is safe: nothing writes into them.
func withoutGlobals(p *basics.AppParams) basics.AppParams {
	c := *p
	c.GlobalState = nil
	return c
}

func appParamsEqual(a, b *basics.AppParams) bool {
	return bytes.Equal(a.ApprovalProgram, b.ApprovalProgram) &&
		bytes.Equal(a.ClearStateProgram, b.ClearStateProgram) &&
		a.StateSchemas == b.StateSchemas &&
		a.ExtraProgramPages == b.ExtraProgramPages &&
		a.Version == b.Version &&
		a.SizeSponsor == b.SizeSponsor &&
		a.ForeignBoxReads == b.ForeignBoxReads &&
		a.FamilyBoxAccess == b.FamilyBoxAccess
}

// same reports whether two values of a record of kind k are equal.
func (v *gVal) same(k gKind, w *gVal) bool {
	if v.ok != w.ok {
		return false
	}
	if !v.ok {
		return true
	}
	switch k {
	case gAcct:
		return v.acct == w.acct
	case gHolding:
		return v.holding == w.holding
	case gAsset:
		return v.asset == w.asset
	case gApp:
		return appParamsEqual(&v.app, &w.app)
	case gLocal:
		return v.local == w.local
	}
	return false
}

// read returns a record's value at the round before. An asset's or app's
// params are read from its creator, which a params record of the delta always
// names (after.creator), even for one destroyed or deleted.
func (r *scReplay) read(k gKey, after *gVal) (gVal, error) {
	if (k.kind == gAsset || k.kind == gApp) && after.creator.IsZero() {
		return gVal{}, fmt.Errorf("the params of %d have no creator", k.id)
	}
	var v gVal
	switch k.kind {
	case gAcct:
		ad, _, err := r.base.LookupWithoutRewards(r.rnd, k.addr)
		if err != nil {
			return gVal{}, err
		}
		v = gVal{ok: !ad.IsZero(), acct: ad}
	case gHolding:
		res, err := r.base.LookupAsset(r.rnd, k.addr, basics.AssetIndex(k.id))
		if err != nil {
			return gVal{}, err
		}
		if res.AssetHolding != nil {
			v = gVal{ok: true, holding: *res.AssetHolding}
		}
	case gAsset:
		v.creator = after.creator
		res, err := r.base.LookupAsset(r.rnd, after.creator, basics.AssetIndex(k.id))
		if err != nil {
			return gVal{}, err
		}
		if res.AssetParams != nil {
			v.ok, v.asset = true, *res.AssetParams
		}
	case gApp:
		v.creator = after.creator
		res, err := r.base.LookupApplication(r.rnd, after.creator, basics.AppIndex(k.id))
		if err != nil {
			return gVal{}, err
		}
		if res.AppParams != nil {
			v.ok, v.app = true, withoutGlobals(res.AppParams)
		}
	case gLocal:
		res, err := r.base.LookupApplication(r.rnd, k.addr, basics.AppIndex(k.id))
		if err != nil {
			return gVal{}, err
		}
		if res.AppLocalState != nil {
			v = gVal{ok: true, local: res.AppLocalState.Schema}
		}
	}
	n := gValSize
	switch k.kind {
	case gAsset:
		n += int64(len(v.asset.AssetName) + len(v.asset.UnitName) + len(v.asset.URL))
	case gApp:
		n += int64(len(v.app.ApprovalProgram) + len(v.app.ClearStateProgram))
	}
	if err := r.charge(n); err != nil {
		return gVal{}, err
	}
	return v, nil
}

// Lower bounds of a group's encoded size, as for the transactions' (see
// changeSizeMin): a group its first, count and txids fields (24) and its txids;
// a change its keys (address 32, an id 8); an account state its required
// fields (7 of them, 80 bytes of names alone), a holding 24, asset params 24,
// app params 40 plus programs, a schema 16.
const (
	groupSizeMin        = 24
	accountStateSizeMin = 80
	holdingSizeMin      = 24
	assetSizeMin        = 24
	appSizeMin          = 40
	schemaSizeMin       = 16
)

func groupValueSizeBound(k gKind, v *gVal) uint64 {
	if !v.ok {
		return 0
	}
	switch k {
	case gAcct:
		return accountStateSizeMin
	case gHolding:
		return holdingSizeMin
	case gAsset:
		return assetSizeMin + uint64(len(v.asset.AssetName)+len(v.asset.UnitName)+len(v.asset.URL))
	case gApp:
		return appSizeMin + uint64(len(v.app.ApprovalProgram)+len(v.app.ClearStateProgram))
	case gLocal:
		return schemaSizeMin
	}
	return 0
}

func ptrIf[T any](ok bool, v T) *T {
	if !ok {
		return nil
	}
	return &v
}

// groupChanges builds the reported groups, reading at the round before the
// values no earlier group set. size is the answer's size bound so far.
func (t *stateChangeTracer) groupChanges(ctx context.Context, r *scReplay, f StateChangesFilter, size *uint64) ([]GroupStateChanges, error) {
	out := make([]GroupStateChanges, 0, len(t.groups))
	for _, g := range t.groups {
		gc := GroupStateChanges{First: g.first, Count: g.count, GroupID: g.id}
		for i := g.first; i < g.first+g.count; i++ {
			gc.TxIDs = append(gc.TxIDs, t.tops[i].ID())
		}
		*size += groupSizeMin + uint64(g.count)*txidSize
		if f.MaxBytes > 0 && *size > f.MaxBytes {
			return nil, ErrStateChangesTooLarge
		}
		keys := slices.Clone(g.keys)
		slices.SortFunc(keys, func(a, b gKey) int {
			return cmp.Or(cmp.Compare(a.kind, b.kind), bytes.Compare(a.addr[:], b.addr[:]), cmp.Compare(a.id, b.id))
		})
		for i, k := range keys {
			if i%256 == 0 && ctx.Err() != nil {
				return nil, context.Cause(ctx)
			}
			after := g.after[k]
			before, ok := g.before[k]
			if !ok {
				var err error
				if before, err = r.read(k, &after); err != nil {
					return nil, err
				}
			}
			if before.same(k.kind, &after) {
				continue
			}
			*size += 40 + groupValueSizeBound(k.kind, &before) + groupValueSizeBound(k.kind, &after)
			if f.MaxBytes > 0 && *size > f.MaxBytes {
				return nil, ErrStateChangesTooLarge
			}
			switch k.kind {
			case gAcct:
				gc.Accounts = append(gc.Accounts, AccountChange{Addr: k.addr,
					Before: ptrIf(before.ok, before.acct), After: ptrIf(after.ok, after.acct)})
			case gHolding:
				gc.Holdings = append(gc.Holdings, HoldingChange{Addr: k.addr, Asset: basics.AssetIndex(k.id),
					Before: ptrIf(before.ok, before.holding), After: ptrIf(after.ok, after.holding)})
			case gAsset:
				gc.Assets = append(gc.Assets, AssetParamsChange{Asset: basics.AssetIndex(k.id), Creator: after.creator,
					Before: ptrIf(before.ok, before.asset), After: ptrIf(after.ok, after.asset)})
			case gApp:
				gc.Apps = append(gc.Apps, AppParamsChange{App: basics.AppIndex(k.id), Creator: after.creator,
					Before: ptrIf(before.ok, before.app), After: ptrIf(after.ok, after.app)})
			case gLocal:
				gc.LocalStates = append(gc.LocalStates, LocalStateChange{Addr: k.addr, App: basics.AppIndex(k.id),
					Before: ptrIf(before.ok, before.local), After: ptrIf(after.ok, after.local)})
			}
		}
		out = append(out, gc)
	}
	return out, nil
}
