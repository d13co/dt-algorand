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

package simulation_test

import (
	"encoding/binary"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// changeCaller runs the change lookup its args ask for: arg0 names it ("acct", "holding", "local",
// "asset", "app", "box", "gkey", "lkey" or "prefix"), arg1 and arg2 are from and to, and the next
// args the lookup's own, ids as 8 bytes. It logs the round and status, each as 8 bytes, then a state
// key's new value as 8 bytes (when created or changed) or the name of a box under the prefix. If its
// last arg is "loop", it runs the lookup again while it returns status 4, from the round it
// returned, and then also logs how many times it ran it.
const changeCaller = `#pragma version 13
txna ApplicationArgs 1; btoi; store 1
txna ApplicationArgs 2; btoi; store 2
txn NumAppArgs; int 1; -; txnas ApplicationArgs; byte "loop"; ==; store 4
int 0; store 3
again:
load 3; int 1; +; store 3
callsub lookup
store 7; store 6; store 5
load 4; bz done
load 6; int 4; ==; bz done
load 5; store 1
b again
done:
load 5; itob; load 6; itob; concat; load 7; concat; log
load 4; bz end
load 3; itob; log
end:
int 1; return
lookup:
txna ApplicationArgs 0; byte "acct"; ==; bnz acct
txna ApplicationArgs 0; byte "holding"; ==; bnz holding
txna ApplicationArgs 0; byte "local"; ==; bnz local
txna ApplicationArgs 0; byte "asset"; ==; bnz asset
txna ApplicationArgs 0; byte "app"; ==; bnz app
txna ApplicationArgs 0; byte "box"; ==; bnz box
txna ApplicationArgs 0; byte "gkey"; ==; bnz gkey
txna ApplicationArgs 0; byte "lkey"; ==; bnz lkey
txna ApplicationArgs 0; byte "prefix"; ==; bnz prefix
err
acct:
txna ApplicationArgs 3; load 1; load 2; tt_acct_change; byte ""; retsub
holding:
txna ApplicationArgs 3; txna ApplicationArgs 4; btoi; load 1; load 2; tt_holding_change; byte ""; retsub
local:
txna ApplicationArgs 3; txna ApplicationArgs 4; btoi; load 1; load 2; tt_local_change; byte ""; retsub
asset:
txna ApplicationArgs 3; btoi; load 1; load 2; tt_asset_change; byte ""; retsub
app:
txna ApplicationArgs 3; btoi; load 1; load 2; tt_app_change; byte ""; retsub
box:
txna ApplicationArgs 3; btoi; txna ApplicationArgs 4; load 1; load 2; tt_box_change; byte ""; retsub
gkey:
txna ApplicationArgs 3; btoi; txna ApplicationArgs 4; load 1; load 2; tt_global_key_change; b keyval
lkey:
txna ApplicationArgs 3; txna ApplicationArgs 4; btoi; txna ApplicationArgs 5; load 1; load 2; tt_local_key_change; b keyval
prefix:
txna ApplicationArgs 3; btoi; txna ApplicationArgs 4; load 1; load 2; tt_box_prefix_change; retsub
keyval:
store 8; dup; int 1; ==; dig 1; int 2; ==; ||; bz keynone
load 8; itob; retsub
keynone:
byte ""; retsub
`

// changeCall is a call of changeCaller for the lookup, over the range from, to, with its args.
func changeCall(lookup string, from, to basics.Round, a ...string) simulation.QueryCall {
	return args(append([]string{lookup, u64(uint64(from)), u64(uint64(to))}, a...)...)
}

// loopCall is changeCall that runs the lookup again while the search is not finished.
func loopCall(lookup string, from, to basics.Round, a ...string) simulation.QueryCall {
	call := changeCall(lookup, from, to, a...)
	call.Args = append(call.Args, []byte("loop"))
	return call
}

// changeResult is what changeCaller logs of a lookup.
type changeResult struct {
	Round  basics.Round
	Status uint8
	Value  string
}

// decodeChange decodes the log of a lookup.
func decodeChange(t *testing.T, log string) changeResult {
	t.Helper()
	require.GreaterOrEqual(t, len(log), 16, "%x", log)
	b := []byte(log)
	status := binary.BigEndian.Uint64(b[8:16])
	require.LessOrEqual(t, status, uint64(4))
	return changeResult{Round: basics.Round(binary.BigEndian.Uint64(b)), Status: uint8(status), Value: string(b[16:])}
}

// changeHistoryApp writes the global, local and box state its args ask for: "g" key uint, "gd" key,
// "l" key uint (the sender's local state), "b" name value, "bd" name.
const changeHistoryApp = `#pragma version 10
txn ApplicationID; bz ok
txn OnCompletion; int OptIn; ==; bnz ok
txna ApplicationArgs 0; byte "g"; ==; bnz g
txna ApplicationArgs 0; byte "gd"; ==; bnz gd
txna ApplicationArgs 0; byte "l"; ==; bnz l
txna ApplicationArgs 0; byte "b"; ==; bnz b
txna ApplicationArgs 0; byte "bd"; ==; bnz bd
err
g:
txna ApplicationArgs 1; txna ApplicationArgs 2; btoi; app_global_put; b ok
gd:
txna ApplicationArgs 1; app_global_del; b ok
l:
txn Sender; txna ApplicationArgs 1; txna ApplicationArgs 2; btoi; app_local_put; b ok
b:
txna ApplicationArgs 1; txna ApplicationArgs 2; box_put; b ok
bd:
txna ApplicationArgs 1; box_del; pop
ok:
int 1`

// changeHistory is a ledger whose rounds change an app's state, a holding and an account in known
// ways.
type changeHistory struct {
	*queryFixture
	app basics.AppIndex
	// the rounds of the changes
	create, x1, y, x2, xDel, p1, p1v2, q1, p1Del, optIn, lk, pay, hold basics.Round
	home                                                               basics.Round
}

func makeChangeHistory(t *testing.T, configure ...func(*config.Local)) *changeHistory {
	f := makeQueryFixture(t, append([]func(*config.Local){func(cfg *config.Local) {
		cfg.EnableTimeTravel = true
		cfg.MaxAcctLookback = 64
	}}, configure...)...)
	h := &changeHistory{queryFixture: f}
	sender := f.env.Accounts[0]
	h.app = f.env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram:   changeHistoryApp,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 4},
		LocalStateSchema:  basics.StateSchema{NumUint: 2},
	})
	h.create = f.env.TxnInfo.LatestRound()
	f.env.TransferAlgos(sender.Addr, h.app.Address(), 1_000_000)
	call := func(from simulationtesting.Account, a ...string) basics.Round {
		txn := txntest.Txn{Type: protocol.ApplicationCallTx, Sender: from.Addr, ApplicationID: h.app}
		for _, s := range a {
			txn.ApplicationArgs = append(txn.ApplicationArgs, []byte(s))
		}
		if a[0] == "b" || a[0] == "bd" {
			txn.Boxes = []transactions.BoxRef{{Name: []byte(a[1])}}
		}
		f.env.Txn(f.env.TxnInfo.NewTxn(txn).Txn().Sign(from.Sk))
		return f.env.TxnInfo.LatestRound()
	}
	h.x1 = call(sender, "g", "x", u64(1))
	h.y = call(sender, "g", "y", u64(5))
	h.x2 = call(sender, "g", "x", u64(2))
	h.xDel = call(sender, "gd", "x")
	h.p1 = call(sender, "b", "p1", "v1")
	h.p1v2 = call(sender, "b", "p1", "v2")
	h.q1 = call(sender, "b", "q1", "w")
	h.p1Del = call(sender, "bd", "p1")
	f.env.OptIntoApp(f.other.Addr, h.app)
	h.optIn = f.env.TxnInfo.LatestRound()
	h.lk = call(f.other, "l", "lk", u64(3))
	f.env.TransferAlgos(sender.Addr, f.other.Addr, 1000)
	h.pay = f.env.TxnInfo.LatestRound()
	f.env.TransferAsset(sender.Addr, f.other.Addr, f.asset, 1)
	h.hold = f.env.TxnInfo.LatestRound()
	// a round that changes none of them
	f.env.TransferAlgos(sender.Addr, f.env.Accounts[2].Addr, 1000)
	h.home = f.env.TxnInfo.LatestRound()
	return h
}

