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
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"

	"github.com/cockroachdb/pebble/v2"
)

// The commitment is a digest of what a store holds up to a round, the same
// for every store with that content however it was written: synced or
// imported, with or without the no-op records the dedup cache missed,
// before or after RemoveNoops, compacted or not, with more rounds after.
//
// It covers three sections, each in key order:
//   - objects: every version of every object at or before the round, without
//     the records RemoveNoops removes (those repeating the object's previous
//     record, tombstones before its first value)
//   - headers: the header record of every round (the round's ChangesDigest
//     and its encoded header)
//   - expired stake: the record of every round that has one
//
// The box index and meta records are derived or local, and left out; a strict
// commitment (CommitmentOptions.Strict) refuses a store that holds anything
// else that reads could see. Each object (a header or expired stake record
// counts as one) hashes to a digest. A section's digests are cut into chunks
// after each digest whose first two bytes are zero, so that chunks depend on
// the content alone and workers can hash any split of the key space; the
// section digest hashes its chunks' digests.
const commitmentDomain = "dtalgorand-ttsnap-commitment/1"

// Section tags of the commitment.
const (
	commitObjects = 'O'
	commitHeaders = 'H'
	commitExpired = 'E'
)

// commitBuffer bounds the digests a worker holds while it waits for a chunk
// boundary (a chunk has 65,536 on average). Past it, the worker hands its
// digests to the merger one batch at a time, which waits for the ranges
// before: content made to avoid boundaries costs parallelism, not memory.
var commitBuffer = 1 << 20

// commitHandBatch is the size of the batches a worker hands over once past
// commitBuffer.
const commitHandBatch = 4096

// commitBoundary reports whether a chunk ends after the object with digest d.
var commitBoundary = func(d *[32]byte) bool { return d[0] == 0 && d[1] == 0 }

// Commitment is the digest of a store's content up to Round, and what it
// counted.
type Commitment struct {
	Digest   [32]byte
	First    uint64 // the store's first round
	Round    uint64
	Objects  uint64 // objects with a version at or before Round
	Versions uint64 // their versions
	Headers  uint64
	Expired  uint64 // expired stake records
}

// CommitmentOptions configure Store.Commitment.
type CommitmentOptions struct {
	// Workers is the goroutines that read the store; 0: GOMAXPROCS.
	Workers int
	// Progress, if not nil, is called from time to time with the key and
	// value bytes read so far, possibly from several goroutines at once.
	Progress func(bytes uint64)
	// Strict refuses a store that holds anything reads could see besides
	// the content the commitment covers, so that the store answers like any
	// other with that commitment once its box index is rebuilt and the
	// records NonCanonical names are deleted: records after the round, keys
	// outside the store's keyspaces, which no build of this store writes, and
	// unknown meta records.
	// For a store from another node, which the commitment is checked against.
	Strict bool
	// NonCanonical, if not nil, is called with the key of every object
	// record at or before the round that the commitment leaves out (those
	// RemoveNoops removes), possibly from several goroutines at once.
	NonCanonical func(key []byte)
}

// Commitment computes the commitment of the store's content up to round upTo
// (see commitmentDomain). It reads a snapshot of the store, so rounds may be
// added meanwhile. Close cancels it.
func (s *Store) Commitment(ctx context.Context, upTo uint64, o CommitmentOptions) (Commitment, error) {
	return s.commitment(ctx, upTo, o, s.keySplits)
}

// splitFunc returns keys of the store in (lo, hi), in any order, at which a
// section's key range is split into about n parts for the workers.
type splitFunc func(snap *pebble.Snapshot, lo, hi []byte, n int) ([][]byte, error)

// metaKeys are the meta records a store may hold.
var metaKeys = [][]byte{keyMeta, keyBoxIndexMeta, keyDivergedMeta, keyInstalledMeta, keyInstalledThroughMeta}

