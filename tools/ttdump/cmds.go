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
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/cmd/util/ttview"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// kinds are the object kinds, in key order, with their names.
var kinds = []struct {
	kind byte
	name string
}{
	{timetravel.KindAssetParams, "asset params"},
	{timetravel.KindCreated, "creator index"},
	{timetravel.KindAppParams, "app params"},
	{timetravel.KindTotals, "totals"},
	{timetravel.KindAccount, "accounts"},
	{timetravel.KindHolding, "holdings"},
	{timetravel.KindKv, "kv (boxes)"},
	{timetravel.KindLocal, "local states"},
}

func kindName(k byte) string {
	for _, kn := range kinds {
		if kn.kind == k {
			return kn.name
		}
	}
	return fmt.Sprintf("kind %q", k)
}

// keyspaces are the store's record keyspaces besides the meta record.
var keyspaces = []struct {
	pfx       byte
	name      string
	versioned bool // keyed by object, then round
}{
	{'E', "expired online stake", false},
	{'H', "headers and digests", false},
	{'a', "asset holders index", false},
	{'b', "box index", false},
	{'c', "versions", true},
}

type estimate struct {
	Keyspace string `json:"keyspace"`
	Kind     string `json:"kind,omitempty"`
	Name     string `json:"name"`
	Bytes    uint64 `json:"bytes"`
}

type infoOut struct {
	Path           string     `json:"path"`
	First          uint64     `json:"first-round"`
	Last           uint64     `json:"last-round"`
	FirstTimestamp int64      `json:"first-timestamp"`
	LastTimestamp  int64      `json:"last-timestamp"`
	DiskBytes      int64      `json:"disk-bytes"`
	BoxIndexFrom   *uint64    `json:"box-index-from,omitempty"` // absent while it is rebuilt
	HoldersIndex   string     `json:"holders-index"`            // complete, stale or missing
	Estimates      []estimate `json:"estimates"`
}

func cmdInfo(e *env, args []string) error {
	if _, err := parse(e.newFlags("info"), args, 0, 0); err != nil {
		return err
	}
	out := infoOut{Path: e.path, First: uint64(e.first), Last: uint64(e.last),
		HoldersIndex: e.store.HoldersIndexState().String()}
	if from, ok := e.store.BoxIndexFrom(); ok {
		out.BoxIndexFrom = &from
	}
	if hdr, err := e.header(e.first); err == nil {
		out.FirstTimestamp = hdr.TimeStamp
	}
	if hdr, err := e.header(e.last); err == nil {
		out.LastTimestamp = hdr.TimeStamp
	}
	var err error
	if out.DiskBytes, err = dirSize(e.path); err != nil {
		return err
	}
	db := e.store.DB()
	for _, ks := range keyspaces {
		if !ks.versioned {
			n, err := db.EstimateDiskUsage([]byte{ks.pfx}, []byte{ks.pfx + 1})
			if err != nil {
				return err
			}
			out.Estimates = append(out.Estimates, estimate{Keyspace: string(ks.pfx), Name: ks.name, Bytes: n})
			continue
		}
		for _, k := range kinds {
			n, err := db.EstimateDiskUsage([]byte{ks.pfx, k.kind}, []byte{ks.pfx, k.kind + 1})
			if err != nil {
				return err
			}
			out.Estimates = append(out.Estimates, estimate{Keyspace: string(ks.pfx), Kind: string(k.kind), Name: k.name, Bytes: n})
		}
	}
	if e.json {
		return e.emit(out)
	}
	fmt.Fprintf(e.out, "store        %s\n", out.Path)
	fmt.Fprintf(e.out, "rounds       %d..%d (%s .. %s)\n", out.First, out.Last, e.when(e.first), e.when(e.last))
	fmt.Fprintf(e.out, "disk         %s\n", sizeString(uint64(out.DiskBytes)))
	switch from := out.BoxIndexFrom; {
	case from == nil:
		fmt.Fprint(e.out, "box index    being rebuilt (ttimport -index-boxes)\n")
	case *from > out.First:
		fmt.Fprintf(e.out, "box index    from round %d (ttimport -index-boxes builds the rest)\n", *from)
	default:
		fmt.Fprint(e.out, "box index    complete\n")
	}
	switch e.store.HoldersIndexState() {
	case timetravel.HoldersComplete:
		fmt.Fprint(e.out, "holders      index complete\n\n")
	case timetravel.HoldersStale:
		fmt.Fprint(e.out, "holders      index stale: a build without it added rounds, or its meta record is bad (ttimport -holders-index rebuilds it)\n\n")
	default:
		fmt.Fprint(e.out, "holders      no index (TimeTravelHoldersIndex, ttimport -holders-index)\n\n")
	}
	rows := make([][]string, 0, len(out.Estimates))
	for _, est := range out.Estimates {
		rows = append(rows, []string{est.Keyspace, est.Kind, est.Name, sizeString(est.Bytes)})
	}
	fmt.Fprintln(e.out, "estimated disk use (sstables only, not the WAL or memtables):")
	return ttview.WriteTable(e.out, []string{"keyspace", "kind", "contents", "disk"}, rows)
}

