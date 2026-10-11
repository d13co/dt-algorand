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
// N = 1 (every version stored in full, one seek per read, blind writes) is
// the recommended setting: with zstd over large blocks, an object's adjacent
// versions compress as well as patches would, at a fraction of the write
// cost. See README.md for the evaluation on mainnet data.
package timetravel

import (
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
}

// Change is the new value of one object in a round.
type Change struct {
	Key     []byte
	Value   []byte
	Deleted bool
}

// Store is the history database. AddRound must be called by a single writer
// with consecutive rounds; Get/Header/Scan may be called concurrently for any
// round up to Last(), and fail with ErrClosed after Close.
type Store struct {
	db          *pebble.DB
	n           uint64
	incremental bool
	last        atomic.Int64 // -1 when empty

	closeMu sync.RWMutex // held for reading by every operation
	closed  bool
}

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
	s := &Store{db: db, n: o.CheckpointInterval, incremental: o.Incremental}
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
// objects not seen since are treated as absent before it.
func (s *Store) AddRound(rnd uint64, changes []Change, hdr []byte, expired *uint64) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	last, ok := s.Last()
	if ok && rnd <= last {
		return nil
	}
	if ok && rnd != last+1 {
		return fmt.Errorf("timetravel: round %d does not follow stored round %d", rnd, last)
	}
	b := s.db.NewBatch()
	defer b.Close()
	c := rnd - rnd%s.n
	if rnd == c {
		if err := s.checkpoint(b, rnd, changes); err != nil {
			return err
		}
	} else {
		var idx []byte
		for _, ch := range changes {
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
	if err := b.Commit(pebble.NoSync); err != nil {
		return err
	}
	s.last.Store(int64(rnd))
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
		if val, ok, err = applyRecord(val, ok, it.Value()); err != nil {
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
// is greater than after, and exists at round rnd. It stops when fn returns false.
func (s *Store) Scan(prefix, after []byte, rnd uint64, fn func(obj, val []byte) bool) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	// Objects exist at rnd only if they have a checkpoint record <= c or a
	// patch record in (c, rnd]; walk the distinct objects of both keyspaces.
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
		v, ok, err := s.get(obj, rnd)
		if err != nil {
			return err
		}
		if ok && !fn(obj, v) {
			return nil
		}
	}
	if cIt.err != nil {
		return cIt.err
	}
	return pIt.err
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
