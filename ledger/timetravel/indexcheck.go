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

package timetravel

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/cockroachdb/pebble/v2"
)

// IndexCheck checks a store's box index and asset holders index against its
// records, which they derive from: two stores whose records agree answer
// BoxChanges and AssetHolders alike if each one's indexes agree with its
// records. Feed it every record of the store in key order with Record, then
// call Finish; nothing may write the store meanwhile.
//
// It derives the entries each index should hold, and compares their sum with
// that of the entries it holds: per app for the box index, whose entries
// cover the rounds from BoxIndexFrom, and per asset for the holders index,
// which it checks only when complete. The sum is of hashes keyed with a
// random key drawn for each check, so that whoever wrote the store cannot
// make entries that add up to the sum of others. A box record that repeats
// its box's state need not have an entry, since BoxChanges skips it either
// way: the check looks up the entry of each such record, and expects it only
// if it is there. Memory grows with the number of assets ever held.
type IndexCheck struct {
	s      *Store
	report func(format string, args ...any)
	stats  IndexCheckStats
	hkey   [32]byte // the hash key
	buf    []byte   // for indexSum.add
	key    []byte   // a box index key suffix

	// the current object and its latest record
	obj, prev []byte

	// the box index, if checked
	boxes   bool
	boxFrom uint64
	boxIt   *pebble.Iterator
	inApp   bool   // whether the records are at a box of app
	app     uint64 // the current app
	box     bool   // whether the current object is a box of app
	name    []byte // its name
	want    indexSum
	apps    map[uint64]bool // the apps whose entries are checked

	// the asset holders index, if checked
	holders   bool
	holding   bool // whether the current object is a holding
	addr      []byte
	asset     uint64
	intervals []byte // the current holding's, so far
	live      bool
	at        uint64               // its last boundary
	assets    map[uint64]*indexSum // the holdings' entries, by asset
}

// IndexCheckStats sum up an IndexCheck.
type IndexCheckStats struct {
	// BoxIndexFrom is the round the box index is checked from; BoxIndex is
	// false if it is being rebuilt, and not checked.
	BoxIndexFrom uint64
	BoxIndex     bool
	// BoxApps and BoxEntries are the apps whose entries were checked, and
	// the entries.
	BoxApps, BoxEntries uint64
	// Holders is the state of the asset holders index, checked if complete.
	Holders HoldersState
	// HolderAssets and HolderEntries are the assets whose entries were
	// checked, and the entries.
	HolderAssets, HolderEntries uint64
	// Mismatches is the number of problems reported.
	Mismatches int
}

// indexSum is the number of a set of index entries and the sum of their
// keyed hashes, in four 64-bit lanes.
type indexSum struct {
	n uint64
	h [4]uint64
}

// add adds the entry (key, val) to x.
func (ic *IndexCheck) add(x *indexSum, key, val []byte) {
	b := append(ic.buf[:0], ic.hkey[:]...)
	b = binary.AppendUvarint(b, uint64(len(key)))
	b = append(append(b, key...), val...)
	sum := sha256.Sum256(b)
	ic.buf = b
	x.n++
	for i := range x.h {
		x.h[i] += binary.BigEndian.Uint64(sum[8*i:])
	}
}

// NewIndexCheck starts checking the indexes of s. report receives each
// problem found. Close it when done.
func (s *Store) NewIndexCheck(report func(format string, args ...any)) (*IndexCheck, error) {
	ic := &IndexCheck{s: s, assets: map[uint64]*indexSum{}, apps: map[uint64]bool{}}
	if _, err := rand.Read(ic.hkey[:]); err != nil {
		return nil, err
	}
	ic.report = func(format string, args ...any) {
		ic.stats.Mismatches++
		report(format, args...)
	}
	ic.boxFrom, ic.boxes = s.BoxIndexFrom()
	ic.stats.BoxIndexFrom, ic.stats.BoxIndex = ic.boxFrom, ic.boxes
	ic.stats.Holders = s.HoldersIndexState()
	ic.holders = ic.stats.Holders == HoldersComplete
	if ic.boxes {
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxBoxIndex}, UpperBound: []byte{pfxBoxIndex + 1}})
		if err != nil {
			return nil, err
		}
		ic.boxIt = it
	}
	return ic, nil
}