func (s *Store) commitment(ctx context.Context, upTo uint64, o CommitmentOptions, splits splitFunc) (Commitment, error) {
	if err := s.acquire(); err != nil {
		return Commitment{}, err
	}
	defer s.closeMu.RUnlock()
	last, ok := s.Last()
	if !ok {
		return Commitment{}, errors.New("timetravel: the store is empty")
	}
	if upTo > last {
		return Commitment{}, fmt.Errorf("timetravel: the store's last round is %d, before %d", last, upTo)
	}
	if o.Workers <= 0 {
		o.Workers = runtime.GOMAXPROCS(0)
	}
	// Close cancels a commitment rather than wait minutes for it
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		select {
		case <-s.closing:
			cancel()
		case <-ctx.Done():
		}
	}()
	snap := s.db.NewSnapshot()
	defer snap.Close()
	if o.Strict {
		if err := strictLayout(snap, upTo); err != nil {
			return Commitment{}, err
		}
	}
	c := Commitment{Round: upTo}
	var err error
	if c.First, err = firstHeader(snap); err != nil {
		return Commitment{}, err
	}
	var read atomic.Uint64
	cs := &commitScan{ctx: ctx, snap: snap, upTo: upTo, o: o, splits: splits, read: &read}

	end := binary.BigEndian.AppendUint64(nil, upTo+1)
	h := sha256.New()
	h.Write([]byte(commitmentDomain))
	h.Write(binary.BigEndian.AppendUint64(nil, c.First))
	h.Write(binary.BigEndian.AppendUint64(nil, upTo))
	for _, sec := range []struct {
		tag    byte
		pfx    byte
		single bool
		count  *uint64
	}{
		{commitObjects, pfxRecord, false, &c.Objects},
		{commitHeaders, pfxHeader, true, &c.Headers},
		{commitExpired, pfxExpired, true, &c.Expired},
	} {
		lo, hi := []byte{sec.pfx}, []byte{sec.pfx + 1}
		if sec.single && upTo != ^uint64(0) {
			hi = append([]byte{sec.pfx}, end...)
		}
		d, objects, versions, err := cs.section(lo, hi, sec.single)
		if err != nil {
			return Commitment{}, err
		}
		*sec.count = objects
		if !sec.single {
			c.Versions = versions
		}
		h.Write([]byte{sec.tag})
		h.Write(binary.BigEndian.AppendUint64(nil, objects))
		h.Write(binary.BigEndian.AppendUint64(nil, versions))
		h.Write(d[:])
	}
	h.Sum(c.Digest[:0])
	if o.Progress != nil {
		o.Progress(read.Load())
	}
	return c, nil
}

// strictLayout checks that the store holds no keys besides the object
// records, the header and expired stake records up to round upTo, the box
// index and the known meta records.
func strictLayout(snap *pebble.Snapshot, upTo uint64) error {
	after := func(pfx byte) []byte {
		if upTo == ^uint64(0) {
			return []byte{pfx + 1}
		}
		return binary.BigEndian.AppendUint64([]byte{pfx}, upTo+1)
	}
	// the store's keyspaces in key order: E, H, b, c, m
	for _, r := range [][2][]byte{
		{nil, {pfxExpired}},
		{after(pfxExpired), {pfxHeader}},
		{after(pfxHeader), {pfxBoxIndex}},
		{{pfxRecord + 1}, {'m'}},
		{{'m' + 1}, nil},
	} {
		it, err := snap.NewIter(&pebble.IterOptions{LowerBound: r[0], UpperBound: r[1]})
		if err != nil {
			return err
		}
		found := it.First()
		var k []byte
		if found {
			k = slices.Clone(it.Key())
		}
		if err := it.Close(); err != nil {
			return err
		}
		if found {
			return fmt.Errorf("%w: the store holds key %x, which is not of its history up to round %d", ErrCorrupt, k, upTo)
		}
	}
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: []byte{'m'}, UpperBound: []byte{'m' + 1}})
	if err != nil {
		return err
	}
	defer it.Close()
	for valid := it.First(); valid; valid = it.Next() {
		if !slices.ContainsFunc(metaKeys, func(m []byte) bool { return bytes.Equal(m, it.Key()) }) {
			return fmt.Errorf("%w: the store holds an unknown meta record %x", ErrCorrupt, it.Key())
		}
	}
	return it.Error()
}

