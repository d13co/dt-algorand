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

package simulation

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func init() {
	// Every test in this package's binary, including the external simulation_test ones, checks
	// the resource tracker's running totals against a full recomputation (for small trackers).
	debugTrackerChecks = true
}

func TestAppAccounts(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	proto := config.Consensus[protocol.ConsensusFuture]
	txns := make([]transactions.SignedTxnWithAD, 1)
	txns[0].Txn.Type = protocol.ApplicationCallTx
	ep := logic.NewAppEvalParams(txns, &proto, nil)

	appID := basics.AppIndex(12345)
	appAccount := appID.Address()

	for _, globalSharing := range []bool{false, true} {
		groupAssignment := makeGroupResourceTracker(txns, &proto, 1)
		var resources *ResourceTracker
		if globalSharing {
			resources = &groupAssignment.globalResources
		} else {
			resources = &groupAssignment.localTxnResources[0]
		}

		require.Empty(t, resources.Apps)
		require.Empty(t, resources.Accounts)

		require.False(t, groupAssignment.hasApp(appID, globalSharing, 0))
		require.False(t, groupAssignment.hasAccount(appAccount, ep, 7, globalSharing, 0))

		require.True(t, groupAssignment.addAccount(appAccount, globalSharing, 0))

		require.Empty(t, resources.Apps)
		require.Equal(t, map[basics.Address]struct{}{
			appAccount: {},
		}, resources.Accounts)

		require.False(t, groupAssignment.hasApp(appID, globalSharing, 0))
		require.True(t, groupAssignment.hasAccount(appAccount, ep, 7, globalSharing, 0))

		require.True(t, groupAssignment.addApp(appID, ep, 7, globalSharing, 0))

		require.Equal(t, map[basics.AppIndex]struct{}{
			appID: {},
		}, resources.Apps)
		require.Empty(t, resources.Accounts)

		require.True(t, groupAssignment.hasApp(appID, globalSharing, 0))
		require.True(t, groupAssignment.hasAccount(appAccount, ep, 7, globalSharing, 0))

		require.False(t, groupAssignment.hasAccount(appAccount, ep, 6, globalSharing, 0))
	}
}