// query runs changeCaller with the calls at the home round, and returns their first logs, or
// their errors.
func (h *changeHistory) query(t *testing.T, limits func(*simulation.QueryLimits), calls ...simulation.QueryCall) []simulation.QueryCallResult {
	t.Helper()
	req := h.request(t, changeCaller, calls...)
	if limits != nil {
		limits(&req.Limits)
	}
	result, err := simulation.MakeSimulator(h.env.Ledger, false).Query(req)
	require.NoError(t, err)
	require.Len(t, result.Rounds, 1)
	require.Empty(t, result.Rounds[0].Error)
	return result.Rounds[0].Calls
}

// TestQueryChange: each change lookup finds the first change in its range, oldest first when
// from <= to and newest first otherwise, with its status and, for state keys and box prefixes, the
// new value or the box's name.
func TestQueryChange(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	h := makeChangeHistory(t)
	defer h.env.Close()
	app := u64(uint64(h.app))
	other := string(h.other.Addr[:])
	x, p, q, lk := "x", "p", "q", "lk"
	type tc struct {
		name string
		call simulation.QueryCall
		want changeResult
	}
	cases := []tc{
		{"app created", changeCall("app", 0, h.home, app), changeResult{h.create, 1, ""}},
		{"app last change", changeCall("app", h.home, 0, app), changeResult{h.xDel, 2, ""}},
		{"app not in a round", changeCall("app", h.p1, h.p1, app), changeResult{}},
		{"app in a round", changeCall("app", h.y, h.y, app), changeResult{h.y, 2, ""}},
		{"app before it existed", changeCall("app", h.create-1, 0, app), changeResult{}},

		{"key created", changeCall("gkey", 0, h.home, app, x), changeResult{h.x1, 1, u64(1)}},
		{"key next change", changeCall("gkey", h.x1+1, h.home, app, x), changeResult{h.x2, 2, u64(2)}},
		{"key deleted", changeCall("gkey", h.x2+1, h.home, app, x), changeResult{h.xDel, 3, ""}},
		{"key unchanged after", changeCall("gkey", h.xDel+1, h.home, app, x), changeResult{}},
		{"key last change", changeCall("gkey", h.home, 0, app, x), changeResult{h.xDel, 3, ""}},
		{"key before a round", changeCall("gkey", h.xDel-1, 0, app, x), changeResult{h.x2, 2, u64(2)}},
		{"key not changed by another key", changeCall("gkey", h.y, h.y, app, x), changeResult{}},
		{"other key", changeCall("gkey", h.home, 0, app, "y"), changeResult{h.y, 1, u64(5)}},
		{"missing key", changeCall("gkey", 0, h.home, app, "z"), changeResult{}},

		{"box created", changeCall("box", 0, h.home, app, "p1"), changeResult{h.p1, 1, ""}},
		{"box changed", changeCall("box", h.p1+1, h.home, app, "p1"), changeResult{h.p1v2, 2, ""}},
		{"box deleted", changeCall("box", h.home, 0, app, "p1"), changeResult{h.p1Del, 3, ""}},

		{"prefix created", changeCall("prefix", 0, h.home, app, p), changeResult{h.p1, 1, "p1"}},
		{"prefix changed", changeCall("prefix", h.p1+1, h.home, app, p), changeResult{h.p1v2, 2, "p1"}},
		{"prefix skips other boxes", changeCall("prefix", h.p1v2+1, h.home, app, p), changeResult{h.p1Del, 3, "p1"}},
		{"prefix newest first", changeCall("prefix", h.home, 0, app, q), changeResult{h.q1, 1, "q1"}},
		{"empty prefix", changeCall("prefix", h.home, 0, app, ""), changeResult{h.p1Del, 3, "p1"}},
		{"prefix none", changeCall("prefix", 0, h.home, app, "z"), changeResult{}},

		{"local state", changeCall("local", h.home, 0, other, app), changeResult{h.lk, 2, ""}},
		{"local state created", changeCall("local", 0, h.home, other, app), changeResult{h.optIn, 1, ""}},
		{"local key", changeCall("lkey", h.home, 0, other, app, lk), changeResult{h.lk, 1, u64(3)}},
		{"local key none before", changeCall("lkey", h.lk-1, 0, other, app, lk), changeResult{}},

		{"account", changeCall("acct", h.home, h.pay, other), changeResult{h.pay, 2, ""}},
		{"holding", changeCall("holding", h.home, 0, other, u64(uint64(h.asset))), changeResult{h.hold, 2, ""}},
		{"asset", changeCall("asset", h.home, 0, u64(uint64(h.asset))), changeResult{Status: 1}},
	}
	calls := make([]simulation.QueryCall, len(cases))
	for i, c := range cases {
		calls[i] = c.call
	}
	results := h.query(t, nil, calls...)
	for i, c := range cases {
		require.Empty(t, results[i].Error, c.name)
		require.Len(t, results[i].Logs, 1, c.name)
		got := decodeChange(t, results[i].Logs[0])
		if c.name == "asset" {
			// created when the fixture made it
			require.Equal(t, uint8(1), got.Status)
			require.NotZero(t, got.Round)
			require.Less(t, got.Round, h.create)
			continue
		}
		require.Equal(t, c.want, got, c.name)
	}
}