func sizeString(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.2f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.1f MiB", float64(b)/(1<<20))
	case b >= 1<<10:
		return fmt.Sprintf("%.1f KiB", float64(b)/(1<<10))
	}
	return fmt.Sprintf("%d B", b)
}

// dirSize returns the total size of the files in dir.
func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err == nil {
			n += fi.Size()
		}
		return err
	})
	return n, err
}

type getOut struct {
	Object    string `json:"object"`
	Round     uint64 `json:"round"`
	ChangedAt uint64 `json:"changed-at,omitempty"`
	Deleted   bool   `json:"deleted,omitempty"`
	CreatedAt uint64 `json:"created-at,omitempty"` // of an object absent at round
	Value     any    `json:"value"`
}

func cmdGet(e *env, args []string) error {
	fs := e.newFlags("get")
	var r roundFlag
	fs.Var(&r, "r", "round (default: the last)")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	o, err := parseSpec(pos[0])
	if err != nil {
		return usageError{err}
	}
	rnd := r.or(e.last)
	if err = e.checkRound(rnd); err != nil {
		return err
	}
	out := getOut{Object: o.String(), Round: uint64(rnd)}
	// the latest version at or before rnd
	found := false
	err = e.store.Versions(o.key, 0, uint64(rnd), true, func(v uint64, _ []byte, ok bool) bool {
		found, out.ChangedAt, out.Deleted = true, v, !ok
		return false
	})
	if err != nil {
		return err
	}
	if found && !out.Deleted {
		if out.Value, err = e.valueAt(o, rnd); err != nil {
			return err
		}
	} else if rnd < e.last {
		err = e.store.Versions(o.key, uint64(rnd)+1, uint64(e.last), false, func(v uint64, _ []byte, ok bool) bool {
			if ok {
				out.CreatedAt = v
			}
			return !ok
		})
		if err != nil {
			return err
		}
	}
	if e.json {
		return e.emit(out)
	}
	fmt.Fprintf(e.out, "%s at round %d (%s): ", out.Object, rnd, e.when(rnd))
	switch {
	case !found && out.CreatedAt > 0:
		fmt.Fprintf(e.out, "absent; created at round %d\n", out.CreatedAt)
	case !found:
		fmt.Fprintf(e.out, "absent; no version up to round %d\n", e.last)
	case out.Deleted:
		fmt.Fprintf(e.out, "deleted at round %d (%s)", out.ChangedAt, e.when(basics.Round(out.ChangedAt)))
		if out.CreatedAt > 0 {
			fmt.Fprintf(e.out, "; created again at round %d", out.CreatedAt)
		}
		fmt.Fprintln(e.out)
	default:
		fmt.Fprintf(e.out, "changed at round %d (%s)\n", out.ChangedAt, e.when(basics.Round(out.ChangedAt)))
		return e.writeValue(o, out.Value)
	}
	return nil
}