func TestGlobalVsLocalResources(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	proto := config.Consensus[protocol.ConsensusFuture]
	txns := make([]transactions.SignedTxnWithAD, 3)
	txns[0].Txn.Type = protocol.ApplicationCallTx
	txns[1].Txn.Type = protocol.ApplicationCallTx
	txns[2].Txn.Type = protocol.ApplicationCallTx
	ep := logic.NewAppEvalParams(txns, &proto, nil)

	// For this test, we assume only the txn at index 1 supports group sharing.

	t.Run("accounts", func(t *testing.T) {
		account1 := basics.Address{1, 1, 1}
		account2 := basics.Address{2, 2, 2}

		groupAssignment := makeGroupResourceTracker(txns, &proto, 1)

		require.Empty(t, groupAssignment.globalResources.Accounts)
		require.Empty(t, groupAssignment.localTxnResources[0].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[1].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[2].Accounts)

		require.True(t, groupAssignment.addAccount(account1, false, 0))

		// account1 should be present in txn 0's local assignment
		require.Empty(t, groupAssignment.globalResources.Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
		}, groupAssignment.localTxnResources[0].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[1].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[2].Accounts)

		// Txn 1, which used global resources, can see account1
		require.True(t, groupAssignment.hasAccount(account1, ep, 7, true, 1))

		require.True(t, groupAssignment.addAccount(account2, true, 1))

		// account2 should be present in the global assignment
		require.Equal(t, map[basics.Address]struct{}{
			account2: {},
		}, groupAssignment.globalResources.Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
		}, groupAssignment.localTxnResources[0].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[1].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[2].Accounts)

		// Txn 2, which does not use global resources, cannot see either account
		require.False(t, groupAssignment.hasAccount(account1, ep, 7, false, 2))
		require.False(t, groupAssignment.hasAccount(account2, ep, 7, false, 2))

		require.True(t, groupAssignment.addAccount(account1, false, 2))

		// account1 should be present in txn 2's local assignment
		require.Equal(t, map[basics.Address]struct{}{
			account2: {},
		}, groupAssignment.globalResources.Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
		}, groupAssignment.localTxnResources[0].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[1].Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
		}, groupAssignment.localTxnResources[2].Accounts)

		require.True(t, groupAssignment.addAccount(account2, false, 2))

		// account2 gets moved from the global assignment to the local assignment of txn 2
		require.Empty(t, groupAssignment.globalResources.Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
		}, groupAssignment.localTxnResources[0].Accounts)
		require.Empty(t, groupAssignment.localTxnResources[1].Accounts)
		require.Equal(t, map[basics.Address]struct{}{
			account1: {},
			account2: {},
		}, groupAssignment.localTxnResources[2].Accounts)
	})

	t.Run("assets", func(t *testing.T) {
		asset1 := basics.AssetIndex(100)
		asset2 := basics.AssetIndex(200)

		groupAssignment := makeGroupResourceTracker(txns, &proto, 1)

		require.Empty(t, groupAssignment.globalResources.Assets)
		require.Empty(t, groupAssignment.localTxnResources[0].Assets)
		require.Empty(t, groupAssignment.localTxnResources[1].Assets)
		require.Empty(t, groupAssignment.localTxnResources[2].Assets)

		require.True(t, groupAssignment.addAsset(asset1, false, 0))

		// asset1 should be present in txn 0's local assignment
		require.Empty(t, groupAssignment.globalResources.Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
		}, groupAssignment.localTxnResources[0].Assets)
		require.Empty(t, groupAssignment.localTxnResources[1].Assets)
		require.Empty(t, groupAssignment.localTxnResources[2].Assets)

		// Txn 1, which used global resources, can see asset1
		require.True(t, groupAssignment.hasAsset(asset1, true, 1))

		require.True(t, groupAssignment.addAsset(asset2, true, 1))

		// asset2 should be present in the global assignment
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset2: {},
		}, groupAssignment.globalResources.Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
		}, groupAssignment.localTxnResources[0].Assets)
		require.Empty(t, groupAssignment.localTxnResources[1].Assets)
		require.Empty(t, groupAssignment.localTxnResources[2].Assets)

		// Txn 2, which does not use global resources, cannot see either asset
		require.False(t, groupAssignment.hasAsset(asset1, false, 2))
		require.False(t, groupAssignment.hasAsset(asset2, false, 2))

		require.True(t, groupAssignment.addAsset(asset1, false, 2))

		// asset1 should be present in txn 2's local assignment
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset2: {},
		}, groupAssignment.globalResources.Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
		}, groupAssignment.localTxnResources[0].Assets)
		require.Empty(t, groupAssignment.localTxnResources[1].Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
		}, groupAssignment.localTxnResources[2].Assets)

		require.True(t, groupAssignment.addAsset(asset2, false, 2))

		// asset2 gets moved from the global assignment to the local assignment of txn 2
		require.Empty(t, groupAssignment.globalResources.Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
		}, groupAssignment.localTxnResources[0].Assets)
		require.Empty(t, groupAssignment.localTxnResources[1].Assets)
		require.Equal(t, map[basics.AssetIndex]struct{}{
			asset1: {},
			asset2: {},
		}, groupAssignment.localTxnResources[2].Assets)
	})

	t.Run("apps", func(t *testing.T) {
		app1 := basics.AppIndex(100)
		app2 := basics.AppIndex(200)

		groupAssignment := makeGroupResourceTracker(txns, &proto, 1)

		require.Empty(t, groupAssignment.globalResources.Apps)
		require.Empty(t, groupAssignment.localTxnResources[0].Apps)
		require.Empty(t, groupAssignment.localTxnResources[1].Apps)
		require.Empty(t, groupAssignment.localTxnResources[2].Apps)

		require.True(t, groupAssignment.addApp(app1, ep, 7, false, 0))

		// app1 should be present in txn 0's local assignment
		require.Empty(t, groupAssignment.globalResources.Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
		}, groupAssignment.localTxnResources[0].Apps)
		require.Empty(t, groupAssignment.localTxnResources[1].Apps)
		require.Empty(t, groupAssignment.localTxnResources[2].Apps)

		// Txn 1, which used global resources, can see app1
		require.True(t, groupAssignment.hasApp(app1, true, 1))

		require.True(t, groupAssignment.addApp(app2, ep, 7, true, 1))

		// app2 should be present in the global assignment
		require.Equal(t, map[basics.AppIndex]struct{}{
			app2: {},
		}, groupAssignment.globalResources.Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
		}, groupAssignment.localTxnResources[0].Apps)
		require.Empty(t, groupAssignment.localTxnResources[1].Apps)
		require.Empty(t, groupAssignment.localTxnResources[2].Apps)

		// Txn 2, which does not use global resources, cannot see either app
		require.False(t, groupAssignment.hasApp(app1, false, 2))
		require.False(t, groupAssignment.hasApp(app2, false, 2))

		require.True(t, groupAssignment.addApp(app1, ep, 7, false, 2))

		// app1 should be present in txn 2's local assignment
		require.Equal(t, map[basics.AppIndex]struct{}{
			app2: {},
		}, groupAssignment.globalResources.Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
		}, groupAssignment.localTxnResources[0].Apps)
		require.Empty(t, groupAssignment.localTxnResources[1].Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
		}, groupAssignment.localTxnResources[2].Apps)

		require.True(t, groupAssignment.addApp(app2, ep, 7, false, 2))

		// app2 gets moved from the global assignment to the local assignment of txn 2
		require.Empty(t, groupAssignment.globalResources.Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
		}, groupAssignment.localTxnResources[0].Apps)
		require.Empty(t, groupAssignment.localTxnResources[1].Apps)
		require.Equal(t, map[basics.AppIndex]struct{}{
			app1: {},
			app2: {},
		}, groupAssignment.localTxnResources[2].Apps)
	})
}

