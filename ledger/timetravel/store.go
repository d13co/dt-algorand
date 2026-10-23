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

// Package timetravel stores the full history of ledger objects so that any
// object can be projected at any round.
//
// Every object (account, holding, local state, asset/app params, kv/box,
// totals) is addressed by an object key. Its history lives in two
// object-major keyspaces, ordered by (object, round):
//
//	'c' obj round -> full value or tombstone   (sparse checkpoints)
//	'p' obj round -> patch, full value or tombstone
//
// Every N rounds a checkpoint round c writes the value of every object that
// changed in (c-N, c]; objects that did not change are not repeated. Rounds
// in (c, c+N) write, per changed object, a patch against the object's value
// at c. Reading obj@R is two bounded seeks: the latest checkpoint record at
// or before c(R) -- however many sparse checkpoints back that is -- and the
// latest patch in (c(R), R]. Genesis (round 0) is the only full checkpoint.
//
// 'r' round -> keys changed in that round, kept only until the next
// checkpoint is materialized. 'H' round -> encoded block header.
// 'E' round -> the round's expired online stake (uvarint), see ExpiredStake.
// 'b' app round name -> 'F' or 'D', the box index: an entry per box record
// AddRound writes, round-major per app, so that BoxChanges reads a range of
// an app's rounds without visiting every box the app ever had (see
// BoxIndexFrom).
//
// N = 1 (every version stored in full, one seek per read, writes that read
// nothing) is the recommended setting: with zstd over large blocks, an
// object's adjacent versions compress as well as patches would, at a fraction
// of the write cost. See README.md for the evaluation on mainnet data.
//
// A change that repeats the state of its object's latest record is not
// stored when an in-memory cache of recently written objects knows that state
// (see lastWritten); RemoveNoops removes the ones it missed.
package timetravel

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"

	"github.com/algorand/go-deadlock"
)

const (
	pfxCheckpoint = 'c'
	pfxPatch      = 'p'
	pfxRoundIndex = 'r'
	pfxHeader     = 'H'
	pfxExpired    = 'E'
	pfxBoxIndex   = 'b'
)

var keyMeta = []byte("m")

// keyBoxIndexMeta holds the box index's state: the round it is complete from
// (boxIndexNone while IndexBoxes or RemoveNoops rebuilds it), then the last
// round whose entries AddRound wrote. A store that a build without the index
// extends is behind the latter, and Open then moves the former past it.
var keyBoxIndexMeta = []byte("mb")

// boxIndexNone is the round the box index is complete from while it is rebuilt.
const boxIndexNone = ^uint64(0)

// record tags
const (
	recFull    = 'F'
	recDeleted = 'D'
	recPatch   = 'X'
)

// Options configure a new store. Existing stores keep the parameters they
// were created with.
type Options struct {
	// CheckpointInterval is N. 1 stores every version in full.
	CheckpointInterval uint64
	// Incremental chains patches from the previous version instead of the
	// checkpoint value. Smaller patches, reads apply up to N-1 of them.
	Incremental bool
	// CacheSize is the block cache size in bytes.
	CacheSize int64
	// BlockSize is the sstable block size; larger compresses better.
	BlockSize int
	// Compression defaults to zstd.
	Compression *sstable.CompressionProfile
	// Logger receives engine messages; nil drops informational ones.
	Logger pebble.Logger
	// DedupEntries bounds the recently written objects whose changes
	// AddRound skips when they repeat the stored state; 0 is the default
	// (2^18, ~30 MB), negative stores every change.
	DedupEntries int
	// ReadOnly opens an existing store without writing to it: no new store,
	// no compactions, and AddRound, RemoveNoops, CompactCheckpoints and Sync
	// fail with ErrReadOnly. The store is still locked, so another process
	// cannot open it meanwhile.
	ReadOnly bool
}

// Change is the new value of one object in a round.
type Change struct {
	Key     []byte
	Value   []byte
	Deleted bool
}

// Store is the history database. AddRound must be called with consecutive
// rounds; Get/Header/Scan may be called concurrently for any round up to
// Last(), and fail with ErrClosed after Close.
type Store struct {
	db          *pebble.DB
	n           uint64
	incremental bool
	readOnly    bool
	last        atomic.Int64 // -1 when empty

	writeMu deadlock.Mutex // serializes AddRound, RemoveNoops and IndexBoxes
	dedup   *lastWritten   // nil: store every change; guarded by writeMu

	boxFrom atomic.Uint64 // see BoxIndexFrom
	boxGen  atomic.Uint64 // odd while boxFrom changes; see setBoxFrom

	// closeMu is held for reading by every operation, including while Versions,
	// BoxChanges and the like run their callbacks. A callback that reads the
	// store takes it again, which deadlocks if Close is waiting in between
	// (a waiting writer blocks new readers), so algod callbacks must not do
	// that. ttdump versions does (it reads headers), which is safe only
	// because nothing closes its store concurrently; go-deadlock would report
	// it as recursive locking in ttdump's tests, so it stays a sync.RWMutex.
	closeMu sync.RWMutex `algofix:"allow sync.Mutex"`
	closed  bool
	// closing is closed when Close starts, so that long reads (Commitment)
	// stop instead of holding Close, and every reader behind it, for minutes
	closing   chan struct{}
	closeOnce sync.Once
}

// commitBatch commits a round's batch; tests make it fail.
var commitBatch = func(b *pebble.Batch) error { return b.Commit(pebble.NoSync) }

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("timetravel: store closed")

// ErrCorrupt is wrapped by the errors of reads that find a record they can't
// decode.
var ErrCorrupt = errors.New("timetravel: corrupt record")

// ErrReadOnly is returned by writes to a store opened with Options.ReadOnly.
var ErrReadOnly = errors.New("timetravel: store opened read-only")

func (s *Store) acquire() error {
	s.closeMu.RLock()
	if s.closed {
		s.closeMu.RUnlock()
		return ErrClosed
	}
	return nil
}

