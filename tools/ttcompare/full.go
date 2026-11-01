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
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/algorand/go-algorand/ledger/timetravel"
)

// The store's keyspaces (see the timetravel package documentation).
const (
	pfxRecord  = 'c' // 'c' obj round -> 'F' value, or 'D'
	pfxHeader  = 'H' // 'H' round -> digest, header
	pfxExpired = 'E' // 'E' round -> uvarint
)

// runFull compares everything the two stores hold for the rounds both have:
// every round's header, change digest and expired stake, and every object's
// state at the first common round and each of its versions after it. It reads
// each store once, in key order, so it takes as long as reading the larger
// store. Records that repeat their object's previous state are not versions
// and are skipped on both sides.
func (c *comparer) runFull(pathA, pathB string) error {
	closeStores, err := c.open(pathA, pathB)
	if err != nil {
		return err
	}
	defer closeStores()

	start := time.Now()
	if err = c.fullRounds(); err != nil {
		return err
	}
	fmt.Printf("rounds compared in %v: %d differences\n", time.Since(start).Round(time.Millisecond), c.diffs)
	if err = c.fullRecords(); err != nil {
		return err
	}
	fmt.Printf("compared %d rounds and %d objects (%d states and versions agree) in %v: %d differences\n",
		c.roundsChecked, c.objsChecked, c.versionsChecked, time.Since(start).Round(time.Second), c.diffs)
	return nil
}

// roundRange iterates the per-round keyspace pfx of s over [c.lo, c.hi].
func (c *comparer) roundRange(s *timetravel.Store, pfx byte) (*pebble.Iterator, error) {
	it, err := s.DB().NewIter(&pebble.IterOptions{
		LowerBound: binary.BigEndian.AppendUint64([]byte{pfx}, c.lo),
		UpperBound: binary.BigEndian.AppendUint64([]byte{pfx}, c.hi+1),
	})
	if err != nil {
		return nil, err
	}
	it.First()
	return it, nil
}

// fullRounds compares the header records and expired stake of every common
// round.
func (c *comparer) fullRounds() error {
	type check struct {
		pfx  byte
		name string
		cmp  func(r uint64, a, b []byte)
	}
	checks := []check{
		{pfxHeader, "header record", func(r uint64, a, b []byte) {
			if len(a) < timetravel.DigestLen || len(b) < timetravel.DigestLen {
				c.diff("round %d: short header record (A %d bytes, B %d bytes)", r, len(a), len(b))
				return
			}
			if !bytes.Equal(a[:timetravel.DigestLen], b[:timetravel.DigestLen]) {
				c.diff("round %d: change digests differ (A: %x, B: %x)", r, a[:timetravel.DigestLen], b[:timetravel.DigestLen])
			}
			if !bytes.Equal(a[timetravel.DigestLen:], b[timetravel.DigestLen:]) {
				c.diff("round %d: headers differ (A: %d bytes, B: %d bytes)", r, len(a)-timetravel.DigestLen, len(b)-timetravel.DigestLen)
			}
		}},
		{pfxExpired, "expired stake", func(r uint64, a, b []byte) {
			if !bytes.Equal(a, b) {
				ea, _ := binary.Uvarint(a)
				eb, _ := binary.Uvarint(b)
				c.diff("round %d: expired stake differs (A: %d, B: %d)", r, ea, eb)
			}
		}},
	}
	for _, ck := range checks {
		if err := c.compareRoundKeyspace(ck.pfx, ck.name, ck.cmp); err != nil {
			return err
		}
	}
	return nil
}

// compareRoundKeyspace merges the records of the per-round keyspace pfx of
// both stores over [c.lo, c.hi], and compares those of the same round with cmp.
func (c *comparer) compareRoundKeyspace(pfx byte, name string, cmp func(r uint64, a, b []byte)) error {
	ia, err := c.roundRange(c.a, pfx)
	if err != nil {
		return err
	}
	defer ia.Close()
	ib, err := c.roundRange(c.b, pfx)
	if err != nil {
		return err
	}
	defer ib.Close()
	// valid is it.Valid(), failing on an iterator error or a malformed key
	valid := func(it *pebble.Iterator) (bool, error) {
		if !it.Valid() {
			return false, it.Error()
		}
		if len(it.Key()) != 1+8 {
			return false, fmt.Errorf("%w: bad %s key %x", timetravel.ErrCorrupt, name, it.Key())
		}
		return true, nil
	}
	round := func(it *pebble.Iterator) uint64 { return binary.BigEndian.Uint64(it.Key()[1:]) }
	var missingA, missingB uint64
	for {
		okA, err := valid(ia)
		if err != nil {
			return fmt.Errorf("store A: %w", err)
		}
		okB, err := valid(ib)
		if err != nil {
			return fmt.Errorf("store B: %w", err)
		}
		switch {
		case !okA && !okB:
			if missingA+missingB > 0 {
				fmt.Printf("%s: %d rounds missing in A, %d missing in B\n", name, missingA, missingB)
			}
			return nil
		case !okB || (okA && round(ia) < round(ib)):
			missingB++
			c.diff("round %d: %s only in A", round(ia), name)
			ia.Next()
		case !okA || round(ib) < round(ia):
			missingA++
			c.diff("round %d: %s only in B", round(ib), name)
			ib.Next()
		default:
			if pfx == pfxHeader {
				c.roundsChecked++
			}
			cmp(round(ia), ia.Value(), ib.Value())
			ia.Next()
			ib.Next()
		}
	}
}

