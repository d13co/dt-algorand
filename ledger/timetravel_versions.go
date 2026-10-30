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
	"container/heap"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/algorand/msgp/msgp"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// VersionRange selects a page of versions.
type VersionRange struct {
	Min, Max basics.Round // inclusive; Max at most Latest()
	Desc     bool         // newest first
	Limit    uint64       // versions (rounds) per page; must be > 0
	Values   bool         // include the new values
	// Keys lists the global or local state keys each app params or local
	// state version changed against the value at the round before.
	Keys bool
	// MaxBytes ends a page early once its values exceed it (0: no limit).
	// With Keys, the previous values the keys are found from count too. A
	// page always has at least one version, except with Key or Prefix: their
	// pages can have none and still have More.
	MaxBytes uint64
	// Key, if not nil, keeps only the versions of an app's params or local
	// state at which this global or local state key's value changed (added,
	// removed, or a new value or type), and reports its new value (KeyValue).
	// AppVersions and AccountAppVersions take it; AccountAppVersions then
	// lists the local state only. It excludes Rounds. A page examines at most
	// MaxScanned versions and reads at most MaxReadBytes of values (each
	// version's and the one before it), so it can hold fewer than Limit
	// versions, or none, and still have More; Next is then the round to
	// resume from, which need not be a version.
	Key []byte
	// Prefix, if not nil, narrows AppBoxVersions to the boxes whose names
	// start with it. Range pages are bounded by MaxScanned as for Key; with
	// Rounds, MaxObjects counts only the boxes under the prefix.
	Prefix []byte
	// MaxScanned bounds the work of a Key or Prefix range page (0: no limit):
	// versions examined (Key), or box index entries stepped through (Prefix).
	MaxScanned uint64
	// MaxObjects bounds the resources (holdings, local states, created assets
	// and apps) an account lookup merges, counting every one the account ever
	// had (0: no limit). With Rounds, it bounds an app's boxes likewise.
	MaxObjects uint64
	// Rounds, if set, asks for the objects as of each of these rounds, in
	// page order, instead of their versions in Min..Max: per round, each
	// object's latest version at or before it, with its round as Changed.
	// Min and Max must bound them. Limit counts rounds.
	Rounds []basics.Round
	// MaxEntries ends a Rounds page of an app's boxes early once it holds
	// that many boxes (0: no limit). A round is never split: one that holds
	// more boxes, or more values than MaxBytes, fails with ErrRoundTooLarge.
	MaxEntries uint64
	// MaxSeeks ends a Rounds page early, before a round that would take its
	// lookups (one per object per round) past it (0: no limit). A page always
	// has at least one round.
	MaxSeeks uint64
	// MaxReadBytes ends a Rounds page early, before a round whose lookups
	// would take the values they read past it, whether or not the page
	// returns them (0: no limit). A lookup counts the value of the object's
	// version and the value before it (for a deletion, the value deleted).
	// That can be less than it reads: the repeated writes a descending step
	// passes, and the stored value an object had before its rounds still in
	// memory, are not counted. A page always has at least one round, except
	// that a round of an app's boxes that reads more fails with
	// ErrRoundTooLarge. A Key page counts each record its scan reads once: the
	// values of the versions it examines, and the one before the oldest.
	MaxReadBytes uint64
}

// VersionPage is a page of versions, in page order.
type VersionPage struct {
	Versions []Version
	// More reports that versions remain in the range. Next is then the round
	// of the next one: the next page's Min (ascending) or Max (descending).
	More bool
	Next basics.Round
}

// Version lists the objects of a versions lookup whose value changed in a
// round, with their new values if requested.
type Version struct {
	Round     basics.Round
	Timestamp int64
	Proto     protocol.ConsensusVersion

	Account     *AccountVersion // the account's base record
	Holdings    []HoldingVersion
	LocalStates []LocalStateVersion
	Assets      []AssetVersion // asset params
	Apps        []AppVersion   // app params
	Box         *BoxVersion
	Boxes       []BoxChange // AppBoxVersions: the app's boxes that changed, by name
}

// AccountVersion is a version of an account's base record. Data has the
// rewards pending at the version's round applied; WithoutRewards is the
// balance before them.
type AccountVersion struct {
	Deleted        bool
	Changed        *basics.Round // with Rounds: the round of the version in effect; nil if none
	Data           *ledgercore.AccountData
	WithoutRewards basics.MicroAlgos
}

// HoldingVersion is a version of an asset holding.
type HoldingVersion struct {
	Asset   basics.AssetIndex
	Deleted bool
	Changed *basics.Round // with Rounds, as for AccountVersion
	Holding *basics.AssetHolding
}

// LocalStateVersion is a version of an app local state. ChangedKeys, set
// only with VersionRange.Keys and not on deletions, are the keys added,
// removed or changed in the round, sorted. KeyValue, only with
// VersionRange.Key, is the key's new value: nil when the version removed it
// or deleted the state.
type LocalStateVersion struct {
	App         basics.AppIndex
	Deleted     bool
	Changed     *basics.Round // with Rounds, as for AccountVersion
	State       *basics.AppLocalState
	ChangedKeys [][]byte
	KeyValue    *basics.TealValue
}

// AssetVersion is a version of an asset's params. Params is nil when
// deleted, when values were not requested, or when the lookup excluded the
// params of created assets (the version is then the asset's creation or
// destruction).
type AssetVersion struct {
	Asset   basics.AssetIndex
	Deleted bool
	Changed *basics.Round // with Rounds, as for AccountVersion
	Creator basics.Address
	Params  *basics.AssetParams
}

// AppVersion is a version of an app's params, like AssetVersion.
// ChangedKeys are the global state keys the version changed, as for
// LocalStateVersion; versions from the creator index have none. KeyValue is
// as for LocalStateVersion.
type AppVersion struct {
	App         basics.AppIndex
	Deleted     bool
	Changed     *basics.Round // with Rounds, as for AccountVersion
	Creator     basics.Address
	Params      *basics.AppParams
	ChangedKeys [][]byte
	KeyValue    *basics.TealValue
}