// Open opens or creates a store in dir.
func Open(dir string, o Options) (*Store, error) {
	if o.CheckpointInterval == 0 {
		o.CheckpointInterval = 1
	}
	if o.CacheSize == 0 {
		o.CacheSize = 256 << 20
	}
	if o.BlockSize == 0 {
		o.BlockSize = 128 << 10
	}
	if o.Compression == nil {
		o.Compression = sstable.ZstdCompression
	}
	if o.Logger == nil {
		o.Logger = quietLogger{}
	}
	cache := pebble.NewCache(o.CacheSize)
	defer cache.Unref()
	opts := &pebble.Options{
		Cache:                      cache,
		MemTableSize:               64 << 20,
		CompactionConcurrencyRange: func() (int, int) { return 1, 4 },
		L0CompactionThreshold:      4,
		L0StopWritesThreshold:      64,
		LBaseMaxBytes:              512 << 20,
		FormatMajorVersion:         pebble.FormatNewest,
		MaxOpenFiles:               4096,
		Logger:                     o.Logger,
		ReadOnly:                   o.ReadOnly,
	}
	for i := range opts.Levels {
		l := &opts.Levels[i]
		l.BlockSize = o.BlockSize
		l.IndexBlockSize = 256 << 10
		l.Compression = func() *sstable.CompressionProfile { return o.Compression }
	}
	opts.TargetFileSizes[0] = 64 << 20
	opts.EnsureDefaults()
	db, err := pebble.Open(dir, opts)
	if err != nil {
		return nil, err
	}
	s := &Store{db: db, n: o.CheckpointInterval, incremental: o.Incremental, readOnly: o.ReadOnly, closing: make(chan struct{})}
	if !o.ReadOnly {
		s.dedup = newLastWritten(o.DedupEntries)
	}
	s.last.Store(-1)
	if err := s.loadMeta(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadBoxIndex(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) loadMeta() error {
	v, closer, err := s.db.Get(keyMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer closer.Close()
	if len(v) != 17 {
		return fmt.Errorf("timetravel: bad meta record")
	}
	s.last.Store(int64(binary.BigEndian.Uint64(v)))
	s.n = binary.BigEndian.Uint64(v[8:])
	s.incremental = v[16] == 1
	return nil
}

// next is the first round that AddRound has not added.
func (s *Store) next() uint64 { return uint64(s.last.Load() + 1) }

func boxIndexMeta(from, next uint64) []byte {
	return binary.BigEndian.AppendUint64(binary.BigEndian.AppendUint64(nil, from), next)
}

// loadBoxIndex sets the round the box index is complete from and, unless the
// store is read-only, records it.
func (s *Store) loadBoxIndex() error {
	next := s.next()
	var from uint64
	v, closer, err := s.db.Get(keyBoxIndexMeta)
	switch {
	case errors.Is(err, pebble.ErrNotFound):
		// a store from before the index is complete if it has no box
		var has bool
		if has, err = s.hasBoxes(); err != nil {
			return err
		}
		if has {
			from = next
		}
	case err != nil:
		return err
	default:
		if len(v) != 16 {
			closer.Close()
			return fmt.Errorf("timetravel: bad box index meta record")
		}
		// indexed: the first round AddRound did not index
		stored, indexed := binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])
		from = stored
		closer.Close()
		switch {
		case from == boxIndexNone: // a rebuild was interrupted: the rounds added since are indexed
			from = next
		case indexed < next: // a build without the index added rounds
			from = max(from, next)
		}
		if from == stored && indexed == next {
			s.boxFrom.Store(from)
			return nil
		}
	}
	s.boxFrom.Store(from)
	if s.readOnly {
		return nil
	}
	return s.db.Set(keyBoxIndexMeta, boxIndexMeta(from, next), pebble.Sync)
}

// hasBoxes reports whether the store holds a record of any box.
func (s *Store) hasBoxes() (bool, error) {
	for _, pfx := range []byte{pfxCheckpoint, pfxPatch} {
		lo := append([]byte{pfx}, kvBoxPrefix...)
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
		if err != nil {
			return false, err
		}
		found := it.First()
		if err := it.Close(); err != nil {
			return false, err
		}
		if found {
			return true, nil
		}
	}
	return false, nil
}

// setBoxFrom records the round the box index is complete from. The caller
// holds writeMu.
func (s *Store) setBoxFrom(from uint64) error {
	// BoxChanges reads the generation around opening its iterators: a reader
	// whose iterators may predate a rebuild's start or end sees it change
	s.boxGen.Add(1)
	s.boxFrom.Store(from)
	s.boxGen.Add(1)
	return s.db.Set(keyBoxIndexMeta, boxIndexMeta(from, s.next()), pebble.Sync)
}

// BoxIndexFrom returns the round from which the box index lists every change
// of every box, so that BoxChanges answers for ranges that start there. ok is
// false while IndexBoxes or RemoveNoops rebuilds the index. The index of a
// store created by a build that has it, or first opened by one while it held
// no box, is complete from round 0. Otherwise it starts at the store's next
// round at the time, until IndexBoxes builds it; likewise once a build
// without the index has added rounds.
func (s *Store) BoxIndexFrom() (uint64, bool) {
	from := s.boxFrom.Load()
	return from, from != boxIndexNone
}

// ErrNoBoxIndex is returned by BoxChanges for a range that starts before
// BoxIndexFrom.
var ErrNoBoxIndex = errors.New("timetravel: the box index is incomplete")

func boxIndexError(from uint64) error {
	if from == boxIndexNone {
		return fmt.Errorf("%w: it is being rebuilt", ErrNoBoxIndex)
	}
	return fmt.Errorf("%w before round %d", ErrNoBoxIndex, from)
}

type quietLogger struct{}

func (quietLogger) Infof(string, ...any)         {}
func (quietLogger) Errorf(f string, args ...any) { pebble.DefaultLogger.Errorf(f, args...) }
func (quietLogger) Fatalf(f string, args ...any) { pebble.DefaultLogger.Fatalf(f, args...) }

// Close closes the store, waiting for in-flight operations.
func (s *Store) Close() error {
	s.closeOnce.Do(func() { close(s.closing) })
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	return s.db.Close()
}

// Last returns the last stored round, or ok=false if the store is empty.
func (s *Store) Last() (uint64, bool) {
	l := s.last.Load()
	return uint64(l), l >= 0
}

// First returns the first stored round, the round of the first header
// record, or ok=false if the store is empty. A store built from genesis
// starts at 0.
func (s *Store) First() (uint64, bool, error) {
	if err := s.acquire(); err != nil {
		return 0, false, err
	}
	defer s.closeMu.RUnlock()
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxHeader}, UpperBound: []byte{pfxHeader + 1}})
	if err != nil {
		return 0, false, err
	}
	defer it.Close()
	if !it.First() {
		return 0, false, it.Error()
	}
	return binary.BigEndian.Uint64(it.Key()[1:]), true, nil
}

