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
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/algorand/go-deadlock"
)

// keyTruncateMeta holds, while TruncateAfter runs, the round it deletes the
// records after. A store opened with it finishes the truncation first.
// Strict commitments refuse a store that holds it: metaKeys leaves it out.
var keyTruncateMeta = []byte("mt")

// TruncatedAfter returns the round the last TruncateAfter of this process,
// or of Open finishing one, truncated the store after, if AddRound has added
// no round since: the store then holds no record of a later round. It is not
// stored, since a store from elsewhere could claim it.
func (s *Store) TruncatedAfter() (uint64, bool) {
	v := s.truncatedAfter.Load()
	return v - 1, v != 0
}

// errTruncating is returned by AddRound while a truncation is unfinished.
var errTruncating = errors.New("timetravel: a truncation of the store is unfinished; reopen the store to finish it")

// Truncating reports whether a truncation of the store is unfinished: one
// that failed, or was cancelled, leaves the store refusing rounds until it is
// opened again, which finishes it.
func (s *Store) Truncating() bool {
	return s.truncating.Load()
}

// TruncateOptions configure Store.TruncateAfter.
type TruncateOptions struct {
	// Workers is the goroutines that scan the object records; 0: GOMAXPROCS.
	Workers int
	// Progress, if not nil, is called from time to time with the object
	// records scanned and the records deleted so far, possibly from several
	// goroutines at once.
	Progress func(scanned, deleted uint64)
}

// TruncateAfter deletes every record of the rounds after b: object versions,
// headers, expired stake and box index entries. The store then ends at b and
// holds nothing of a later round that its own AddRound calls did not write,
// which is what a node needs of a store installed from elsewhere before it
// writes the rounds after b from its own evaluation. It returns the object
// records and box index entries it deleted.
//
// It reads every object record, and refuses a store with a record key whose
// object part is not a ValidObjectKey, or out of order, which no node writes:
// reads of an object take every key that starts with the object's for one of
// its records, so such a key could pass for a version of any round. It marks
// the store diverged, ends it at b and returns an error wrapping ErrCorrupt.
// It needs N = 1, as commitments do.
//
// An interrupted or failed truncation (a crash, Close, which cancels it, or
// an error) finishes when the store is next opened writable; until then
// AddRound fails (Truncating), and so does a truncation after a later round.
// Reads may run concurrently, and see the rounds up to b as before: the store
// ends at b from the start of the truncation.
func (s *Store) TruncateAfter(ctx context.Context, b uint64, o TruncateOptions) (uint64, error) {
	return s.truncateAfter(ctx, b, o, s.keySplits)
}

func (s *Store) truncateAfter(ctx context.Context, b uint64, o TruncateOptions, splits splitFunc) (uint64, error) {
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
		return 0, fmt.Errorf("timetravel: truncating needs a store with N = 1, not %d", s.n)
	}
	pending, ok, err := s.pendingTruncate()
	if err != nil {
		return 0, err
	}
	if ok && b > pending {
		return 0, fmt.Errorf("timetravel: a truncation after round %d is unfinished; reopen the store to finish it", pending)
	}
	last, ok := s.Last()
	if !ok {
		return 0, errors.New("timetravel: the store is empty")
	}
	if last < b {
		return 0, fmt.Errorf("timetravel: the store ends at round %d, before %d", last, b)
	}
	if err = s.db.Set(keyTruncateMeta, binary.BigEndian.AppendUint64(nil, b), pebble.Sync); err != nil {
		return 0, err
	}
	s.truncating.Store(true)
	// the records after b go first (headers), so the store ends at b now
	s.last.Store(int64(b))
	// Close cancels a truncation rather than wait minutes for it
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	deleted, err := s.truncate(ctx, b, o, splits)
	if errors.Is(err, ErrCorrupt) {
		// deleting can't make such a store the chain's
		if aerr := s.abandonTruncate(b, err.Error()); aerr != nil {
			// not ErrCorrupt: the store is still marked as truncating
			return deleted, fmt.Errorf("timetravel: abandoning the truncation of a corrupt store (%v): %w", err, aerr)
		}
	}
	return deleted, err
}

// truncate is TruncateAfter once the store is marked. The caller holds
// writeMu.
func (s *Store) truncate(ctx context.Context, b uint64, o TruncateOptions, splits splitFunc) (uint64, error) {
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	after := func(pfx byte) []byte { return binary.BigEndian.AppendUint64([]byte{pfx}, b+1) }
	// the records keyed by round, and the patches and round indexes, which
	// an N = 1 store never writes
	for _, r := range [][2][]byte{
		{after(pfxHeader), {pfxHeader + 1}},
		{after(pfxExpired), {pfxExpired + 1}},
		{{pfxPatch}, {pfxPatch + 1}},
		{{pfxRoundIndex}, {pfxRoundIndex + 1}},
	} {
		if err := s.db.DeleteRange(r[0], r[1], pebble.NoSync); err != nil {
			return 0, err
		}
	}
	var scanned, deleted atomic.Uint64
	report := func() {
		if o.Progress != nil {
			o.Progress(scanned.Load(), deleted.Load())
		}
	}
	if err := s.truncateBoxIndex(ctx, b, &deleted); err != nil {
		return deleted.Load(), err
	}
	if err := s.truncateVersions(ctx, b, o.Workers, splits, &scanned, &deleted, report); err != nil {
		return deleted.Load(), err
	}

	if err := s.endTruncate(b, nil); err != nil {
		return deleted.Load(), err
	}
	report()
	return deleted.Load(), nil
}