// Close releases the check's resources.
func (ic *IndexCheck) Close() error {
	if ic.boxIt == nil {
		return nil
	}
	err := ic.boxIt.Close()
	ic.boxIt = nil
	return err
}

// Record checks the record (key, val) of the store, the next in key order.
func (ic *IndexCheck) Record(key, val []byte) error {
	if len(key) < 1+1+8 || key[0] != pfxRecord || len(val) == 0 || (val[0] != recFull && !(val[0] == recDeleted && len(val) == 1)) {
		return fmt.Errorf("%w: bad record %x at key %x", ErrCorrupt, val, key)
	}
	obj, rnd := key[1:len(key)-8], binary.BigEndian.Uint64(key[len(key)-8:])
	if !bytes.Equal(obj, ic.obj) {
		if err := ic.startObject(obj); err != nil {
			return err
		}
	}
	repeat := bytes.Equal(val, ic.prev)
	ic.prev = append(ic.prev[:0], val...)
	deleted := val[0] == recDeleted
	if ic.box && rnd >= ic.boxFrom {
		ic.key = append(binary.BigEndian.AppendUint64(ic.key[:0], rnd), ic.name...)
		want := true
		if repeat { // expected only if there
			var err error
			if want, err = ic.hasBoxEntry(); err != nil {
				return err
			}
		}
		if want {
			ic.add(&ic.want, ic.key, boxIndexEntry(deleted))
		}
	}
	// a tombstone ends a holding, anything else starts or continues one, as
	// IndexHolders builds it
	if ic.holding && deleted == ic.live {
		ic.intervals = binary.AppendUvarint(ic.intervals, rnd-ic.at)
		ic.at, ic.live = rnd, !ic.live
	}
	return nil
}

// startObject ends the current object and starts obj.
func (ic *IndexCheck) startObject(obj []byte) error {
	ic.endHolding()
	ic.obj = append(ic.obj[:0], obj...)
	ic.prev = append(ic.prev[:0], recDeleted) // absent before its first record
	if ic.holders {
		addr, asset, ok := HoldingOf(obj)
		ic.holding = ok
		ic.addr, ic.asset = append(ic.addr[:0], addr[:]...), asset
		ic.intervals, ic.live, ic.at = ic.intervals[:0], false, 0
	}
	if ic.boxes {
		app, name, ok := BoxOf(obj)
		if ic.inApp && (!ok || app != ic.app) {
			if err := ic.endApp(); err != nil {
				return err
			}
		}
		if ok && !ic.inApp {
			ic.inApp, ic.app = true, app
		}
		ic.box = ok
		ic.name = append(ic.name[:0], name...)
	}
	return nil
}

// hasBoxEntry reports whether the box index has an entry at the current app
// and ic.key.
func (ic *IndexCheck) hasBoxEntry() (bool, error) {
	k := append(binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, ic.app), ic.key...)
	_, closer, err := ic.s.db.Get(k)
	if errors.Is(err, pebble.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, closer.Close()
}

func (ic *IndexCheck) endHolding() {
	if !ic.holding || len(ic.intervals) == 0 { // never held: tombstones only
		return
	}
	sum := ic.assets[ic.asset]
	if sum == nil {
		sum = &indexSum{}
		ic.assets[ic.asset] = sum
	}
	ic.add(sum, ic.addr, ic.intervals)
	ic.holding = false
}

// endApp compares the box index entries of the current app with its records.
func (ic *IndexCheck) endApp() error {
	app := ic.app
	ic.inApp = false
	ic.apps[app] = true
	pfx := binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, app)
	end := prefixEnd(pfx)
	var got indexSum
	for valid := ic.boxIt.SeekGE(binary.BigEndian.AppendUint64(slices.Clip(pfx), ic.boxFrom)); valid && bytes.Compare(ic.boxIt.Key(), end) < 0; valid = ic.boxIt.Next() {
		k, v := ic.boxIt.Key(), ic.boxIt.Value()
		if len(k) < 1+8+8 {
			ic.report("box index: bad key %x", k)
			continue
		}
		ic.add(&got, k[1+8:], v)
	}
	if err := ic.boxIt.Error(); err != nil {
		return err
	}
	if got != ic.want {
		ic.report("app %d: box index disagrees with its box records from round %d: %d entries for %d changes", app, ic.boxFrom, got.n, ic.want.n)
	}
	ic.stats.BoxApps++
	ic.stats.BoxEntries += got.n
	ic.want = indexSum{}
	return nil
}