// CheckpointInterval returns N.
func (s *Store) CheckpointInterval() uint64 { return s.n }

// Incremental reports whether patches chain from the previous version.
func (s *Store) Incremental() bool { return s.incremental }

// Sync makes all added rounds durable.
func (s *Store) Sync() error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	return s.db.LogData(nil, pebble.Sync)
}

// DB exposes the underlying engine (metrics, compaction in tools).
func (s *Store) DB() *pebble.DB { return s.db }

func verKey(pfx byte, obj []byte, rnd uint64) []byte {
	k := make([]byte, 0, 1+len(obj)+8)
	k = append(k, pfx)
	k = append(k, obj...)
	return binary.BigEndian.AppendUint64(k, rnd)
}

// AddRound stores the changes of round rnd, its encoded header and, if not
// nil, its expired online stake. Rounds at or below Last() are skipped, so
// replaying after a crash is harmless. An empty store starts at any round;
// objects not seen since are treated as absent before it. Changes that the
// dedup cache knows repeat their object's stored state are not stored; the
// round's digest covers all of them. AddRound does not modify changes.
func (s *Store) AddRound(rnd uint64, changes []Change, hdr []byte, expired *uint64) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	last, ok := s.Last()
	if ok && rnd <= last {
		return nil
	}
	if ok && rnd != last+1 {
		return fmt.Errorf("timetravel: round %d does not follow stored round %d", rnd, last)
	}
	write, states := s.dedup.filter(changes)
	b := s.db.NewBatch()
	defer b.Close()
	for _, ch := range write {
		if err := indexBox(b, ch, rnd); err != nil {
			return err
		}
	}
	if err := b.Set(keyBoxIndexMeta, boxIndexMeta(s.boxFrom.Load(), rnd+1), nil); err != nil {
		return err
	}
	c := rnd - rnd%s.n
	if rnd == c {
		if err := s.checkpoint(b, rnd, write); err != nil {
			return err
		}
	} else {
		var idx []byte
		for _, ch := range write {
			rec, err := s.patchRecord(ch, rnd, c)
			if err != nil {
				return err
			}
			if err := b.Set(verKey(pfxPatch, ch.Key, rnd), rec, nil); err != nil {
				return err
			}
			idx = binary.AppendUvarint(idx, uint64(len(ch.Key)))
			idx = append(idx, ch.Key...)
		}
		if len(idx) > 0 {
			if err := b.Set(binary.BigEndian.AppendUint64([]byte{pfxRoundIndex}, rnd), idx, nil); err != nil {
				return err
			}
		}
	}
	// the header record also carries the round's ChangesDigest
	if err := b.Set(binary.BigEndian.AppendUint64([]byte{pfxHeader}, rnd), append(ChangesDigest(changes), hdr...), nil); err != nil {
		return err
	}
	if expired != nil {
		if err := b.Set(binary.BigEndian.AppendUint64([]byte{pfxExpired}, rnd), binary.AppendUvarint(nil, *expired), nil); err != nil {
			return err
		}
	}
	meta := binary.BigEndian.AppendUint64(nil, rnd)
	meta = binary.BigEndian.AppendUint64(meta, s.n)
	meta = append(meta, boolByte(s.incremental))
	if err := b.Set(keyMeta, meta, nil); err != nil {
		return err
	}
	if err := commitBatch(b); err != nil {
		return err
	}
	s.last.Store(int64(rnd))
	// only now: a round that failed to commit is added again, and must not
	// find its own changes in the cache
	s.dedup.update(states)
	return nil
}

func boxIndexKey(app, rnd uint64, name []byte) []byte {
	k := make([]byte, 0, 1+8+8+len(name))
	k = append(k, pfxBoxIndex)
	k = binary.BigEndian.AppendUint64(k, app)
	k = binary.BigEndian.AppendUint64(k, rnd)
	return append(k, name...)
}

// boxIndexEntry is the value of a box index entry: the box's record written
// or deleted.
func boxIndexEntry(deleted bool) []byte {
	if deleted {
		return []byte{recDeleted}
	}
	return []byte{recFull}
}

// indexBox adds the box index entry of ch, if it changes a box, in round rnd.
func indexBox(b *pebble.Batch, ch Change, rnd uint64) error {
	app, name, ok := BoxOf(ch.Key)
	if !ok {
		return nil
	}
	return b.Set(boxIndexKey(app, rnd, name), boxIndexEntry(ch.Deleted), nil)
}

func boolByte(b bool) byte {
	if b {
		return 1
	}
	return 0
}

func fullRecord(ch Change) []byte {
	if ch.Deleted {
		return []byte{recDeleted}
	}
	return append([]byte{recFull}, ch.Value...)
}

func (s *Store) patchRecord(ch Change, rnd, c uint64) ([]byte, error) {
	if ch.Deleted {
		return []byte{recDeleted}, nil
	}
	var base []byte
	var ok bool
	var err error
	if s.incremental {
		base, ok, err = s.get(ch.Key, rnd-1)
	} else {
		base, ok, err = s.checkpointValue(ch.Key, c)
	}
	if err != nil || !ok {
		return fullRecord(ch), err
	}
	p := Diff(base, ch.Value)
	if len(p) >= len(ch.Value) {
		return fullRecord(ch), nil
	}
	return append([]byte{recPatch}, p...), nil
}

