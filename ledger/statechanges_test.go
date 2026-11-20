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
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// scriptApp runs its app args as (op, key, value) triples:
//
//	g/G global put/del   l/L sender's local put/del   b/B box put/del
//	r box_replace at 0   R box_replace itob(n) at 0 for n below btoi(value)
//	f app_box_put to Applications 1   F app_box_put to the caller
//	i/o/e/c/d inner NoOp/OptIn/CloseOut/ClearState/Delete call of app btoi(key)
//	  with the args packed in value (pack), passing on its Applications
//	x fail
//
// It is also its own clear program. At creation it opts into family box access.
const scriptApp = `#pragma version 13
txn ApplicationID
bnz run
int 1
app_params_set AppFamilyBoxAccess
run:
int 0
store 0
loop:
load 0
txn NumAppArgs
>=
bnz done
load 0
txnas ApplicationArgs
store 1
load 0
int 1
+
txnas ApplicationArgs
store 2
load 0
int 2
+
txnas ApplicationArgs
store 3
load 0
int 3
+
store 0
load 1
byte "g"
==
bnz opg
load 1
byte "G"
==
bnz opG
load 1
byte "l"
==
bnz opl
load 1
byte "L"
==
bnz opL
load 1
byte "b"
==
bnz opb
load 1
byte "B"
==
bnz opB
load 1
byte "r"
==
bnz opr
load 1
byte "f"
==
bnz opf
load 1
byte "F"
==
bnz opF
load 1
byte "R"
==
bnz opR
load 1
byte "i"
==
bnz opi
load 1
byte "o"
==
bnz opo
load 1
byte "e"
==
bnz ope
load 1
byte "c"
==
bnz opc
load 1
byte "d"
==
bnz opd
err
opg:
load 2
load 3
app_global_put
b loop
opG:
load 2
app_global_del
b loop
opl:
txn Sender
load 2
load 3
app_local_put
b loop
opL:
txn Sender
load 2
app_local_del
b loop
opb:
load 2
load 3
box_put
b loop
opB:
load 2
box_del
pop
b loop
opr:
load 2
int 0
load 3
box_replace
b loop
opf:
txn Applications 1
load 2
load 3
app_box_put
b loop
opF:
global CallerApplicationID
load 2
load 3
app_box_put
b loop
opR:
int 0
store 8
rloop:
load 8
load 3
btoi
>=
bnz loop
load 2
int 0
load 8
itob
box_replace
load 8
int 1
+
store 8
b rloop
opi:
int NoOp
store 6
b inner
opo:
int OptIn
store 6
b inner
ope:
int CloseOut
store 6
b inner
opc:
int ClearState
store 6
b inner
opd:
int DeleteApplication
store 6
b inner
inner:
itxn_begin
int appl
itxn_field TypeEnum
load 2
btoi
itxn_field ApplicationID
load 6
itxn_field OnCompletion
int 1
store 7
apps:
load 7
txn NumApplications
>
bnz args
load 7
txnas Applications
itxn_field Applications
load 7
int 1
+
store 7
b apps
args:
load 3
store 4
iloop:
load 4
len
bz isubmit
load 4
int 0
getbyte
store 5
load 4
int 1
load 5
extract3
itxn_field ApplicationArgs
load 4
load 5
int 1
+
load 4
len
substring3
store 4
b iloop
isubmit:
itxn_submit
b loop
done:
int 1
`

// pack packs inner call args for scriptApp: each one length-prefixed.
func pack(args ...string) string {
	var b []byte
	for _, a := range args {
		b = append(b, byte(len(a)))
		b = append(b, a...)
	}
	return string(b)
}

func itob(app basics.AppIndex) string {
	return string(binary.BigEndian.AppendUint64(nil, uint64(app)))
}

func script(args ...string) [][]byte {
	out := make([][]byte, len(args))
	for i, a := range args {
		out[i] = []byte(a)
	}
	return out
}

func tvp(s string) *basics.TealValue {
	return &basics.TealValue{Type: basics.TealBytesType, Bytes: s}
}

func gChange(app basics.AppIndex, key string, before, after *basics.TealValue) StateChange {
	return StateChange{App: app, Type: logic.GlobalState, Key: key, Before: before, After: after}
}

func lChange(app basics.AppIndex, acct basics.Address, key string, before, after *basics.TealValue) StateChange {
	return StateChange{App: app, Type: logic.LocalState, Account: acct, Key: key, Before: before, After: after}
}

func bChange(app basics.AppIndex, key string, before, after *basics.TealValue) StateChange {
	return StateChange{App: app, Type: logic.BoxState, Key: key, Before: before, After: after}
}

// scByPath indexes a StateChanges by path.
func scByPath(sc StateChanges) map[string]TxnStateChanges {
	out := make(map[string]TxnStateChanges, len(sc.Txns))
	for _, tx := range sc.Txns {
		out[fmt.Sprint(tx.Path)] = tx
	}
	return out
}

