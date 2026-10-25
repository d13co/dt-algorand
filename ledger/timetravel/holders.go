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
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/cockroachdb/pebble/v2"
)

// The asset holders index answers "which accounts held asset X at round R"
// without reading every holding: holding records are account-major ('c' 'h'
// addr asset round), so the index keeps an entry per holding ever, asset-major:
//
//	'a' asset(8) addr(32) -> intervals
//
// The intervals are the rounds at which the holding was created and deleted,
// alternating, as uvarint deltas (the first from round 0): an odd count means
// the holding is live. A holding is held at R if its latest record at or
// before R is not a tombstone, exactly what Get answers; a holding closed and
// reopened within a round has no boundary.
//
// IndexHolders builds the index from the holding records; from then on every
// AddRound keeps it up to date, in the round's batch. keyHoldersMeta holds
// the round after the last one the index covers, and exists only once
// IndexHolders has finished. A build without the index adds rounds without
// moving it: the index is then stale, and AddRound leaves it alone, since
// finding the holdings those rounds changed means reading every holding, a
// rebuild anyway. Open never writes the index, nor deletes it.
//
// Records are never removed except by RemoveNoops, which removes only records
// that repeat their object's state and tombstones before an object's first
// value: no holding's existence changes, so neither does the index.
//
// IndexHolders and DropHoldersIndex rewrite the index without telling readers
// (no generation counter, unlike the box index's): they run only while nothing
// reads the store, in a tool on a stopped node or before the node's services
// start.

// keyHoldersMeta holds the round after the last one the asset holders index
// covers.
var keyHoldersMeta = []byte("mh")

// HoldersState is the state of the asset holders index.
type HoldersState uint32

const (
	// HoldersNone means there is no index, or IndexHolders has not finished.
	HoldersNone HoldersState = iota
	// HoldersStale means that a build without the index added rounds since it
	// was built.
	HoldersStale
	// HoldersComplete means the index covers every round of the store.
	HoldersComplete
)

func (st HoldersState) String() string {
	switch st {
	case HoldersNone:
		return "missing"
	case HoldersStale:
		return "stale"
	case HoldersComplete:
		return "complete"
	}
	return fmt.Sprintf("HoldersState(%d)", uint32(st))
}

// ErrNoHoldersIndex is returned by AssetHolders while the asset holders index
// is missing or stale. The error says which.
var ErrNoHoldersIndex = errors.New("timetravel: the asset holders index is not complete")

type holdersIndexError struct{ state HoldersState }

func (e holdersIndexError) Error() string        { return "the asset holders index is " + e.state.String() }
func (e holdersIndexError) Is(target error) bool { return target == ErrNoHoldersIndex }

// HoldersIndexState returns the state of the asset holders index.
func (s *Store) HoldersIndexState() HoldersState { return HoldersState(s.holders.Load()) }

// loadHolders reads the state of the holders index, writing nothing.
func (s *Store) loadHolders() error {
	v, closer, err := s.db.Get(keyHoldersMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		s.holders.Store(uint32(HoldersNone))
		return nil
	}
	if err != nil {
		return err
	}
	defer closer.Close()
	if len(v) != 8 {
		return fmt.Errorf("timetravel: bad asset holders index meta record")
	}
	// an empty store takes N from the options: one built on with N = 1 and
	// reopened with another N has an index nothing may maintain
	st := HoldersStale
	if binary.BigEndian.Uint64(v) == s.next() && s.n == 1 {
		st = HoldersComplete
	}
	s.holders.Store(uint32(st))
	return nil
}

func holdersMeta(next uint64) []byte { return binary.BigEndian.AppendUint64(nil, next) }

func holdersKey(asset uint64, addr []byte) []byte {
	k := make([]byte, 0, 1+8+len(addr))
	k = append(k, pfxHolders)
	k = binary.BigEndian.AppendUint64(k, asset)
	return append(k, addr...)
}

// boundaries returns the number of boundaries in intervals and the last one.
func boundaries(intervals []byte) (n int, last uint64, err error) {
	for len(intervals) > 0 {
		d, k := binary.Uvarint(intervals)
		if k <= 0 {
			return 0, 0, fmt.Errorf("timetravel: bad asset holders index entry")
		}
		intervals = intervals[k:]
		last += d
		n++
	}
	return n, last, nil
}