// checkpoint materializes checkpoint rnd: the value at rnd of every object
// changed since the previous checkpoint.
func (s *Store) checkpoint(b *pebble.Batch, rnd uint64, changes []Change) error {
	written := make(map[string]bool, len(changes))
	for _, ch := range changes {
		written[string(ch.Key)] = true
		if err := b.Set(verKey(pfxCheckpoint, ch.Key, rnd), fullRecord(ch), nil); err != nil {
			return err
		}
	}
	if rnd == 0 || s.n == 1 {
		return nil
	}
	lo := binary.BigEndian.AppendUint64([]byte{pfxRoundIndex}, rnd-s.n+1)
	hi := binary.BigEndian.AppendUint64([]byte{pfxRoundIndex}, rnd)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		idx := it.Value()
		for len(idx) > 0 {
			l, k := binary.Uvarint(idx)
			if k <= 0 || uint64(len(idx)-k) < l {
				return fmt.Errorf("timetravel: corrupt round index")
			}
			key := idx[k : k+int(l)]
			idx = idx[k+int(l):]
			if written[string(key)] {
				continue
			}
			written[string(key)] = true
			v, ok, err := s.get(key, rnd-1)
			if err != nil {
				return err
			}
			if err := b.Set(verKey(pfxCheckpoint, key, rnd), fullRecord(Change{Value: v, Deleted: !ok}), nil); err != nil {
				return err
			}
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	return b.DeleteRange(lo, hi, nil)
}

// removeNoopsBatchOps is the number of writes RemoveNoops and IndexBoxes
// commit at once.
var removeNoopsBatchOps = uint32(1 << 16)

// RemoveNoops deletes the records of an N = 1 store that change nothing:
// those that repeat the object's previous record (the no-op writes of stores
// written before AddRound skipped them, and the ones its cache missed), and
// tombstones before an object's first value (objects created and deleted in
// one round). Every read answers as before, except that Objects no longer
// lists objects that never had a value. In the same pass it rebuilds the box
// index, as IndexBoxes does. CompactCheckpoints reclaims the space. progress,
// if not nil, is called with the records scanned and removed after each batch
// of writes and at the end. Reads may run concurrently; AddRound waits.
func (s *Store) RemoveNoops(ctx context.Context, progress func(scanned, removed uint64)) (uint64, error) {
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
		return 0, fmt.Errorf("timetravel: RemoveNoops needs a store with N = 1, not %d", s.n)
	}
	removed, _, err := s.sweep(ctx, []byte{pfxCheckpoint}, []byte{pfxCheckpoint + 1}, true, func(scanned, removed, _ uint64) {
		if progress != nil {
			progress(scanned, removed)
		}
	})
	return removed, s.rebuilt(err)
}

// IndexBoxes rebuilds the box index from the box records, so that it is
// complete from round 0 (BoxIndexFrom), and returns its entries: one per box
// version, so none for the no-op writes that AddRound indexes when its cache
// misses them. It is idempotent; CompactBoxIndex reclaims the space of the
// entries it replaced. progress, if not nil, is called with the records (N =
// 1) or boxes (N > 1) scanned and the entries written after each batch of
// writes and at the end. Reads may run concurrently, but BoxChanges fails
// until it is done; AddRound waits.
func (s *Store) IndexBoxes(ctx context.Context, progress func(scanned, indexed uint64)) (uint64, error) {
	if err := s.acquire(); err != nil {
		return 0, err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return 0, ErrReadOnly
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	report := func(scanned, _, indexed uint64) {
		if progress != nil {
			progress(scanned, indexed)
		}
	}
	var indexed uint64
	var err error
	if s.n == 1 {
		lo := append([]byte{pfxCheckpoint}, kvBoxPrefix...)
		_, indexed, err = s.sweep(ctx, lo, prefixEnd(lo), false, report)
	} else {
		indexed, err = s.indexPatched(ctx, report)
	}
	return indexed, s.rebuilt(err)
}

// rebuilt ends a rebuild of the box index that returned err. One that failed
// leaves the index complete from the next round, which AddRound will index.
func (s *Store) rebuilt(err error) error {
	if err != nil && s.boxFrom.Load() == boxIndexNone {
		if serr := s.setBoxFrom(s.next()); serr != nil {
			return errors.Join(err, serr)
		}
	}
	return err
}

// writer commits a long series of writes in batches.
type writer struct {
	s        *Store
	b        *pebble.Batch
	progress func()
}

func (s *Store) newWriter(progress func()) *writer {
	return &writer{s: s, b: s.db.NewBatch(), progress: progress}
}

// maybeCommit commits the batch once it holds removeNoopsBatchOps writes.
func (w *writer) maybeCommit() error {
	if w.b.Count() < removeNoopsBatchOps {
		return nil
	}
	if err := w.b.Commit(pebble.NoSync); err != nil {
		return err
	}
	w.b.Close()
	w.b = w.s.db.NewBatch()
	w.progress()
	return nil
}

func (w *writer) finish() error {
	if err := w.b.Commit(pebble.Sync); err != nil {
		return err
	}
	w.progress()
	return nil
}

func (w *writer) close() { w.b.Close() }

// clearBoxIndex marks the box index incomplete and deletes its entries, for a
// rebuild. The caller holds writeMu.
func (s *Store) clearBoxIndex() error {
	// BoxChanges checks the mark after opening its iterator: one that misses
	// it reads the entries as they were before the deletion
	if err := s.setBoxFrom(boxIndexNone); err != nil {
		return err
	}
	return s.db.DeleteRange([]byte{pfxBoxIndex}, []byte{pfxBoxIndex + 1}, pebble.Sync)
}

// sweep walks the checkpoint records of an N = 1 store with keys in [lower,
// upper), each compared with its object's previous record, and rebuilds the
// box index from the box records that are versions. With remove, it also
// deletes the records that are not. It returns the records it found that are
// not versions, and the index entries it wrote. The caller holds writeMu.
func (s *Store) sweep(ctx context.Context, lower, upper []byte, remove bool, progress func(scanned, removed, indexed uint64)) (removed, indexed uint64, err error) {
	if err = s.clearBoxIndex(); err != nil {
		return 0, 0, err
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lower, UpperBound: upper})
	if err != nil {
		return 0, 0, err
	}
	defer it.Close()
	var scanned uint64
	w := s.newWriter(func() { progress(scanned, removed, indexed) })
	defer func() { w.close() }()
	// the current object, its box if it is one, its latest kept record, and
	// the run of records [first, last] that repeat it
	var obj, prev, first, last, name []byte
	var app uint64
	var box bool
	deleteRun := func() error {
		if first == nil {
			return nil
		}
		var err error
		if bytes.Equal(first, last) {
			err = w.b.Delete(first, nil)
		} else {
			// the keys in between are the object's: object keys never prefix one another
			err = w.b.DeleteRange(first, append(last, 0), nil)
		}
		first = nil
		if err != nil {
			return err
		}
		return w.maybeCommit()
	}
	for valid := it.First(); valid; valid = it.Next() {
		if scanned%4096 == 0 && ctx.Err() != nil {
			return removed, indexed, ctx.Err()
		}
		scanned++
		k, v := it.Key(), it.Value()
		if o := k[1 : len(k)-8]; !bytes.Equal(o, obj) {
			if err := deleteRun(); err != nil {
				return removed, indexed, err
			}
			obj = append(obj[:0], o...)
			prev = append(prev[:0], recDeleted) // absent before its first record
			app, name, box = BoxOf(obj)
		}
		if bytes.Equal(v, prev) {
			if remove {
				if first == nil {
					first = append([]byte(nil), k...)
				}
				last = append(last[:0], k...)
			}
			removed++
			continue
		}
		if err := deleteRun(); err != nil {
			return removed, indexed, err
		}
		prev = append(prev[:0], v...)
		if box {
			rnd := binary.BigEndian.Uint64(k[len(k)-8:])
			if err := w.b.Set(boxIndexKey(app, rnd, name), boxIndexEntry(v[0] == recDeleted), nil); err != nil {
				return removed, indexed, err
			}
			indexed++
			if err := w.maybeCommit(); err != nil {
				return removed, indexed, err
			}
		}
	}
	if err := it.Error(); err != nil {
		return removed, indexed, err
	}
	if err := deleteRun(); err != nil {
		return removed, indexed, err
	}
	if err := w.finish(); err != nil {
		return removed, indexed, err
	}
	return removed, indexed, s.setBoxFrom(0)
}

