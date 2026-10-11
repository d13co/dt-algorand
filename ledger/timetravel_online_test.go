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
	"maps"
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// walkStake is the expired stake as historyTracker computed it before
// onlineSet: a walk over every online account.
func walkStake(accts map[basics.Address]onlineEntry, voteRnd basics.Round, rewardUnit, level uint64) uint64 {
	var total uint64
	for _, e := range accts {
		if e.voteLastValid != 0 && e.voteLastValid < voteRnd {
			stake, _, _ := basics.WithUpdatedRewards(rewardUnit, basics.Online, basics.MicroAlgos{Raw: e.microAlgos}, basics.MicroAlgos{}, e.rewardsBase, level)
			total += stake.Raw
		}
	}
	return total
}

// TestOnlineSetExpiredStake checks onlineSet's expired stake against a walk over
// every online account. It starts from accounts loaded as loadOnline does, then
// goes through random changes, accounts going offline, keys renewed or never
// expiring, vote rounds that jump ahead or go back, and reward unit changes.
func TestOnlineSetExpiredStake(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	rng := rand.New(rand.NewSource(1))
	addrs := make([]basics.Address, 40)
	for i := range addrs {
		addrs[i][0] = byte(i)
	}
	voteRnd, rewardUnit, level := basics.Round(1000), uint64(1_000_000), uint64(100)
	entry := func(addr basics.Address, walk map[basics.Address]onlineEntry) onlineEntry {
		e := onlineEntry{microAlgos: rng.Uint64() % 10_000_000_000_000, rewardsBase: level - min(level, uint64(rng.Intn(20)))}
		switch rng.Intn(4) {
		case 0: // never expires
		case 1: // a balance change: same keys
			e.voteLastValid = walk[addr].voteLastValid
		default:
			e.voteLastValid = (voteRnd + basics.Round(rng.Intn(200))).SubSaturate(50)
		}
		return e
	}
	walk := map[basics.Address]onlineEntry{}
	for _, addr := range addrs[:30] {
		walk[addr] = entry(addr, walk)
	}
	set := onlineSet{accts: maps.Clone(walk)}
	for step := range 50_000 {
		level += uint64(rng.Intn(3))
		for range rng.Intn(4) {
			addr := addrs[rng.Intn(len(addrs))]
			if rng.Intn(5) == 0 {
				delete(walk, addr)
				set.drop(addr)
				continue
			}
			e := entry(addr, walk)
			walk[addr] = e
			set.set(addr, e)
		}
		switch {
		case rng.Intn(300) == 0:
			voteRnd = voteRnd.SubSaturate(basics.Round(rng.Intn(20)))
		case rng.Intn(1000) == 0:
			rewardUnit = uint64(1_000_000 / (1 + rng.Intn(10)))
		case rng.Intn(50) == 0:
			voteRnd += basics.Round(rng.Intn(30))
		default:
			voteRnd += basics.Round(rng.Intn(3))
		}
		set.advance(voteRnd, rewardUnit)
		require.LessOrEqual(t, len(set.unexpired), maxUnexpired(len(set.accts)))
		require.Equal(t, walkStake(walk, voteRnd, rewardUnit, level), set.stake(level), "step %d", step)
	}
}

// TestOnlineSetCompactsStaleEntries checks that accounts changing keys or going
// offline before their keys expire do not grow unexpired without bound.
func TestOnlineSetCompactsStaleEntries(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	rekeyed, toggled, expired := basics.Address{1}, basics.Address{2}, basics.Address{3}
	set := onlineSet{accts: map[basics.Address]onlineEntry{}}
	set.set(expired, onlineEntry{microAlgos: 5_000_000, voteLastValid: 1})
	for rnd := basics.Round(1); rnd <= 20_000; rnd++ {
		// keys valid long after the test
		set.set(rekeyed, onlineEntry{microAlgos: 1_000_000, voteLastValid: 1<<24 + rnd%2})
		if rnd%2 == 0 {
			set.set(toggled, onlineEntry{microAlgos: 1_000_000, voteLastValid: 1 << 24})
		} else {
			set.drop(toggled)
		}
		set.advance(rnd+320, 1_000_000)
		require.LessOrEqual(t, len(set.unexpired), maxUnexpired(len(set.accts)))
		require.Equal(t, uint64(5_000_000), set.stake(0), "round %d", rnd)
	}
}

// TestOnlineSetWrappingSums checks the stake when units*rewardsLevel exceeds a
// uint64 but the stake does not.
func TestOnlineSetWrappingSums(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	const level = 2_000_000_000
	set := onlineSet{accts: map[basics.Address]onlineEntry{}}
	set.set(basics.Address{1}, onlineEntry{microAlgos: 10_000_000_000_000_000, rewardsBase: level, voteLastValid: 1})
	set.advance(10, 1_000_000)
	require.Equal(t, uint64(10_000_000_000_000_000), set.stake(level))
	set.drop(basics.Address{1})
	set.set(basics.Address{2}, onlineEntry{microAlgos: 3_000_000, rewardsBase: level - 1, voteLastValid: 1})
	require.Equal(t, uint64(3_000_003), set.stake(level))
}