// TestStateChangesForRound builds a block whose transactions overlap on
// globals, locals and boxes, through inner calls, failed ClearState programs
// and clears, and checks each transaction's before and after values, against
// expectations, the state at the rounds before and after, and the apply data.
func TestStateChangesForRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
	// The validator's deltas list the app params of a creator in another
	// order when several of its apps change in a block, so checkBlock is off
	// and the generator answers.
	dl.validator.Close()
	dl.validator = nil
	defer dl.generator.Close()
	l := dl.generator

	schemas := []basics.StateSchema{{NumByteSlice: 8}, {NumByteSlice: 4}}
	create := func() basics.AppIndex {
		vb := dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[0], ApprovalProgram: scriptApp, ClearStateProgram: scriptApp,
			GlobalStateSchema: schemas[0], LocalStateSchema: schemas[1]})
		return vb.Block().Payset[0].ApplyData.ApplicationID
	}
	p1, p2, p3 := create(), create(), create()
	for _, app := range []basics.AppIndex{p1, p2, p3} {
		dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: app.Address(), Amount: 10_000_000})
	}
	call := func(sender basics.Address, app basics.AppIndex, args ...string) *txntest.Txn {
		return &txntest.Txn{Type: "appl", Sender: sender, ApplicationID: app, ApplicationArgs: script(args...),
			Fee: 20_000, Boxes: []transactions.BoxRef{{Name: []byte("bx")}, {Name: []byte("by")}}}
	}
	withApps := func(tx *txntest.Txn, ids ...basics.AppIndex) *txntest.Txn {
		tx.ForeignApps = ids
		return tx
	}
	oc := func(tx *txntest.Txn, c transactions.OnCompletion) *txntest.Txn {
		tx.OnCompletion = c
		return tx
	}

	// state before the block
	dl.fullBlock(call(addrs[0], p1, "g", "ga", "A0", "g", "gz", "Z0", "b", "bx", "B0", "b", "by", "Y0"))
	dl.fullBlock(call(addrs[0], p3, "g", "p3", "P30"))
	dl.fullBlock(
		oc(call(addrs[1], p1, "l", "la", "L0"), transactions.OptInOC),
		oc(call(addrs[2], p1, "l", "lk", "lv"), transactions.OptInOC),
		withApps(call(addrs[0], p1, "o", itob(p2), pack("l", "lq", "Q0")), p2),
	)

	dl.beginBlock()
	dl.txns(
		// 0: box_replace
		call(addrs[3], p1, "g", "ga", "A1", "r", "bx", "X1"),
		// 1: the same global and box, a local
		call(addrs[1], p1, "g", "ga", "A2", "l", "la", "L2", "b", "bx", "B2"),
		// 2: foreign app_box_put into p1's box
		&txntest.Txn{Type: "appl", Sender: addrs[3], ApplicationID: p2, ApplicationArgs: script("f", "bx", "F3"),
			ForeignApps: []basics.AppIndex{p1}, Boxes: []transactions.BoxRef{{Index: 1, Name: []byte("bx")}}},
		// 3: the same local again
		call(addrs[1], p1, "l", "la", "L3"),
		// 4: an inner call writes its parent's box, then the parent writes it again
		withApps(call(addrs[3], p1, "i", itob(p2), pack("F", "bx", "C4"), "b", "bx", "P4"), p2),
		// 5: three inner calls in three itxn_submit
		withApps(call(addrs[3], p1,
			"i", itob(p2), pack("g", "k2", "V5a"),
			"i", itob(p3), pack("g", "k3", "V5b"),
			"i", itob(p2), pack("g", "k2", "V5c")), p2, p3),
		// 6: a write restored within the txn, and one that stays
		call(addrs[3], p1, "g", "ga", "TMP", "g", "ga", "A2", "g", "gb", "NEW"),
		// 7: a ClearState program that writes, calls p3, then fails
		oc(withApps(call(addrs[1], p1, "g", "ga", "CLR", "i", itob(p3), pack("g", "k3", "CLR"), "x", "", ""), p3), transactions.ClearStateOC),
		// 8: an inner ClearState whose program writes, calls p3, then fails
		withApps(call(addrs[3], p1, "c", itob(p2), pack("g", "k2", "CLR2", "i", itob(p3), pack("g", "k3", "CLR3"), "x", "", "")), p2, p3),
		// 9: CloseOut whose approval writes the local state first
		oc(call(addrs[2], p1, "l", "lk2", "V9"), transactions.CloseOutOC),
		// 10: OptIn again
		oc(call(addrs[2], p1, "l", "lk", "LV10"), transactions.OptInOC),
		// 11: app create
		&txntest.Txn{Type: "appl", Sender: addrs[4], ApprovalProgram: scriptApp, ClearStateProgram: scriptApp,
			GlobalStateSchema: schemas[0], ApplicationArgs: script("g", "c1", "C1")},
		// 12: app delete whose approval writes first
		oc(call(addrs[0], p3, "g", "k3", "DEL"), transactions.DeleteApplicationOC),
	)
	vb := dl.endBlock()
	r := vb.Block().Round()
	payset := vb.Block().Payset
	flat, err := vb.Block().DecodePaysetFlat()
	require.NoError(t, err)
	require.Len(t, payset, 13)
	p4 := payset[11].ApplyData.ApplicationID
	require.NotZero(t, p4)
	require.Len(t, payset[5].EvalDelta.InnerTxns, 3)
	require.Empty(t, payset[7].EvalDelta.InnerTxns, "the failed clear program's inner txn is not in the block")
	require.Len(t, payset[8].EvalDelta.InnerTxns, 1)
	require.Empty(t, payset[8].EvalDelta.InnerTxns[0].EvalDelta.InnerTxns)

	ctx := context.Background()
	sc, err := l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.NoError(t, err)
	require.Equal(t, r, sc.Round)
	got := scByPath(sc)
	paths := make([]string, 0, len(sc.Txns))
	for _, tx := range sc.Txns {
		paths = append(paths, fmt.Sprint(tx.Path))
	}
	require.Equal(t, []string{"[0]", "[1]", "[2]", "[3]", "[4]", "[4 0]", "[5]", "[5 0]", "[5 1]", "[5 2]",
		"[6]", "[7]", "[8]", "[8 0]", "[9]", "[10]", "[11]", "[12]"}, paths)
	for _, tx := range sc.Txns {
		if len(tx.Path) == 1 {
			require.Equal(t, flat[tx.Path[0]].ID(), tx.TxID)
		} else {
			require.Zero(t, tx.TxID)
		}
	}

	want := map[string][]StateChange{
		"[0]":   {gChange(p1, "ga", tvp("A0"), tvp("A1")), bChange(p1, "bx", tvp("B0"), tvp("X1"))},
		"[1]":   {gChange(p1, "ga", tvp("A1"), tvp("A2")), lChange(p1, addrs[1], "la", tvp("L0"), tvp("L2")), bChange(p1, "bx", tvp("X1"), tvp("B2"))},
		"[2]":   {bChange(p1, "bx", tvp("B2"), tvp("F3"))},
		"[3]":   {lChange(p1, addrs[1], "la", tvp("L2"), tvp("L3"))},
		"[4]":   {bChange(p1, "bx", tvp("F3"), tvp("P4"))},
		"[4 0]": {bChange(p1, "bx", tvp("F3"), tvp("C4"))},
		"[5]":   {gChange(p2, "k2", nil, tvp("V5c")), gChange(p3, "k3", nil, tvp("V5b"))},
		"[5 0]": {gChange(p2, "k2", nil, tvp("V5a"))},
		"[5 1]": {gChange(p3, "k3", nil, tvp("V5b"))},
		"[5 2]": {gChange(p2, "k2", tvp("V5a"), tvp("V5c"))},
		"[6]":   {gChange(p1, "gb", nil, tvp("NEW"))},
		"[7]":   {lChange(p1, addrs[1], "la", tvp("L3"), nil)},
		"[8]":   {lChange(p2, p1.Address(), "lq", tvp("Q0"), nil)},
		"[8 0]": {lChange(p2, p1.Address(), "lq", tvp("Q0"), nil)},
		"[9]":   {lChange(p1, addrs[2], "lk", tvp("lv"), nil)},
		"[10]":  {lChange(p1, addrs[2], "lk", nil, tvp("LV10"))},
		"[11]":  {gChange(p4, "c1", nil, tvp("C1"))},
		"[12]":  {gChange(p3, "k3", tvp("V5b"), nil), gChange(p3, "p3", tvp("P30"), nil)},
	}
	for path, changes := range want {
		require.Equal(t, changes, got[path].Changes, "txn %s", path)
	}

	checkAgainstRounds(t, l, r, sc)

	// budget: every program run, failed ClearState programs included (their
	// inner calls fail before their programs run); scriptApp reads no app state
	dev, err := l.StateChangesForRound(ctx, r, StateChangesFilter{Include: IncludeBudget | IncludeReads})
	require.NoError(t, err)
	require.Equal(t, sc.Txns, dev.Txns)
	require.Empty(t, dev.Reads)
	runs := make(map[string]ProgramBudget)
	var order []string
	for _, b := range dev.Budget {
		path := fmt.Sprint(b.Path)
		require.NotContains(t, runs, path)
		require.Positive(t, b.Cost, path)
		require.Equal(t, len(b.Path) == 1, b.TxID != (transactions.Txid{}), path)
		runs[path] = b
		order = append(order, path)
	}
	require.Equal(t, []string{"[0]", "[1]", "[2]", "[3]", "[4]", "[4 0]", "[5]", "[5 0]", "[5 1]", "[5 2]",
		"[6]", "[7]", "[8]", "[8 0]", "[9]", "[10]", "[11]", "[12]"}, order)
	for path, b := range runs {
		clear := path == "[7]" || path == "[8 0]"
		require.Equal(t, clear, b.Kind == ProgramClearState, path)
		require.Equal(t, clear, b.Failed, path)
	}
	require.Equal(t, p4, runs["[11]"].App)
	require.Equal(t, p2, runs["[8 0]"].App)
	// unpooled budgets would not carry over; each lone txn starts with its own
	require.Equal(t, runs["[4]"].BudgetBefore, runs["[0]"].BudgetBefore)

	// the evaluation is /v2/deltas', and each frame's own writes are its apply data's
	tr := newStateChangeTracer(StateChangesFilter{})
	sd, err := l.reevalBlock(ctx, r, nil, 0, tr, nil)
	require.NoError(t, err)
	require.NoError(t, tr.err)
	stored, err := l.GetStateDeltaForRound(r)
	require.NoError(t, err)
	untraced, err := l.reevalDelta(ctx, r, 0)
	require.NoError(t, err)
	// the order of a creator's app params varies between evaluations
	require.Equal(t, sortedChanges(&stored), sortedChanges(&sd))
	require.Equal(t, sortedChanges(&stored), sortedChanges(&untraced))
	checkOwnWrites(t, tr, payset)

	// filters
	for _, app := range []basics.AppIndex{p1, p2, p3, p4} {
		only, err := l.StateChangesForRound(ctx, r, StateChangesFilter{App: app})
		require.NoError(t, err)
		var want []TxnStateChanges // the unfiltered answer, restricted to app
		for _, tx := range sc.Txns {
			var changes []StateChange
			for _, c := range tx.Changes {
				if c.App == app {
					changes = append(changes, c)
				}
			}
			if changes != nil {
				want = append(want, TxnStateChanges{Path: tx.Path, TxID: tx.TxID, Changes: changes})
			}
		}
		require.NotEmpty(t, want)
		require.Equal(t, want, only.Txns, "app %d", app)
	}
	// the tracer logs only the app's changes
	appOnly := newStateChangeTracer(StateChangesFilter{App: p2})
	_, err = l.reevalBlock(ctx, r, nil, 0, appOnly, nil)
	require.NoError(t, err)
	require.NotEmpty(t, appOnly.log)
	require.Less(t, len(appOnly.log), len(tr.log))
	for _, ev := range appOnly.log {
		require.Equal(t, p2, ev.app)
	}

	one, err := l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{flat[4].ID()}})
	require.NoError(t, err)
	require.Len(t, one.Txns, 2)
	require.Equal(t, got["[4]"], one.Txns[0])
	require.Equal(t, got["[4 0]"], one.Txns[1])

	// several, out of order and repeated: the unfiltered answer's entries for
	// them, in execution order, with the state the skipped ones (3) left
	some, err := l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{flat[5].ID(), flat[2].ID(), flat[5].ID(), flat[4].ID()}})
	require.NoError(t, err)
	var wantSome []TxnStateChanges
	for _, tx := range sc.Txns {
		if p := tx.Path[0]; p == 2 || p == 4 || p == 5 {
			wantSome = append(wantSome, tx)
		}
	}
	require.Len(t, wantSome, 7)
	require.Equal(t, wantSome, some.Txns)
	// one skipped between two: 1 changes bx between 0 and 2
	ends, err := l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{flat[2].ID(), flat[0].ID()}})
	require.NoError(t, err)
	require.Equal(t, []TxnStateChanges{got["[0]"], got["[2]"]}, ends.Txns)
	require.Equal(t, tvp("B2"), ends.Txns[1].Changes[0].Before)
	// with app: both filters' intersection
	both, err := l.StateChangesForRound(ctx, r, StateChangesFilter{App: p3, TxIDs: []transactions.Txid{flat[5].ID(), flat[12].ID()}})
	require.NoError(t, err)
	require.Equal(t, []TxnStateChanges{
		{Path: []uint64{5}, TxID: flat[5].ID(), Changes: []StateChange{gChange(p3, "k3", nil, tvp("V5b"))}},
		got["[5 1]"],
		got["[12]"],
	}, both.Txns)
	// the tracer logs nothing after the last of them
	lastOnly := newStateChangeTracer(StateChangesFilter{})
	lastOnly.lastTop = 5
	_, err = l.reevalBlock(ctx, r, nil, 0, lastOnly, nil)
	require.NoError(t, err)
	require.NotEmpty(t, lastOnly.log)
	for _, ev := range lastOnly.log {
		require.LessOrEqual(t, ev.path[0], 5)
	}

	missing := transactions.Txid{1}
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{missing}})
	require.ErrorIs(t, err, ErrTxnNotInRound)
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{flat[4].ID(), missing}})
	require.ErrorIs(t, err, ErrTxnNotInRound, "one missing is enough")
	require.ErrorContains(t, err, missing.String())

	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{MaxBytes: 10})
	require.ErrorIs(t, err, ErrStateChangesTooLarge)

	// rounds without a block to evaluate
	_, err = l.StateChangesForRound(ctx, 0, StateChangesFilter{})
	require.Error(t, err)
	_, err = l.StateChangesForRound(ctx, l.Latest()+1, StateChangesFilter{})
	require.Error(t, err)
	require.NotErrorIs(t, err, ErrDeltaReevaluation)

	// busy slots, and a stopped request
	for range cap(l.history.reeval) {
		l.history.reeval <- struct{}{}
	}
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.ErrorIs(t, err, ErrReevalBusy)
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{TxIDs: []transactions.Txid{missing}})
	require.ErrorIs(t, err, ErrTxnNotInRound, "found without an evaluation")
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = l.StateChangesForRound(canceled, r, StateChangesFilter{})
	require.ErrorIs(t, err, context.Canceled)
	for range cap(l.history.reeval) {
		<-l.history.reeval
	}

	// a round whose stored changes disagree with the evaluation fails
	l.history.mu.Lock()
	i := slices.IndexFunc(l.history.pending, func(p historyRound) bool { return p.rnd == r })
	require.GreaterOrEqual(t, i, 0, "round %d is not written to the store yet", r)
	saved := l.history.pending[i].changes
	l.history.pending[i].changes = saved[1:]
	l.history.mu.Unlock()
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.ErrorIs(t, err, ErrDeltaReevaluation)
	l.history.mu.Lock()
	l.history.pending[i].changes = saved
	l.history.mu.Unlock()

	// once the round left the trackers, the history gives the same answer
	for range 8 {
		dl.fullBlock()
	}
	commitRoundLookback(4, l)
	_, err = l.accts.lookupStateDelta(r)
	require.Error(t, err, "round %d left the trackers", r)
	again, err := l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.NoError(t, err)
	require.Equal(t, sc, again)
}