// indexPatched is IndexBoxes for N > 1: every box's versions, one box at a
// time.
func (s *Store) indexPatched(ctx context.Context, progress func(scanned, removed, indexed uint64)) (uint64, error) {
	if err := s.clearBoxIndex(); err != nil {
		return 0, err
	}
	last := uint64(s.last.Load())
	if s.last.Load() < 0 {
		return 0, s.setBoxFrom(0)
	}
	var scanned, indexed uint64
	w := s.newWriter(func() { progress(scanned, 0, indexed) })
	defer func() { w.close() }()
	var ferr error
	err := s.distinct(kvBoxPrefix, nil, func(obj []byte) bool {
		app, name, ok := BoxOf(obj)
		if !ok {
			return true
		}
		if ferr = ctx.Err(); ferr != nil {
			return false
		}
		scanned++
		err := s.patchedVersions(obj, 0, last, false, func(rnd uint64, _ []byte, ok bool) bool {
			if ferr = w.b.Set(boxIndexKey(app, rnd, name), boxIndexEntry(!ok), nil); ferr != nil {
				return false
			}
			indexed++
			ferr = w.maybeCommit()
			return ferr == nil
		})
		if ferr == nil {
			ferr = err
		}
		return ferr == nil
	})
	if err == nil {
		err = ferr
	}
	if err == nil {
		err = w.finish()
	}
	if err != nil {
		return indexed, err
	}
	return indexed, s.setBoxFrom(0)
}

// CompactCheckpoints rewrites the checkpoint keyspace and the box index,
// dropping the records that RemoveNoops deleted or replaced.
func (s *Store) CompactCheckpoints(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	// the box index precedes the checkpoints
	return s.compactRange(ctx, []byte{pfxBoxIndex}, []byte{pfxCheckpoint + 1})
}

// CompactBoxIndex rewrites the box index, dropping the entries that
// IndexBoxes replaced.
func (s *Store) CompactBoxIndex(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	return s.compactRange(ctx, []byte{pfxBoxIndex}, []byte{pfxBoxIndex + 1})
}

// compactChunk is the bytes of sstables, counted in key order across levels,
// after which compactRange starts a new step; tests lower it.
var compactChunk uint64 = 1 << 30

// compactStep compacts one step of compactRange; tests record the steps.
var compactStep = func(ctx context.Context, db *pebble.DB, start, end []byte) error {
	return db.Compact(ctx, start, end, true)
}

// compactRange compacts [start, end) in consecutive steps. A manual
// compaction deletes its input sstables only when it finishes, and Pebble
// compacts all of a level's sstables that overlap the range together: over
// keys written all over the keyspace (accounts are keyed by address) that is
// every sstable of the bottom level, so compacting the range at once needs as
// much free disk as the range takes. A step needs about the sstables of the
// level above the bottom that overlap it, usually one or two, and the bottom
// level beneath them: ~2 GiB on an 8.7 GiB store, more on larger ones, whose
// upper levels have larger sstables.
func (s *Store) compactRange(ctx context.Context, start, end []byte) error {
	levels, err := s.db.SSTables(pebble.WithKeyRangeFilter(start, end))
	if err != nil {
		return err
	}
	var tables []pebble.SSTableInfo
	for _, l := range levels {
		tables = append(tables, l...)
	}
	slices.SortFunc(tables, func(a, b pebble.SSTableInfo) int {
		return bytes.Compare(a.Smallest.UserKey, b.Smallest.UserKey)
	})
	var size uint64
	for _, t := range tables {
		if size >= compactChunk && bytes.Compare(t.Smallest.UserKey, start) > 0 {
			if err = compactStep(ctx, s.db, start, t.Smallest.UserKey); err != nil {
				return err
			}
			start, size = t.Smallest.UserKey, 0
		}
		size += t.Size
	}
	return compactStep(ctx, s.db, start, end)
}

