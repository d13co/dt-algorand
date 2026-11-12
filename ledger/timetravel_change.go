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

// Change lookups: the round at which an object, a state key, or a box under
// a name prefix first changed in a range of rounds, searched in either
// direction. POST /v2/query programs run them with the time-travel opcodes
// tt_*_change (data/transactions/logic/timetravel.go). Non-standard fork
// feature.

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// ChangeStatus says what a change lookup found.
//
//msgp:ignore ChangeStatus
type ChangeStatus uint8

const (
	// ChangeNone means nothing changed in the range.
	ChangeNone ChangeStatus = iota
	// ChangeCreated means absent before the round, present after it.
	ChangeCreated
	// ChangeUpdated means present before and after the round, with another value.
	ChangeUpdated
	// ChangeDeleted means present before the round, absent after it.
	ChangeDeleted
	// ChangeMore means the scan ran out of its byte budget first. Change.Round is
	// the round to resume from, as the range's start.
	ChangeMore
)

// Change is what a change lookup found.
type Change struct {
	Status ChangeStatus
	// Round is the round of the change: its block changed the value. With
	// ChangeMore it is where to resume, and with ChangeNone 0.
	Round basics.Round
	// KeyValue is the state key's new value (StateKeyChange), nil if it was
	// deleted or nothing was found.
	KeyValue *basics.TealValue
	// Name is the name of the box that changed (BoxPrefixChange).
	Name []byte
	// Bytes is what the scan read (StateKeyChange and BoxPrefixChange), as
	// their maxBytes counts it.
	Bytes uint64
	// Versions is the number of versions the lookup examined: of the object,
	// of the state the key is in, or of the boxes under the prefix (a box
	// prefix search newest first reads the change's round twice). Stored
	// repeats and the box index entries of other boxes are not versions.
	Versions uint64
}

// ErrNoHistory is the error of a change lookup on a node that keeps no
// history (config EnableTimeTravel off).
var ErrNoHistory = errors.New("this node keeps no history (EnableTimeTravel is off)")

// changeRange turns a lookup's from and to into a scan: from <= to searches
// [from, to] oldest first, from > to searches [to, from] newest first.
func changeRange(from, to basics.Round) (lo, hi uint64, desc bool) {
	if from <= to {
		return uint64(from), uint64(to), false
	}
	return uint64(to), uint64(from), true
}

// changeView returns the history view that reaches the rounds of a lookup
// between from and to, counting into e (nil: not counted) as one history read.
func (l *Ledger) changeView(from, to basics.Round, e *ledgercore.Effort) (*histView, error) {
	if l.history == nil {
		return nil, ErrNoHistory
	}
	h := l.historyNow(max(from, to), e)
	if h == nil {
		return nil, errHistoryUnavailable
	}
	// A store installed from a snapshot holds nothing before its first
	// round, where a lookup would find no change.
	first, ok, err := h.store.First()
	if err != nil {
		return nil, err
	}
	if ok && uint64(min(from, to)) < first {
		return nil, fmt.Errorf("the history starts at round %d", first)
	}
	if e != nil {
		e.History()
	}
	return h, nil
}

// changeStatus is the status of a change from (prevOK) to (ok).
func changeStatus(ok, prevOK bool) ChangeStatus {
	switch {
	case !ok:
		return ChangeDeleted
	case !prevOK:
		return ChangeCreated
	default:
		return ChangeUpdated
	}
}

// ObjectChange finds the first change of the history object obj (a key of
// timetravel/objects.go) from round from toward round to, both inclusive:
// the oldest in [from, to] if from <= to, else the newest in [to, from]. As
// for versions, a stored write that repeats the value is not a change. Both
// rounds must be in the history: the first round of a store that does not
// start at genesis reports the objects it holds as created.
//
// Its cost is one iterator, a seek, and the records it steps through: the
// change's, the one before it, and any stored repeats between them, which
// ttimport -dedup removes.
func (l *Ledger) ObjectChange(obj []byte, from, to basics.Round, e *ledgercore.Effort) (Change, error) {
	h, err := l.changeView(from, to, e)
	if err != nil {
		return Change{}, err
	}
	return histObjectChange(h, obj, from, to)
}

func histObjectChange(h *histView, obj []byte, from, to basics.Round) (Change, error) {
	lo, hi, desc := changeRange(from, to)
	var c Change
	var versions uint64
	err := h.VersionsPrev(obj, lo, hi, desc, func(rnd uint64, _ []byte, ok bool, _ []byte, prevOK bool) bool {
		versions++
		c = Change{Status: changeStatus(ok, prevOK), Round: basics.Round(rnd)}
		return false
	})
	if err != nil {
		return Change{}, err
	}
	c.Versions = versions
	return c, nil
}