// endTruncate records that the store ends at b, in one write with the
// truncation mark's removal and what extra adds. The caller holds writeMu.
func (s *Store) endTruncate(b uint64, extra func(*pebble.Batch) error) error {
	batch := s.db.NewBatch()
	defer batch.Close()
	meta := binary.BigEndian.AppendUint64(nil, b)
	meta = binary.BigEndian.AppendUint64(meta, s.n)
	meta = append(meta, boolByte(s.incremental))
	// the index lists every box change from b+1 on once AddRound adds them,
	// whatever it was complete from before
	from := s.boxFrom.Load()
	if from == boxIndexNone || from > b+1 {
		from = b + 1
	}
	for _, err := range []error{
		batch.Set(keyMeta, meta, nil),
		batch.Set(keyBoxIndexMeta, boxIndexMeta(from, b+1), nil),
		batch.Delete(keyTruncateMeta, nil),
	} {
		if err != nil {
			return err
		}
	}
	// the rounds after b are gone, so none of the history after b is
	// installed: the node writes it
	// (a bad record is taken as installed through b, the safe side)
	through, ok, err := s.installedThrough()
	if err != nil && !errors.Is(err, ErrCorrupt) {
		return err
	}
	if err != nil || ok && through > b {
		if err := batch.Set(keyInstalledThroughMeta, binary.BigEndian.AppendUint64(nil, b), nil); err != nil {
			return err
		}
	}
	if extra != nil {
		if err := extra(batch); err != nil {
			return err
		}
	}
	if err := batch.Commit(pebble.Sync); err != nil {
		return err
	}
	s.boxGen.Add(1)
	s.boxFrom.Store(from)
	s.boxGen.Add(1)
	s.last.Store(int64(b))
	s.truncatedAfter.Store(b + 1)
	// it may know states of records that are gone
	s.dedup.reset()
	s.truncating.Store(false)
	return nil
}

// truncateBoxIndex deletes the box index entries of rounds after b, and the
// keys too short to be entries, which BoxChanges refuses.
func (s *Store) truncateBoxIndex(ctx context.Context, b uint64, deleted *atomic.Uint64) error {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxBoxIndex}, UpperBound: []byte{pfxBoxIndex + 1}})
	if err != nil {
		return err
	}
	defer it.Close()
	var del uint64
	w := s.newWriter(func() { deleted.Add(del); del = 0 })
	defer func() { w.close() }()
	// an app's entries are by round: seek past its rounds up to b
	for valid := it.First(); valid; {
		if err := ctx.Err(); err != nil {
			return err
		}
		k := it.Key()
		if len(k) >= 1+8+8 && binary.BigEndian.Uint64(k[9:17]) <= b {
			valid = it.SeekGE(binary.BigEndian.AppendUint64(slices.Clone(k[:9]), b+1))
			continue
		}
		if err := w.b.Delete(k, nil); err != nil {
			return err
		}
		del++
		if err := w.maybeCommit(); err != nil {
			return err
		}
		valid = it.Next()
	}
	if err := it.Error(); err != nil {
		return err
	}
	return w.finish()
}

// truncateVersions deletes the object records of rounds after b, with
// workers goroutines that each scan a range of whole objects, and checks
// that no object key starts with another.
func (s *Store) truncateVersions(ctx context.Context, b uint64, workers int, splits splitFunc, scanned, deleted *atomic.Uint64, report func()) error {
	// the deletions go to the store; the scans read what it held before
	snap := s.db.NewSnapshot()
	defer snap.Close()
	lo, hi := []byte{pfxCheckpoint}, []byte{pfxCheckpoint + 1}
	keys, err := splits(snap, lo, hi, 16*workers)
	if err != nil {
		return err
	}
	bounds := rangeBounds(lo, hi, keys, false)
	n := len(bounds) - 1

	// each range's first and last objects, to check them across ranges
	type ends struct{ first, last []byte }
	parts := make([]ends, n)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var failOnce sync.Once
	var failed error
	next := make(chan int, n)
	for i := range n {
		next <- i
	}
	close(next)
	var wg sync.WaitGroup
	for range min(workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				first, last, err := s.truncateRange(ctx, snap, bounds[i], bounds[i+1], b, scanned, deleted, report)
				if err != nil {
					failOnce.Do(func() { failed = err; cancel() })
					return
				}
				parts[i] = ends{first, last}
			}
		}()
	}
	wg.Wait()
	if failed != nil {
		return failed
	}
	var prev []byte
	for _, p := range parts {
		if p.first == nil {
			continue
		}
		if prev != nil && !objectsInOrder(prev, p.first) {
			return fmt.Errorf("%w: object key %x follows %x", ErrCorrupt, p.first, prev)
		}
		prev = p.last
	}
	return nil
}