// TestQueryChangeSteps: a key or box prefix search that reads more than a step returns status 4
// and the round to resume from; resuming reaches the answer of an unbounded search, in either
// direction. Once a call's searches have read their total, the next search fails the call.
func TestQueryChangeSteps(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	h := makeChangeHistory(t)
	defer h.env.Close()
	app := u64(uint64(h.app))
	x, p := "x", "p"
	calls := []simulation.QueryCall{
		loopCall("gkey", h.x1+1, h.home, app, x),
		loopCall("gkey", h.home, 0, app, "y"),
		loopCall("gkey", 0, h.home, app, "z"),
		loopCall("prefix", h.p1v2+1, h.home, app, p),
		loopCall("prefix", h.home, 0, app, "q"),
	}
	want := []changeResult{{h.x2, 2, u64(2)}, {h.y, 1, u64(5)}, {}, {h.p1Del, 3, "p1"}, {h.q1, 1, "q1"}}

	// unbounded, each finds the answer at once
	results := h.query(t, nil, calls...)
	for i := range calls {
		require.Empty(t, results[i].Error, i)
		require.Equal(t, want[i], decodeChange(t, results[i].Logs[0]), i)
		require.Equal(t, u64(1), results[i].Logs[1], i)
	}

	// a step of a byte, they get there in more calls
	results = h.query(t, func(l *simulation.QueryLimits) { l.MaxScanStepBytes = 1 }, calls...)
	for i := range calls {
		require.Empty(t, results[i].Error, i)
		require.Equal(t, want[i], decodeChange(t, results[i].Logs[0]), i)
		require.Greater(t, binary.BigEndian.Uint64([]byte(results[i].Logs[1])), uint64(1), i)
	}

	// a call total of a byte: the first search runs, the next fails the call; the next call on the
	// same worker starts afresh
	one := changeCall("gkey", h.home, 0, app, x)
	results = h.query(t, func(l *simulation.QueryLimits) {
		l.MaxScanStepBytes, l.MaxCallScanBytes, l.Parallelism = 1, 1, 1
	}, calls[0], one, one)
	require.Contains(t, results[0].Error, "the call's searches read more than 1 bytes (MaxAPIQueryCallScanBytes)")
	// one search each: had the total carried over, the second would fail
	require.Empty(t, results[1].Error)
	require.Empty(t, results[2].Error)
	require.Equal(t, results[1].Logs, results[2].Logs)

	// 0 turns the searches off, not the other lookups
	results = h.query(t, func(l *simulation.QueryLimits) { l.MaxCallScanBytes = 0 },
		changeCall("gkey", 0, h.home, app, x), changeCall("app", 0, h.home, app))
	require.Contains(t, results[0].Error, "key and box prefix searches are off on this node (MaxAPIQueryCallScanBytes is 0)")
	require.Empty(t, results[1].Error)
}