// HolderIntervals decodes an asset holders index entry: the rounds at which
// the holding was created and deleted, alternating, the first a creation.
func HolderIntervals(intervals []byte) ([]uint64, error) {
	var out []uint64
	var last uint64
	for len(intervals) > 0 {
		d, k := binary.Uvarint(intervals)
		if k <= 0 {
			return nil, fmt.Errorf("timetravel: bad asset holders index entry")
		}
		intervals = intervals[k:]
		last += d
		out = append(out, last)
	}
	return out, nil
}

// HeldAt reports whether a holding whose index entry is intervals, as
// AssetHolders returns it, is held at round rnd.
func HeldAt(intervals []byte, rnd uint64) bool {
	var at uint64
	held := false
	for len(intervals) > 0 {
		d, k := binary.Uvarint(intervals)
		if k <= 0 {
			return false // AssetHolders returns valid entries only
		}
		intervals = intervals[k:]
		if at += d; at > rnd {
			break
		}
		held = !held
	}
	return held
}

// indexHolders adds to b the index entries of the holdings that the changes
// write of round rnd create or delete, and the meta record of the index then
// covering rnd. The caller holds writeMu.
func (s *Store) indexHolders(b *pebble.Batch, write []Change, rnd uint64) error {
	// without the dedup cache a key can be changed twice in a round: its last
	// change counts, as the batch leaves it
	var last map[string]int
	for i, ch := range write {
		if _, _, ok := HoldingOf(ch.Key); ok {
			if last == nil {
				last = map[string]int{}
			}
			last[string(ch.Key)] = i
		}
	}
	for i, ch := range write {
		addr, asset, ok := HoldingOf(ch.Key)
		if !ok || last[string(ch.Key)] != i {
			continue
		}
		live := !ch.Deleted
		// the cache knows the state of hot holdings: no read for an update
		if s.dedup != nil {
			if prev, known := s.dedup.get(ch.Key); known && !prev.deleted == live {
				continue
			}
		}
		key := holdersKey(asset, addr[:])
		cur, err := s.holdersEntry(key)
		if err != nil {
			return err
		}
		n, at, err := boundaries(cur)
		if err != nil {
			return err
		}
		if (n%2 == 1) == live { // an update, or deleting what is not there
			continue
		}
		if err := b.Set(key, binary.AppendUvarint(cur, rnd-at), nil); err != nil {
			return err
		}
	}
	return b.Set(keyHoldersMeta, holdersMeta(rnd+1), nil)
}

// holdersEntry returns the index entry at key, nil if there is none.
func (s *Store) holdersEntry(key []byte) ([]byte, error) {
	v, closer, err := s.db.Get(key)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer closer.Close()
	return bytes.Clone(v), nil
}

// clearHolders deletes the holders index and its meta record in one synced
// batch: deleting is instant, whatever the index size, and Pebble reclaims the
// space in its own compactions. The caller holds writeMu.
func (s *Store) clearHolders() error {
	s.holders.Store(uint32(HoldersNone))
	b := s.db.NewBatch()
	defer b.Close()
	if err := b.Delete(keyHoldersMeta, nil); err != nil {
		return err
	}
	if err := b.DeleteRange([]byte{pfxHolders}, []byte{pfxHolders + 1}, nil); err != nil {
		return err
	}
	return b.Commit(pebble.Sync)
}

