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
	"math/rand"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestOnlineSetExpiredStake checks onlineSet's expired stake against a walk over
// every online account, through random changes, accounts going offline, keys
// renewed or never expiring, a vote round that sometimes goes back, and reward
// unit changes.
func TestOnlineSetExpiredStake(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	rng := rand.New(rand.NewSource(1))
	addrs := make([]basics.Address, 40)
	for i := range addrs {
		addrs[i][0] = byte(i)
	}
	walk := map[basics.Address]onlineEntry{}
	set := onlineSet{accts: map[basics.Address]onlineEntry{}}
	voteRnd, rewardUnit, level := basics.Round(0), uint64(1_000_000), uint64(0)
	for step := range 50_000 {
		level += uint64(rng.Intn(3))
		for range rng.Intn(4) {
			addr := addrs[rng.Intn(len(addrs))]
			if rng.Intn(5) == 0 {
				delete(walk, addr)
				set.drop(addr)
				continue
			}
			e := onlineEntry{microAlgos: rng.Uint64() % 10_000_000_000_000, rewardsBase: level - min(level, uint64(rng.Intn(20)))}
			switch rng.Intn(4) {
			case 0: // never expires
			case 1: // a balance change: same keys
				e.voteLastValid = walk[addr].voteLastValid
			default:
				e.voteLastValid = (voteRnd + basics.Round(rng.Intn(200))).SubSaturate(50)
			}
			walk[addr] = e
			set.set(addr, e)
		}
		switch {
		case rng.Intn(300) == 0:
			voteRnd = voteRnd.SubSaturate(basics.Round(rng.Intn(20)))
		case rng.Intn(1000) == 0:
			rewardUnit = uint64(1_000_000 / (1 + rng.Intn(10)))
		default:
			voteRnd += basics.Round(rng.Intn(3))
		}
		set.advance(voteRnd, rewardUnit)

		var want uint64
		for _, e := range walk {
			if e.voteLastValid != 0 && e.voteLastValid < voteRnd {
				stake, _, _ := basics.WithUpdatedRewards(rewardUnit, basics.Online, basics.MicroAlgos{Raw: e.microAlgos}, basics.MicroAlgos{}, e.rewardsBase, level)
				want += stake.Raw
			}
		}
		got, ok := set.stake(level)
		require.True(t, ok)
		require.Equal(t, want, got, "step %d", step)
	}
}