// event is an object's state at a round: at c.lo, the state it starts the
// common range in (only if it exists), and after it, each version.
type event struct {
	obj []byte
	rnd uint64
	rec []byte // the raw record: 'F' value, or 'D'
}

func (e *event) value() ([]byte, bool) {
	if e.rec[0] == 'D' {
		return nil, false
	}
	return e.rec[1:], true
}

// eventStream turns a store's records, in key order, into events. Equal raw
// records are equal states, so it tells versions from repeats by comparing
// each record with the object's previous one, as RemoveNoops does.
type eventStream struct {
	it     *pebble.Iterator
	lo, hi uint64

	obj      []byte // the current object
	prev     []byte // its latest record so far ('D' before its first)
	baseDue  bool   // its state at lo is not emitted yet
	ev       event
	finished bool
	bad      error // a record that is not one

	records, repeats uint64
}

func newEventStream(s *timetravel.Store, lo, hi uint64) (*eventStream, error) {
	it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{pfxRecord}, UpperBound: []byte{pfxRecord + 1}})
	if err != nil {
		return nil, err
	}
	it.First()
	return &eventStream{it: it, lo: lo, hi: hi}, nil
}

// emitBase sets ev to the current object's state at lo, if it exists then.
func (es *eventStream) emitBase() bool {
	es.baseDue = false
	if es.prev[0] == 'D' {
		return false
	}
	es.ev = event{obj: es.obj, rnd: es.lo, rec: es.prev}
	return true
}

// next advances to the next event; false at the end or on error (see err).
// The event's buffers stay valid until the following call.
func (es *eventStream) next() bool {
	for !es.finished {
		if !es.it.Valid() {
			es.finished = true
			return es.baseDue && es.emitBase()
		}
		k, v := es.it.Key(), es.it.Value()
		if len(k) < 1+1+8 || len(v) == 0 || (v[0] != 'F' && !(v[0] == 'D' && len(v) == 1)) {
			es.finished = true
			es.bad = fmt.Errorf("%w: bad record %x at key %x", timetravel.ErrCorrupt, v, k)
			return false
		}
		obj, rnd := k[1:len(k)-8], binary.BigEndian.Uint64(k[len(k)-8:])
		if !bytes.Equal(obj, es.obj) {
			if es.baseDue && es.emitBase() {
				return true // this record is read again by the next call
			}
			if !timetravel.ValidObjectKey(obj) {
				es.finished = true
				es.bad = fmt.Errorf("%w: bad object key in record key %x", timetravel.ErrCorrupt, k)
				return false
			}
			es.obj = append(es.obj[:0], obj...)
			es.prev = append(es.prev[:0], 'D')
			es.baseDue = true
		}
		if rnd <= es.lo {
			es.count(v)
			es.prev = append(es.prev[:0], v...)
			es.it.Next()
			continue
		}
		if es.baseDue && es.emitBase() {
			return true
		}
		if rnd > es.hi { // a round only this store has
			es.it.Next()
			continue
		}
		if es.count(v) {
			es.it.Next()
			continue
		}
		es.prev = append(es.prev[:0], v...)
		es.ev = event{obj: es.obj, rnd: rnd, rec: es.prev}
		es.it.Next()
		return true
	}
	return false
}

// count counts record v of the current object, and returns whether it repeats
// the previous one.
func (es *eventStream) count(v []byte) bool {
	es.records++
	if bytes.Equal(v, es.prev) {
		es.repeats++
		return true
	}
	return false
}

func (es *eventStream) err() error {
	if es.bad != nil {
		return es.bad
	}
	return es.it.Error()
}

func compareEvents(a, b *event) int {
	if c := bytes.Compare(a.obj, b.obj); c != 0 {
		return c
	}
	switch {
	case a.rnd < b.rnd:
		return -1
	case a.rnd > b.rnd:
		return 1
	}
	return 0
}