// writeValue writes a model as indented "path  value" lines.
func (e *env) writeValue(o object, v any) error {
	fields, err := ttview.Flatten(v, viewOptions(o))
	if err != nil {
		return err
	}
	return ttview.WriteFields(e.out, fields, "  ")
}

// parseAddr parses an address, bare or as acct:ADDR.
func parseAddr(s string) (basics.Address, error) {
	a, err := basics.UnmarshalChecksumAddress(strings.TrimPrefix(s, "acct:"))
	if err != nil {
		return a, usagef("bad address %q: %v", s, err)
	}
	return a, nil
}

// parseExclude parses -exclude like GET /v2/accounts/{address} does.
func parseExclude(s string) (ledger.AccountVersionsExclude, error) {
	var ex ledger.AccountVersionsExclude
	switch s {
	case "", "none":
		return ex, nil
	case "all":
		ex.All = true
		return ex, nil
	}
	for v := range strings.SplitSeq(s, ",") {
		switch v {
		case "created-apps-params":
			ex.CreatedAppsParams = true
		case "created-assets-params":
			ex.CreatedAssetsParams = true
		default:
			return ex, usagef("bad -exclude value %q", v)
		}
	}
	return ex, nil
}

func cmdAccount(e *env, args []string) error {
	fs := e.newFlags("account")
	var r roundFlag
	fs.Var(&r, "r", "round (default: the last)")
	exclude := fs.String("exclude", "", "all, or created-apps-params and/or created-assets-params, comma-separated")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	addr, err := parseAddr(pos[0])
	if err != nil {
		return err
	}
	ex, err := parseExclude(*exclude)
	if err != nil {
		return err
	}
	rnd := r.or(e.last)
	if err = e.checkRound(rnd); err != nil {
		return err
	}
	reader, err := e.historyReader()
	if err != nil {
		return err
	}
	_, proto, err := e.consensus(rnd)
	if err != nil {
		return err
	}
	var acct model.Account
	if ex.All {
		ad, without, err := reader.LookupAccountAt(rnd, addr)
		if err != nil {
			return err
		}
		if acct, err = v2.BaseAccount(addr, ad, rnd, &proto, without); err != nil {
			return err
		}
	} else {
		ad, _, without, err := reader.LookupLatestAt(rnd, addr)
		if err != nil {
			return err
		}
		acct, err = v2.AccountDataToAccount(addr.String(), &ad, rnd, &proto, without, v2.AccountDataToAccountOptions{
			ExcludeCreatedAppsParams:   ex.CreatedAppsParams,
			ExcludeCreatedAssetsParams: ex.CreatedAssetsParams,
		})
		if err != nil {
			return err
		}
	}
	if e.json {
		return e.emit(acct)
	}
	fmt.Fprintf(e.out, "acct:%s at round %d (%s)\n", addr, rnd, e.when(rnd))
	return e.writeValue(object{kind: timetravel.KindAccount}, acct)
}

// versionsPage is how many versions a page of the ledger's versions reads.
const versionsPage = 1000

// versionEntry is one object's change in a version, for the text history.
type versionEntry struct {
	obj     object
	deleted bool
	value   any // nil when deleted or not requested
}