func firstHeader(snap *pebble.Snapshot) (uint64, error) {
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: []byte{pfxHeader}, UpperBound: []byte{pfxHeader + 1}})
	if err != nil {
		return 0, err
	}
	defer it.Close()
	if !it.First() {
		if err := it.Error(); err != nil {
			return 0, err
		}
		return 0, errors.New("timetravel: the store has no header")
	}
	if len(it.Key()) != 1+8 {
		return 0, fmt.Errorf("%w: header key %x", ErrCorrupt, it.Key())
	}
	return binary.BigEndian.Uint64(it.Key()[1:]), nil
}

// keySplits returns up to n-1 keys of the store in (lo, hi) that split it
// into parts of similar size: it halves the largest part (by the engine's
// estimate of its size on disk) at the midpoint of its first and last keys,
// until there are n parts or none is worth splitting.
func (s *Store) keySplits(snap *pebble.Snapshot, lo, hi []byte, n int) ([][]byte, error) {
	type part struct {
		lo, first, last []byte
		size            uint64
		whole           bool // can't be split
	}
	it, err := snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return nil, err
	}
	defer it.Close()
	// probe returns the part of [lo, hi) from its first key, nil if empty
	probe := func(lo, hi []byte) (*part, error) {
		if !it.SeekGE(lo) || bytes.Compare(it.Key(), hi) >= 0 {
			return nil, it.Error()
		}
		p := &part{first: slices.Clone(it.Key())}
		p.lo = p.first
		if !it.SeekLT(hi) {
			return nil, it.Error()
		}
		p.last = slices.Clone(it.Key())
		var serr error
		p.size, serr = s.db.EstimateDiskUsage(p.first, p.last)
		return p, serr
	}
	whole, err := probe(lo, hi)
	if whole == nil || err != nil {
		return nil, err
	}
	parts := []*part{whole}
	for len(parts) < n {
		i := -1
		for j, p := range parts {
			if !p.whole && (i < 0 || p.size > parts[i].size) {
				i = j
			}
		}
		if i < 0 || parts[i].size < minSplitBytes {
			break
		}
		p := parts[i]
		left, right := (*part)(nil), (*part)(nil)
		if m := midKey(p.first, p.last); m != nil {
			if left, err = probe(p.lo, m); err != nil {
				return nil, err
			}
			end := hi
			if i+1 < len(parts) {
				end = parts[i+1].lo
			}
			if right, err = probe(m, end); err != nil {
				return nil, err
			}
		}
		if left == nil || right == nil {
			p.whole = true
			continue
		}
		parts = slices.Replace(parts, i, i+1, left, right)
	}
	out := make([][]byte, 0, len(parts)-1)
	for _, p := range parts[1:] {
		out = append(out, p.lo)
	}
	return out, nil
}

// minSplitBytes is the size below which keySplits leaves a part whole.
var minSplitBytes uint64 = 4 << 20

// midKey returns a key between a and b (a < b), a < m <= b, by interpolating
// the 8 bytes after their common prefix; nil if they are too close.
func midKey(a, b []byte) []byte {
	c := 0
	for c < len(a) && c < len(b) && a[c] == b[c] {
		c++
	}
	word := func(k []byte) uint64 {
		var w [8]byte
		if c < len(k) {
			copy(w[:], k[c:])
		}
		return binary.BigEndian.Uint64(w[:])
	}
	x, y := word(a), word(b)
	if y <= x || y-x < 2 {
		return nil
	}
	return binary.BigEndian.AppendUint64(slices.Clone(a[:c]), x+(y-x)/2)
}