// BoxVersion is a version of a box.
type BoxVersion struct {
	Deleted bool
	Changed *basics.Round // with Rounds, as for AccountVersion
	Value   []byte
}

// BoxChange is a change of one of an app's boxes.
type BoxChange struct {
	Name    []byte
	Deleted bool
	Changed *basics.Round // with Rounds: the round of the box's version in effect
	Value   []byte        // nil when deleted or values were not requested
}

// AccountVersionsExclude narrows an account's versions the way exclude
// narrows GET /v2/accounts/{address}.
type AccountVersionsExclude struct {
	All                 bool // the base record only
	CreatedAssetsParams bool // created assets version on creation and destruction only
	CreatedAppsParams   bool // created apps likewise
}

// ErrTooManyVersionObjects is returned when an account has more objects
// than VersionRange.MaxObjects.
var ErrTooManyVersionObjects = errors.New("too many objects to merge")

// ErrRoundTooLarge is returned when a Rounds page of an app's boxes would
// hold a round with more boxes than VersionRange.MaxEntries, more values
// than MaxBytes, or whose lookups read more values than MaxReadBytes.
var ErrRoundTooLarge = errors.New("one round of boxes is too large")

// AccountVersions lists the versions of everything GET /v2/accounts/{addr}
// returns: its base record, holdings, local states, and the params of the
// assets and apps it created, narrowed by ex.
func (l *Ledger) AccountVersions(addr basics.Address, ex AccountVersionsExclude, r VersionRange) (VersionPage, error) {
	h := l.historyNow(r.Max)
	if h == nil {
		return VersionPage{}, errHistoryUnavailable
	}
	return histAccountVersions(h, addr, ex, r)
}

func histAccountVersions(h *histView, addr basics.Address, ex AccountVersionsExclude, r VersionRange) (VersionPage, error) {
	srcs := [][]byte{timetravel.AccountKey(addr)}
	add := func(obj []byte) bool {
		srcs = append(srcs, obj)
		return r.MaxObjects == 0 || uint64(len(srcs)-1) <= r.MaxObjects // srcs[0] is the base record
	}
	if !ex.All {
		for _, pfx := range [][]byte{timetravel.HoldingKey(addr, 0)[:1+len(addr)], timetravel.LocalKey(addr, 0)[:1+len(addr)]} {
			if err := h.Objects(pfx, add); err != nil {
				return VersionPage{}, err
			}
		}
		for _, c := range []struct {
			ctype         basics.CreatableType
			excludeParams bool
		}{{basics.AssetCreatable, ex.CreatedAssetsParams}, {basics.AppCreatable, ex.CreatedAppsParams}} {
			err := h.Objects(timetravel.CreatedPrefix(addr, c.ctype), func(obj []byte) bool {
				switch id := objID(obj); {
				case c.excludeParams:
					return add(obj)
				case c.ctype == basics.AssetCreatable:
					return add(timetravel.AssetParamsKey(basics.AssetIndex(id)))
				default:
					return add(timetravel.AppParamsKey(basics.AppIndex(id)))
				}
			})
			if err != nil {
				return VersionPage{}, err
			}
		}
	}
	if r.MaxObjects > 0 && uint64(len(srcs)-1) > r.MaxObjects {
		return VersionPage{}, fmt.Errorf("%w: account %s has had more than %d resources", ErrTooManyVersionObjects, addr, r.MaxObjects)
	}
	return versions(h, srcs, r, true)
}

// AccountAssetVersions lists the versions of an account's holding of an
// asset and, if the account created it, of the asset's params.
func (l *Ledger) AccountAssetVersions(addr basics.Address, aidx basics.AssetIndex, r VersionRange) (VersionPage, error) {
	h := l.historyNow(r.Max)
	if h == nil {
		return VersionPage{}, errHistoryUnavailable
	}
	return histAccountAssetVersions(h, addr, aidx, r)
}

// AccountAppVersions lists the versions of an account's local state of an
// app and, if the account created it, of the app's params.
func (l *Ledger) AccountAppVersions(addr basics.Address, aidx basics.AppIndex, r VersionRange) (VersionPage, error) {
	h := l.historyNow(r.Max)
	if h == nil {
		return VersionPage{}, errHistoryUnavailable
	}
	return histAccountAppVersions(h, addr, aidx, r)
}

func histAccountAssetVersions(h *histView, addr basics.Address, aidx basics.AssetIndex, r VersionRange) (VersionPage, error) {
	return histAccountResourceVersions(h, addr, basics.CreatableIndex(aidx), basics.AssetCreatable,
		timetravel.HoldingKey(addr, aidx), timetravel.AssetParamsKey(aidx), r)
}

func histAccountAppVersions(h *histView, addr basics.Address, aidx basics.AppIndex, r VersionRange) (VersionPage, error) {
	return histAccountResourceVersions(h, addr, basics.CreatableIndex(aidx), basics.AppCreatable,
		timetravel.LocalKey(addr, aidx), timetravel.AppParamsKey(aidx), r)
}

func histAccountResourceVersions(h *histView, addr basics.Address, cidx basics.CreatableIndex, ctype basics.CreatableType, resKey, paramsKey []byte, r VersionRange) (VersionPage, error) {
	srcs := [][]byte{resKey}
	if r.Key != nil { // a local state key: the params are not looked at
		return versions(h, srcs, r, false)
	}
	// creators never change: the params are the account's if it ever created the creatable
	created := false
	if err := h.Objects(timetravel.CreatedKey(addr, ctype, cidx), func([]byte) bool { created = true; return false }); err != nil {
		return VersionPage{}, err
	}
	if created {
		srcs = append(srcs, paramsKey)
	}
	return versions(h, srcs, r, false)
}

// AssetVersions lists the versions of an asset's params.
func (l *Ledger) AssetVersions(aidx basics.AssetIndex, r VersionRange) (VersionPage, error) {
	return l.objectVersions(timetravel.AssetParamsKey(aidx), r)
}