func cmdVersions(e *env, args []string) error {
	fs := e.newFlags("versions")
	var from, to roundFlag
	fs.Var(&from, "from", "first round (default: the store's first)")
	fs.Var(&to, "to", "last round (default: the store's last)")
	desc := fs.Bool("desc", false, "newest first (prints whole values)")
	n := fs.Uint64("n", 0, "at most this many versions (0: all)")
	full := fs.Bool("full", false, "print whole values instead of changes")
	exclude := fs.String("exclude", "", "for accounts: all, or created-apps-params and/or created-assets-params")
	pos, err := parse(fs, args, 1, 1)
	if err != nil {
		return err
	}
	var o object
	if a, ok := strings.CutPrefix(pos[0], "boxes:"); ok {
		o = object{kind: timetravel.KindKv, appBoxes: true}
		if o.app, err = strconv.ParseUint(a, 10, 64); err != nil {
			return usagef("bad object %q: expected boxes:APP", pos[0])
		}
	} else if o, err = parseSpec(pos[0]); err != nil {
		return usageError{err}
	}
	ex, err := parseExclude(*exclude)
	if err != nil {
		return err
	}
	// nothing exists before the store's first round
	lo, hi := max(from.or(e.first), e.first), to.or(e.last)
	if err = e.checkRound(hi); err != nil {
		return err
	}
	if lo > hi {
		return usagef("-from %d is after -to %d", lo, hi)
	}
	h := &ttview.History{W: e.out, Full: *full || *desc}
	seen := map[string]bool{}
	// add writes an entry to the history, after its object's value before lo
	add := func(rnd basics.Round, ts int64, en versionEntry) error {
		label := en.obj.String()
		h.Opts = viewOptions(en.obj)
		if !seen[label] && !h.Full {
			seen[label] = true
			var before any // absent before the store's first round
			if lo > e.first {
				v, verr := e.valueAt(en.obj, lo-1)
				if verr != nil {
					return verr
				}
				before = v
			}
			if berr := h.Baseline(label, before); berr != nil {
				return berr
			}
		}
		return h.Add(uint64(rnd), ts, label, en.deleted, en.value)
	}
	count := uint64(0)
	more := func() bool { return *n == 0 || count < *n }

	switch o.kind {
	case timetravel.KindTotals, timetravel.KindCreated, 0:
		// not served by the REST API: the store's versions as they are
		var ferr error
		err = e.store.Versions(o.key, uint64(lo), uint64(hi), *desc, func(rnd uint64, val []byte, ok bool) bool {
			hdr, herr := e.header(basics.Round(rnd))
			if herr != nil {
				ferr = herr
				return false
			}
			en := versionEntry{obj: o, deleted: !ok}
			if ok {
				if en.value, ferr = e.model(o, val, basics.Round(rnd)); ferr != nil {
					return false
				}
			}
			if e.json {
				ferr = e.emit(struct {
					Round     uint64 `json:"round"`
					Timestamp int64  `json:"timestamp"`
					Deleted   bool   `json:"deleted"`
					Value     any    `json:"value,omitempty"`
				}{rnd, hdr.TimeStamp, !ok, en.value})
			} else {
				ferr = add(basics.Round(rnd), hdr.TimeStamp, en)
			}
			count++
			return ferr == nil && more()
		})
		if err == nil {
			err = ferr
		}
		return err
	}

	reader, err := e.historyReader()
	if err != nil {
		return err
	}
	var page func(ledger.VersionRange) (ledger.VersionPage, error)
	switch o.kind {
	case timetravel.KindAccount:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) { return reader.AccountVersions(o.addr, ex, r) }
	case timetravel.KindHolding:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) {
			return reader.AccountAssetVersions(o.addr, basics.AssetIndex(o.id), r)
		}
	case timetravel.KindLocal:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) {
			return reader.AccountAppVersions(o.addr, basics.AppIndex(o.id), r)
		}
	case timetravel.KindAssetParams:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) {
			return reader.AssetVersions(basics.AssetIndex(o.id), r)
		}
	case timetravel.KindAppParams:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) {
			return reader.AppVersions(basics.AppIndex(o.id), r)
		}
	case timetravel.KindKv:
		page = func(r ledger.VersionRange) (ledger.VersionPage, error) { return reader.KvVersions(o.kv, r) }
		if o.appBoxes {
			page = func(r ledger.VersionRange) (ledger.VersionPage, error) {
				return reader.AppBoxVersions(basics.AppIndex(o.app), r)
			}
		}
	}
	vr := ledger.VersionRange{Min: lo, Max: hi, Desc: *desc, Limit: versionsPage, Values: true}
	for more() {
		p, err := page(vr)
		if err != nil {
			return err
		}
		for _, v := range p.Versions {
			if !more() {
				break
			}
			count++
			if err := e.version(o, v, add); err != nil {
				return err
			}
		}
		if !p.More {
			break
		}
		if *desc {
			vr.Max = p.Next
		} else {
			vr.Min = p.Next
		}
	}
	return nil
}