func TestEffectiveMultiplier(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		m        uint64
		expected int
	}{
		{0, 1},
		{1, 1},
		{2, 2},
		{MaxResourceLimitMultiplier, MaxResourceLimitMultiplier},
		{MaxResourceLimitMultiplier + 1, MaxResourceLimitMultiplier},
		{^uint64(0), MaxResourceLimitMultiplier},
	} {
		require.Equal(t, tc.expected, effectiveMultiplier(tc.m), "m=%d", tc.m)
	}
}

// TestGroupResourceTrackerMultiplier checks that the multiplier scales every limit of the group and
// txn trackers, and that the references a txn lists are subtracted after scaling.
func TestGroupResourceTrackerMultiplier(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	proto := config.Consensus[protocol.ConsensusFuture]
	limits := func(r ResourceTracker) [6]int {
		return [6]int{r.MaxAccounts, r.MaxAssets, r.MaxApps, r.MaxBoxes, r.MaxTotalRefs, r.MaxCrossProductReferences}
	}

	t.Run("one app call", func(t *testing.T) {
		t.Parallel()
		txns := make([]transactions.SignedTxnWithAD, 1)
		txns[0].Txn.Type = protocol.ApplicationCallTx

		one := makeGroupResourceTracker(txns, &proto, 1)
		three := makeGroupResourceTracker(txns, &proto, 3)

		// sanity: the 1x global limits come from the consensus params, so none of them is zero
		for i, v := range limits(one.globalResources) {
			require.Positive(t, v, "limit %d", i)
		}
		for i, v := range limits(one.globalResources) {
			require.Equal(t, 3*v, limits(three.globalResources)[i], "limit %d", i)
		}
		for i, v := range limits(one.localTxnResources[0]) {
			require.Equal(t, 3*v, limits(three.localTxnResources[0])[i], "limit %d", i)
		}

		// spot check against the consensus params
		require.Equal(t, 3*proto.MaxTxGroupSize*proto.MaxAppBoxReferences, three.globalResources.MaxBoxes)
		require.Equal(t, 3*proto.MaxAppTxnForeignApps*(proto.MaxAppTxnForeignApps+2)*proto.MaxTxGroupSize,
			three.globalResources.MaxCrossProductReferences)
		require.Equal(t, 3*proto.MaxAppTotalTxnReferences, three.localTxnResources[0].MaxTotalRefs)
	})

	t.Run("listed refs", func(t *testing.T) {
		t.Parallel()
		txns := make([]transactions.SignedTxnWithAD, 2)
		txns[0].Txn.Type = protocol.ApplicationCallTx
		txns[0].Txn.Accounts = []basics.Address{{1}}
		txns[0].Txn.ForeignApps = []basics.AppIndex{11}
		txns[0].Txn.ForeignAssets = []basics.AssetIndex{21, 22}
		txns[0].Txn.Boxes = []transactions.BoxRef{{Index: 0, Name: []byte("b")}}
		txns[1].Txn.Type = protocol.PaymentTx

		const m = 3
		tracker := makeGroupResourceTracker(txns, &proto, m)

		local := tracker.localTxnResources[0]
		require.Equal(t, m*(proto.MaxAppTxnAccounts+proto.MaxAppTxnForeignApps)-2, local.MaxAccounts)
		require.Equal(t, m*proto.MaxAppTxnForeignAssets-2, local.MaxAssets)
		require.Equal(t, m*proto.MaxAppTxnForeignApps-1, local.MaxApps)
		require.Equal(t, m*proto.MaxAppBoxReferences-1, local.MaxBoxes)
		require.Equal(t, m*proto.MaxAppTotalTxnReferences-5, local.MaxTotalRefs)
		require.Zero(t, local.MaxCrossProductReferences)

		// a payment brings no resources, whatever the multiplier
		require.Equal(t, ResourceTracker{}, tracker.localTxnResources[1])

		// the global tracker adds the scaled capacity of the unused group slots; the payment
		// counts as a used slot, and the listed refs are subtracted once (through the txn tracker)
		unused := proto.MaxTxGroupSize - 2
		global := tracker.globalResources
		require.Equal(t, (unused+1)*m*(proto.MaxAppTxnAccounts+proto.MaxAppTxnForeignApps)-2, global.MaxAccounts)
		require.Equal(t, (unused+1)*m*proto.MaxAppTxnForeignAssets-2, global.MaxAssets)
		require.Equal(t, (unused+1)*m*proto.MaxAppTxnForeignApps-1, global.MaxApps)
		require.Equal(t, (unused+1)*m*proto.MaxAppBoxReferences-1, global.MaxBoxes)
		require.Equal(t, (unused+1)*m*proto.MaxAppTotalTxnReferences-5, global.MaxTotalRefs)
		require.Equal(t, m*proto.MaxAppTxnForeignApps*(proto.MaxAppTxnForeignApps+2)*(proto.MaxTxGroupSize-1),
			global.MaxCrossProductReferences)

		// the box I/O baseline counts the submitted refs, which are not multiplied
		require.Equal(t, 1, tracker.startingBoxes)
	})
}