// AppVersions lists the versions of an app's params.
func (l *Ledger) AppVersions(aidx basics.AppIndex, r VersionRange) (VersionPage, error) {
	return l.objectVersions(timetravel.AppParamsKey(aidx), r)
}

// KvVersions lists the versions of a kv entry (box).
func (l *Ledger) KvVersions(key string, r VersionRange) (VersionPage, error) {
	return l.objectVersions(timetravel.KvKey(key), r)
}

// ErrNoBoxIndex is returned by AppBoxVersions for rounds before the time
// travel store's box index is complete.
var ErrNoBoxIndex = timetravel.ErrNoBoxIndex

// AppBoxVersions lists the rounds at which any box of an app changed, with
// the boxes that did. Pages cost what they hold, however many boxes the app
// ever had: the store indexes box changes by app and round. With r.Prefix,
// only the boxes whose names start with it count: a page then steps through
// every index entry of the app in its rounds, up to r.MaxScanned, so it can
// be short or empty and still have More.
func (l *Ledger) AppBoxVersions(app basics.AppIndex, r VersionRange) (VersionPage, error) {
	h := l.historyNow(r.Max)
	if h == nil {
		return VersionPage{}, errHistoryUnavailable
	}
	return histAppBoxVersions(h, app, r)
}

func histAppBoxVersions(h *histView, app basics.AppIndex, r VersionRange) (VersionPage, error) {
	if r.Key != nil {
		return VersionPage{}, errors.New("a state key applies to app params or a local state")
	}
	if r.Rounds != nil {
		return appBoxesAt(h, app, r)
	}
	var page VersionPage
	var cur Version // the round being read, not yet in the page
	var curSize, size uint64
	// add adds cur to the page unless its values exceed the budget; a round is
	// never split
	add := func() bool {
		if r.MaxBytes > 0 && len(page.Versions) > 0 && size+curSize > r.MaxBytes {
			page.More, page.Next = true, cur.Round
			return false
		}
		page.Versions = append(page.Versions, cur)
		size += curSize
		return true
	}
	// the callback must not read the store: it runs inside the store's read
	// lock, which Close waits for while blocking new readers
	o := timetravel.BoxChangesOpts{Prefix: r.Prefix, MaxScanned: r.MaxScanned}
	end, err := h.BoxChanges(uint64(app), uint64(r.Min), uint64(r.Max), r.Desc, o, func(rnd uint64, name, val []byte, ok bool) bool {
		if R := basics.Round(rnd); len(cur.Boxes) == 0 || R != cur.Round {
			if len(cur.Boxes) > 0 && !add() {
				return false
			}
			if uint64(len(page.Versions)) == r.Limit {
				page.More, page.Next = true, R
				return false
			}
			cur, curSize = Version{Round: R}, 0
		}
		bc := BoxChange{Name: name, Deleted: !ok}
		if r.Values && ok {
			bc.Value = val
			curSize += uint64(len(val))
		}
		cur.Boxes = append(cur.Boxes, bc)
		return true
	})
	// a descending page that reached rounds the box index lacks ends with
	// the changes it found, all of those since; the next page fails
	partial := errors.Is(err, ErrNoBoxIndex) && r.Desc && (len(page.Versions) > 0 || len(cur.Boxes) > 0)
	if err != nil && !partial {
		return VersionPage{}, err
	}
	if len(cur.Boxes) > 0 && !page.More {
		add() // complete: the scan stops between rounds
	}
	if end.More && !page.More { // the scan budget ran out: the page can be empty
		page.More, page.Next = true, basics.Round(end.Next)
	}
	if partial && !page.More {
		page.More, page.Next = true, page.Versions[len(page.Versions)-1].Round-1
	}
	for i := range page.Versions {
		v := &page.Versions[i]
		hdr, err := histHeader(h, v.Round)
		if err != nil {
			return VersionPage{}, err
		}
		v.Timestamp, v.Proto = hdr.TimeStamp, hdr.CurrentProtocol
		slices.SortFunc(v.Boxes, func(a, b BoxChange) int { return bytes.Compare(a.Name, b.Name) })
	}
	return page, nil
}

func (l *Ledger) objectVersions(obj []byte, r VersionRange) (VersionPage, error) {
	h := l.historyNow(r.Max)
	if h == nil {
		return VersionPage{}, errHistoryUnavailable
	}
	return versions(h, [][]byte{obj}, r, true)
}

// objID is the asset or app id that ends a holding, local state, params or
// creator index key.
func objID(obj []byte) uint64 { return binary.BigEndian.Uint64(obj[len(obj)-8:]) }

type srcChange struct {
	src int // index into the sources
	ok  bool
}

// versions merges the versions of the objects srcs into a page. It first
// finds the page's rounds, keeping no more than Limit+1 of them at any time
// and stopping each object's scan once it passes them, then reads the values
// of only the page's versions.
//
// With r.Rounds it builds a page of the objects as of those rounds instead
// (versionsAt); first then reports srcs[0] at every round.
func versions(h *histView, srcs [][]byte, r VersionRange, first bool) (VersionPage, error) {
	if r.Prefix != nil {
		return VersionPage{}, errors.New("a name prefix applies to an app's boxes")
	}
	if r.Key != nil {
		if r.Rounds != nil {
			return VersionPage{}, errors.New("a state key cannot be combined with rounds")
		}
		if len(srcs) != 1 || (srcs[0][0] != timetravel.KindAppParams && srcs[0][0] != timetravel.KindLocal) {
			return VersionPage{}, errors.New("a state key applies to app params or a local state")
		}
		return keyVersions(h, srcs[0], r)
	}
	if r.Rounds != nil {
		return versionsAt(h, srcs, r, first)
	}
	keep := int(r.Limit) + 1 // one more tells whether there are more
	rounds := map[basics.Round][]srcChange{}
	kept := &pageHeap{desc: r.Desc}
	for i, obj := range srcs {
		err := h.Versions(obj, uint64(r.Min), uint64(r.Max), r.Desc, func(rnd uint64, _ []byte, ok bool) bool {
			R := basics.Round(rnd)
			if _, seen := rounds[R]; !seen {
				if len(rounds) == keep {
					if !kept.before(R, kept.rs[0]) {
						return false // so are this object's next versions
					}
					delete(rounds, heap.Pop(kept).(basics.Round))
				}
				heap.Push(kept, R)
			}
			rounds[R] = append(rounds[R], srcChange{i, ok})
			return true
		})
		if err != nil {
			return VersionPage{}, err
		}
	}

	order := slices.Sorted(maps.Keys(rounds))
	if r.Desc {
		slices.Reverse(order)
	}
	var page VersionPage
	if uint64(len(order)) > r.Limit {
		page.More, page.Next = true, order[r.Limit]
		order = order[:r.Limit]
	}
	var size uint64
	for _, R := range order {
		v, n, err := readVersion(h, srcs, R, rounds[R], r.Values, r.Keys)
		if err != nil {
			return VersionPage{}, err
		}
		if r.MaxBytes > 0 && len(page.Versions) > 0 && size+n > r.MaxBytes {
			page.More, page.Next = true, R
			break
		}
		size += n
		page.Versions = append(page.Versions, v)
	}
	return page, nil
}

