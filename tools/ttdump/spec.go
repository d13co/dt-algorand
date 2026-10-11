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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/cmd/util/ttview"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// object is a store object, named by a spec ("app:12", "box:12:str:name").
type object struct {
	key   []byte // the store's object key
	kind  byte   // timetravel.Kind*, 0 for a key of no known kind
	addr  basics.Address
	id    uint64               // asset or app
	kv    string               // kv key
	boxed bool                 // the kv key is a box's
	app   uint64               // box: the app
	box   string               // box: the name
	ct    basics.CreatableType // creator index entry
}

const specHelp = `objects:
  acct:ADDR (or ADDR)        an account's base record
  holding:ADDR:ASSET         an asset holding
  local:ADDR:APP             an app local state
  asset:ID, app:ID           asset or app params
  box:APP:NAME               a box; NAME as in goal: str:, b64:, int:, addr:, ...
  kv:NAME                    a kv entry by its raw key, encoded likewise
  totals                     the account totals
  created:ADDR:asset:ID      a creator index entry (or created:ADDR:app:ID)
  hex:HEX                    any object key`

var specForms = map[string]string{
	"acct": "acct:ADDR", "holding": "holding:ADDR:ASSET", "local": "local:ADDR:APP",
	"asset": "asset:ID", "app": "app:ID", "box": "box:APP:NAME", "kv": "kv:NAME", "totals": "totals",
	"created": "created:ADDR:asset:ID or created:ADDR:app:ID", "hex": "hex:HEX",
}

// parseSpec parses an object spec.
func parseSpec(s string) (object, error) {
	kind, rest, _ := strings.Cut(s, ":")
	parts := strings.SplitN(rest, ":", 2) // the last part may hold colons (box names)
	bad := func(err error) (object, error) {
		if err == nil {
			err = fmt.Errorf("expected %s", specForms[kind])
		}
		return object{}, fmt.Errorf("bad object %q: %w", s, err)
	}
	var o object
	var err error
	switch kind {
	case "acct":
		if o.addr, err = basics.UnmarshalChecksumAddress(rest); err != nil {
			return bad(err)
		}
		o.kind, o.key = timetravel.KindAccount, timetravel.AccountKey(o.addr)
	case "holding", "local":
		if len(parts) != 2 {
			return bad(nil)
		}
		if o.addr, err = basics.UnmarshalChecksumAddress(parts[0]); err != nil {
			return bad(err)
		}
		if o.id, err = strconv.ParseUint(parts[1], 10, 64); err != nil {
			return bad(err)
		}
		if kind == "holding" {
			o.kind, o.key = timetravel.KindHolding, timetravel.HoldingKey(o.addr, basics.AssetIndex(o.id))
		} else {
			o.kind, o.key = timetravel.KindLocal, timetravel.LocalKey(o.addr, basics.AppIndex(o.id))
		}
	case "asset", "app":
		if o.id, err = strconv.ParseUint(rest, 10, 64); err != nil {
			return bad(err)
		}
		if kind == "asset" {
			o.kind, o.key = timetravel.KindAssetParams, timetravel.AssetParamsKey(basics.AssetIndex(o.id))
		} else {
			o.kind, o.key = timetravel.KindAppParams, timetravel.AppParamsKey(basics.AppIndex(o.id))
		}
	case "box":
		if len(parts) != 2 {
			return bad(nil)
		}
		if o.app, err = strconv.ParseUint(parts[0], 10, 64); err != nil {
			return bad(err)
		}
		if o.box, err = appCallBytes(parts[1]); err != nil {
			return bad(err)
		}
		o.kv, o.boxed = apps.MakeBoxKey(o.app, o.box), true
		o.kind, o.key = timetravel.KindKv, timetravel.KvKey(o.kv)
	case "kv":
		if o.kv, err = appCallBytes(rest); err != nil {
			return bad(err)
		}
		o.kind, o.key = timetravel.KindKv, timetravel.KvKey(o.kv)
		o.splitBox()
	case "totals":
		if rest != "" {
			return bad(nil)
		}
		o.kind, o.key = timetravel.KindTotals, timetravel.TotalsKey
	case "created":
		ps := strings.Split(rest, ":")
		if len(ps) != 3 || (ps[1] != "asset" && ps[1] != "app") {
			return bad(nil)
		}
		if o.addr, err = basics.UnmarshalChecksumAddress(ps[0]); err != nil {
			return bad(err)
		}
		if o.id, err = strconv.ParseUint(ps[2], 10, 64); err != nil {
			return bad(err)
		}
		o.ct = basics.AssetCreatable
		if ps[1] == "app" {
			o.ct = basics.AppCreatable
		}
		o.kind, o.key = timetravel.KindCreated, timetravel.CreatedKey(o.addr, o.ct, basics.CreatableIndex(o.id))
	case "hex":
		key, err := hex.DecodeString(rest)
		if err != nil || len(key) == 0 {
			return bad(err)
		}
		return objectOf(key), nil
	default:
		if len(s) == 58 && !strings.Contains(s, ":") { // a bare address
			return parseSpec("acct:" + s)
		}
		return object{}, fmt.Errorf("bad object %q\n%s", s, specHelp)
	}
	return o, nil
}