type commitScan struct {
	ctx    context.Context
	snap   *pebble.Snapshot
	upTo   uint64
	o      CommitmentOptions
	splits splitFunc
	read   *atomic.Uint64
}

// A worker hands the merger, in order, what it made of its range: digests
// that belong to the chunk in progress, chunk ends, and whole chunks that
// started and ended in the range, then the range's first and last objects.
type commitEvent struct {
	digests [][32]byte // of the chunk in progress
	end     bool       // the chunk in progress ends after digests
	chunk   *[32]byte  // a whole chunk's digest
	done    bool       // the range is done; first and last are its objects
	first   []byte
	last    []byte
	objects uint64
	vers    uint64
	err     error
}

// section returns the digest of the section in [lo, hi), and the objects and
// versions it holds. single: each key is an object of one version, its round
// the key's last 8 bytes, and no record is left out.
func (cs *commitScan) section(lo, hi []byte, single bool) ([32]byte, uint64, uint64, error) {
	var zero [32]byte
	keys, err := cs.splits(cs.snap, lo, hi, 16*cs.o.Workers)
	if err != nil {
		return zero, 0, 0, err
	}
	bounds := rangeBounds(lo, hi, keys, single)

	n := len(bounds) - 1
	events := make([]chan commitEvent, n)
	for i := range events {
		events[i] = make(chan commitEvent, 4)
	}
	ctx, cancel := context.WithCancel(cs.ctx)
	var wg sync.WaitGroup
	// the workers read the snapshot, which the caller closes
	defer func() {
		cancel()
		wg.Wait()
	}()
	next := make(chan int, n)
	for i := range n {
		next <- i
	}
	close(next)
	for range min(cs.o.Workers, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range next {
				if !cs.scanRange(ctx, bounds[i], bounds[i+1], single, events[i]) {
					return
				}
			}
		}()
	}

	sh := sha256.New() // the section
	ch := sha256.New() // the chunk in progress
	var objects, versions uint64
	var prevLast []byte
	pending := false // the chunk in progress has digests
	for i := range n {
		for {
			var e commitEvent
			select {
			case e = <-events[i]:
			case <-ctx.Done():
				return zero, 0, 0, ctx.Err()
			}
			if e.err != nil {
				return zero, 0, 0, e.err
			}
			for j := range e.digests {
				ch.Write(e.digests[j][:])
				pending = true
			}
			if e.end {
				var d [32]byte
				sh.Write(ch.Sum(d[:0]))
				ch.Reset()
				pending = false
			}
			if e.chunk != nil {
				sh.Write(e.chunk[:])
			}
			if !e.done {
				continue
			}
			if e.first != nil {
				if prevLast != nil && !objectsInOrder(prevLast, e.first) {
					return zero, 0, 0, fmt.Errorf("%w: object key %x follows %x", ErrCorrupt, e.first, prevLast)
				}
				prevLast = e.last
			}
			objects += e.objects
			versions += e.vers
			break
		}
	}
	// the last chunk ends with the section
	var d [32]byte
	if pending {
		sh.Write(ch.Sum(d[:0]))
	}
	sh.Sum(d[:0])
	return d, objects, versions, nil
}

// rangeBounds returns the bounds of the ranges that split keys cut [lo, hi)
// into, lo first and hi last. Unless single (each key an object), a range
// holds whole objects.
func rangeBounds(lo, hi []byte, keys [][]byte, single bool) [][]byte {
	bounds := [][]byte{lo}
	for _, k := range keys {
		if bytes.Compare(k, lo) <= 0 || bytes.Compare(k, hi) >= 0 {
			continue
		}
		if !single {
			// split before the object, so that a range holds whole objects:
			// the keys of objects before it sort before it, since object
			// keys never prefix one another (the scans refuse a store where
			// one does)
			if len(k) < 1+8+1 {
				continue
			}
			k = k[:len(k)-8]
			if bytes.Compare(k, lo) <= 0 {
				continue
			}
		}
		bounds = append(bounds, k)
	}
	slices.SortFunc(bounds[1:], bytes.Compare)
	bounds = slices.CompactFunc(bounds, bytes.Equal)
	return append(bounds, hi)
}

