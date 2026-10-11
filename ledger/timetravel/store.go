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
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"
)

const (
	pfxCheckpoint = 'c'
	pfxPatch      = 'p'
	pfxRoundIndex = 'r'
	pfxHeader     = 'H'
	pfxExpired    = 'E'
)

var keyMeta = []byte("m")

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
	last        atomic.Int64 // -1 when empty

	writeMu sync.Mutex   // serializes AddRound and RemoveNoops
	dedup   *lastWritten // nil: store every change; guarded by writeMu

	closeMu sync.RWMutex // held for reading by every operation
	closed  bool
}

// commitBatch commits a round's batch; tests make it fail.
var commitBatch = func(b *pebble.Batch) error { return b.Commit(pebble.NoSync) }

// ErrClosed is returned by operations on a closed store.
var ErrClosed = errors.New("timetravel: store closed")

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
	s := &Store{db: db, n: o.CheckpointInterval, incremental: o.Incremental, dedup: newLastWritten(o.DedupEntries)}
	s.last.Store(-1)
	v, closer, err := db.Get(keyMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return s, nil
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	defer closer.Close()
	if len(v) != 17 {
		db.Close()
		return nil, fmt.Errorf("timetravel: bad meta record")
	}
	s.last.Store(int64(binary.BigEndian.Uint64(v)))
	s.n = binary.BigEndian.Uint64(v[8:])
	s.incremental = v[16] == 1
	return s, nil
}

type quietLogger struct{}

func (quietLogger) Infof(string, ...interface{})         {}
func (quietLogger) Errorf(f string, args ...interface{}) { pebble.DefaultLogger.Errorf(f, args...) }
func (quietLogger) Fatalf(f string, args ...interface{}) { pebble.DefaultLogger.Fatalf(f, args...) }

// Close closes the store, waiting for in-flight operations.
func (s *Store) Close() error {
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

// CheckpointInterval returns N.
func (s *Store) CheckpointInterval() uint64 { return s.n }

// Sync makes all added rounds durable.
func (s *Store) Sync() error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
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

// removeNoopsBatchOps is the number of deletions RemoveNoops commits at once.
var removeNoopsBatchOps = uint32(1 << 16)

// RemoveNoops deletes the records of an N = 1 store that change nothing:
// those that repeat the object's previous record (the no-op writes of stores
// written before AddRound skipped them, and the ones its cache missed), and
// tombstones before an object's first value (objects created and deleted in
// one round). Every read answers as before, except that Objects no longer
// lists objects that never had a value. CompactCheckpoints reclaims the
// space. progress, if not nil, is called with the records scanned and removed
// after each batch of deletions and at the end. Reads may run concurrently;
// AddRound waits.
func (s *Store) RemoveNoops(ctx context.Context, progress func(scanned, removed uint64)) (uint64, error) {
	if err := s.acquire(); err != nil {
		return 0, err
	}
	defer s.closeMu.RUnlock()
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.n != 1 {
		return 0, fmt.Errorf("timetravel: RemoveNoops needs a store with N = 1, not %d", s.n)
	}
	it, err := s.db.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxCheckpoint}, UpperBound: []byte{pfxCheckpoint + 1}})
	if err != nil {
		return 0, err
	}
	defer it.Close()
	b := s.db.NewBatch()
	defer func() { b.Close() }()
	var scanned, removed uint64
	// the current object, its latest kept record, and the run of records
	// [first, last] that repeat it
	var obj, prev, first, last []byte
	deleteRun := func() error {
		if first == nil {
			return nil
		}
		var err error
		if bytes.Equal(first, last) {
			err = b.Delete(first, nil)
		} else {
			// the keys in between are the object's: object keys never prefix one another
			err = b.DeleteRange(first, append(last, 0), nil)
		}
		first = nil
		if err != nil || b.Count() < removeNoopsBatchOps {
			return err
		}
		if err := b.Commit(pebble.NoSync); err != nil {
			return err
		}
		b.Close()
		b = s.db.NewBatch()
		if progress != nil {
			progress(scanned, removed)
		}
		return nil
	}
	for valid := it.First(); valid; valid = it.Next() {
		if scanned%4096 == 0 && ctx.Err() != nil {
			return removed, ctx.Err()
		}
		scanned++
		k, v := it.Key(), it.Value()
		if o := k[1 : len(k)-8]; !bytes.Equal(o, obj) {
			if err := deleteRun(); err != nil {
				return removed, err
			}
			obj = append(obj[:0], o...)
			prev = append(prev[:0], recDeleted) // absent before its first record
		}
		if bytes.Equal(v, prev) {
			if first == nil {
				first = append([]byte(nil), k...)
			}
			last = append(last[:0], k...)
			removed++
			continue
		}
		if err := deleteRun(); err != nil {
			return removed, err
		}
		prev = append(prev[:0], v...)
	}
	if err := it.Error(); err != nil {
		return removed, err
	}
	if err := deleteRun(); err != nil {
		return removed, err
	}
	if err := b.Commit(pebble.Sync); err != nil {
		return removed, err
	}
	if progress != nil {
		progress(scanned, removed)
	}
	return removed, nil
}

// CompactCheckpoints rewrites the checkpoint keyspace, dropping the records
// that RemoveNoops deleted.
func (s *Store) CompactCheckpoints(ctx context.Context) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	return s.db.Compact(ctx, []byte{pfxCheckpoint}, []byte{pfxCheckpoint + 1}, true)
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
	return nil, false, fmt.Errorf("timetravel: bad record %q", rec)
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
		rec, found, err := s.latest(pfxPatch, obj, c+1, rnd)
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
		return 0, false, fmt.Errorf("timetravel: bad expired stake record for round %d", rnd)
	}
	return e, true, nil
}

func (s *Store) headerRecord(rnd uint64) ([]byte, bool, error) {
	if err := s.acquire(); err != nil {
		return nil, false, err
	}
	defer s.closeMu.RUnlock()
	v, closer, err := s.db.Get(binary.BigEndian.AppendUint64([]byte{pfxHeader}, rnd))
	if errors.Is(err, pebble.ErrNotFound) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer closer.Close()
	if len(v) < DigestLen {
		return nil, false, fmt.Errorf("timetravel: short header record for round %d", rnd)
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
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	return s.distinct(prefix, nil, fn)
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