func appCallBytes(s string) (string, error) {
	acb, err := apps.NewAppCallBytes(s)
	if err != nil {
		return "", err
	}
	b, err := acb.Raw()
	return string(b), err
}

// objectOf decodes a store object key. A key of no known kind, or of the
// wrong length for its kind, is an object of kind 0, printed as hex:.
func objectOf(key []byte) object {
	o := object{key: key}
	n := len(key)
	if n == 0 {
		return o
	}
	const al = len(basics.Address{})
	id := func() uint64 { return binary.BigEndian.Uint64(key[n-8:]) }
	switch key[0] {
	case timetravel.KindAccount:
		if n == 1+al {
			copy(o.addr[:], key[1:])
			o.kind = key[0]
		}
	case timetravel.KindHolding, timetravel.KindLocal:
		if n == 1+al+8 {
			copy(o.addr[:], key[1:])
			o.kind, o.id = key[0], id()
		}
	case timetravel.KindAssetParams, timetravel.KindAppParams:
		if n == 1+8 {
			o.kind, o.id = key[0], id()
		}
	case timetravel.KindKv:
		if n >= 3 && key[n-2] == 0 && key[n-1] == 0 {
			o.kind, o.kv = key[0], timetravel.KvKeyString(key)
			o.splitBox()
		}
	case timetravel.KindTotals:
		if n == 1 {
			o.kind = key[0]
		}
	case timetravel.KindCreated:
		if n == 1+al+1+8 {
			copy(o.addr[:], key[1:])
			o.kind, o.ct, o.id = key[0], basics.CreatableType(key[1+al]), id()
		}
	}
	return o
}

func (o *object) splitBox() {
	if app, name, err := apps.SplitBoxKey(o.kv); err == nil {
		o.app, o.box, o.boxed = app, name, true
	}
}

// String is the object's spec.
func (o object) String() string {
	switch o.kind {
	case timetravel.KindAccount:
		return "acct:" + o.addr.String()
	case timetravel.KindHolding:
		return fmt.Sprintf("holding:%s:%d", o.addr, o.id)
	case timetravel.KindLocal:
		return fmt.Sprintf("local:%s:%d", o.addr, o.id)
	case timetravel.KindAssetParams:
		return fmt.Sprintf("asset:%d", o.id)
	case timetravel.KindAppParams:
		return fmt.Sprintf("app:%d", o.id)
	case timetravel.KindKv:
		if o.boxed {
			return fmt.Sprintf("box:%d:%s", o.app, ttview.AppCallBytes([]byte(o.box)))
		}
		return "kv:" + ttview.AppCallBytes([]byte(o.kv))
	case timetravel.KindTotals:
		return "totals"
	case timetravel.KindCreated:
		t := "asset"
		if o.ct == basics.AppCreatable {
			t = "app"
		}
		return fmt.Sprintf("created:%s:%s:%d", o.addr, t, o.id)
	}
	return "hex:" + hex.EncodeToString(o.key)
}