// sortedChanges returns the objects sd changes, by key.
func sortedChanges(sd *ledgercore.StateDelta) []timetravel.Change {
	c := timetravel.Changes(sd)
	slices.SortFunc(c, func(a, b timetravel.Change) int { return bytes.Compare(a.Key, b.Key) })
	return c
}

// TestStateChangesLogBounds checks that the tracer keeps one event per key
// for each stretch of a program between inner calls, stops the evaluation
// once its log passes the limit, and that a ClearState program that fails on
// a short stack still lists its clear.
func TestStateChangesLogBounds(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
	dl.validator.Close()
	dl.validator = nil
	defer dl.generator.Close()
	l := dl.generator

	create := func(clear string) basics.AppIndex {
		vb := dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[0], ApprovalProgram: scriptApp, ClearStateProgram: clear,
			GlobalStateSchema: basics.StateSchema{NumByteSlice: 8}, LocalStateSchema: basics.StateSchema{NumByteSlice: 4}})
		app := vb.Block().Payset[0].ApplyData.ApplicationID
		dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: app.Address(), Amount: 10_000_000})
		return app
	}
	p1, p2 := create(scriptApp), create(scriptApp)
	// fails on a short stack: the assembler doesn't check it
	short := create("#pragma version 13\n#pragma typetrack false\nbyte \"k\"\napp_global_put\nint 1")
	call := func(sender basics.Address, app basics.AppIndex, args ...string) *txntest.Txn {
		return &txntest.Txn{Type: "appl", Sender: sender, ApplicationID: app, ApplicationArgs: script(args...),
			Fee: 10_000, ForeignApps: []basics.AppIndex{p2}, Boxes: []transactions.BoxRef{{Name: []byte("bz")}}}
	}
	zero := string(make([]byte, 8))
	big := string(make([]byte, 1000))
	bigRefs := []transactions.BoxRef{{Name: []byte("big")}, {}}
	dl.fullBlock(
		call(addrs[0], p1, "b", "bz", zero),
		&txntest.Txn{Type: "appl", Sender: addrs[0], ApplicationID: p1, ApplicationArgs: script("b", "big", big), Boxes: bigRefs},
		&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: short, OnCompletion: transactions.OptInOC, ApplicationArgs: script("l", "lv", "V")},
		&txntest.Txn{Type: "appl", Sender: addrs[2], ApplicationID: p1, OnCompletion: transactions.OptInOC},
	)

	n := func(i int) string { return string(binary.BigEndian.AppendUint64(nil, uint64(i))) }
	vb := dl.fullBlock(
		call(addrs[0], p1, "R", "bz", n(40)),
		call(addrs[0], p1, "R", "bz", n(20), "i", itob(p2), pack("g", "x", "X"), "R", "bz", n(10)),
		&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: short, OnCompletion: transactions.ClearStateOC},
	)
	r := vb.Block().Round()
	flat, err := vb.Block().DecodePaysetFlat()
	require.NoError(t, err)
	ctx := context.Background()

	tr := newStateChangeTracer(StateChangesFilter{})
	_, err = l.reevalBlock(ctx, r, nil, 0, tr, nil)
	require.NoError(t, err)
	require.NoError(t, tr.err)
	var paths []string
	for _, ev := range tr.log {
		paths = append(paths, fmt.Sprint(ev.path, ev.kind))
	}
	// 40 writes, then 20, the inner call, 10; then the clear
	require.Equal(t, []string{"[0] 0", "[1] 0", "[1 0] 0", "[1] 0", "[2] 1"}, paths)

	sc, err := l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.NoError(t, err)
	require.Equal(t, []TxnStateChanges{
		{Path: []uint64{0}, TxID: flat[0].ID(), Changes: []StateChange{bChange(p1, "bz", tvp(zero), tvp(n(39)))}},
		{Path: []uint64{1}, TxID: flat[1].ID(), Changes: []StateChange{bChange(p1, "bz", tvp(n(39)), tvp(n(9))), gChange(p2, "x", nil, tvp("X"))}},
		{Path: []uint64{1, 0}, Changes: []StateChange{gChange(p2, "x", nil, tvp("X"))}},
		{Path: []uint64{2}, TxID: flat[2].ID(), Changes: []StateChange{lChange(short, addrs[1], "lv", tvp("V"), nil)}},
	}, sc.Txns)

	// a log past the limit stops the evaluation
	tr = newStateChangeTracer(StateChangesFilter{MaxBytes: 50})
	evalCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	tr.cancel = cancel
	_, err = l.reevalBlock(evalCtx, r, nil, 0, tr, nil)
	require.Error(t, err)
	require.True(t, tr.tooLarge)
	require.Less(t, len(tr.tops), 3, "the evaluation stopped")
	require.Nil(t, tr.log)
	// which the answer reports as too large, not as stopped
	_, err = l.StateChangesForRound(ctx, r, StateChangesFilter{MaxBytes: 50})
	require.ErrorIs(t, err, ErrStateChangesTooLarge)
	require.NoError(t, ctx.Err())

	// the limit caught inside a ClearState program, which swallows the abort:
	// the first write fits, the second doesn't
	clear := call(addrs[2], p1, "g", "k1", "v", "g", "k2", "v")
	clear.OnCompletion = transactions.ClearStateOC
	vb = dl.fullBlock(clear)
	cr := vb.Block().Round()
	limit := uint64(eventBytes(&scEvent{key: "k1", val: basics.TealValue{Bytes: "v"}})+3) / 4
	tr = newStateChangeTracer(StateChangesFilter{MaxBytes: limit})
	_, _ = l.reevalBlock(ctx, cr, nil, 0, tr, nil)
	require.True(t, tr.tooLarge)
	_, err = l.StateChangesForRound(ctx, cr, StateChangesFilter{MaxBytes: limit})
	require.ErrorIs(t, err, ErrStateChangesTooLarge)
	_, err = l.StateChangesForRound(ctx, cr, StateChangesFilter{MaxBytes: 100 * limit})
	require.NoError(t, err)

	// the post-pass counts what it reads from the round before: a delete is
	// a small event, but the box it deletes is loaded
	vb = dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[0], ApplicationID: p1, ApplicationArgs: script("B", "big", ""), Boxes: bigRefs})
	dr := vb.Block().Round()
	del, err := l.StateChangesForRound(ctx, dr, StateChangesFilter{})
	require.NoError(t, err)
	require.Equal(t, []StateChange{bChange(p1, "big", tvp(big), nil)}, del.Txns[0].Changes)
	limit = uint64(eventBytes(&scEvent{key: "big"})+500+3) / 4 // fits the event, not the box
	_, err = l.StateChangesForRound(ctx, dr, StateChangesFilter{MaxBytes: limit})
	require.ErrorIs(t, err, ErrStateChangesTooLarge)
}