// truncateRange is truncateVersions for the object records in [lo, hi). It
// returns the range's first and last objects, nil if it has none.
func (s *Store) truncateRange(ctx context.Context, snap *pebble.Snapshot, lo, hi []byte, b uint64, scanned, deleted *atomic.Uint64, report func()) (first, last []byte, err error) {
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, nil, err
	}
	defer it.Close()
	var n, del uint64
	flush := func() {
		scanned.Add(n)
		deleted.Add(del)
		n, del = 0, 0
		report()
	}
	w := s.newWriter(flush)
	defer func() { w.close() }()
	var obj []byte
	in := false
	for valid := it.First(); valid; valid = it.Next() {
		if n%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, nil, err
			}
			if n >= 1<<20 {
				flush()
			}
		}
		n++
		k := it.Key()
		if len(k) < 1+8 {
			return nil, nil, fmt.Errorf("%w: key %x", ErrCorrupt, k)
		}
		if o := k[1 : len(k)-8]; !in || !bytes.Equal(o, obj) {
			// the reads of an object take every key that starts with it for
			// one of its records: a key that is not an object's and a round
			// could pass for a record of any round
			if !ValidObjectKey(o) {
				return nil, nil, fmt.Errorf("%w: record key %x is not of an object", ErrCorrupt, k)
			}
			if in && !objectsInOrder(obj, o) {
				return nil, nil, fmt.Errorf("%w: object key %x follows %x", ErrCorrupt, o, obj)
			}
			if !in {
				first = slices.Clone(o)
			}
			obj, in = append(obj[:0], o...), true
		}
		if binary.BigEndian.Uint64(k[len(k)-8:]) <= b {
			continue
		}
		if err := w.b.Delete(k, nil); err != nil {
			return nil, nil, err
		}
		del++
		if err := w.maybeCommit(); err != nil {
			return nil, nil, err
		}
	}
	if err := it.Error(); err != nil {
		return nil, nil, err
	}
	if err := w.finish(); err != nil {
		return nil, nil, err
	}
	if in {
		last = slices.Clone(obj)
	}
	return first, last, nil
}

// abandonTruncate ends a truncation that found the store corrupt: it marks
// the store diverged, unless it is already, and ends it at b, which the
// records it already deleted no longer let it pass, so that it opens. The
// caller holds writeMu.
func (s *Store) abandonTruncate(b uint64, reason string) error {
	_, diverged, err := s.divergedLocked()
	if err != nil {
		return err
	}
	return s.endTruncate(b, func(batch *pebble.Batch) error {
		if diverged {
			return nil
		}
		return batch.Set(keyDivergedMeta, []byte(reason), nil)
	})
}

// pendingTruncate returns the round of an unfinished truncation.
func (s *Store) pendingTruncate() (uint64, bool, error) {
	v, closer, err := s.db.Get(keyTruncateMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, false, fmt.Errorf("%w: bad truncation record", ErrCorrupt)
	}
	return binary.BigEndian.Uint64(v), true, nil
}

// finishTruncate finishes, as Open does, a truncation that was interrupted.
// One that finds the store corrupt leaves it marked diverged, and open.
func (s *Store) finishTruncate() error {
	b, ok, err := s.pendingTruncate()
	if err != nil || !ok {
		return err
	}
	if s.readOnly {
		return fmt.Errorf("timetravel: a truncation of the store after round %d is unfinished: open it writable (start algod) to finish it", b)
	}
	s.truncating.Store(true)
	infof := s.infof
	infof("timetravel: finishing an interrupted truncation of the store after round %d", b)
	var mu deadlock.Mutex
	logged := time.Now()
	deleted, err := s.TruncateAfter(context.Background(), b, TruncateOptions{Progress: func(scanned, deleted uint64) {
		mu.Lock()
		defer mu.Unlock()
		if time.Since(logged) >= 30*time.Second {
			logged = time.Now()
			infof("timetravel: truncating the store after round %d: %d records scanned, %d deleted", b, scanned, deleted)
		}
	}})
	if errors.Is(err, ErrCorrupt) {
		infof("timetravel: the store is corrupt, marked diverged: %v", err)
		return nil
	}
	if err == nil {
		infof("timetravel: truncated the store after round %d: %d records deleted", b, deleted)
	}
	return err
}