// BoxChanges calls fn, in round order (newest first if desc), for every
// change of a box of app with round in [lo, hi], with the box's name and new
// value (ok false: deleted). A round's boxes come in name order (reversed if
// desc). As with Versions, a write that repeats a box's state is not a change.
// fn owns name and val, and must not call the store (Close would deadlock).
// BoxChanges stops when fn returns false. It fails with ErrNoBoxIndex if it
// needs a round before BoxIndexFrom: if lo is before it, ascending, or once
// it reaches it, descending (fn has then seen the later changes). Reading a
// page costs a seek per index entry in it, however many boxes the app ever
// had.
func (s *Store) BoxChanges(app, lo, hi uint64, desc bool, fn func(rnd uint64, name, val []byte, ok bool) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if lo > hi {
		return nil
	}
	gen := s.boxGen.Load()
	pfx := binary.BigEndian.AppendUint64([]byte{pfxBoxIndex}, app)
	it, err := s.db.NewIter(&pebble.IterOptions{
		LowerBound: binary.BigEndian.AppendUint64(slices.Clip(pfx), lo),
		UpperBound: prefixEnd(binary.BigEndian.AppendUint64(slices.Clip(pfx), hi)),
	})
	if err != nil {
		return err
	}
	defer it.Close()
	// N = 1: the records of the app's boxes, to read each entry's record and
	// the one before it
	var recs *pebble.Iterator
	if s.n == 1 {
		lo := append([]byte{pfxCheckpoint}, BoxKvPrefix(app)...)
		if recs, err = s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)}); err != nil {
			return err
		}
		defer recs.Close()
	}
	if boxChangesOpened != nil {
		boxChangesOpened()
	}
	// only now: a rebuild marks the index incomplete before deleting it, and
	// complete once it has written it
	from := s.boxFrom.Load()
	if gen%2 == 1 || s.boxGen.Load() != gen {
		return boxIndexError(boxIndexNone)
	}
	if lo < from && !desc {
		return boxIndexError(from)
	}
	valid := it.First()
	if desc {
		valid = it.Last()
	}
	for ; valid; valid = step(it, desc) {
		k := it.Key()
		if len(k) < 1+8+8 {
			return fmt.Errorf("timetravel: bad box index key %x", k)
		}
		rnd, name := binary.BigEndian.Uint64(k[9:17]), k[17:]
		if rnd < from {
			return boxIndexError(from)
		}
		obj := boxObject(app, name)
		var val, prev []byte
		var ok, prevOK bool
		if recs != nil {
			var found bool
			if val, ok, prev, prevOK, found, err = fullChange(recs, obj, rnd); err != nil {
				return err
			}
			if !found { // a no-op record that RemoveNoops deleted
				continue
			}
		} else {
			if val, ok, err = s.get(obj, rnd); err != nil {
				return err
			}
			if rnd > 0 {
				if prev, prevOK, err = s.get(obj, rnd-1); err != nil {
					return err
				}
			}
		}
		if !sameState(prev, prevOK, val, ok) && !fn(rnd, bytes.Clone(name), val, ok) {
			return nil
		}
	}
	if err := it.Error(); err != nil {
		return err
	}
	if lo < from { // descending, through the first indexed round
		return boxIndexError(from)
	}
	return nil
}

// boxChangesOpened is called by BoxChanges once its iterators are open; tests
// set it.
var boxChangesOpened func()

func step(it *pebble.Iterator, desc bool) bool {
	if desc {
		return it.Prev()
	}
	return it.Next()
}

// fullChange reads, from an iterator over the checkpoint records of an N = 1
// store, obj's record at rnd, if there is one, and the record before it.
func fullChange(recs *pebble.Iterator, obj []byte, rnd uint64) (val []byte, ok bool, prev []byte, prevOK, found bool, err error) {
	key := verKey(pfxCheckpoint, obj, rnd)
	if !recs.SeekGE(key) || !bytes.Equal(recs.Key(), key) {
		return nil, false, nil, false, false, recs.Error()
	}
	if val, ok, err = applyRecord(nil, false, recs.Value()); err != nil {
		return
	}
	val = bytes.Clone(val) // Prev may reuse the buffer
	if recs.Prev() {
		if k := recs.Key(); len(k) == len(key) && bytes.Equal(k[:len(k)-8], key[:len(key)-8]) {
			if prev, prevOK, err = applyRecord(nil, false, recs.Value()); err != nil {
				return
			}
		}
	}
	return val, ok, prev, prevOK, true, recs.Error()
}

// latest returns the newest record of obj in keyspace pfx with round in [lo, hi].
func (s *Store) latest(pfx byte, obj []byte, lo, hi uint64) ([]byte, bool, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfx, obj, lo), UpperBound: verKey(pfx, obj, hi+1)})
	if err != nil {
		return nil, false, err
	}
	defer it.Close()
	if !it.Last() {
		return nil, false, it.Error()
	}
	return append([]byte(nil), it.Value()...), true, nil
}

func (s *Store) checkpointValue(obj []byte, c uint64) ([]byte, bool, error) {
	rec, found, err := s.latest(pfxCheckpoint, obj, 0, c)
	if err != nil || !found {
		return nil, false, err
	}
	return applyRecord(nil, false, rec)
}