// TestQueryChangeErrors: a range that ends after the query round fails the call, as does any lookup
// on a node with time travel off or without a history; a lookup adds to the state the call read.
func TestQueryChangeErrors(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	h := makeChangeHistory(t)
	defer h.env.Close()
	app := u64(uint64(h.app))
	results := h.query(t, nil,
		changeCall("app", h.home+1, 0, app),
		changeCall("app", 0, h.home+1, app),
		changeCall("app", h.home, h.home, app))
	require.Contains(t, results[0].Error, fmt.Sprintf("round %d is after the query round %d", h.home+1, h.home))
	require.Contains(t, results[1].Error, "is after the query round")
	require.Empty(t, results[2].Error)
	require.Equal(t, changeResult{}, decodeChange(t, results[2].Logs[0]))

	results = h.query(t, func(l *simulation.QueryLimits) { l.MaxSeekRounds = 0 }, changeCall("app", 0, h.home, app))
	require.Contains(t, results[0].Error, "time travel is off on this node")

	// each lookup counts as an object read: one fits in the state limit, with what the call reads
	// besides (under 1024 bytes), the five of a search in steps do not
	results = h.query(t, func(l *simulation.QueryLimits) { l.MaxScanStepBytes, l.MaxCallStateBytes = 1, 1280 },
		changeCall("app", 0, h.home, app),
		loopCall("gkey", 0, h.home, app, "z"))
	require.Empty(t, results[0].Error)
	require.Contains(t, results[1].Error, "MaxAPIQueryCallStateBytes")

	// a node without a history
	f := makeQueryFixture(t)
	defer f.env.Close()
	req := f.request(t, changeCaller, changeCall("app", 0, f.env.TxnInfo.LatestRound(), u64(uint64(f.reader))))
	result, err := simulation.MakeSimulator(f.env.Ledger, false).Query(req)
	require.NoError(t, err)
	require.Empty(t, result.Rounds[0].Error)
	require.Contains(t, result.Rounds[0].Calls[0].Error, "this node keeps no history (EnableTimeTravel is off)")
}
