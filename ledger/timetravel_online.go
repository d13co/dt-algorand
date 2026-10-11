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

import (
	"container/heap"

	"github.com/algorand/go-algorand/data/basics"
)

// onlineSet holds the accounts online as of the last round applied, and the
// stake of those whose keys expire before a vote round, for the history's
// expired stake (onlineAccounts.expiredOnlineCirculation). An account's stake
// at rewards level L is microAlgos + units*(L-rewardsBase), with units
// microAlgos/RewardUnit (basics.WithUpdatedRewards), so the set keeps the sums
// of those terms over the expired accounts and the others by VoteLastValid,
// moving them as the vote round passes it: a round costs its changed and newly
// expired accounts, not a walk over every online account.
type onlineSet struct {
	accts      map[basics.Address]onlineEntry
	unexpired  unexpiredHeap
	voteRnd    basics.Round // accounts whose keys expire before it are in sums
	rewardUnit uint64
	sums       expiredSums
}

// onlineEntry is what onlineAccounts keeps of an online account to compute
// the expired stake (trackerdb.BaseOnlineAccountData).
type onlineEntry struct {
	microAlgos    uint64
	rewardsBase   uint64
	voteLastValid basics.Round // 0: never expires
	expired       bool         // counted in sums
}

// expiredSums are the expired accounts' microAlgos, units and
// units*rewardsBase.
type expiredSums struct {
	microAlgos, units, unitsBase uint64
	overflowed                   bool
}

func (s *expiredSums) add(e onlineEntry, rewardUnit uint64) {
	var ot basics.OverflowTracker
	units := e.microAlgos / rewardUnit
	s.microAlgos = ot.Add(s.microAlgos, e.microAlgos)
	s.units = ot.Add(s.units, units)
	s.unitsBase = ot.Add(s.unitsBase, ot.Mul(units, e.rewardsBase))
	s.overflowed = s.overflowed || ot.Overflowed
}

// remove undoes add; sums that overflowed stay marked until a rebuild.
func (s *expiredSums) remove(e onlineEntry, rewardUnit uint64) {
	units := e.microAlgos / rewardUnit
	s.microAlgos -= e.microAlgos
	s.units -= units
	s.unitsBase -= units * e.rewardsBase
}

// set records addr as online with e.
func (s *onlineSet) set(addr basics.Address, e onlineEntry) {
	old, had := s.accts[addr]
	if had && old.expired {
		s.sums.remove(old, s.rewardUnit)
	}
	e.expired = false
	switch {
	case e.voteLastValid == 0:
	case e.voteLastValid < s.voteRnd:
		e.expired = true
		s.sums.add(e, s.rewardUnit)
	case !had || old.voteLastValid != e.voteLastValid:
		// otherwise old waits in unexpired under the same key (an expired old
		// has a voteLastValid below voteRnd, so it differs from e's)
		heap.Push(&s.unexpired, unexpiredEntry{e.voteLastValid, addr})
	}
	s.accts[addr] = e
}

// drop records addr as offline.
func (s *onlineSet) drop(addr basics.Address) {
	old, ok := s.accts[addr]
	if !ok {
		return
	}
	if old.expired {
		s.sums.remove(old, s.rewardUnit)
	}
	delete(s.accts, addr)
}

// advance moves into sums the accounts whose keys expire before voteRnd. It
// rebuilds the set when the vote round goes back or the reward unit changes.
func (s *onlineSet) advance(voteRnd basics.Round, rewardUnit uint64) {
	if voteRnd < s.voteRnd || rewardUnit != s.rewardUnit {
		s.rebuild(rewardUnit)
	}
	s.voteRnd = voteRnd
	for len(s.unexpired) > 0 && s.unexpired[0].voteLastValid < voteRnd {
		u := heap.Pop(&s.unexpired).(unexpiredEntry)
		e, ok := s.accts[u.addr]
		if !ok || e.expired || e.voteLastValid != u.voteLastValid {
			continue // stale: the account went offline or changed keys since
		}
		e.expired = true
		s.accts[u.addr] = e
		s.sums.add(e, s.rewardUnit)
	}
}

// rebuild empties sums, with every account waiting to expire.
func (s *onlineSet) rebuild(rewardUnit uint64) {
	s.voteRnd, s.rewardUnit, s.sums = 0, rewardUnit, expiredSums{}
	s.unexpired = s.unexpired[:0]
	for addr, e := range s.accts {
		e.expired = false
		s.accts[addr] = e
		if e.voteLastValid != 0 {
			s.unexpired = append(s.unexpired, unexpiredEntry{e.voteLastValid, addr})
		}
	}
	heap.Init(&s.unexpired)
}

// stake is the expired accounts' stake at rewards level rewardsLevel, false
// on overflow.
func (s *onlineSet) stake(rewardsLevel uint64) (uint64, bool) {
	var ot basics.OverflowTracker
	// each account's rewardsBase is at most rewardsLevel, so the difference is not negative
	total := ot.Add(s.sums.microAlgos, ot.Sub(ot.Mul(s.sums.units, rewardsLevel), s.sums.unitsBase))
	return total, !s.sums.overflowed && !ot.Overflowed
}

// unexpiredHeap orders accounts by VoteLastValid (container/heap). It keeps
// stale entries for accounts that went offline or changed keys since, which
// advance skips.
type unexpiredHeap []unexpiredEntry

type unexpiredEntry struct {
	voteLastValid basics.Round
	addr          basics.Address
}

func (h unexpiredHeap) Len() int           { return len(h) }
func (h unexpiredHeap) Less(i, j int) bool { return h[i].voteLastValid < h[j].voteLastValid }
func (h unexpiredHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *unexpiredHeap) Push(x any)        { *h = append(*h, x.(unexpiredEntry)) }
func (h *unexpiredHeap) Pop() any {
	old := *h
	e := old[len(old)-1]
	*h = old[:len(old)-1]
	return e
}
