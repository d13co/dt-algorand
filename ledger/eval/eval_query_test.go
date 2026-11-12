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

package eval

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"
	"unsafe"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// newTestQueryState returns a QueryState on the genesis state of a test ledger.
func newTestQueryState(t *testing.T) (*QueryState, bookkeeping.BlockHeader) {
	genesisInitState, _, _ := ledgertesting.Genesis(10)
	l := newTestLedger(t, bookkeeping.GenesisBalances{
		Balances:    genesisInitState.Accounts,
		FeeSink:     testSinkAddr,
		RewardsPool: testPoolAddr,
	})
	prevHdr, err := l.BlockHdr(0)
	require.NoError(t, err)
	nextHdr := bookkeeping.MakeBlock(prevHdr).BlockHeader
	_, totals, err := l.LatestTotals()
	require.NoError(t, err)
	q, err := NewQueryState(l, prevHdr, nextHdr, totals, true)
	require.NoError(t, err)
	return q, prevHdr
}

// queryCreate returns an app create running source, in the round after prevHdr.
func queryCreate(t *testing.T, prevHdr bookkeeping.BlockHeader, source string) transactions.SignedTxn {
	ops, err := logic.AssembleString(source)
	require.NoError(t, err, ops.Errors)
	clear, err := logic.AssembleString("#pragma version 10\nint 1")
	require.NoError(t, err)
	return transactions.SignedTxn{Txn: transactions.Transaction{
		Type: protocol.ApplicationCallTx,
		Header: transactions.Header{
			Sender:      testSinkAddr,
			FirstValid:  prevHdr.Round + 1,
			LastValid:   prevHdr.Round + 1,
			GenesisHash: prevHdr.GenesisHash,
		},
		ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
			ApprovalProgram:   ops.Program,
			ClearStateProgram: clear.Program,
			GlobalStateSchema: basics.StateSchema{NumUint: 1},
		},
	}}
}

func be64(v uint64) string {
	return string(binary.BigEndian.AppendUint64(nil, v))
}

// TestQueryStateCallsIsolated: every call creates the app the next block would, and sees the state
// the round left, not the writes of the calls before it, which never reach the QueryState.
func TestQueryStateCallsIsolated(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	q, prevHdr := newTestQueryState(t)
	stxn := queryCreate(t, prevHdr, `#pragma version 10
global CurrentApplicationID; itob; log
byte "k"; app_global_get; itob; log
byte "k"; int 1; app_global_put
int 1`)
	want := []string{be64(prevHdr.TxnCounter + 1), be64(0)}
	for range 3 {
		logs, err := q.Call(stxn, QueryCallOptions{})
		require.NoError(t, err)
		require.Equal(t, want, logs)
	}
	require.Zero(t, q.cow.mods.Accts.Len())
	require.Empty(t, q.cow.mods.Creatables)
	require.Zero(t, q.cow.txnCount)
}

// countingQueryLedger counts the account lookups of its ledger, by address.
type countingQueryLedger struct {
	LedgerForCowBase
	lookups map[basics.Address]int
}

func (l *countingQueryLedger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, error) {
	l.lookups[addr]++
	return l.LedgerForCowBase.LookupWithoutRewards(rnd, addr)
}

// TestQueryStateCacheResetPerCall: each call starts with an empty lookup cache, so it asks the
// ledger once for each account it reads, however often it reads it and whatever the calls before
// it read.
func TestQueryStateCacheResetPerCall(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, addrs, _ := ledgertesting.Genesis(10)
	tl := newTestLedger(t, bookkeeping.GenesisBalances{
		Balances:    genesisInitState.Accounts,
		FeeSink:     testSinkAddr,
		RewardsPool: testPoolAddr,
	})
	l := &countingQueryLedger{LedgerForCowBase: tl, lookups: make(map[basics.Address]int)}
	prevHdr, err := l.BlockHdr(0)
	require.NoError(t, err)
	_, totals, err := tl.LatestTotals()
	require.NoError(t, err)
	q, err := NewQueryState(l, prevHdr, bookkeeping.MakeBlock(prevHdr).BlockHeader, totals, true)
	require.NoError(t, err)

	stxn := queryCreate(t, prevHdr, fmt.Sprintf(`#pragma version 10
addr %[1]s; balance; pop
addr %[1]s; balance; pop
int 1`, addrs[0]))
	stxn.Txn.Accounts = []basics.Address{addrs[0]}
	for i := 1; i <= 3; i++ {
		_, err := q.Call(stxn, QueryCallOptions{})
		require.NoError(t, err)
		require.Equal(t, i, l.lookups[addrs[0]], "call %d", i)
	}
}

