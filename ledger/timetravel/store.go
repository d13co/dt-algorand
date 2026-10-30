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
// totals) is addressed by an object key. Its history lives in an
// object-major keyspace, ordered by (object, round):
//
//	'c' obj round -> full value or tombstone
//
// Each round writes the new value of every object that changed in it; objects
// that did not change are not repeated. Reading obj@R is one seek: the latest
// record at or before R. Writes are blind appends. With zstd over large
// blocks, an object's adjacent versions compress as well as patches against
// earlier versions would, at a fraction of the write cost (see README.md for
// the evaluation on mainnet data).
//
// 'H' round -> encoded block header.
// 'E' round -> the round's expired online stake (uvarint), see ExpiredStake.
// 'b' app round name -> 'F' or 'D', the box index: an entry per box record
// AddRound writes, round-major per app, so that BoxChanges reads a range of
// an app's rounds without visiting every box the app ever had (see
// BoxIndexFrom).
// 'a' asset addr -> the rounds at which the holding was created and deleted,
// the asset holders index: an entry per holding ever, asset-major, so that
// AssetHolders lists an asset's holders without reading every holding (see
// IndexHolders). Unlike the others it exists only once built.
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
	pfxRecord   = 'c'
	pfxHeader   = 'H'
	pfxExpired  = 'E'
	pfxBoxIndex = 'b'
	pfxHolders  = 'a'
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
)

// MaxOpenFiles is how many files a store keeps open at most. algod counts
// them when it raises its file descriptor limit.
const MaxOpenFiles = 4096