// checkAgainstRounds checks that each top-level txn's first before of a key
// is the value at r-1 when no earlier txn changed it, and the last after is the
// value at r.
func checkAgainstRounds(t *testing.T, l *Ledger, r basics.Round, sc StateChanges) {
	type key struct {
		typ  logic.AppStateEnum
		app  basics.AppIndex
		acct basics.Address
		key  string
	}
	at := func(rnd basics.Round, k key) *basics.TealValue {
		switch k.typ {
		case logic.BoxState:
			v, ok, err := l.lookupKvExists(rnd, apps.MakeBoxKey(uint64(k.app), k.key), nil)
			require.NoError(t, err)
			if !ok {
				return nil
			}
			return tvp(string(v))
		case logic.GlobalState:
			creator, ok, err := l.GetCreatorForRound(rnd, basics.CreatableIndex(k.app), basics.AppCreatable)
			require.NoError(t, err)
			if !ok {
				return nil
			}
			res, err := l.LookupApplication(rnd, creator, k.app)
			require.NoError(t, err)
			if res.AppParams == nil {
				return nil
			}
			if v, ok := res.AppParams.GlobalState[k.key]; ok {
				return &v
			}
		case logic.LocalState:
			res, err := l.LookupApplication(rnd, k.acct, k.app)
			require.NoError(t, err)
			if res.AppLocalState == nil {
				return nil
			}
			if v, ok := res.AppLocalState.KeyValue[k.key]; ok {
				return &v
			}
		}
		return nil
	}
	first := map[key]*basics.TealValue{}
	last := map[key]*basics.TealValue{}
	for _, tx := range sc.Txns {
		if len(tx.Path) != 1 {
			continue
		}
		for _, c := range tx.Changes {
			k := key{c.Type, c.App, c.Account, c.Key}
			if _, ok := first[k]; !ok {
				first[k] = c.Before
			}
			last[k] = c.After
		}
	}
	for k, v := range first {
		require.Equal(t, at(r-1, k), v, "before of %+v", k)
	}
	for k, v := range last {
		require.Equal(t, at(r, k), v, "after of %+v", k)
	}
}