// version writes a version from the ledger: as the REST API's JSON, or its
// objects' changes through add.
func (e *env) version(o object, v ledger.Version, add func(basics.Round, int64, versionEntry) error) error {
	switch o.kind {
	case timetravel.KindAccount, timetravel.KindHolding, timetravel.KindLocal:
		mv, err := v2.AccountVersionToModel(o.addr, v, false)
		if err != nil {
			return err
		}
		if e.json {
			return e.emit(mv)
		}
		var ens []versionEntry
		if b := mv.Base; b != nil {
			var val any
			if b.Account != nil {
				val = b.Account
			}
			ens = append(ens, versionEntry{object{kind: timetravel.KindAccount, addr: o.addr, key: timetravel.AccountKey(o.addr)}, b.Deleted, val})
		}
		for _, h := range ptrSlice(mv.Assets) {
			ob := object{kind: timetravel.KindHolding, addr: o.addr, id: uint64(h.AssetId), key: timetravel.HoldingKey(o.addr, h.AssetId)}
			ens = append(ens, versionEntry{ob, h.Deleted, ptrAny(h.AssetHolding)})
		}
		for _, l := range ptrSlice(mv.AppsLocalState) {
			ob := object{kind: timetravel.KindLocal, addr: o.addr, id: uint64(l.AppId), key: timetravel.LocalKey(o.addr, l.AppId)}
			ens = append(ens, versionEntry{ob, l.Deleted, ptrAny(l.AppLocalState)})
		}
		for _, a := range ptrSlice(mv.CreatedAssets) {
			ob := object{kind: timetravel.KindAssetParams, id: uint64(a.AssetId), key: timetravel.AssetParamsKey(a.AssetId)}
			ens = append(ens, versionEntry{ob, a.Deleted, ptrAny(a.Asset)})
		}
		for _, a := range ptrSlice(mv.CreatedApps) {
			ob := object{kind: timetravel.KindAppParams, id: uint64(a.AppId), key: timetravel.AppParamsKey(a.AppId)}
			ens = append(ens, versionEntry{ob, a.Deleted, ptrAny(a.Application)})
		}
		for _, en := range ens {
			if err := add(v.Round, v.Timestamp, en); err != nil {
				return err
			}
		}
		return nil
	case timetravel.KindAssetParams:
		mv, err := v2.AssetVersionToModel(v)
		if err != nil || e.json {
			return errOr(err, e.emit, mv)
		}
		return add(v.Round, v.Timestamp, versionEntry{o, mv.Deleted, ptrAny(mv.Asset)})
	case timetravel.KindAppParams:
		mv, err := v2.ApplicationVersionToModel(v, false)
		if err != nil || e.json {
			return errOr(err, e.emit, mv)
		}
		return add(v.Round, v.Timestamp, versionEntry{o, mv.Deleted, ptrAny(mv.Application)})
	case timetravel.KindKv:
		if o.appBoxes {
			mv := v2.BoxesVersionToModel(v, true)
			if e.json {
				return e.emit(mv)
			}
			for _, b := range mv.Boxes {
				ob := object{kind: timetravel.KindKv, kv: apps.MakeBoxKey(o.app, string(b.Name))}
				ob.key = timetravel.KvKey(ob.kv)
				ob.splitBox()
				var val any
				if b.Value != nil {
					val = model.Box{Name: b.Name, Round: v.Round, Value: *b.Value}
				}
				if err := add(v.Round, v.Timestamp, versionEntry{ob, b.Deleted, val}); err != nil {
					return err
				}
			}
			return nil
		}
		mv, err := v2.BoxVersionToModel(v, true)
		if err != nil || e.json {
			return errOr(err, e.emit, mv)
		}
		var val any
		if mv.Value != nil {
			name := o.kv
			if o.boxed {
				name = o.box
			}
			val = model.Box{Name: []byte(name), Round: v.Round, Value: *mv.Value}
		}
		return add(v.Round, v.Timestamp, versionEntry{o, mv.Deleted, val})
	}
	return fmt.Errorf("no versions for %s", o)
}