// objectsInOrder reports whether object key b may follow a in a store: after
// it, and not starting with it, since the versions of objects whose keys
// prefix one another would interleave.
func objectsInOrder(a, b []byte) bool {
	return bytes.Compare(a, b) < 0 && !bytes.HasPrefix(b, a)
}

// scanRange hashes the objects in [lo, hi) into events, closing nothing; it
// returns false if it stopped on an error, which it sent.
func (cs *commitScan) scanRange(ctx context.Context, lo, hi []byte, single bool, out chan<- commitEvent) bool {
	send := func(e commitEvent) bool {
		select {
		case out <- e:
			return e.err == nil
		case <-ctx.Done():
			return false
		}
	}
	fail := func(err error) bool { return send(commitEvent{err: err}) && false }
	it, err := cs.snap.NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
	if err != nil {
		return fail(err)
	}
	defer it.Close()
	var (
		cur      [][32]byte // digests since the last chunk end, or the range's start
		bounded  bool       // a chunk ended in the range
		handed   bool       // cur's chunk was handed over in part
		first    []byte     // the range's first object
		obj      []byte     // the current object
		inObj    bool
		prev     []byte // its latest kept record
		kept     bool   // it has a kept version
		h        = sha256.New()
		buf      []byte
		scanned  uint64
		read     uint64
		objects  uint64
		versions uint64
	)
	// handOver passes cur to the merger as part of the chunk in progress
	handOver := func(end bool) bool {
		e := commitEvent{digests: cur, end: end}
		cur = nil
		return send(e)
	}
	finish := func() bool {
		if !kept {
			return true
		}
		var d [32]byte
		h.Sum(d[:0])
		cur = append(cur, d)
		objects++
		switch {
		case commitBoundary(&d):
			// a chunk that started in the range is whole; the first one
			// continues one the merger has in progress
			if bounded && !handed {
				c := chunkDigest(cur)
				cur = nil
				if !send(commitEvent{chunk: &c}) {
					return false
				}
			} else if !handOver(true) {
				return false
			}
			bounded, handed = true, false
		case len(cur) >= commitBuffer || (handed && len(cur) >= commitHandBatch):
			handed = true
			return handOver(false)
		}
		return true
	}
	start := func(o []byte) bool {
		if cs.o.Strict && !single && !ValidObjectKey(o) {
			return fail(fmt.Errorf("%w: record key of %x, which is not an object key", ErrCorrupt, o))
		}
		if inObj && !objectsInOrder(obj, o) {
			return fail(fmt.Errorf("%w: object key %x follows %x", ErrCorrupt, o, obj))
		}
		if first == nil {
			first = slices.Clone(o)
		}
		obj, inObj = append(obj[:0], o...), true
		prev = append(prev[:0], recDeleted) // absent before its first record
		kept = false
		h.Reset()
		buf = binary.AppendUvarint(buf[:0], uint64(len(o)))
		buf = append(buf, o...)
		h.Write(buf)
		return true
	}
	flushRead := func() {
		if read > 0 {
			total := cs.read.Add(read)
			read = 0
			if cs.o.Progress != nil {
				cs.o.Progress(total)
			}
		}
	}
	for valid := it.First(); valid; valid = it.Next() {
		if scanned%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return false
			}
			if read >= 64<<20 {
				flushRead()
			}
		}
		scanned++
		k, v := it.Key(), it.Value()
		read += uint64(len(k) + len(v))
		if len(k) < 1+8 || (single && len(k) != 1+8) {
			return fail(fmt.Errorf("%w: key %x", ErrCorrupt, k))
		}
		rnd := binary.BigEndian.Uint64(k[len(k)-8:])
		if single {
			if !finish() || !start(k[1:]) {
				return false
			}
		} else if o := k[1 : len(k)-8]; !inObj || !bytes.Equal(o, obj) {
			if !finish() || !start(o) {
				return false
			}
		}
		if rnd > cs.upTo {
			if cs.o.Strict {
				return fail(fmt.Errorf("%w: record %x is after round %d", ErrCorrupt, k, cs.upTo))
			}
			continue
		}
		if !single && bytes.Equal(v, prev) {
			if cs.o.NonCanonical != nil {
				cs.o.NonCanonical(slices.Clone(k))
			}
			continue
		}
		prev = append(prev[:0], v...)
		kept = true
		versions++
		buf = binary.BigEndian.AppendUint64(buf[:0], rnd)
		buf = binary.AppendUvarint(buf, uint64(len(v)))
		h.Write(buf)
		h.Write(v)
	}
	if err := it.Error(); err != nil {
		return fail(err)
	}
	if !finish() {
		return false
	}
	flushRead()
	var last []byte
	if inObj {
		last = slices.Clone(obj)
	}
	return send(commitEvent{digests: cur, done: true, first: first, last: last, objects: objects, vers: versions})
}