// keyVersions builds a page of the versions of obj, an app's params or a
// local state, at which r.Key's value changed, from one scan of obj's
// versions, each with the state before it. The scan stops at Limit matches
// and one more, or once it has examined MaxScanned versions or read
// MaxReadBytes of values; it always examines one.
func keyVersions(h *histView, obj []byte, r VersionRange) (VersionPage, error) {
	type match struct {
		rnd        basics.Round
		val, prev  []byte
		ok, prevOK bool
		kv         *basics.TealValue
	}
	var page VersionPage
	var matches []match
	var scanned, read uint64
	var dec keyDecoder
	var derr error
	// the callback must not read the store, as for BoxChanges
	err := h.VersionsPrev(obj, uint64(r.Min), uint64(r.Max), r.Desc, func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
		R := basics.Round(rnd)
		// The scan reads each record once. Ascending, a version's previous
		// value is the version before's value, and only the first version's
		// is read for it alone. Descending, a version's previous value is the
		// next version's value, read here, and the first version's value is
		// the one read for it alone.
		n, other := uint64(len(val)), uint64(len(prev))
		if r.Desc {
			n, other = other, n
		}
		if scanned == 0 {
			n += other
		}
		if scanned > 0 && ((r.MaxScanned > 0 && scanned >= r.MaxScanned) || (r.MaxReadBytes > 0 && read+n > r.MaxReadBytes)) {
			page.More, page.Next = true, R
			return false
		}
		scanned++
		read += n
		var cur, before *basics.TealValue
		if cur, derr = dec.value(obj, val, ok, r.Key); derr != nil {
			return false
		}
		if before, derr = dec.value(obj, prev, prevOK, r.Key); derr != nil {
			return false
		}
		dec.keep(val, ok, cur, prev, prevOK, before)
		if sameTealValue(before, cur) {
			return true
		}
		if uint64(len(matches)) == r.Limit {
			page.More, page.Next = true, R
			return false
		}
		m := match{rnd: R, ok: ok, prevOK: prevOK, kv: cur}
		if r.Values || r.Keys { // keep only the values the page returns or reads keys from
			m.val = val
		}
		if r.Keys {
			m.prev = prev
		}
		matches = append(matches, m)
		return true
	})
	if err == nil {
		err = derr
	}
	if err != nil {
		return VersionPage{}, err
	}
	var size uint64
	for _, m := range matches {
		hdr, err := histHeader(h, m.rnd)
		if err != nil {
			return VersionPage{}, err
		}
		v := Version{Round: m.rnd, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol}
		o := objRead{obj: obj, round: m.rnd, ok: m.ok, havePrev: true}
		if m.prevOK {
			o.prev = m.prev
		}
		if o.needsValue(r.Values, r.Keys) {
			o.val = m.val
		}
		n, err := addObject(h, &v, &hdr, o, r.Values, r.Keys)
		if err != nil {
			return VersionPage{}, err
		}
		if r.MaxBytes > 0 && len(page.Versions) > 0 && size+n > r.MaxBytes {
			page.More, page.Next = true, m.rnd
			break
		}
		size += n
		if obj[0] == timetravel.KindAppParams {
			v.Apps[0].KeyValue = m.kv
		} else {
			v.LocalStates[0].KeyValue = m.kv
		}
		page.Versions = append(page.Versions, v)
	}
	return page, nil
}

// keyDecoder finds a state key's value in app params or local state records,
// reusing the last two it decoded: a scan's next version shares its previous
// state with the version before (ascending) or its new state (descending).
type keyDecoder struct {
	last [2]struct {
		val []byte
		ok  bool
		kv  *basics.TealValue
	}
}

// value returns key's value in obj's record val (ok false: no state), nil
// if it has none.
func (d *keyDecoder) value(obj, val []byte, ok bool, key []byte) (*basics.TealValue, error) {
	if !ok {
		return nil, nil
	}
	for _, l := range d.last {
		if l.ok && bytes.Equal(l.val, val) {
			return l.kv, nil
		}
	}
	return stateKeyValue(obj, val, key)
}

// keep remembers a version's two decoded states.
func (d *keyDecoder) keep(val []byte, ok bool, kv *basics.TealValue, prev []byte, prevOK bool, prevKV *basics.TealValue) {
	d.last[0].val, d.last[0].ok, d.last[0].kv = val, ok, kv
	d.last[1].val, d.last[1].ok, d.last[1].kv = prev, prevOK, prevKV
}

// stateKeyValue returns key's value in the state of an app params or local
// state record, nil if it has none. It walks the record's msgpack to the
// state map and decodes only the key's value (a full decode copies the
// programs, about 70% of a key page's time), falling back to a full decode
// for a record it cannot walk.
func stateKeyValue(obj, val, key []byte) (*basics.TealValue, error) {
	rec, field := val, "tkv" // AppLocalState.KeyValue
	if obj[0] == timetravel.KindAppParams {
		var creator basics.Address
		if len(val) < len(creator) {
			return nil, fmt.Errorf("short params record %x", obj)
		}
		rec, field = val[len(creator):], "gs" // AppParams.GlobalState
	}
	if tv, ok := findStateKey(rec, field, key); ok {
		return tv, nil
	}
	return fullStateKeyValue(obj, val, key)
}