// IndexHolders builds the asset holders index from the holding records of an
// N = 1 store, replacing any index there was, and returns its entries: one
// per holding that ever existed. From then on AddRound keeps it up to date.
// Nothing may read the store meanwhile (see AssetHolders); AddRound waits.
// Cancelled or failed, it leaves no index (HoldersNone), and a rerun starts
// over. CompactHoldersIndex reclaims the space of the entries it replaced.
// progress, if not nil, is called with the records scanned and the entries
// written after each batch of writes and at the end.
func (s *Store) IndexHolders(ctx context.Context, progress func(scanned, indexed uint64)) (uint64, error) {
	if err := s.acquire(); err != nil {
		return 0, err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return 0, ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.n != 1 {
		return 0, fmt.Errorf("timetravel: the asset holders index needs a store with N = 1, not %d", s.n)
	}
	if err := s.clearHolders(); err != nil {
		return 0, err
	}
	lo := []byte{pfxCheckpoint, KindHolding}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
	if err != nil {
		return 0, err
	}
	defer it.Close()
	var scanned, indexed uint64
	committed := false // a batch since the last look at ctx
	w := s.newWriter(func() {
		committed = true
		if progress != nil {
			progress(scanned, indexed)
		}
	})
	defer func() { w.close() }()
	// the current holding, its intervals, whether it is live after the
	// records seen, and its last boundary
	var obj, intervals []byte
	var live bool
	var at uint64
	flush := func() error {
		if len(intervals) == 0 { // never held: tombstones only
			return nil
		}
		addr, asset, ok := HoldingOf(obj)
		if !ok {
			return fmt.Errorf("timetravel: bad holding key %x", obj)
		}
		if err := w.b.Set(holdersKey(asset, addr[:]), intervals, nil); err != nil {
			return err
		}
		indexed++
		return w.maybeCommit()
	}
	for valid := it.First(); valid; valid = it.Next() {
		if (committed || scanned%4096 == 0) && ctx.Err() != nil {
			return indexed, ctx.Err()
		}
		committed = false
		scanned++
		k, v := it.Key(), it.Value()
		if len(k) < 1+1+8 || len(v) == 0 {
			return indexed, fmt.Errorf("timetravel: bad holding record %x", k)
		}
		if o := k[1 : len(k)-8]; !bytes.Equal(o, obj) {
			if err := flush(); err != nil {
				return indexed, err
			}
			obj, intervals, live, at = append(obj[:0], o...), intervals[:0], false, 0
		}
		// a tombstone ends a holding, anything else starts or continues one
		if deleted := v[0] == recDeleted; deleted == live {
			rnd := binary.BigEndian.Uint64(k[len(k)-8:])
			intervals = binary.AppendUvarint(intervals, rnd-at)
			at, live = rnd, !live
		}
	}
	if err := it.Error(); err != nil {
		return indexed, err
	}
	if err := flush(); err != nil {
		return indexed, err
	}
	if err := w.b.Set(keyHoldersMeta, holdersMeta(s.next()), nil); err != nil {
		return indexed, err
	}
	if err := w.finish(); err != nil {
		return indexed, err
	}
	s.holders.Store(uint32(HoldersComplete))
	return indexed, nil
}

// CompactHoldersIndex rewrites the asset holders index, dropping the entries
// that IndexHolders replaced. It compacts in steps, as CompactCheckpoints
// does: at once, the free disk it needs could approach the store's size.
func (s *Store) CompactHoldersIndex(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	return s.compactRange(ctx, []byte{pfxHolders}, []byte{pfxHolders + 1})
}

// DropHoldersIndex deletes the asset holders index, complete, stale or half
// built, and reports whether there was one. It is instant: one synced batch
// with a range tombstone, whatever the index size; Pebble reclaims the space
// in its own compactions. Nothing may read the store meanwhile (see
// AssetHolders).
func (s *Store) DropHoldersIndex() (bool, error) {
	if err := s.acquire(); err != nil {
		return false, err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return false, ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	has, err := s.hasHoldersIndex()
	if err != nil || !has {
		return false, err
	}
	return true, s.clearHolders()
}

// hasHoldersIndex reports whether the store holds the holders index's meta
// record or any of its entries: a half-built index has no meta record.
func (s *Store) hasHoldersIndex() (bool, error) {
	if s.HoldersIndexState() != HoldersNone {
		return true, nil
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxHolders}, UpperBound: []byte{pfxHolders + 1}})
	if err != nil {
		return false, err
	}
	found := it.First()
	return found, it.Close()
}

// AssetHolders calls fn, in address order, for every account after `after`
// (nil: from the first) that ever held asset, with the holding's index entry
// (see HeldAt). It stops when fn returns false. fn owns addr and intervals,
// and must not call the store (Close would deadlock). It fails with
// ErrNoHoldersIndex unless the index is complete. The entries of the rounds
// added while it runs may or may not be seen; they only append boundaries
// later than those of the rounds before. A page of n entries costs a seek and
// n steps, however many holdings the store holds.
func (s *Store) AssetHolders(asset uint64, after []byte, fn func(addr, intervals []byte) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if st := s.HoldersIndexState(); st != HoldersComplete {
		return holdersIndexError{st}
	}
	pfx := holdersKey(asset, nil)
	lo := pfx
	if after != nil {
		lo = prefixEnd(append(slices.Clip(pfx), after...))
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(pfx)})
	if err != nil {
		return err
	}
	defer it.Close()
	for valid := it.First(); valid; valid = it.Next() {
		k, v := it.Key(), it.Value()
		if len(k) != len(pfx)+32 {
			return fmt.Errorf("timetravel: bad asset holders index key %x", k)
		}
		if _, _, err := boundaries(v); err != nil {
			return err
		}
		if !fn(bytes.Clone(k[len(pfx):]), bytes.Clone(v)) {
			return nil
		}
	}
	return it.Error()
}