// StateKeyChange finds the first change of key, a global (obj an app's
// params) or local (obj a local state) state key, from round from toward
// round to, as ObjectChange does, and its new value. The state has no per-key
// index, so it scans the versions of the whole state; it stops once it has
// read maxBytes of records (0: no limit), with ChangeMore and the round to
// resume from, which need not be a version's and can differ between nodes.
// The deletion of the whole state deletes the key.
func (l *Ledger) StateKeyChange(obj, key []byte, from, to basics.Round, maxBytes uint64, e *ledgercore.Effort) (Change, error) {
	if len(obj) == 0 || (obj[0] != timetravel.KindAppParams && obj[0] != timetravel.KindLocal) {
		return Change{}, errors.New("a state key applies to app params or a local state")
	}
	h, err := l.changeView(from, to, e)
	if err != nil {
		return Change{}, err
	}
	return histStateKeyChange(h, obj, key, from, to, maxBytes)
}

func histStateKeyChange(h *histView, obj, key []byte, from, to basics.Round, maxBytes uint64) (Change, error) {
	lo, hi, desc := changeRange(from, to)
	var c Change
	var dec keyDecoder
	var derr error
	var versions uint64
	end, err := h.VersionsScan(obj, lo, hi, desc, maxBytes, func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
		versions++
		var cur, before *basics.TealValue
		if cur, derr = dec.value(obj, val, ok, key); derr != nil {
			return false
		}
		if before, derr = dec.value(obj, prev, prevOK, key); derr != nil {
			return false
		}
		dec.keep(val, ok, cur, prev, prevOK, before)
		if sameTealValue(before, cur) {
			return true
		}
		c = Change{Status: changeStatus(cur != nil, before != nil), Round: basics.Round(rnd), KeyValue: cur}
		return false
	})
	if err == nil {
		err = derr
	}
	if err != nil {
		return Change{}, err
	}
	if c.Status == ChangeNone && end.More {
		c = Change{Status: ChangeMore, Round: basics.Round(end.Next)}
	}
	c.Bytes, c.Versions = end.Bytes, versions
	return c, nil
}

// BoxPrefixChange finds the first change of a box of app whose name starts
// with prefix from round from toward round to, as ObjectChange does, and the
// box's name: of the boxes that changed in that round, the first by name, in
// either direction, so that a search resumed at a round (from == to) gives
// the same answer. It scans the box index, which must reach the rounds it
// reads (ErrNoBoxIndex): ascending, from the range's low round; descending,
// down to the change. It stops once it has read maxBytes (0: no limit) before
// a round, with ChangeMore and the round to resume from, as for
// StateKeyChange.
func (l *Ledger) BoxPrefixChange(app basics.AppIndex, prefix []byte, from, to basics.Round, maxBytes uint64, e *ledgercore.Effort) (Change, error) {
	h, err := l.changeView(from, to, e)
	if err != nil {
		return Change{}, err
	}
	return histBoxPrefixChange(h, app, prefix, from, to, maxBytes)
}

func histBoxPrefixChange(h *histView, app basics.AppIndex, prefix []byte, from, to basics.Round, maxBytes uint64) (Change, error) {
	lo, hi, desc := changeRange(from, to)
	var c Change
	var hit, exists bool
	var versions uint64
	first := func(rnd uint64, name, _ []byte, ok bool) bool {
		versions++
		c.Round, c.Name, hit, exists = basics.Round(rnd), name, true, ok
		return false
	}
	o := timetravel.BoxChangesOpts{Prefix: prefix, MaxBytes: maxBytes}
	end, err := h.BoxChanges(uint64(app), lo, hi, desc, o, first)
	if err != nil {
		return Change{}, err
	}
	if hit && desc {
		// Descending, a round's names come last first: read the round again,
		// ascending, for its first. Reading on instead would scan to the next
		// change below it, however far.
		o.MaxBytes = 0
		again, againErr := h.BoxChanges(uint64(app), uint64(c.Round), uint64(c.Round), false, o, first)
		if againErr != nil {
			return Change{}, againErr
		}
		end.Bytes += again.Bytes
	}
	if hit {
		// BoxChanges passes no state before the change, and fn must not
		// read the store, so it is read now.
		var prevOK bool
		if exists && c.Round > 0 {
			if _, prevOK, err = h.Get(timetravel.BoxKey(uint64(app), c.Name), uint64(c.Round)-1); err != nil {
				return Change{}, err
			}
		}
		c.Status = changeStatus(exists, prevOK)
	} else if end.More {
		c = Change{Status: ChangeMore, Round: basics.Round(end.Next)}
	}
	c.Bytes, c.Versions = end.Bytes, versions
	return c, nil
}