// findStateKey finds key in the state map field of a msgpack-encoded struct
// (a map, as protocol.Encode writes it). ok is false if it cannot walk rec.
func findStateKey(rec []byte, field string, key []byte) (tv *basics.TealValue, ok bool) {
	n, _, b, err := msgp.ReadMapHeaderBytes(rec)
	if err != nil {
		return nil, false
	}
	for range n {
		var f []byte
		if f, b, err = msgp.ReadMapKeyZC(b); err != nil {
			return nil, false
		}
		if string(f) != field {
			if b, err = msgp.Skip(b); err != nil {
				return nil, false
			}
			continue
		}
		var m int
		if m, _, b, err = msgp.ReadMapHeaderBytes(b); err != nil {
			return nil, false
		}
		for range m {
			var k []byte
			if k, b, err = msgp.ReadStringZC(b); err != nil {
				return nil, false
			}
			if bytes.Equal(k, key) {
				var v basics.TealValue
				if _, err = v.UnmarshalMsg(b); err != nil {
					return nil, false
				}
				return &v, true
			}
			if b, err = msgp.Skip(b); err != nil {
				return nil, false
			}
		}
		return nil, true // canonical encodings have each field once
	}
	return nil, true
}

// fullStateKeyValue is stateKeyValue by a full decode of the record.
func fullStateKeyValue(obj, val, key []byte) (*basics.TealValue, error) {
	var kv basics.TealKeyValue
	switch obj[0] {
	case timetravel.KindAppParams:
		var p basics.AppParams
		if _, err := decodeParams(obj, val, &p); err != nil {
			return nil, err
		}
		kv = p.GlobalState
	case timetravel.KindLocal:
		var ls basics.AppLocalState
		if err := protocol.Decode(val, &ls); err != nil {
			return nil, err
		}
		kv = ls.KeyValue
	default:
		return nil, fmt.Errorf("history object %x has no state keys", obj)
	}
	if tv, ok := kv[string(key)]; ok {
		return &tv, nil
	}
	return nil, nil
}

// sameTealValue reports whether two optional values are equal.
func sameTealValue(a, b *basics.TealValue) bool {
	return (a == nil) == (b == nil) && (a == nil || *a == *b)
}

// readVersion builds the version of round rnd from the sources that changed
// in it, and returns the size of the values it read.
func readVersion(h *histView, srcs [][]byte, rnd basics.Round, changes []srcChange, values, keys bool) (Version, uint64, error) {
	hdr, err := histHeader(h, rnd)
	if err != nil {
		return Version{}, 0, err
	}
	v := Version{Round: rnd, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol}
	var size uint64
	for _, ch := range changes {
		o := objRead{obj: srcs[ch.src], round: rnd, ok: ch.ok}
		if o.needsValue(values, keys) {
			var ok bool
			if o.val, ok, err = h.Get(o.obj, uint64(rnd)); err != nil {
				return Version{}, 0, err
			}
			if !ok {
				return Version{}, 0, fmt.Errorf("history object %x has a version at round %d but no value", o.obj, rnd)
			}
		}
		n, err := addObject(h, &v, &hdr, o, values, keys)
		if err != nil {
			return Version{}, 0, err
		}
		size += n
	}
	return v, size, nil
}

// objRead is an object's version to add to a Version.
type objRead struct {
	obj   []byte
	round basics.Round // the version's round
	ok    bool         // false: deleted
	val   []byte       // the new value, if needsValue
	// asOf reports round as the objects' Changed (a Rounds lookup). With
	// absent, the object had no version by then: it is reported deleted,
	// with no Changed.
	asOf, absent bool
	// havePrev: prev is the value before round (nil: none), found with the
	// version, so addObject need not read it for changed keys.
	prev     []byte
	havePrev bool
}

// keyed reports whether the version lists changed state keys with keys.
func (o objRead) keyed(keys bool) bool {
	return keys && o.ok && (o.obj[0] == timetravel.KindAppParams || o.obj[0] == timetravel.KindLocal)
}

// needsValue reports whether addObject needs the new value.
func (o objRead) needsValue(values, keys bool) bool {
	return o.ok && (values || o.keyed(keys))
}