// TestLargeTrackerLinear fills a tracker at MaxResourceLimitMultiplier with as many boxes, and
// separately as many apps, as it allows, and checks that this takes linear time. Box tracking used
// to re-sum every box on each new box, and account lookups looped over every tracked app, which
// took tens of seconds at this size.
func TestLargeTrackerLinear(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	proto := config.Consensus[protocol.ConsensusFuture]
	txns := make([]transactions.SignedTxnWithAD, 1)
	txns[0].Txn.Type = protocol.ApplicationCallTx
	bpr := proto.BytesPerBoxReference
	// generous: the quadratic version needed ~30 s without -race for 80k boxes
	const limit = 10 * time.Second

	t.Run("boxes", func(t *testing.T) {
		t.Parallel()
		tracker := makeGroupResourceTracker(txns, &proto, MaxResourceLimitMultiplier)
		n := tracker.globalResources.MaxBoxes
		require.Greater(t, n, 100_000)
		start := time.Now()
		var expectedRead uint64
		for i := 0; i < n; i++ {
			// alternate small and full reads, so empty refs are added and removed again
			readSize := bpr / 2
			if i%2 == 1 {
				readSize = bpr + bpr/2
			}
			require.True(t, tracker.addBox(basics.AppIndex(1), fmt.Sprintf("box%d", i), false, readSize, bpr), "box %d", i)
			expectedRead += readSize
		}
		elapsed := time.Since(start)
		require.Less(t, elapsed, limit)
		require.Len(t, tracker.globalResources.Boxes, n)
		require.Equal(t, expectedRead, tracker.globalResources.usedReadBudget())
		require.Equal(t, tracker.globalResources.sumReadBudget(), tracker.globalResources.usedReadBudget())
		// the read budget is exactly used up: one box ref per box
		require.Zero(t, tracker.globalResources.NumEmptyBoxRefs)
		// one more box does not fit
		require.False(t, tracker.addBox(basics.AppIndex(1), "extra", false, 0, bpr))
		require.Equal(t, expectedRead, tracker.globalResources.usedReadBudget())
		t.Logf("%d boxes in %v", n, elapsed)
	})

	t.Run("app accounts", func(t *testing.T) {
		t.Parallel()
		tracker := makeGroupResourceTracker(txns, &proto, MaxResourceLimitMultiplier)
		n := tracker.globalResources.MaxApps
		require.Greater(t, n, 100_000)
		start := time.Now()
		for i := 1; i <= n; i++ {
			// programVersion 6 so that addApp does not look at app accounts (ep is nil)
			require.True(t, tracker.addApp(basics.AppIndex(i), nil, 6, true, 0), "app %d", i)
		}
		for i := 1; i <= n; i++ {
			require.True(t, tracker.hasAccount(basics.AppIndex(i).Address(), nil, 8, true, 0), "app %d", i)
		}
		require.False(t, tracker.hasAccount(basics.Address{1}, nil, 8, true, 0))
		// before v7 app accounts are not available through app refs
		require.False(t, tracker.hasAccount(basics.AppIndex(1).Address(), nil, 6, true, 0))
		elapsed := time.Since(start)
		require.Less(t, elapsed, limit)
		t.Logf("%d apps in %v", n, elapsed)
	})
}

// BenchmarkAddBoxesAtMaxMultiplier measures filling a tracker at MaxResourceLimitMultiplier with
// boxes; ns/op is per box, so it stays flat as b.N grows if box tracking is linear.
func BenchmarkAddBoxesAtMaxMultiplier(b *testing.B) {
	proto := config.Consensus[protocol.ConsensusFuture]
	txns := make([]transactions.SignedTxnWithAD, 1)
	txns[0].Txn.Type = protocol.ApplicationCallTx
	tracker := makeGroupResourceTracker(txns, &proto, MaxResourceLimitMultiplier)
	names := make([]string, tracker.globalResources.MaxBoxes)
	for i := range names {
		names[i] = fmt.Sprintf("box%d", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		j := i % len(names)
		if j == 0 && i > 0 {
			b.StopTimer()
			tracker = makeGroupResourceTracker(txns, &proto, MaxResourceLimitMultiplier)
			b.StartTimer()
		}
		if !tracker.addBox(basics.AppIndex(1), names[j], false, proto.BytesPerBoxReference, proto.BytesPerBoxReference) {
			b.Fatalf("box %d did not fit", j)
		}
	}
}