// checkOwnWrites checks each transaction's own global and local writes to its
// app against its EvalDelta, inner transactions included. The EvalDelta can
// leave out a write that changed nothing.
func checkOwnWrites(t *testing.T, tr *stateChangeTracer, payset []transactions.SignedTxnInBlock) {
	own := map[string]map[StateChange]basics.ValueDelta{} // key: path; App, Type, Account, Key only
	for _, ev := range tr.log {
		if ev.kind != scWrite || ev.typ == logic.BoxState {
			continue
		}
		p := fmt.Sprint(ev.path)
		if own[p] == nil {
			own[p] = map[StateChange]basics.ValueDelta{}
		}
		vd := basics.ValueDelta{Action: basics.DeleteAction}
		if ev.exists {
			vd = ev.val.ToValueDelta()
		}
		own[p][StateChange{App: ev.app, Type: ev.typ, Account: ev.acct, Key: ev.key}] = vd
	}
	checked := 0
	var walk func(path []int, stxn transactions.SignedTxnWithAD)
	walk = func(path []int, stxn transactions.SignedTxnWithAD) {
		txn := &stxn.Txn
		app := txn.ApplicationID
		if app == 0 {
			app = stxn.ApplyData.ApplicationID
		}
		writes := own[fmt.Sprint(path)]
		ed := stxn.ApplyData.EvalDelta
		if txn.OnCompletion == transactions.ClearStateOC && len(path) > 1 && len(writes) == 0 {
			// An inner ClearState whose program failed keeps the deltas the
			// program built in place, though the evaluator discarded them.
			for i := range ed.InnerTxns {
				walk(append(slices.Clip(path), i), ed.InnerTxns[i])
			}
			return
		}
		for k, vd := range ed.GlobalDelta {
			require.Equal(t, vd, writes[StateChange{App: app, Type: logic.GlobalState, Key: k}], "global %q of %v", k, path)
			checked++
		}
		for idx, sd := range ed.LocalDeltas {
			var acct basics.Address
			switch {
			case idx == 0:
				acct = txn.Sender
			case int(idx) <= len(txn.Accounts):
				acct = txn.Accounts[idx-1]
			default:
				acct = ed.SharedAccts[int(idx)-1-len(txn.Accounts)]
			}
			for k, vd := range sd {
				require.Equal(t, vd, writes[StateChange{App: app, Type: logic.LocalState, Account: acct, Key: k}], "local %q of %v", k, path)
				checked++
			}
		}
		for i, inner := range ed.InnerTxns {
			walk(append(slices.Clip(path), i), inner)
		}
	}
	for i, stib := range payset {
		walk([]int{i}, stib.SignedTxnWithAD)
	}
	require.NotZero(t, checked)
}

// TestStateChangesTracerFrames checks the frames' paths, that ending a frame
// drops one an inner transaction left open, and that ending one that never
// began is an error.
func TestStateChangesTracerFrames(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep1 := &logic.EvalParams{TxnGroup: make([]transactions.SignedTxnWithAD, 2)}
	ep2 := &logic.EvalParams{TxnGroup: make([]transactions.SignedTxnWithAD, 1)}
	tr := newStateChangeTracer(StateChangesFilter{})
	tr.BeforeTxn(ep1, 0)
	tr.BeforeTxn(ep2, 0)
	tr.AfterTxn(ep2, 0, transactions.ApplyData{}, nil)
	tr.BeforeTxn(ep2, 0)
	require.Equal(t, []int{0, 1}, tr.frames[1].path)
	tr.AfterTxn(ep1, 0, transactions.ApplyData{}, nil)
	require.Empty(t, tr.frames)
	tr.BeforeTxn(ep1, 1)
	require.Equal(t, []int{1}, tr.frames[0].path)
	tr.AfterTxn(ep2, 0, transactions.ApplyData{}, nil)
	require.Error(t, tr.err, "a frame that never began")

	// a truncation forgets the keys it dropped, and their bytes
	tr = newStateChangeTracer(StateChangesFilter{})
	tr.BeforeTxn(ep1, 0)
	mark := len(tr.log)
	for _, k := range []string{"a", "b", "a"} {
		tr.add(scEvent{path: tr.frames[0].path, typ: logic.GlobalState, app: 1, key: k, exists: true})
	}
	require.Len(t, tr.log, 2, "a's writes share an event")
	require.Len(t, tr.lastIdx, 2)
	tr.truncate(mark)
	require.Empty(t, tr.log)
	require.Empty(t, tr.lastIdx)
	require.Zero(t, tr.bytes)
}