// addObject decodes o into v. hdr is v's round's header: the account's
// pending rewards are those at v's round. It returns the size of the values
// read: o.val, and with keys the value before o.round, which it reads
// unless o has it.
func addObject(h *histView, v *Version, hdr *bookkeeping.BlockHeader, o objRead, values, keys bool) (uint64, error) {
	obj, val, deleted := o.obj, o.val, !o.ok
	size := uint64(len(val))
	var changed *basics.Round
	if o.asOf && !o.absent {
		changed = &o.round
	}
	// the keys of a state come from its value and the one before
	withKeys := o.keyed(keys)
	var prev []byte
	if withKeys && o.havePrev {
		prev = o.prev
	} else if withKeys && o.round > 0 {
		var ok bool
		var err error
		if prev, ok, err = h.Get(obj, uint64(o.round)-1); err != nil {
			return 0, err
		}
		if !ok {
			prev = nil // created: every key is new
		}
	}
	size += uint64(len(prev))
	var err error
	switch obj[0] {
	case timetravel.KindAccount:
		av := &AccountVersion{Deleted: deleted, Changed: changed}
		if val != nil {
			var ba trackerdb.BaseAccountData
			if err = protocol.Decode(val, &ba); err != nil {
				return 0, err
			}
			var proto config.ConsensusParams
			proto, err = histProto(*hdr)
			if err != nil {
				return 0, err
			}
			ad := ba.GetLedgerCoreAccountData()
			av.WithoutRewards = ad.MicroAlgos
			ad = ad.WithUpdatedRewards(proto.RewardUnit, hdr.RewardsLevel)
			av.Data = &ad
		}
		v.Account = av
	case timetravel.KindHolding:
		hv := HoldingVersion{Asset: basics.AssetIndex(objID(obj)), Deleted: deleted, Changed: changed}
		if val != nil {
			hv.Holding = new(basics.AssetHolding)
			if err = protocol.Decode(val, hv.Holding); err != nil {
				return 0, err
			}
		}
		v.Holdings = append(v.Holdings, hv)
	case timetravel.KindLocal:
		lv := LocalStateVersion{App: basics.AppIndex(objID(obj)), Deleted: deleted, Changed: changed}
		if val != nil {
			lv.State = new(basics.AppLocalState)
			if err = protocol.Decode(val, lv.State); err != nil {
				return 0, err
			}
		}
		if withKeys {
			var before basics.AppLocalState
			if prev != nil {
				if err = protocol.Decode(prev, &before); err != nil {
					return 0, err
				}
			}
			lv.ChangedKeys = changedKeys(before.KeyValue, lv.State.KeyValue)
		}
		if !values {
			lv.State = nil
		}
		v.LocalStates = append(v.LocalStates, lv)
	case timetravel.KindAssetParams:
		av := AssetVersion{Asset: basics.AssetIndex(objID(obj)), Deleted: deleted, Changed: changed}
		if val != nil {
			av.Params = new(basics.AssetParams)
			if av.Creator, err = decodeParams(obj, val, av.Params); err != nil {
				return 0, err
			}
		}
		v.Assets = append(v.Assets, av)
	case timetravel.KindAppParams:
		av := AppVersion{App: basics.AppIndex(objID(obj)), Deleted: deleted, Changed: changed}
		if val != nil {
			av.Params = new(basics.AppParams)
			if av.Creator, err = decodeParams(obj, val, av.Params); err != nil {
				return 0, err
			}
		}
		if withKeys {
			var before basics.AppParams
			if prev != nil {
				if _, err = decodeParams(obj, prev, &before); err != nil {
					return 0, err
				}
			}
			av.ChangedKeys = changedKeys(before.GlobalState, av.Params.GlobalState)
		}
		if !values {
			av.Creator, av.Params = basics.Address{}, nil
		}
		v.Apps = append(v.Apps, av)
	case timetravel.KindCreated: // CreatedKey: kind, creator, creatable type, id
		var creator basics.Address
		copy(creator[:], obj[1:])
		id := objID(obj)
		if basics.CreatableType(obj[1+len(creator)]) == basics.AssetCreatable {
			v.Assets = append(v.Assets, AssetVersion{Asset: basics.AssetIndex(id), Deleted: deleted, Changed: changed, Creator: creator})
		} else {
			v.Apps = append(v.Apps, AppVersion{App: basics.AppIndex(id), Deleted: deleted, Changed: changed, Creator: creator})
		}
	case timetravel.KindKv:
		v.Box = &BoxVersion{Deleted: deleted, Changed: changed, Value: val}
	default:
		return 0, fmt.Errorf("no versions for history object %x", obj)
	}
	return size, nil
}

// latestVersion finds obj's latest version at or before rnd: its round, and
// its value (ok false: deleted). found is false if it has none. prev is the
// value before it, at at-1, for changed keys (nil: none).
func latestVersion(h *histView, obj []byte, rnd basics.Round) (at basics.Round, val, prev []byte, ok, found bool, err error) {
	err = h.VersionsPrev(obj, 0, uint64(rnd), true, func(r uint64, v []byte, exists bool, p []byte, pOK bool) bool {
		at, val, ok, found = basics.Round(r), v, exists, true
		if pOK {
			prev = p
		}
		return false
	})
	return at, val, prev, ok, found, err
}

// versionsAt builds a Rounds page: per round, each of srcs as of it, from its
// latest version at or before it. Objects that do not exist at a round are
// left out, except srcs[0] if first is set, which is then reported deleted.
func versionsAt(h *histView, srcs [][]byte, r VersionRange, first bool) (VersionPage, error) {
	var page VersionPage
	var size, read, seeks uint64
	for i, R := range r.Rounds {
		if uint64(i) == r.Limit || (i > 0 && r.MaxSeeks > 0 && seeks+uint64(len(srcs)) > r.MaxSeeks) {
			page.More, page.Next = true, R
			break
		}
		seeks += uint64(len(srcs))
		hdr, err := histHeader(h, R)
		if err != nil {
			return VersionPage{}, err
		}
		v := Version{Round: R, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol}
		var n, rn uint64 // the round's values: returned, and read
		// over reports that the page cannot hold the round; it always
		// holds its first
		over := func() bool {
			return len(page.Versions) > 0 && ((r.MaxBytes > 0 && size+n > r.MaxBytes) ||
				(r.MaxReadBytes > 0 && read+rn > r.MaxReadBytes))
		}
		for j, obj := range srcs {
			at, val, prev, ok, found, err := latestVersion(h, obj, R)
			if err != nil {
				return VersionPage{}, err
			}
			rn += uint64(len(val) + len(prev))
			if (j == 0 && first) || (found && ok) {
				o := objRead{obj: obj, round: at, ok: found && ok, asOf: true, absent: !found, prev: prev, havePrev: true}
				if o.needsValue(r.Values, r.Keys) {
					o.val = val
				}
				m, err := addObject(h, &v, &hdr, o, r.Values, r.Keys)
				if err != nil {
					return VersionPage{}, err
				}
				n += m
			}
			if over() {
				break // the page ends before this round
			}
		}
		if over() {
			page.More, page.Next = true, R
			break
		}
		size += n
		read += rn
		page.Versions = append(page.Versions, v)
	}
	return page, nil
}