// TestQueryStateResetCache: resetQueryCache leaves a base as makeRoundCowBase makes it, whatever
// its caches held. It fills every map of the base, so a map resetQueryCache kept without emptying
// it fails it.
func TestQueryStateResetCache(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, n := range []int{1, maxKeptQueryCacheLen + 1} {
		b := makeRoundCowBase(nil, 7, 8, 9, config.Consensus[protocol.ConsensusCurrentVersion])
		v := reflect.ValueOf(b).Elem()
		for i := range v.NumField() {
			f := v.Field(i)
			if f.Kind() != reflect.Map {
				continue
			}
			f = reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem()
			for k := range n {
				key := reflect.New(f.Type().Key()).Elem()
				switch key.Kind() {
				case reflect.String:
					key.SetString(fmt.Sprint(k))
				default:
					// every other key type starts with an address or an index: vary its first two bytes
					reflect.NewAt(reflect.TypeFor[uint16](), unsafe.Pointer(key.Addr().Pointer())).Elem().SetUint(uint64(k))
				}
				f.SetMapIndex(key, reflect.New(f.Type().Elem()).Elem())
			}
			require.Equal(t, n, f.Len(), v.Type().Field(i).Name)
		}
		b.totalOnline = basics.MicroAlgos{Raw: 1}
		b.resetQueryCache()
		require.Equal(t, makeRoundCowBase(nil, 7, 8, 9, config.Consensus[protocol.ConsensusCurrentVersion]), b)
	}
}

// TestQueryStateFailure: a failed call keeps the logs it made.
func TestQueryStateFailure(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	q, prevHdr := newTestQueryState(t)
	logs, err := q.Call(queryCreate(t, prevHdr, "#pragma version 10\nbyte \"a\"; log; byte \"b\"; log; err"), QueryCallOptions{})
	require.ErrorContains(t, err, "err opcode executed")
	require.Equal(t, []string{"a", "b"}, logs)

	stxn := queryCreate(t, prevHdr, "#pragma version 10\nint 1")
	stxn.Txn.Type = protocol.PaymentTx
	_, err = q.Call(stxn, QueryCallOptions{})
	require.ErrorIs(t, err, ErrQueryNotAppCall)

	// WellFormed applies: no app call may be valid for more rounds than MaxTxnLife.
	stxn = queryCreate(t, prevHdr, "#pragma version 10\nint 1")
	stxn.Txn.LastValid = stxn.Txn.FirstValid + 100_000
	_, err = q.Call(stxn, QueryCallOptions{})
	require.ErrorContains(t, err, "window size excessive")
}

// TestQueryStateBudgetAndLogs: the call's budget and log limits replace the protocol's.
func TestQueryStateBudgetAndLogs(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	q, prevHdr := newTestQueryState(t)
	// About 4 budget per iteration: 1,000 iterations need more than the protocol's 700.
	loop := queryCreate(t, prevHdr, `#pragma version 10
int 1000
loop: int 1; -; dup; bnz loop
pop; int 1`)
	_, err := q.Call(loop, QueryCallOptions{})
	require.ErrorContains(t, err, "dynamic cost budget exceeded")
	_, err = q.Call(loop, QueryCallOptions{Budget: 10_000})
	require.NoError(t, err)

	// 100 logs of 100 bytes: over the protocol's 32 logs and 1024 bytes.
	logs := queryCreate(t, prevHdr, `#pragma version 10
int 100
loop: int 100; bzero; log; int 1; -; dup; bnz loop
pop; int 1`)
	_, err = q.Call(logs, QueryCallOptions{Budget: 10_000})
	require.ErrorContains(t, err, "program logs too large")
	got, err := q.Call(logs, QueryCallOptions{Budget: 10_000, MaxLogBytes: 10_000})
	require.NoError(t, err)
	require.Len(t, got, 100)
	got, err = q.Call(logs, QueryCallOptions{Budget: 10_000, MaxLogBytes: 5_000})
	require.ErrorContains(t, err, "program logs too large")
	require.Len(t, got, 50)
}