func chunkDigest(ds [][32]byte) [32]byte {
	h := sha256.New()
	for i := range ds {
		h.Write(ds[i][:])
	}
	var d [32]byte
	h.Sum(d[:0])
	return d
}

// keyInstalledMeta marks a store installed from another node's snapshot
// (ttsnap fetch), with the name of the snapshot: its history is the
// producer's, which the node's own statements must not vouch for.
var keyInstalledMeta = []byte("mi")

// keyInstalledThroughMeta holds the last round of the installed snapshot
// (BE uint64), lowered to b by a truncation after b. The ledger tells the
// installed rounds it replays from the rounds the node wrote itself by it.
var keyInstalledThroughMeta = []byte("mx")

// SetInstalled marks the store as installed from the snapshot named name,
// whose last round is through.
func (s *Store) SetInstalled(name string, through uint64) error {
	if err := s.acquire(); err != nil {
		return err
	}
	defer s.closeMu.RUnlock()
	if s.readOnly {
		return ErrReadOnly
	}
	batch := s.db.NewBatch()
	defer batch.Close()
	if err := batch.Set(keyInstalledMeta, []byte(name), nil); err != nil {
		return err
	}
	if err := batch.Set(keyInstalledThroughMeta, binary.BigEndian.AppendUint64(nil, through), nil); err != nil {
		return err
	}
	return batch.Commit(pebble.Sync)
}

// InstalledThrough returns the last round of the store's history that came
// from an installed snapshot; ok is false if none did (or the store was
// installed by a build that did not record it).
func (s *Store) InstalledThrough() (through uint64, ok bool, err error) {
	if err = s.acquire(); err != nil {
		return 0, false, err
	}
	defer s.closeMu.RUnlock()
	return s.installedThrough()
}

func (s *Store) installedThrough() (uint64, bool, error) {
	v, closer, err := s.db.Get(keyInstalledThroughMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	defer closer.Close()
	if len(v) != 8 {
		return 0, false, fmt.Errorf("%w: the installed round record is %d bytes", ErrCorrupt, len(v))
	}
	return binary.BigEndian.Uint64(v), true, nil
}

// Installed returns the name of the snapshot the store was installed from;
// ok is false for a store the node built.
func (s *Store) Installed() (name string, ok bool, err error) {
	if err = s.acquire(); err != nil {
		return "", false, err
	}
	defer s.closeMu.RUnlock()
	v, closer, err := s.db.Get(keyInstalledMeta)
	if errors.Is(err, pebble.ErrNotFound) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	defer closer.Close()
	return string(v), true, nil
}