// appBoxesAt builds a Rounds page of an app's boxes: per round, every box
// that exists at it, in name order. It merges every box the app ever had
// (with r.Prefix, every one whose name starts with it), at most r.MaxObjects
// of them.
func appBoxesAt(h *histView, app basics.AppIndex, r VersionRange) (VersionPage, error) {
	type box struct{ obj, name []byte }
	var boxes []box
	pfx := timetravel.BoxKvPrefix(uint64(app))
	if r.Prefix != nil {
		pfx = timetravel.BoxKvNamePrefix(uint64(app), r.Prefix)
	}
	err := h.Objects(pfx, func(obj []byte) bool {
		if _, name, ok := timetravel.BoxOf(obj); ok {
			boxes = append(boxes, box{obj, name})
		}
		return r.MaxObjects == 0 || uint64(len(boxes)) <= r.MaxObjects
	})
	if err != nil {
		return VersionPage{}, err
	}
	if r.MaxObjects > 0 && uint64(len(boxes)) > r.MaxObjects {
		under := ""
		if r.Prefix != nil {
			under = fmt.Sprintf(" under prefix %x", r.Prefix)
		}
		return VersionPage{}, fmt.Errorf("%w: app %d has had more than %d boxes%s", ErrTooManyVersionObjects, app, r.MaxObjects, under)
	}
	var page VersionPage
	var size, entries, read, seeks uint64
	for i, R := range r.Rounds {
		if uint64(i) == r.Limit || (i > 0 && r.MaxSeeks > 0 && seeks+uint64(len(boxes)) > r.MaxSeeks) {
			page.More, page.Next = true, R
			break
		}
		seeks += uint64(len(boxes))
		hdr, err := histHeader(h, R)
		if err != nil {
			return VersionPage{}, err
		}
		v := Version{Round: R, Timestamp: hdr.TimeStamp, Proto: hdr.CurrentProtocol, Boxes: []BoxChange{}}
		var n, rn uint64 // the round's values: returned, and read
		// over reports that the page cannot hold the round
		over := func() bool {
			return (r.MaxEntries > 0 && entries+uint64(len(v.Boxes)) > r.MaxEntries) ||
				(r.MaxBytes > 0 && size+n > r.MaxBytes) ||
				(r.MaxReadBytes > 0 && read+rn > r.MaxReadBytes)
		}
		for _, b := range boxes {
			at, val, prev, ok, found, err := latestVersion(h, b.obj, R)
			if err != nil {
				return VersionPage{}, err
			}
			rn += uint64(len(val) + len(prev))
			if found && ok {
				bc := BoxChange{Name: b.name, Changed: &at}
				if r.Values {
					bc.Value = val
					n += uint64(len(val))
				}
				v.Boxes = append(v.Boxes, bc)
			}
			if over() {
				// a round is never split, so one too large for a page fails
				if len(page.Versions) == 0 {
					var why string
					switch {
					case r.MaxEntries > 0 && uint64(len(v.Boxes)) > r.MaxEntries:
						why = fmt.Sprintf("has more than %d boxes", r.MaxEntries)
					case r.MaxBytes > 0 && n > r.MaxBytes:
						why = fmt.Sprintf("has more than %d bytes of values", r.MaxBytes)
					default:
						why = fmt.Sprintf("reads more than %d bytes of values, counting deleted boxes", r.MaxReadBytes)
					}
					return VersionPage{}, fmt.Errorf("%w: app %d %s at round %d", ErrRoundTooLarge, app, why, R)
				}
				break // the page ends before this round
			}
		}
		if over() {
			page.More, page.Next = true, R
			break
		}
		size += n
		entries += uint64(len(v.Boxes))
		read += rn
		page.Versions = append(page.Versions, v)
	}
	return page, nil
}

// changedKeys lists the keys added, removed or changed from prev to cur,
// sorted by their bytes. It is never nil, so that no changes can be told
// from keys not requested.
func changedKeys(prev, cur basics.TealKeyValue) [][]byte {
	keys := [][]byte{}
	for k, v := range cur {
		if pv, ok := prev[k]; !ok || pv != v {
			keys = append(keys, []byte(k))
		}
	}
	for k := range prev {
		if _, ok := cur[k]; !ok {
			keys = append(keys, []byte(k))
		}
	}
	slices.SortFunc(keys, bytes.Compare)
	return keys
}

// decodeParams decodes an asset or app params record: creator, then params.
func decodeParams(obj, val []byte, p msgp.Unmarshaler) (basics.Address, error) {
	var creator basics.Address
	if len(val) < len(creator) {
		return creator, fmt.Errorf("short params record %x", obj)
	}
	copy(creator[:], val)
	return creator, protocol.Decode(val[len(creator):], p)
}

// pageHeap holds the rounds kept for a page, the last in page order on top.
type pageHeap struct {
	rs   []basics.Round
	desc bool
}

// before reports whether round a comes before round b in page order.
func (h *pageHeap) before(a, b basics.Round) bool {
	if h.desc {
		return a > b
	}
	return a < b
}

func (h *pageHeap) Len() int           { return len(h.rs) }
func (h *pageHeap) Less(i, j int) bool { return h.before(h.rs[j], h.rs[i]) }
func (h *pageHeap) Swap(i, j int)      { h.rs[i], h.rs[j] = h.rs[j], h.rs[i] }
func (h *pageHeap) Push(x any)         { h.rs = append(h.rs, x.(basics.Round)) }
func (h *pageHeap) Pop() any {
	r := h.rs[len(h.rs)-1]
	h.rs = h.rs[:len(h.rs)-1]
	return r
}

// Versions is timetravel.Store.Versions with the in-memory rounds merged in.
// fn must not modify val.
func (v *histView) Versions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool) bool) error {
	return v.VersionsPrev(obj, lo, hi, desc, func(rnd uint64, val []byte, ok bool, _ []byte, _ bool) bool {
		return fn(rnd, val, ok)
	})
}