// Options configure how a store is opened.
type Options struct {
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
	// Infof, if not nil, receives the messages an operator should see:
	// those of a truncation that Open finishes, which takes minutes on a
	// large store.
	Infof func(format string, args ...any)
	// ReadOnly opens an existing store without writing to it: no new store,
	// no compactions, and AddRound, RemoveNoops, CompactRecords and Sync
	// fail with ErrReadOnly. The store is still locked, so another process
	// cannot open it meanwhile.
	ReadOnly bool
	// HoldersIndex makes a new store index asset holders from its first
	// round: AddRound writes that round and the index in one batch, and every
	// later round keeps the index up to date. An existing store
	// ignores it: build its index with IndexHolders.
	HoldersIndex bool
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
	db       *pebble.DB
	readOnly bool
	last     atomic.Int64 // -1 when empty

	writeMu deadlock.Mutex // serializes AddRound, RemoveNoops, IndexBoxes and the holders index's writers
	dedup   *lastWritten   // nil: store every change; guarded by writeMu

	boxFrom atomic.Uint64 // see BoxIndexFrom
	boxGen  atomic.Uint64 // odd while boxFrom changes; see setBoxFrom

	holders     atomic.Uint32 // HoldersState; changes under writeMu
	skipHolders bool          // tests: AddRound leaves the holders index alone, as a build without it does
	newHolders  bool          // Options.HoldersIndex: the first round of an empty store starts the holders index

	truncating atomic.Bool // a TruncateAfter is unfinished; AddRound fails
	// truncatedAfter is 1 + the round the last TruncateAfter of this
	// process truncated after, until AddRound adds a round; 0: none
	truncatedAfter atomic.Uint64

	infof func(format string, args ...any) // Options.Infof, never nil

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
		MaxOpenFiles:               MaxOpenFiles,
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
	s := &Store{db: db, readOnly: o.ReadOnly, newHolders: o.HoldersIndex, closing: make(chan struct{}), infof: o.Infof}
	if s.infof == nil {
		s.infof = func(string, ...any) {}
	}
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
	if err := s.finishTruncate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.loadHolders(); err != nil {
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
	// the checkpoint interval N of the patch layouts, which builds before
	// their removal could create; with N = 1 they wrote full records only,
	// whatever the incremental flag
	if n := binary.BigEndian.Uint64(v[8:]); n != 1 {
		return fmt.Errorf("timetravel: the store was created with TimeTravelCheckpointInterval %d, which is no longer supported: rebuild it", n)
	}
	s.last.Store(int64(binary.BigEndian.Uint64(v)))
	return nil
}

// metaRecord is the meta record of a store whose last round is last. It
// keeps the layout of the builds that had a checkpoint interval: N = 1, not
// incremental.
func metaRecord(last uint64) []byte {
	meta := binary.BigEndian.AppendUint64(nil, last)
	meta = binary.BigEndian.AppendUint64(meta, 1)
	return append(meta, 0)
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
	lo := append([]byte{pfxRecord}, kvBoxPrefix...)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
	if err != nil {
		return false, err
	}
	found := it.First()
	if err := it.Close(); err != nil {
		return false, err
	}
	return found, nil
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
	if s.truncating.Load() {
		return errTruncating
	}
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
	// a complete holders index is kept up to date, in the round's batch, and
	// a new store with Options.HoldersIndex starts one with its first round:
	// an empty store has no holding records, so it has no entries to clear
	startHolders := !ok && s.newHolders
	if (startHolders || s.HoldersIndexState() == HoldersComplete) && !s.skipHolders {
		if err := s.indexHolders(b, write, rnd); err != nil {
			return err
		}
	}
	for _, ch := range write {
		if err := b.Set(verKey(pfxRecord, ch.Key, rnd), fullRecord(ch), nil); err != nil {
			return err
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
	if err := b.Set(keyMeta, metaRecord(rnd), nil); err != nil {
		return err
	}
	if err := commitBatch(b); err != nil {
		return err
	}
	s.last.Store(int64(rnd))
	s.truncatedAfter.Store(0)
	if startHolders && !s.skipHolders {
		s.holders.Store(uint32(HoldersComplete))
	}
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

func fullRecord(ch Change) []byte {
	if ch.Deleted {
		return []byte{recDeleted}
	}
	return append([]byte{recFull}, ch.Value...)
}

// removeNoopsBatchOps is the number of writes RemoveNoops and IndexBoxes
// commit at once.
var removeNoopsBatchOps = uint32(1 << 16)

// RemoveNoops deletes the records of the store that change nothing:
// those that repeat the object's previous record (the no-op writes of stores
// written before AddRound skipped them, and the ones its cache missed), and
// tombstones before an object's first value (objects created and deleted in
// one round). Every read answers as before, except that Objects no longer
// lists objects that never had a value. In the same pass it rebuilds the box
// index, as IndexBoxes does. The asset holders index stays as it is: no
// record it removes creates or deletes a holding. CompactRecords reclaims
// the space. progress,
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
	removed, _, err := s.sweep(ctx, []byte{pfxRecord}, []byte{pfxRecord + 1}, true, func(scanned, removed, _ uint64) {
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
// entries it replaced. progress, if not nil, is called with the records
// scanned and the entries written after each batch of writes and at the end.
// Reads may run concurrently, but BoxChanges fails until it is done; AddRound
// waits.
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
	lo := append([]byte{pfxRecord}, kvBoxPrefix...)
	_, indexed, err := s.sweep(ctx, lo, prefixEnd(lo), false, report)
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

// sweep walks the records of the store with keys in [lower,
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

// CompactRecords rewrites the records keyspace and the box index,
// dropping the records that RemoveNoops deleted or replaced.
func (s *Store) CompactRecords(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	// the box index precedes the records
	return s.compactRange(ctx, []byte{pfxBoxIndex}, []byte{pfxRecord + 1})
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
	// the records of the app's boxes, to read each entry's record and the one
	// before it
	rlo := append([]byte{pfxRecord}, BoxKvPrefix(app)...)
	recs, err := s.db.NewIter(&pebble.IterOptions{LowerBound: rlo, UpperBound: prefixEnd(rlo)})
	if err != nil {
		return err
	}
	defer recs.Close()
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
		val, ok, prev, prevOK, found, err := fullChange(recs, boxObject(app, name), rnd)
		if err != nil {
			return err
		}
		if !found { // a no-op record that RemoveNoops deleted
			continue
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

// fullChange reads, from an iterator over the records of the store, obj's
// record at rnd, if there is one, and the record before it.
func fullChange(recs *pebble.Iterator, obj []byte, rnd uint64) (val []byte, ok bool, prev []byte, prevOK, found bool, err error) {
	key := verKey(pfxRecord, obj, rnd)
	if !recs.SeekGE(key) || !bytes.Equal(recs.Key(), key) {
		return nil, false, nil, false, false, recs.Error()
	}
	if val, ok, err = decodeRecord(recs.Value()); err != nil {
		return
	}
	val = bytes.Clone(val) // Prev may reuse the buffer
	if recs.Prev() {
		if k := recs.Key(); len(k) == len(key) && bytes.Equal(k[:len(k)-8], key[:len(key)-8]) {
			if prev, prevOK, err = decodeRecord(recs.Value()); err != nil {
				return
			}
		}
	}
	return val, ok, prev, prevOK, true, recs.Error()
}

// decodeRecord decodes a record: the object's value, or ok false for a
// tombstone.
func decodeRecord(rec []byte) ([]byte, bool, error) {
	switch {
	case len(rec) == 0:
	case rec[0] == recFull:
		return rec[1:], true, nil
	case rec[0] == recDeleted:
		return nil, false, nil
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

func (s *Store) get(obj []byte, rnd uint64) ([]byte, bool, error) {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfxRecord, obj, 0), UpperBound: verKey(pfxRecord, obj, rnd+1)})
	if err != nil {
		return nil, false, err
	}
	defer it.Close()
	if !it.Last() {
		return nil, false, it.Error()
	}
	return decodeRecord(bytes.Clone(it.Value()))
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

// distinct calls fn for the distinct objects whose key starts with prefix
// and is greater than after.
func (s *Store) distinct(prefix, after []byte, fn func(obj []byte) bool) error {
	it, err := s.objects(prefix, after)
	if err != nil {
		return err
	}
	defer it.close()
	for it.obj != nil {
		obj := it.obj
		it.next()
		if !fn(obj) {
			return nil
		}
	}
	return it.err
}

// Versions calls fn, in round order (newest first if desc), for every
// version of obj with round in [lo, hi]: each round at which its value
// changed, with the new value (ok false: deleted). Records that repeat the
// previous value, no-op writes, are not versions. fn may keep val but must not
// modify it: Versions compares it with the next record. Versions stops when fn
// returns false.
func (s *Store) Versions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool) bool) error {
	return s.VersionsPrev(obj, lo, hi, desc, func(rnd uint64, val []byte, ok bool, _ []byte, _ bool) bool {
		return fn(rnd, val, ok)
	})
}

// VersionsPrev is Versions that also passes each version's previous state,
// the value of obj at rnd-1 (prevOK false: it did not exist), which Versions
// reads anyway to tell versions from repeats. fn may keep val and prev but
// must not modify them: they may be shared with adjacent calls.
func (s *Store) VersionsPrev(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if lo > hi {
		return nil
	}
	return s.fullVersions(obj, lo, hi, desc, fn)
}

func sameState(a []byte, aok bool, b []byte, bok bool) bool {
	return aok == bok && (!aok || bytes.Equal(a, b))
}

// fullVersions is Versions past its checks. Every record is a full value or
// a tombstone: one iterator, plus the record before the range to compare the
// first one with.
func (s *Store) fullVersions(obj []byte, lo, hi uint64, desc bool, fn func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool) error {
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: verKey(pfxRecord, obj, 0), UpperBound: verKey(pfxRecord, obj, hi+1)})
	if err != nil {
		return err
	}
	defer it.Close()
	record := func() (rnd uint64, v []byte, ok bool, err error) {
		k := it.Key()
		v, ok, err = decodeRecord(it.Value())
		// Clone keeps an empty value non-nil, as Get returns it
		return binary.BigEndian.Uint64(k[len(k)-8:]), bytes.Clone(v), ok, err
	}
	if !desc {
		var prev []byte
		var prevOK bool
		valid := it.SeekLT(verKey(pfxRecord, obj, lo))
		if valid {
			if _, prev, prevOK, err = record(); err != nil {
				return err
			}
			valid = it.Next()
		} else {
			valid = it.SeekGE(verKey(pfxRecord, obj, lo))
		}
		for ; valid; valid = it.Next() {
			rnd, v, ok, err := record()
			if err != nil {
				return err
			}
			if !sameState(prev, prevOK, v, ok) && !fn(rnd, v, ok, prev, prevOK) {
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
				fn(candRnd, cand, true, nil, false)
			}
			return nil
		}
		rnd, v, ok, err := record()
		if err != nil {
			return err
		}
		if have && !sameState(v, ok, cand, candOK) && !fn(candRnd, cand, candOK, v, ok) {
			return nil
		}
		if rnd < lo {
			return nil
		}
		cand, candRnd, candOK, have = v, rnd, ok, true
	}
}

// objIter yields the distinct objects of the versioned keyspace.
type objIter struct {
	it  *pebble.Iterator
	obj []byte
	err error
}

func (s *Store) objects(prefix, after []byte) (*objIter, error) {
	lo := append([]byte{pfxRecord}, prefix...)
	hi := prefixEnd(lo)
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	oi := &objIter{it: it}
	if after != nil {
		it.SeekGE(prefixEnd(append([]byte{pfxRecord}, after...)))
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
	oi.it.SeekGE(prefixEnd(append([]byte{pfxRecord}, oi.obj...)))
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
