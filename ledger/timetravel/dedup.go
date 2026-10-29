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

import "crypto/sha256"

// defaultDedupEntries bounds the objects lastWritten remembers, ~30 MB.
const defaultDedupEntries = 1 << 18

// recSum identifies an object's state: deleted, or a value by its SHA-256.
// The hash is cryptographic because box and app values are chosen by users,
// and a collision would drop a real change.
type recSum struct {
	sum     [sha256.Size]byte
	deleted bool
}

func sumOf(ch Change) recSum {
	if ch.Deleted {
		return recSum{deleted: true}
	}
	return recSum{sum: sha256.Sum256(ch.Value)}
}

// lastWritten remembers the state that the latest stored record of recently
// written objects holds, so that AddRound can skip changes that repeat it
// without reading the store: deltas rewrite unchanged objects, the totals and
// the rewards pool in every round. Objects it has forgotten (or never saw,
// before a restart) are written as before. It keeps two generations: a round's
// objects go to cur, which becomes old once it holds half entries.
type lastWritten struct {
	cur, old map[string]recSum
	half     int
}

// newLastWritten returns a cache of about entries objects, nil (no dedup) if
// entries is negative.
func newLastWritten(entries int) *lastWritten {
	if entries < 0 {
		return nil
	}
	if entries == 0 {
		entries = defaultDedupEntries
	}
	half := max(entries/2, 1)
	return &lastWritten{cur: make(map[string]recSum, half), half: half}
}

func (lw *lastWritten) get(key []byte) (recSum, bool) {
	if s, ok := lw.cur[string(key)]; ok {
		return s, true
	}
	s, ok := lw.old[string(key)]
	return s, ok
}

// roundState is an object's state after a round: its last change in the
// round, at index at.
type roundState struct {
	sum recSum
	at  int
}

// filter returns, in a new slice, the changes that leave their object in a
// different state than its stored one, and every object's state after the
// round, for update once the round is stored. Of a key changed more than once
// in the round, only the last change is kept, as the batch would leave it.
func (lw *lastWritten) filter(changes []Change) ([]Change, map[string]roundState) {
	if lw == nil {
		return changes, nil
	}
	round := make(map[string]roundState, len(changes))
	for i, ch := range changes {
		round[string(ch.Key)] = roundState{at: i}
	}
	write := make([]Change, 0, len(changes))
	for i, ch := range changes {
		st := round[string(ch.Key)]
		if st.at != i {
			continue
		}
		st.sum = sumOf(ch)
		round[string(ch.Key)] = st
		if prev, ok := lw.get(ch.Key); !ok || prev != st.sum {
			write = append(write, ch)
		}
	}
	return write, round
}

// update records the state of the objects of a stored round. Generations
// turn over between rounds only, so the cache, and what AddRound writes,
// depend only on the rounds added since the store was opened.
func (lw *lastWritten) update(round map[string]roundState) {
	if lw == nil {
		return
	}
	for k, st := range round {
		lw.cur[k] = st.sum
	}
	switch {
	case len(lw.cur) > 2*lw.half: // one round larger than the cache
		lw.cur, lw.old = make(map[string]recSum, lw.half), nil
	case len(lw.cur) >= lw.half:
		lw.cur, lw.old = make(map[string]recSum, lw.half), lw.cur
	}
}

// reset forgets every object, for a store whose latest records changed other
// than through AddRound.
func (lw *lastWritten) reset() {
	if lw == nil {
		return
	}
	lw.cur, lw.old = make(map[string]recSum, lw.half), nil
}
