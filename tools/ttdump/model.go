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

package main

import (
	"encoding/json"
	"fmt"

	"github.com/algorand/msgp/msgp"

	"github.com/algorand/go-algorand/cmd/util/ttview"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// createdEntry is the value of a creator index entry, which the store keeps
// as an empty record.
type createdEntry struct {
	Creator string `json:"creator"`
	Type    string `json:"type"`
	ID      uint64 `json:"id"`
}

// rawValue is the value of an object of no known kind.
type rawValue struct {
	Value []byte `json:"value"`
}

// model decodes val, the value of o at round rnd, into what algod's REST API
// returns for it: model.Account (the base record, with pending rewards),
// model.AssetHolding, model.ApplicationLocalState, model.Asset,
// model.Application or model.Box. The totals are ledgercore.AccountTotals in
// codec JSON, as /v2/ledger/supply cannot show them.
func (e *env) model(o object, val []byte, rnd basics.Round) (any, error) {
	switch o.kind {
	case timetravel.KindAccount:
		var ba trackerdb.BaseAccountData
		if err := protocol.Decode(val, &ba); err != nil {
			return nil, err
		}
		hdr, proto, err := e.consensus(rnd)
		if err != nil {
			return nil, err
		}
		ad := ba.GetLedgerCoreAccountData()
		without := ad.MicroAlgos
		ad = ad.WithUpdatedRewards(proto.RewardUnit, hdr.RewardsLevel)
		return v2.BaseAccount(o.addr, ad, rnd, &proto, without)
	case timetravel.KindHolding:
		var h basics.AssetHolding
		if err := protocol.Decode(val, &h); err != nil {
			return nil, err
		}
		return v2.AssetHolding(h, basics.AssetIndex(o.id)), nil
	case timetravel.KindLocal:
		var ls basics.AppLocalState
		if err := protocol.Decode(val, &ls); err != nil {
			return nil, err
		}
		return v2.AppLocalState(ls, basics.AppIndex(o.id)), nil
	case timetravel.KindAssetParams:
		var p basics.AssetParams
		creator, err := decodeParams(val, &p)
		if err != nil {
			return nil, err
		}
		return v2.AssetParamsToAsset(creator.String(), basics.AssetIndex(o.id), &p), nil
	case timetravel.KindAppParams:
		var p basics.AppParams
		creator, err := decodeParams(val, &p)
		if err != nil {
			return nil, err
		}
		return v2.AppParamsToApplication(creator.String(), basics.AppIndex(o.id), &p), nil
	case timetravel.KindKv:
		name := o.kv
		if o.boxed {
			name = o.box
		}
		return model.Box{Name: []byte(name), Round: rnd, Value: val}, nil
	case timetravel.KindTotals:
		var t ledgercore.AccountTotals
		if err := protocol.Decode(val, &t); err != nil {
			return nil, err
		}
		return json.RawMessage(protocol.EncodeJSON(&t)), nil
	case timetravel.KindCreated:
		t := "asset"
		if o.ct == basics.AppCreatable {
			t = "app"
		}
		return createdEntry{Creator: o.addr.String(), Type: t, ID: o.id}, nil
	}
	return rawValue{Value: val}, nil
}

// decodeParams decodes an asset or app params record: creator, then params.
func decodeParams(val []byte, p msgp.Unmarshaler) (basics.Address, error) {
	var creator basics.Address
	if len(val) < len(creator) {
		return creator, fmt.Errorf("short params record")
	}
	copy(creator[:], val)
	return creator, protocol.Decode(val[len(creator):], p)
}

// valueAt returns the model of o at round rnd, or nil if o did not exist.
func (e *env) valueAt(o object, rnd basics.Round) (any, error) {
	val, ok, err := e.store.Get(o.key, uint64(rnd))
	if err != nil || !ok {
		return nil, err
	}
	return e.model(o, val, rnd)
}

// viewOptions tells ttview how to show the values of o's kind.
func viewOptions(o object) ttview.Options {
	switch o.kind {
	case timetravel.KindKv:
		return ttview.Options{Bytes: []string{"name", "value"}}
	case timetravel.KindAssetParams, timetravel.KindAppParams:
		return ttview.Options{Trim: "params."}
	case 0:
		return ttview.Options{Bytes: []string{"value"}}
	}
	return ttview.Options{}
}