// scBase is a stateChangesBase over fixed state.
type scBase struct {
	creators map[basics.AppIndex]basics.Address
	res      map[basics.Address]map[basics.AppIndex]ledgercore.AppResource
	kv       map[string][]byte
}

func (b scBase) GetCreatorForRound(_ basics.Round, cidx basics.CreatableIndex, _ basics.CreatableType) (basics.Address, bool, error) {
	c, ok := b.creators[basics.AppIndex(cidx)]
	return c, ok, nil
}

func (b scBase) LookupApplication(_ basics.Round, addr basics.Address, app basics.AppIndex) (ledgercore.AppResource, error) {
	return b.res[addr][app], nil
}

func (b scBase) LookupKvExists(_ basics.Round, key string) ([]byte, bool, error) {
	v, ok := b.kv[key]
	return v, ok, nil
}

func (b scBase) LookupAsset(basics.Round, basics.Address, basics.AssetIndex) (ledgercore.AssetResource, error) {
	return ledgercore.AssetResource{}, nil
}

func (b scBase) LookupWithoutRewards(basics.Round, basics.Address) (ledgercore.AccountData, basics.Round, error) {
	return ledgercore.AccountData{}, 0, nil
}

// TestStateChangesReplay checks the post-pass on a hand-made log: an existing
// empty box, subtree nets where a parent writes after its child, a delete
// that a later write overwrites, and a clear whose keys a write made.
func TestStateChangesReplay(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	const app = basics.AppIndex(5)
	var creator, acct basics.Address
	creator[0], acct[0] = 1, 2
	base := scBase{
		creators: map[basics.AppIndex]basics.Address{app: creator},
		res: map[basics.Address]map[basics.AppIndex]ledgercore.AppResource{
			creator: {app: {AppParams: &basics.AppParams{GlobalState: basics.TealKeyValue{"g": {Type: basics.TealUintType, Uint: 7}}}}},
			acct:    {app: {AppLocalState: &basics.AppLocalState{KeyValue: basics.TealKeyValue{"l": {Type: basics.TealUintType, Uint: 1}}}}},
		},
		kv: map[string][]byte{apps.MakeBoxKey(uint64(app), "empty"): {}},
	}
	u := func(n uint64) basics.TealValue { return basics.TealValue{Type: basics.TealUintType, Uint: n} }
	up := func(n uint64) *basics.TealValue { v := u(n); return &v }
	tr := newStateChangeTracer(StateChangesFilter{})
	tr.tops = []*transactions.Transaction{{}, {}}
	p0, p00, p1 := []int{0}, []int{0, 0}, []int{1}
	tr.log = []scEvent{
		{path: p00, typ: logic.BoxState, app: app, key: "empty", exists: true, val: basics.TealValue{Type: basics.TealBytesType, Bytes: "x"}},
		{path: p00, typ: logic.GlobalState, app: app, key: "g"},                         // delete
		{path: p0, typ: logic.GlobalState, app: app, key: "g", exists: true, val: u(8)}, // parent writes after its child
		{path: p0, typ: logic.LocalState, app: app, acct: acct, key: "n", exists: true, val: u(2)},
		{path: p1, typ: logic.LocalState, app: app, acct: acct, key: "l", exists: true, val: u(3)},
		{path: p1, kind: scClearLocals, app: app, acct: acct},
		{path: p1, typ: logic.GlobalState, app: app, key: "g"},
		{path: p1, typ: logic.GlobalState, app: app, key: "g", exists: true, val: u(8)}, // restored
	}
	sc, err := tr.changes(context.Background(), base, 10, StateChangesFilter{})
	require.NoError(t, err)
	require.Equal(t, basics.Round(10), sc.Round)
	empty, x := tvp(""), tvp("x")
	require.Equal(t, []TxnStateChanges{
		{Path: []uint64{0}, TxID: transactions.Transaction{}.ID(), Changes: []StateChange{
			bChange(app, "empty", empty, x),
			gChange(app, "g", up(7), up(8)),
			lChange(app, acct, "n", nil, up(2)),
		}},
		{Path: []uint64{0, 0}, Changes: []StateChange{
			bChange(app, "empty", empty, x),
			gChange(app, "g", up(7), nil),
		}},
		{Path: []uint64{1}, TxID: transactions.Transaction{}.ID(), Changes: []StateChange{
			lChange(app, acct, "l", up(1), nil),
			lChange(app, acct, "n", up(2), nil),
		}},
	}, sc.Txns)
}

// BenchmarkStateChangesForRound compares StateChangesForRound with the
// re-evaluation of /v2/deltas on a block of app calls that each write a
// global, a local and a box.
func BenchmarkStateChangesForRound(b *testing.B) {
	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(b, genBalances, protocol.ConsensusFuture, cfg)
	dl.validator.Close()
	dl.validator = nil
	defer dl.generator.Close()
	l := dl.generator

	vb := dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[0], ApprovalProgram: scriptApp, ClearStateProgram: scriptApp,
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 8}, LocalStateSchema: basics.StateSchema{NumByteSlice: 4}})
	app := vb.Block().Payset[0].ApplyData.ApplicationID
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: app.Address(), Amount: 100_000_000})
	dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: app, OnCompletion: transactions.OptInOC})
	box := string(make([]byte, 1024))
	dl.beginBlock()
	for i := range 256 {
		dl.txn(&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: app, Note: []byte{byte(i), byte(i >> 8)},
			ApplicationArgs: script("g", "g", fmt.Sprint(i), "l", "l", fmt.Sprint(i), "b", "bx", box),
			Boxes:           []transactions.BoxRef{{Name: []byte("bx")}, {}, {}}})
	}
	r := dl.endBlock().Block().Round()
	ctx := context.Background()

	b.Run("deltas", func(b *testing.B) {
		for range b.N {
			_, err := l.reevalDelta(ctx, r, 0)
			require.NoError(b, err)
		}
	})
	b.Run("state-changes", func(b *testing.B) {
		for range b.N {
			sc, err := l.StateChangesForRound(ctx, r, StateChangesFilter{})
			require.NoError(b, err)
			require.Len(b, sc.Txns, 256)
		}
	})
}