// Finish ends the check: it compares the entries left, and returns the
// totals.
func (ic *IndexCheck) Finish() (IndexCheckStats, error) {
	ic.endHolding()
	if ic.boxes {
		if ic.inApp {
			if err := ic.endApp(); err != nil {
				return ic.stats, err
			}
		}
		if err := ic.finishBoxes(); err != nil {
			return ic.stats, err
		}
	}
	if ic.holders {
		if err := ic.finishHolders(); err != nil {
			return ic.stats, err
		}
	}
	return ic.stats, nil
}

// finishBoxes reports the box index entries of apps with no box records.
func (ic *IndexCheck) finishBoxes() error {
	it := ic.boxIt
	var extra map[uint64]uint64
	for valid := it.First(); valid; {
		k := it.Key()
		if len(k) >= 1+8 {
			if app := binary.BigEndian.Uint64(k[1:]); ic.apps[app] { // endApp checked it
				if app == ^uint64(0) {
					break
				}
				valid = it.SeekGE(binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, app+1))
				continue
			}
		}
		if len(k) < 1+8+8 {
			ic.report("box index: bad key %x", k)
			valid = it.Next()
			continue
		}
		app := binary.BigEndian.Uint64(k[1:])
		if binary.BigEndian.Uint64(k[1+8:]) >= ic.boxFrom {
			if extra == nil {
				extra = map[uint64]uint64{}
			}
			extra[app]++
		}
		valid = it.Next()
	}
	if err := it.Error(); err != nil {
		return err
	}
	for _, app := range slices.Sorted(maps.Keys(extra)) {
		ic.report("app %d: box index has %d entries from round %d, but the app has no box records", app, extra[app], ic.boxFrom)
	}
	return nil
}

// finishHolders compares the asset holders index with the holdings.
func (ic *IndexCheck) finishHolders() error {
	it, err := ic.s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxHolders}, UpperBound: []byte{pfxHolders + 1}})
	if err != nil {
		return err
	}
	defer it.Close()
	var got indexSum
	var cur uint64
	endAsset := func() {
		want := ic.assets[cur]
		delete(ic.assets, cur)
		if want == nil {
			want = &indexSum{}
		}
		if got != *want {
			ic.report("asset %d: holders index disagrees with its holding records: %d entries for %d holdings", cur, got.n, want.n)
		}
		ic.stats.HolderAssets++
		ic.stats.HolderEntries += got.n
		got = indexSum{}
	}
	started := false
	for valid := it.First(); valid; valid = it.Next() {
		k := it.Key()
		if len(k) != 1+8+32 {
			ic.report("holders index: bad key %x", k)
			continue
		}
		asset := binary.BigEndian.Uint64(k[1:])
		if started && asset != cur {
			endAsset()
		}
		cur, started = asset, true
		ic.add(&got, k[1+8:], it.Value())
	}
	if err := it.Error(); err != nil {
		return err
	}
	if started {
		endAsset()
	}
	for _, asset := range slices.Sorted(maps.Keys(ic.assets)) {
		ic.report("asset %d: holders index has no entries for its %d holdings", asset, ic.assets[asset].n)
	}
	return nil
}

// CheckIndexes checks the box index and asset holders index of the store
// against its box and holding records (see IndexCheck).
func (s *Store) CheckIndexes(report func(format string, args ...any)) (IndexCheckStats, error) {
	if err := s.acquire(); err != nil {
		return IndexCheckStats{}, err
	}
	defer s.closeMu.RUnlock()
	ic, err := s.NewIndexCheck(report)
	if err != nil {
		return IndexCheckStats{}, err
	}
	defer ic.Close()
	// in key order: holdings, then boxes
	for _, lo := range [][]byte{{pfxRecord, KindHolding}, append([]byte{pfxRecord}, kvBoxPrefix...)} {
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
		if err != nil {
			return IndexCheckStats{}, err
		}
		for valid := it.First(); valid; valid = it.Next() {
			if err = ic.Record(it.Key(), it.Value()); err != nil {
				break
			}
		}
		err = cmp.Or(err, it.Error(), it.Close())
		if err != nil {
			return IndexCheckStats{}, err
		}
	}
	return ic.Finish()
}