func errOr(err error, emit func(any) error, v any) error {
	if err != nil {
		return err
	}
	return emit(v)
}

func ptrSlice[T any](p *[]T) []T {
	if p == nil {
		return nil
	}
	return *p
}

// ptrAny returns p as an any, nil (not a typed nil) if p is nil.
func ptrAny[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

// lsKinds are what ls lists: the object key prefix, from the argument if any.
var lsKinds = map[string]struct {
	arg    string // the argument it takes, if any
	prefix func(arg string) ([]byte, error)
}{
	"accounts": {"", func(string) ([]byte, error) { return []byte{timetravel.KindAccount}, nil }},
	"assets":   {"", func(string) ([]byte, error) { return []byte{timetravel.KindAssetParams}, nil }},
	"apps":     {"", func(string) ([]byte, error) { return []byte{timetravel.KindAppParams}, nil }},
	"boxes": {"APP", func(a string) ([]byte, error) {
		app, err := strconv.ParseUint(a, 10, 64)
		return timetravel.KvPrefix(apps.MakeBoxKey(app, "")), err
	}},
	"kv": {"[PREFIX]", func(a string) ([]byte, error) {
		p, err := appCallBytes(a)
		if a == "" {
			p, err = "", nil
		}
		return timetravel.KvPrefix(p), err
	}},
	"holdings": {"ADDR", func(a string) ([]byte, error) {
		addr, err := parseAddr(a)
		return timetravel.HoldingKey(addr, 0)[:1+len(addr)], err
	}},
	"locals": {"ADDR", func(a string) ([]byte, error) {
		addr, err := parseAddr(a)
		return timetravel.LocalKey(addr, 0)[:1+len(addr)], err
	}},
	"created": {"ADDR", func(a string) ([]byte, error) {
		addr, err := parseAddr(a)
		return append([]byte{timetravel.KindCreated}, addr[:]...), err
	}},
}

// lsFlushRows bounds the rows a text listing aligns at once.
const lsFlushRows = 1000

func cmdLs(e *env, args []string) error {
	fs := e.newFlags("ls")
	var r roundFlag
	fs.Var(&r, "r", "round (default: the last)")
	ever := fs.Bool("ever", false, "every object that ever had a record, with its first and last round")
	after := fs.String("after", "", "start after this object")
	n := fs.Uint64("n", 1000, "at most this many objects (0: all)")
	pos, err := parse(fs, args, 1, 2)
	if err != nil {
		return err
	}
	lk, ok := lsKinds[pos[0]]
	if !ok {
		return usagef("ls lists accounts, assets, apps, boxes APP, kv [PREFIX], holdings ADDR, locals ADDR or created ADDR, not %q", pos[0])
	}
	arg := ""
	if len(pos) == 2 {
		arg = pos[1]
	}
	if (lk.arg == "") != (arg == "") && lk.arg != "[PREFIX]" {
		return usagef("ls %s takes %s", pos[0], lk.arg)
	}
	prefix, err := lk.prefix(arg)
	if err != nil {
		return usageError{err}
	}
	var afterKey []byte
	if *after != "" {
		var o object
		if o, err = parseSpec(*after); err != nil {
			return usageError{err}
		}
		afterKey = o.key
	}
	if *ever {
		return lsEver(e, prefix, afterKey, *n)
	}
	rnd := r.or(e.last)
	if err = e.checkRound(rnd); err != nil {
		return err
	}
	t := &table{w: e.out}
	count := uint64(0)
	var last object
	truncated := false
	var ferr error
	err = e.store.Scan(prefix, afterKey, uint64(rnd), func(obj, val []byte) bool {
		if *n > 0 && count == *n {
			truncated = true
			return false
		}
		o := objectOf(obj)
		m, merr := e.model(o, val, rnd)
		if merr != nil {
			ferr = fmt.Errorf("%s: %w", o, merr)
			return false
		}
		count++
		last = o
		if e.json {
			ferr = e.emit(m)
		} else {
			ferr = t.add(lsRow(o, m))
		}
		return ferr == nil
	})
	if err == nil {
		err = ferr
	}
	if err == nil && !e.json {
		if count == 0 {
			fmt.Fprintf(e.out, "nothing at round %d\n", rnd)
		}
		err = t.flush()
	}
	if err == nil && truncated {
		e.more(last.String())
	}
	return err
}

// lsRow is the table row of a listed object, and the table's header.
func lsRow(o object, m any) ([]string, []string) {
	switch v := m.(type) {
	case model.Account:
		return []string{"address", "amount", "status", "assets", "apps", "created assets", "created apps"},
			[]string{v.Address, strconv.FormatUint(v.Amount, 10), v.Status, strconv.FormatUint(v.TotalAssetsOptedIn, 10),
				strconv.FormatUint(v.TotalAppsOptedIn, 10), strconv.FormatUint(v.TotalCreatedAssets, 10), strconv.FormatUint(v.TotalCreatedApps, 10)}
	case model.AssetHolding:
		return []string{"asset", "amount", "frozen"},
			[]string{strconv.FormatUint(uint64(v.AssetID), 10), strconv.FormatUint(v.Amount, 10), strconv.FormatBool(v.IsFrozen)}
	case model.ApplicationLocalState:
		keys := 0
		if v.KeyValue != nil {
			keys = len(*v.KeyValue)
		}
		return []string{"app", "keys", "schema uints", "schema bytes"},
			[]string{strconv.FormatUint(uint64(v.Id), 10), strconv.Itoa(keys), strconv.FormatUint(v.Schema.NumUint, 10), strconv.FormatUint(v.Schema.NumByteSlice, 10)}
	case model.Asset:
		return []string{"asset", "creator", "unit", "name", "total", "decimals"},
			[]string{strconv.FormatUint(uint64(v.Index), 10), v.Params.Creator, deref(v.Params.UnitName), deref(v.Params.Name),
				strconv.FormatUint(v.Params.Total, 10), strconv.FormatUint(v.Params.Decimals, 10)}
	case model.Application:
		keys := 0
		if v.Params.GlobalState != nil {
			keys = len(*v.Params.GlobalState)
		}
		return []string{"app", "creator", "global keys", "approval bytes"},
			[]string{strconv.FormatUint(uint64(v.Id), 10), v.Params.Creator, strconv.Itoa(keys), strconv.Itoa(len(v.Params.ApprovalProgram))}
	case model.Box:
		return []string{"object", "size", "value"}, []string{o.String(), strconv.Itoa(len(v.Value)), ttview.FormatBytes(v.Value)}
	case createdEntry:
		return []string{"type", "id", "object"}, []string{v.Type, strconv.FormatUint(v.ID, 10), o.String()}
	}
	return []string{"object"}, []string{o.String()}
}

// deref is *s if it is printable, its b64: encoding otherwise.
func deref(s *string) string {
	if s == nil {
		return ""
	}
	if b := ttview.AppCallBytes([]byte(*s)); !strings.HasPrefix(b, "str:") {
		return b
	}
	return *s
}

// table writes rows in aligned blocks of lsFlushRows, the header first, so
// that long listings need not be held in memory.
type table struct {
	w       io.Writer
	header  []string
	rows    [][]string
	started bool
}

func (t *table) add(header, row []string) error {
	t.header = header
	t.rows = append(t.rows, row)
	if len(t.rows) == lsFlushRows {
		return t.flush()
	}
	return nil
}

func (t *table) flush() error {
	if len(t.rows) == 0 {
		return nil
	}
	h := t.header
	if t.started {
		h = nil
	}
	t.started = true
	err := ttview.WriteTable(t.w, h, t.rows)
	t.rows = t.rows[:0]
	return err
}

// shortProto is the last part of a consensus protocol's URL, as wide as a table can show.
func shortProto(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		p = p[i+1:]
	}
	if len(p) > 12 {
		p = p[:12]
	}
	return p
}