func applyRecord(base []byte, ok bool, rec []byte) ([]byte, bool, error) {
	switch {
	case len(rec) == 0:
	case rec[0] == recFull:
		return rec[1:], true, nil
	case rec[0] == recDeleted:
		return nil, false, nil
	case rec[0] == recPatch && ok:
		v, err := Apply(base, rec[1:])
		return v, err == nil, err
	}
	return nil, false, fmt.Errorf("%w: bad record %q", ErrCorrupt, rec)
}

// Get returns the value of obj at round rnd; ok is false if it did not exist.
func (s *Store) Get(obj []byte, rnd uint64) ([]byte, bool, error) {
	if err := s.acquire(); err != nil {
		return nil, false, err
	}
	defer s.closeMu.RUnlock()
	return s.get(obj, rnd)
}

func (s *Store) get(obj []byte, rnd uint64) (val []byte, ok bool, err error) {
	c := rnd - rnd%s.n
	val, ok, err = s.checkpointValue(obj, c)
	if err != nil || rnd == c {
		return
	}
	if !s.incremental {
		var rec []byte
		var found bool
		rec, found, err = s.latest(pfxPatch, obj, c+1, rnd)
		if err != nil || !found {
			return val, ok, err
		}
		return applyRecord(val, ok, rec)
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfxPatch, obj, c+1), UpperBound: verKey(pfxPatch, obj, rnd+1)})
	if err != nil {
		return nil, false, err
	}
	defer it.Close()
	for it.First(); it.Valid(); it.Next() {
		rec := it.Value()
		if len(rec) > 0 && rec[0] == recFull {
			// a full record's value is the record, which Next may overwrite
			rec = append([]byte(nil), rec...)
		}
		if val, ok, err = applyRecord(val, ok, rec); err != nil {
			return nil, false, err
		}
	}
	return val, ok, it.Error()
}

// Header returns the encoded block header of round rnd.
func (s *Store) Header(rnd uint64) ([]byte, bool, error) {
	v, ok, err := s.headerRecord(rnd)
	if !ok || len(v) == DigestLen {
		return nil, false, err
	}
	return v[DigestLen:], true, nil
}

// Digest returns the ChangesDigest of round rnd.
func (s *Store) Digest(rnd uint64) ([]byte, bool, error) {
	v, ok, err := s.headerRecord(rnd)
	if !ok {
		return nil, false, err
	}
	return v[:DigestLen], true, nil
}

// ExpiredStake returns the expired online stake stored with round rnd: the
// stake of the accounts online at rnd whose keys expire before the vote round
// that has rnd as its balance round.
func (s *Store) ExpiredStake(rnd uint64) (uint64, bool, error) {
	if err := s.acquire(); err != nil {
		return 0, false, err
	}
	defer s.closeMu.RUnlock()
	return s.expiredStakeLocked(rnd)
}

// expiredStakeLocked is ExpiredStake for callers that hold closeMu.
func (s *Store) expiredStakeLocked(rnd uint64) (uint64, bool, error) {
	v, closer, err := s.db.Get(binary.BigEndian.AppendUint64([]byte{pfxExpired}, rnd))
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer closer.Close()
	e, n := binary.Uvarint(v)
	if n <= 0 {
		return 0, false, fmt.Errorf("%w: bad expired stake record for round %d", ErrCorrupt, rnd)
	}
	return e, true, nil
}

func (s *Store) headerRecord(rnd uint64) ([]byte, bool, error) {
	if err := s.acquire(); err != nil {
		return nil, false, err
	}
	defer s.closeMu.RUnlock()
	return s.headerRecordLocked(rnd)
}

// headerRecordLocked is headerRecord for callers that hold closeMu.
func (s *Store) headerRecordLocked(rnd uint64) ([]byte, bool, error) {
	v, closer, err := s.db.Get(binary.BigEndian.AppendUint64([]byte{pfxHeader}, rnd))
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer closer.Close()
	if len(v) < DigestLen {
		return nil, false, fmt.Errorf("%w: short header record for round %d", ErrCorrupt, rnd)
	}
	return append([]byte(nil), v...), true, nil
}

// Scan calls fn, in key order, for every object whose key starts with prefix,
// is greater than after, and exists at round rnd. fn owns val. Scan stops
// when fn returns false.
func (s *Store) Scan(prefix, after []byte, rnd uint64, fn func(obj, val []byte) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	// Objects exist at rnd only if they have a checkpoint record <= c or a
	// patch record in (c, rnd]; walk the distinct objects of both keyspaces.
	var err error
	derr := s.distinct(prefix, after, func(obj []byte) bool {
		var v []byte
		var ok bool
		if v, ok, err = s.get(obj, rnd); err != nil {
			return false
		}
		return !ok || fn(obj, v)
	})
	if err != nil {
		return err
	}
	return derr
}

// Objects calls fn, in key order, for every object whose key starts with
// prefix and that has a record at any round, including objects deleted
// since. It stops when fn returns false.
func (s *Store) Objects(prefix []byte, fn func(obj []byte) bool) error {
	return s.ObjectsAfter(prefix, nil, fn)
}

// ObjectsAfter is Objects for the objects whose key is also greater than
// after, as in Scan.
func (s *Store) ObjectsAfter(prefix, after []byte, fn func(obj []byte) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	return s.distinct(prefix, after, fn)
}

// distinct calls fn for the distinct objects of both versioned keyspaces
// whose key starts with prefix and is greater than after.
func (s *Store) distinct(prefix, after []byte, fn func(obj []byte) bool) error {
	cIt, err := s.objects(pfxCheckpoint, prefix, after)
	if err != nil {
		return err
	}
	defer cIt.close()
	pIt, err := s.objects(pfxPatch, prefix, after)
	if err != nil {
		return err
	}
	defer pIt.close()
	for cIt.obj != nil || pIt.obj != nil {
		var obj []byte
		switch {
		case pIt.obj == nil || (cIt.obj != nil && string(cIt.obj) < string(pIt.obj)):
			obj = cIt.obj
			cIt.next()
		case cIt.obj == nil || string(pIt.obj) < string(cIt.obj):
			obj = pIt.obj
			pIt.next()
		default:
			obj = cIt.obj
			cIt.next()
			pIt.next()
		}
		if !fn(obj) {
			return nil
		}
	}
	if cIt.err != nil {
		return cIt.err
	}
	return pIt.err
}