// fullRecords merges the event streams of both stores and reports the events
// that differ: the first of each object in full, and every one in the counts.
func (c *comparer) fullRecords() error {
	sa, err := newEventStream(c.a, c.lo, c.hi)
	if err != nil {
		return err
	}
	defer sa.it.Close()
	sb, err := newEventStream(c.b, c.lo, c.hi)
	if err != nil {
		return err
	}
	defer sb.it.Close()

	type kindStats struct{ objs, diffObjs, diffEvents uint64 }
	stats := map[byte]*kindStats{}
	kind := func(obj []byte) *kindStats {
		ks := stats[obj[0]]
		if ks == nil {
			ks = &kindStats{}
			stats[obj[0]] = ks
		}
		return ks
	}
	var lastObj, lastDiffObj []byte // the last object seen and the last that differed
	var events uint64
	seen := func(obj []byte) {
		if !bytes.Equal(obj, lastObj) {
			lastObj = append(lastObj[:0], obj...)
			c.objsChecked++
			kind(obj).objs++
		}
	}
	report := func(obj []byte, format string, args ...any) {
		ks := kind(obj)
		ks.diffEvents++
		if bytes.Equal(obj, lastDiffObj) {
			return // one printed difference per object: the rest follow from it
		}
		lastDiffObj = append(lastDiffObj[:0], obj...)
		ks.diffObjs++
		c.diff(format, args...)
	}
	at := func(e *event) string {
		if e.rnd == c.lo {
			return fmt.Sprintf("at round %d", e.rnd)
		}
		return fmt.Sprintf("version at round %d", e.rnd)
	}

	// advance moves es to its next event, failing at once on an error: a
	// stream that stopped early would make every later event of the other
	// look like a difference
	advance := func(es *eventStream, name string) (bool, error) {
		if es.next() {
			return true, nil
		}
		if serr := es.err(); serr != nil {
			return false, fmt.Errorf("store %s: %w", name, serr)
		}
		return false, nil
	}
	okA, err := advance(sa, "A")
	if err != nil {
		return err
	}
	okB, err := advance(sb, "B")
	if err != nil {
		return err
	}
	lastReport := time.Now()
	for okA || okB {
		events++
		if events%(1<<20) == 0 && time.Since(lastReport) > 30*time.Second {
			lastReport = time.Now()
			fmt.Fprintf(os.Stderr, "... %d objects, %d+%d records, %d differences; at %s\n",
				c.objsChecked, sa.records, sb.records, c.diffs, describe(lastObj))
		}
		cmp := 0
		switch {
		case !okB:
			cmp = -1
		case !okA:
			cmp = 1
		default:
			cmp = compareEvents(&sa.ev, &sb.ev)
		}
		switch cmp {
		case -1:
			seen(sa.ev.obj)
			va, oka := sa.ev.value()
			report(sa.ev.obj, "%s: only A has a %s (%s); B has none", describe(sa.ev.obj), at(&sa.ev), showValue(va, oka))
			okA, err = advance(sa, "A")
		case 1:
			seen(sb.ev.obj)
			vb, okb := sb.ev.value()
			report(sb.ev.obj, "%s: only B has a %s (%s); A has none", describe(sb.ev.obj), at(&sb.ev), showValue(vb, okb))
			okB, err = advance(sb, "B")
		default:
			seen(sa.ev.obj)
			if bytes.Equal(sa.ev.rec, sb.ev.rec) {
				c.versionsChecked++
			} else {
				va, oka := sa.ev.value()
				vb, okb := sb.ev.value()
				report(sa.ev.obj, "%s %s: A %s, B %s", describe(sa.ev.obj), at(&sa.ev), showValue(va, oka), showValue(vb, okb))
			}
			if okA, err = advance(sa, "A"); err == nil {
				okB, err = advance(sb, "B")
			}
		}
		if err != nil {
			return err
		}
	}

	fmt.Printf("records: A %d (%d repeats), B %d (%d repeats)\n", sa.records, sa.repeats, sb.records, sb.repeats)
	for k := 0; k < 256; k++ {
		if ks := stats[byte(k)]; ks != nil {
			fmt.Printf("  %-14s %12d objects, %d differ (%d differing versions)\n", kindName(byte(k)), ks.objs, ks.diffObjs, ks.diffEvents)
		}
	}
	return nil
}

func kindName(k byte) string {
	switch k {
	case timetravel.KindTotals:
		return "totals"
	case timetravel.KindAccount:
		return "accounts"
	case timetravel.KindHolding:
		return "holdings"
	case timetravel.KindLocal:
		return "local states"
	case timetravel.KindAssetParams:
		return "asset params"
	case timetravel.KindAppParams:
		return "app params"
	case timetravel.KindCreated:
		return "creatables"
	case timetravel.KindKv:
		return "kv/boxes"
	}
	return fmt.Sprintf("kind %#x", k)
}