type headerOut struct {
	Round        uint64          `json:"round"`
	Digest       string          `json:"digest"`
	ExpiredStake *uint64         `json:"expired-stake,omitempty"`
	Header       json.RawMessage `json:"header"`
}

type headerRow struct {
	Round        uint64  `json:"round"`
	Timestamp    int64   `json:"timestamp"`
	Proto        string  `json:"proto"`
	TxnCounter   uint64  `json:"txn-counter"`
	Digest       string  `json:"digest"`
	ExpiredStake *uint64 `json:"expired-stake,omitempty"`
}

func cmdHeader(e *env, args []string) error {
	pos, err := parse(e.newFlags("header"), args, 1, 2)
	if err != nil {
		return err
	}
	var rs []basics.Round
	for _, p := range pos {
		v, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return usagef("bad round %q", p)
		}
		if err = e.checkRound(basics.Round(v)); err != nil {
			return err
		}
		rs = append(rs, basics.Round(v))
	}
	digestAndStake := func(rnd basics.Round) (string, *uint64, error) {
		d, _, err := e.store.Digest(uint64(rnd))
		if err != nil {
			return "", nil, err
		}
		s, ok, err := e.store.ExpiredStake(uint64(rnd))
		if !ok || err != nil {
			return hex.EncodeToString(d), nil, err
		}
		return hex.EncodeToString(d), &s, nil
	}
	if len(rs) == 1 {
		hdr, err := e.header(rs[0])
		if err != nil {
			return err
		}
		out := headerOut{Round: uint64(rs[0]), Header: protocol.EncodeJSON(&hdr)}
		if out.Digest, out.ExpiredStake, err = digestAndStake(rs[0]); err != nil {
			return err
		}
		if e.json {
			return e.emit(out)
		}
		stake := "-"
		if out.ExpiredStake != nil {
			stake = strconv.FormatUint(*out.ExpiredStake, 10)
		}
		fmt.Fprintf(e.out, "round %d (%s)\n  changes digest  %s\n  expired stake   %s\n", out.Round, e.when(rs[0]), out.Digest, stake)
		fields, err := ttview.Flatten(out.Header, ttview.Options{})
		if err != nil {
			return err
		}
		return ttview.WriteFields(e.out, fields, "  ")
	}
	if rs[0] > rs[1] {
		return usagef("round %d is after round %d", rs[0], rs[1])
	}
	t := &table{w: e.out}
	for rnd := rs[0]; rnd <= rs[1]; rnd++ {
		hdr, err := e.header(rnd)
		if err != nil {
			return err
		}
		row := headerRow{Round: uint64(rnd), Timestamp: hdr.TimeStamp, Proto: string(hdr.CurrentProtocol), TxnCounter: hdr.TxnCounter}
		if row.Digest, row.ExpiredStake, err = digestAndStake(rnd); err != nil {
			return err
		}
		if e.json {
			err = e.emit(row)
		} else {
			stake := "-"
			if row.ExpiredStake != nil {
				stake = strconv.FormatUint(*row.ExpiredStake, 10)
			}
			err = t.add([]string{"round", "time", "proto", "txn counter", "digest", "expired stake"},
				[]string{strconv.FormatUint(row.Round, 10), e.when(rnd), shortProto(row.Proto), strconv.FormatUint(row.TxnCounter, 10), row.Digest, stake})
		}
		if err != nil {
			return err
		}
	}
	return t.flush()
}