// Versions calls fn, in round order (newest first if desc), for every
// version of obj with round in [lo, hi]: each round at which its value
// changed, with the new value (ok false: deleted). Records that repeat the
// previous value are not versions: no-op writes, and with N > 1 the
// checkpoint copies of objects that changed earlier in the interval. fn owns
// val. Versions stops when fn returns false.
func (s *Store) Versions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if lo > hi {
		return nil
	}
	if s.n == 1 {
		return s.fullVersions(obj, lo, hi, desc, fn)
	}
	return s.patchedVersions(obj, lo, hi, desc, fn)
}

func sameState(a []byte, aok bool, b []byte, bok bool) bool {
	return aok == bok && (!aok || bytes.Equal(a, b))
}

// fullVersions is Versions for N = 1, where every record is a full value or
// a tombstone in the checkpoint keyspace: one iterator, plus the record
// before the range to compare the first one with.
func (s *Store) fullVersions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool) bool) error {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfxCheckpoint, obj, 0), UpperBound: verKey(pfxCheckpoint, obj, hi+1)})
	if err != nil {
		return err
	}
	defer it.Close()
	record := func() (rnd uint64, v []byte, ok bool, err error) {
		k := it.Key()
		v, ok, err = applyRecord(nil, false, it.Value())
		return binary.BigEndian.Uint64(k[len(k)-8:]), append([]byte(nil), v...), ok, err
	}
	if !desc {
		var prev []byte
		var prevOK bool
		valid := it.SeekLT(verKey(pfxCheckpoint, obj, lo))
		if valid {
			if _, prev, prevOK, err = record(); err != nil {
				return err
			}
			valid = it.Next()
		} else {
			valid = it.SeekGE(verKey(pfxCheckpoint, obj, lo))
		}
		for ; valid; valid = it.Next() {
			rnd, v, ok, err := record()
			if err != nil {
				return err
			}
			if !sameState(prev, prevOK, v, ok) && !fn(rnd, v, ok) {
				return nil
			}
			prev, prevOK = v, ok
		}
		return it.Error()
	}
	// Newest first: a record is a version if it differs from the next older
	// one, which may lie before lo.
	var cand []byte
	var candRnd uint64
	var candOK, have bool
	for valid := it.Last(); ; valid = it.Prev() {
		if !valid {
			if err := it.Error(); err != nil {
				return err
			}
			if have && candOK { // the first record ever is a version unless it is a tombstone
				fn(candRnd, cand, true)
			}
			return nil
		}
		rnd, v, ok, err := record()
		if err != nil {
			return err
		}
		if have && !sameState(v, ok, cand, candOK) && !fn(candRnd, cand, candOK) {
			return nil
		}
		if rnd < lo {
			return nil
		}
		cand, candRnd, candOK, have = v, rnd, ok, true
	}
}

// patchedVersions is Versions for N > 1: the rounds that have a record in
// either keyspace are the candidates, and each one is compared with the
// value just before it.
func (s *Store) patchedVersions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool) bool) error {
	var its [2]*pebble.Iterator
	var valid [2]bool
	for i, pfx := range []byte{pfxCheckpoint, pfxPatch} {
		it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfx, obj, lo), UpperBound: verKey(pfx, obj, hi+1)})
		if err != nil {
			return err
		}
		defer it.Close()
		its[i] = it
		if desc {
			valid[i] = it.Last()
		} else {
			valid[i] = it.First()
		}
	}
	round := func(i int) uint64 {
		k := its[i].Key()
		return binary.BigEndian.Uint64(k[len(k)-8:])
	}
	var prev []byte
	var prevOK bool
	if !desc && lo > 0 {
		var err error
		if prev, prevOK, err = s.get(obj, lo-1); err != nil {
			return err
		}
	}
	// checkpoint and patch rounds never coincide: checkpoints are multiples of N
	for valid[0] || valid[1] {
		i := 0
		if !valid[0] || (valid[1] && (round(1) < round(0)) != desc) {
			i = 1
		}
		rnd := round(i)
		if desc {
			valid[i] = its[i].Prev()
		} else {
			valid[i] = its[i].Next()
		}
		v, ok, err := s.get(obj, rnd)
		if err != nil {
			return err
		}
		if desc {
			prev, prevOK = nil, false
			if rnd > 0 {
				if prev, prevOK, err = s.get(obj, rnd-1); err != nil {
					return err
				}
			}
		}
		if !sameState(prev, prevOK, v, ok) && !fn(rnd, v, ok) {
			return nil
		}
		prev, prevOK = v, ok
	}
	if err := its[0].Error(); err != nil {
		return err
	}
	return its[1].Error()
}

// objIter yields the distinct objects of one versioned keyspace.
type objIter struct {
	it  *pebble.Iterator
	pfx byte
	obj []byte
	err error
}

func (s *Store) objects(pfx byte, prefix, after []byte) (*objIter, error) {
	lo := append([]byte{pfx}, prefix...)
	hi := prefixEnd(lo)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	oi := &objIter{it: it, pfx: pfx}
	if after != nil {
		it.SeekGE(prefixEnd(append([]byte{pfx}, after...)))
	} else {
		it.First()
	}
	oi.load()
	return oi, nil
}

func (oi *objIter) load() {
	if !oi.it.Valid() {
		oi.obj, oi.err = nil, oi.it.Error()
		return
	}
	k := oi.it.Key()
	oi.obj = append([]byte(nil), k[1:len(k)-8]...)
}

func (oi *objIter) next() {
	// skip the remaining versions of the current object
	oi.it.SeekGE(prefixEnd(append([]byte{oi.pfx}, oi.obj...)))
	oi.load()
}

func (oi *objIter) close() { oi.it.Close() }

// prefixEnd returns the smallest key greater than every key starting with p.
func prefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}