// scFlat is a group's change as a record key and values.
type scFlat struct {
	k             gKey
	before, after gVal
}

func flattenGroup(g GroupStateChanges) []scFlat {
	var out []scFlat
	acct := func(p *ledgercore.AccountData) gVal {
		if p == nil {
			return gVal{}
		}
		return gVal{ok: true, acct: *p}
	}
	for _, c := range g.Accounts {
		out = append(out, scFlat{gKey{kind: gAcct, addr: c.Addr}, acct(c.Before), acct(c.After)})
	}
	for _, c := range g.Holdings {
		h := func(p *basics.AssetHolding) gVal {
			if p == nil {
				return gVal{}
			}
			return gVal{ok: true, holding: *p}
		}
		out = append(out, scFlat{gKey{kind: gHolding, addr: c.Addr, id: uint64(c.Asset)}, h(c.Before), h(c.After)})
	}
	for _, c := range g.Assets {
		a := func(p *basics.AssetParams) gVal {
			if p == nil {
				return gVal{creator: c.Creator}
			}
			return gVal{ok: true, creator: c.Creator, asset: *p}
		}
		out = append(out, scFlat{gKey{kind: gAsset, id: uint64(c.Asset)}, a(c.Before), a(c.After)})
	}
	for _, c := range g.Apps {
		a := func(p *basics.AppParams) gVal {
			if p == nil {
				return gVal{creator: c.Creator}
			}
			return gVal{ok: true, creator: c.Creator, app: *p}
		}
		out = append(out, scFlat{gKey{kind: gApp, id: uint64(c.App)}, a(c.Before), a(c.After)})
	}
	for _, c := range g.LocalStates {
		s := func(p *basics.StateSchema) gVal {
			if p == nil {
				return gVal{}
			}
			return gVal{ok: true, local: *p}
		}
		out = append(out, scFlat{gKey{kind: gLocal, addr: c.Addr, id: uint64(c.App)}, s(c.Before), s(c.After)})
	}
	return out
}