// VersionsScan is timetravel.Store.VersionsScan with the in-memory rounds
// merged in. Each in-memory version counts its object key, round and value
// toward maxBytes, as does the value before the first. It stops only where
// resuming makes progress: after lo ascending, before hi descending, so it
// can read past maxBytes by a version. fn must not modify val or prev.
func (v *histView) VersionsScan(obj []byte, lo, hi uint64, desc bool, maxBytes uint64, fn func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool) (timetravel.VersionsEnd, error) {
	var end timetravel.VersionsEnd
	if lo > hi {
		return end, nil
	}
	if v.effort != nil {
		var scanned uint64
		defer func() { v.effort.AddScanned(scanned) }()
		inner := fn
		fn = func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
			scanned++
			return inner(rnd, val, ok, prev, prevOK)
		}
	}
	spent := func() bool { return maxBytes > 0 && end.Bytes >= maxBytes }
	// passMem passes an in-memory version to fn; false: stop, at the budget
	// (More) or as fn says
	passMem := func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
		if spent() && ((!desc && rnd > lo) || (desc && rnd < hi)) {
			end.More, end.Next = true, rnd
			return false
		}
		end.Bytes += uint64(len(obj) + 8 + len(val))
		return fn(rnd, val, ok, prev, prevOK)
	}
	// inMem is what pending round i wrote to obj, if it is in the range and
	// the store doesn't hold it yet
	inMem := func(i int) (val []byte, ok, found bool) {
		p := &v.pending[i]
		if p.rnd <= v.last || uint64(p.rnd) < lo || uint64(p.rnd) > hi {
			return nil, false, false
		}
		j, found := p.idx[string(obj)]
		if !found {
			return nil, false, false
		}
		return p.changes[j].Value, !p.changes[j].Deleted, true
	}
	// before is obj's value before round rnd, counted as read
	before := func(rnd uint64) ([]byte, bool, error) {
		prev, prevOK, err := v.Get(obj, rnd-1)
		if err == nil {
			end.Bytes += uint64(len(obj) + 8 + len(prev))
		}
		return prev, prevOK, err
	}
	// The in-memory versions are newer than the store's: newest first they
	// come before it. A write is a version if it differs from the one before
	// it, so each is passed once the next older write is known, and the
	// rounds are read only until fn stops.
	if desc {
		var cand []byte
		var candRnd uint64
		var candOK, have bool
		for i := len(v.pending) - 1; i >= 0 && uint64(v.pending[i].rnd) >= lo; i-- {
			val, ok, found := inMem(i)
			if !found {
				continue
			}
			if have && !sameValue(val, ok, cand, candOK) && !passMem(candRnd, cand, candOK, val, ok) {
				return end, nil
			}
			cand, candRnd, candOK, have = val, uint64(v.pending[i].rnd), ok, true
		}
		if have {
			prev, prevOK, err := before(candRnd)
			if err != nil {
				return end, err
			}
			if !sameValue(prev, prevOK, cand, candOK) && !passMem(candRnd, cand, candOK, prev, prevOK) {
				return end, nil
			}
		}
	}
	if storeHi := min(hi, uint64(v.last)); lo <= storeHi {
		budget := maxBytes
		if maxBytes > 0 {
			// descending, after the in-memory rounds (ascending, the store
			// is read first)
			if desc && spent() && storeHi < hi {
				end.More, end.Next = true, storeHi
				return end, nil
			}
			// the store always makes progress, so a budget of 1 reads what
			// it must
			budget = max(1, maxBytes-min(maxBytes, end.Bytes))
		}
		stopped := false
		se, err := v.store.VersionsScan(obj, lo, storeHi, desc, budget, func(rnd uint64, val []byte, ok bool, prev []byte, prevOK bool) bool {
			stopped = !fn(rnd, val, ok, prev, prevOK)
			return !stopped
		})
		end.Bytes += se.Bytes
		if err != nil || stopped {
			return end, err
		}
		if se.More {
			end.More, end.Next = true, se.Next
			return end, nil
		}
	}
	if !desc {
		var prev []byte
		var prevOK, have bool
		for i := 0; i < len(v.pending) && uint64(v.pending[i].rnd) <= hi; i++ {
			val, ok, found := inMem(i)
			if !found {
				continue
			}
			rnd := uint64(v.pending[i].rnd)
			if !have {
				var err error
				if prev, prevOK, err = before(rnd); err != nil {
					return end, err
				}
				have = true
			}
			if !sameValue(prev, prevOK, val, ok) && !passMem(rnd, val, ok, prev, prevOK) {
				return end, nil
			}
			prev, prevOK = val, ok
		}
	}
	return end, nil
}

// sameValue reports whether two values of an object, each with whether it
// exists, are the same: a write of the same value is no version.
func sameValue(a []byte, aok bool, b []byte, bok bool) bool {
	return aok == bok && (!aok || bytes.Equal(a, b))
}