// VersionsPrev is timetravel.Store.VersionsPrev with the in-memory rounds
// merged in. fn must not modify val or prev.
func (v *histView) VersionsPrev(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool) error {
	type version struct {
		rnd    uint64
		val    []byte
		ok     bool
		prev   []byte // the value at rnd-1
		prevOK bool
	}
	// the in-memory rounds that change obj, oldest first
	var mem []version
	for i := range v.pending {
		p := &v.pending[i]
		if p.rnd <= v.last || uint64(p.rnd) < lo || uint64(p.rnd) > hi {
			continue
		}
		if j, ok := p.idx[string(obj)]; ok {
			mem = append(mem, version{rnd: uint64(p.rnd), val: p.changes[j].Value, ok: !p.changes[j].Deleted})
		}
	}
	if len(mem) > 0 {
		prev, prevOK, err := v.Get(obj, mem[0].rnd-1)
		if err != nil {
			return err
		}
		changed := mem[:0]
		for _, m := range mem {
			if prevOK != m.ok || (m.ok && !bytes.Equal(prev, m.val)) {
				m.prev, m.prevOK = prev, prevOK
				changed = append(changed, m)
			}
			prev, prevOK = m.val, m.ok
		}
		mem = changed
	}
	if desc {
		for i := len(mem) - 1; i >= 0; i-- {
			if !fn(mem[i].rnd, mem[i].val, mem[i].ok, mem[i].prev, mem[i].prevOK) {
				return nil
			}
		}
	}
	if storeHi := min(hi, uint64(v.last)); lo <= storeHi {
		stopped := false
		err := v.store.VersionsPrev(obj, lo, storeHi, desc, func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
			stopped = !fn(rnd, val, ok, prev, prevOK)
			return !stopped
		})
		if err != nil || stopped {
			return err
		}
	}
	if !desc {
		for _, m := range mem {
			if !fn(m.rnd, m.val, m.ok, m.prev, m.prevOK) {
				return nil
			}
		}
	}
	return nil
}

// Objects is timetravel.Store.Objects with the in-memory rounds merged in.
func (v *histView) Objects(prefix []byte, fn func(obj []byte) bool) error {
	mem := map[string]bool{}
	for i := range v.pending {
		if v.pending[i].rnd <= v.last {
			continue
		}
		for _, ch := range v.pending[i].changes {
			if bytes.HasPrefix(ch.Key, prefix) {
				mem[string(ch.Key)] = true
			}
		}
	}
	keys := slices.Sorted(maps.Keys(mem))
	stopped := false
	err := v.store.Objects(prefix, func(obj []byte) bool {
		for len(keys) > 0 && keys[0] <= string(obj) {
			k := keys[0]
			keys = keys[1:]
			if k != string(obj) && !fn([]byte(k)) {
				stopped = true
				return false
			}
		}
		stopped = !fn(obj)
		return !stopped
	})
	if err != nil || stopped {
		return err
	}
	for _, k := range keys {
		if !fn([]byte(k)) {
			return nil
		}
	}
	return nil
}

// BoxChanges is timetravel.Store.BoxChanges with the in-memory rounds merged
// in, under the same options: each box an in-memory round writes counts as
// an index entry toward o.MaxScanned. fn must not modify name or val.
func (v *histView) BoxChanges(app, lo, hi uint64, desc bool, o timetravel.BoxChangesOpts, fn func(rnd uint64, name, val []byte, ok bool) bool) (timetravel.BoxChangesEnd, error) {
	var end timetravel.BoxChangesEnd
	type change struct {
		name, val []byte
		ok        bool
	}
	// memRound is an in-memory round that writes boxes of app: how many, and
	// the changes of those under the prefix, by name
	type memRound struct {
		rnd     uint64
		scanned uint64
		changes []change
	}
	var mem []memRound // oldest first
	prefix := timetravel.BoxKvPrefix(app)
	for i := range v.pending {
		p := &v.pending[i]
		if p.rnd <= v.last || uint64(p.rnd) < lo || uint64(p.rnd) > hi {
			continue
		}
		m := memRound{rnd: uint64(p.rnd)}
		for j, ch := range p.changes {
			if !bytes.HasPrefix(ch.Key, prefix) || p.idx[string(ch.Key)] != j { // a key's last change counts
				continue
			}
			_, name, isBox := timetravel.BoxOf(ch.Key)
			if !isBox {
				continue
			}
			m.scanned++
			if !bytes.HasPrefix(name, o.Prefix) {
				continue
			}
			prev, prevOK, err := v.Get(ch.Key, uint64(p.rnd)-1)
			if err != nil {
				return end, err
			}
			if prevOK != !ch.Deleted || (prevOK && !bytes.Equal(prev, ch.Value)) {
				m.changes = append(m.changes, change{name, ch.Value, !ch.Deleted})
			}
		}
		if m.scanned > 0 {
			slices.SortFunc(m.changes, func(a, b change) int { return bytes.Compare(a.name, b.name) })
			mem = append(mem, m)
		}
	}
	spent := func() bool { return o.MaxScanned > 0 && end.Scanned >= o.MaxScanned }
	// readMem reads an in-memory round; false: stop, at the budget (More) or
	// as fn says
	readMem := func(m *memRound) bool {
		if spent() {
			end.More, end.Next = true, m.rnd
			return false
		}
		end.Scanned += m.scanned
		for i := range m.changes {
			c := &m.changes[i]
			if desc {
				c = &m.changes[len(m.changes)-1-i]
			}
			if !fn(m.rnd, c.name, c.val, c.ok) {
				return false
			}
		}
		return true
	}
	if desc {
		for i := len(mem) - 1; i >= 0; i-- {
			if !readMem(&mem[i]) {
				return end, nil
			}
		}
	}
	if storeHi := min(hi, uint64(v.last)); lo <= storeHi {
		if spent() {
			end.More, end.Next = true, storeHi
			return end, nil
		}
		so := o
		if o.MaxScanned > 0 {
			so.MaxScanned = o.MaxScanned - end.Scanned
		}
		stopped := false
		se, err := v.store.BoxChanges(app, lo, storeHi, desc, so, func(rnd uint64, name, val []byte, ok bool) bool {
			stopped = !fn(rnd, name, val, ok)
			return !stopped
		})
		end.Scanned += se.Scanned
		if err != nil || stopped {
			return end, err
		}
		if se.More {
			end.More, end.Next = true, se.Next
			return end, nil
		}
	}
	if !desc {
		for i := range mem {
			if !readMem(&mem[i]) {
				return end, nil
			}
		}
	}
	return end, nil
}