// TestStateChangesGroups checks the per-group changes besides app state: a new
// account funded twice (the second group's before is the first's after), a
// group with a rekey, an asset opt-in, transfer, freeze and opt-out, an asset
// created and destroyed, an app created, updated and deleted, a local opt-in
// and close-out, a closed account, and a global write, which lists no app
// params. Chaining the groups from the round before reaches the round's state.
func TestStateChangesGroups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg)
	dl.validator.Close()
	dl.validator = nil
	defer dl.generator.Close()
	l := dl.generator

	local := basics.StateSchema{NumByteSlice: 4}
	vb := dl.fullBlock(&txntest.Txn{Type: "appl", Sender: addrs[0], ApprovalProgram: scriptApp, ClearStateProgram: scriptApp,
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 8}, LocalStateSchema: local})
	pA := vb.Block().Payset[0].ApplyData.ApplicationID
	vb = dl.fullBlock(&txntest.Txn{Type: "acfg", Sender: addrs[0],
		AssetParams: basics.AssetParams{Total: 1000, UnitName: "X", Manager: addrs[0], Freeze: addrs[0]}})
	x := vb.Block().Payset[0].ApplyData.ConfigAsset
	dl.fullBlock(&txntest.Txn{Type: "axfer", Sender: addrs[1], XferAsset: x, AssetReceiver: addrs[1]})
	dl.fullBlock(&txntest.Txn{Type: "axfer", Sender: addrs[0], XferAsset: x, AssetReceiver: addrs[1], AssetAmount: 100})

	hdr, err := l.BlockHdr(l.Latest())
	require.NoError(t, err)
	created := func(i int) uint64 { return hdr.TxnCounter + uint64(i) + 1 } // the creatable made by payset index i
	y, q := basics.AssetIndex(created(8)), basics.AppIndex(created(10))
	fresh := ledgertesting.RandomAddress()
	updated := strings.Replace(scriptApp, "#pragma version 13", "#pragma version 13\nint 1\npop", 1)

	dl.beginBlock()
	dl.txns(
		&txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: fresh, Amount: 1_000_000}, // 0
		&txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: fresh, Amount: 1000},      // 1
	)
	dl.txgroup("", // 2, 3
		&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: addrs[3], Amount: 1000},
		&txntest.Txn{Type: "pay", Sender: addrs[3], Receiver: addrs[3], RekeyTo: addrs[4]},
	)
	dl.txns(
		&txntest.Txn{Type: "axfer", Sender: addrs[2], XferAsset: x, AssetReceiver: addrs[2]},                         // 4
		&txntest.Txn{Type: "axfer", Sender: addrs[1], XferAsset: x, AssetReceiver: addrs[2], AssetAmount: 10},        // 5
		&txntest.Txn{Type: "afrz", Sender: addrs[0], FreezeAsset: x, FreezeAccount: addrs[2], AssetFrozen: true},     // 6
		&txntest.Txn{Type: "axfer", Sender: addrs[1], XferAsset: x, AssetReceiver: addrs[0], AssetCloseTo: addrs[0]}, // 7
		&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 5, Manager: addrs[0]}},   // 8
		&txntest.Txn{Type: "acfg", Sender: addrs[0], ConfigAsset: y},                                                 // 9
		&txntest.Txn{Type: "appl", Sender: addrs[4], ApprovalProgram: scriptApp, ClearStateProgram: scriptApp},       // 10
		&txntest.Txn{Type: "appl", Sender: addrs[4], ApplicationID: q, OnCompletion: transactions.UpdateApplicationOC,
			ApprovalProgram: updated, ClearStateProgram: scriptApp}, // 11
		&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: pA, OnCompletion: transactions.OptInOC},            // 12
		&txntest.Txn{Type: "appl", Sender: addrs[1], ApplicationID: pA, OnCompletion: transactions.CloseOutOC},         // 13
		&txntest.Txn{Type: "appl", Sender: addrs[4], ApplicationID: q, OnCompletion: transactions.DeleteApplicationOC}, // 14
		&txntest.Txn{Type: "pay", Sender: fresh, Receiver: addrs[0], CloseRemainderTo: addrs[0]},                       // 15
		&txntest.Txn{Type: "appl", Sender: addrs[0], ApplicationID: pA, ApplicationArgs: script("g", "k", "v")},        // 16
	)
	vb = dl.endBlock()
	r := vb.Block().Round()
	flat, err := vb.Block().DecodePaysetFlat()
	require.NoError(t, err)
	require.Len(t, flat, 17)

	ctx := context.Background()
	all := IncludeAccounts | IncludeAssets | IncludeApps
	sc, err := l.StateChangesForRound(ctx, r, StateChangesFilter{Include: all})
	require.NoError(t, err)
	require.Equal(t, vb.Block().FeeSink, sc.FeeSink)
	require.Equal(t, vb.Block().CurrentProtocol, sc.Proto)
	require.Len(t, sc.Groups, 16)
	for i, g := range sc.Groups {
		first := i
		if i >= 3 {
			first = i + 1
		}
		require.Equal(t, first, g.First, "group %d", i)
		if i == 2 {
			require.Equal(t, 2, g.Count)
			require.Equal(t, flat[2].Txn.Group, g.GroupID)
			require.NotZero(t, g.GroupID)
			require.Equal(t, []transactions.Txid{flat[2].ID(), flat[3].ID()}, g.TxIDs)
		} else {
			require.Equal(t, 1, g.Count)
			require.Zero(t, g.GroupID)
			require.Equal(t, []transactions.Txid{flat[first].ID()}, g.TxIDs)
		}
	}

	// Chain the groups from the round before; every before is the running
	// value, and what is left is the round's state. The fee sink and the
	// rewards pool also change at the end of the block, outside any group.
	base := l.Counted(nil)
	at := func(rnd basics.Round, k gKey, creator basics.Address) gVal {
		rp := &scReplay{base: base, rnd: rnd}
		v, err := rp.read(k, &gVal{creator: creator})
		require.NoError(t, err)
		return v
	}
	running := make(map[gKey]gVal)
	creators := make(map[gKey]basics.Address)
	for i, g := range sc.Groups {
		for _, c := range flattenGroup(g) {
			creators[c.k] = c.after.creator
			want, ok := running[c.k]
			if !ok {
				want = at(r-1, c.k, c.after.creator)
			}
			require.True(t, want.same(c.k.kind, &c.before), "group %d %+v: before", i, c.k)
			require.False(t, c.before.same(c.k.kind, &c.after), "group %d %+v: unchanged but listed", i, c.k)
			running[c.k] = c.after
		}
	}
	for k, v := range running {
		if k.kind == gAcct && (k.addr == vb.Block().FeeSink || k.addr == vb.Block().RewardsPool) {
			continue
		}
		got := at(r, k, creators[k])
		require.True(t, got.same(k.kind, &v), "%+v at round %d", k, r)
	}

	byKey := func(g GroupStateChanges, k gKey) scFlat {
		for _, c := range flattenGroup(g) {
			if c.k == k {
				return c
			}
		}
		require.Failf(t, "missing", "%+v", k)
		return scFlat{}
	}
	acct := func(a basics.Address) gKey { return gKey{kind: gAcct, addr: a} }
	g := sc.Groups
	c := byKey(g[0], acct(fresh))
	require.False(t, c.before.ok)
	require.Equal(t, uint64(1_000_000), c.after.acct.MicroAlgos.Raw)
	byKey(g[0], acct(sc.FeeSink)) // the fee
	c = byKey(g[1], acct(fresh))
	require.Equal(t, uint64(1_000_000), c.before.acct.MicroAlgos.Raw)
	require.Equal(t, uint64(1_001_000), c.after.acct.MicroAlgos.Raw)
	c = byKey(g[2], acct(addrs[3]))
	require.Zero(t, c.before.acct.AuthAddr)
	require.Equal(t, addrs[4], c.after.acct.AuthAddr)
	holding := func(a basics.Address) gKey { return gKey{kind: gHolding, addr: a, id: uint64(x)} }
	c = byKey(g[3], holding(addrs[2]))
	require.False(t, c.before.ok)
	require.Equal(t, basics.AssetHolding{}, c.after.holding)
	c = byKey(g[4], holding(addrs[2]))
	require.Equal(t, uint64(10), c.after.holding.Amount)
	c = byKey(g[5], holding(addrs[2]))
	require.False(t, c.before.holding.Frozen)
	require.True(t, c.after.holding.Frozen)
	c = byKey(g[6], holding(addrs[1]))
	require.Equal(t, uint64(90), c.before.holding.Amount)
	require.False(t, c.after.ok)
	c = byKey(g[7], gKey{kind: gAsset, id: uint64(y)})
	require.False(t, c.before.ok)
	require.Equal(t, uint64(5), c.after.asset.Total)
	require.Equal(t, addrs[0], c.after.creator)
	c = byKey(g[8], gKey{kind: gAsset, id: uint64(y)})
	require.True(t, c.before.ok)
	require.False(t, c.after.ok)
	c = byKey(g[9], gKey{kind: gApp, id: uint64(q)})
	require.False(t, c.before.ok)
	require.Equal(t, addrs[4], c.after.creator)
	require.Nil(t, c.after.app.GlobalState)
	c = byKey(g[10], gKey{kind: gApp, id: uint64(q)})
	require.NotEqual(t, c.before.app.ApprovalProgram, c.after.app.ApprovalProgram)
	c = byKey(g[11], gKey{kind: gLocal, addr: addrs[1], id: uint64(pA)})
	require.False(t, c.before.ok)
	require.Equal(t, local, c.after.local)
	c = byKey(g[12], gKey{kind: gLocal, addr: addrs[1], id: uint64(pA)})
	require.True(t, c.before.ok)
	require.False(t, c.after.ok)
	c = byKey(g[13], gKey{kind: gApp, id: uint64(q)})
	require.False(t, c.after.ok)
	c = byKey(g[14], acct(fresh))
	require.False(t, c.after.ok)
	require.Empty(t, g[15].Apps, "a global write is not an app params change")
	require.Empty(t, g[15].LocalStates)

	// the global write is still in the transactions
	require.NotEmpty(t, sc.Txns)
	require.Equal(t, []uint64{16}, sc.Txns[len(sc.Txns)-1].Path)

	// filtered: a group whose before comes from an unreported group, and a
	// group asked for by its second transaction
	sc2, err := l.StateChangesForRound(ctx, r, StateChangesFilter{Include: all, TxIDs: []transactions.Txid{flat[3].ID(), flat[1].ID()}})
	require.NoError(t, err)
	require.Len(t, sc2.Groups, 2)
	require.Equal(t, g[1], sc2.Groups[0])
	require.Equal(t, g[2], sc2.Groups[1])

	// only accounts
	sc3, err := l.StateChangesForRound(ctx, r, StateChangesFilter{Include: IncludeAccounts})
	require.NoError(t, err)
	for _, g := range sc3.Groups {
		require.Empty(t, g.Holdings)
		require.Empty(t, g.Assets)
		require.Empty(t, g.Apps)
		require.Empty(t, g.LocalStates)
	}
	require.Equal(t, g[1].Accounts, sc3.Groups[1].Accounts)

	// without Include: no groups, the transactions as before
	sc4, err := l.StateChangesForRound(ctx, r, StateChangesFilter{})
	require.NoError(t, err)
	require.Nil(t, sc4.Groups)
	require.Equal(t, sc.Txns, sc4.Txns)
}
