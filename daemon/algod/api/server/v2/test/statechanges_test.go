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

package test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/server"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// stateChangesLedger answers StateChangesForRound with sc or err, and records the filter. slow
// waits until the request's context is done.
type stateChangesLedger struct {
	*mockLedger
	sc   ledger.StateChanges
	err  error
	slow bool
	got  *ledger.StateChangesFilter
	rnd  *basics.Round
}

func (l stateChangesLedger) StateChangesForRound(ctx context.Context, rnd basics.Round, f ledger.StateChangesFilter) (ledger.StateChanges, error) {
	*l.got, *l.rnd = f, rnd
	if l.slow {
		<-ctx.Done()
		return ledger.StateChanges{}, fmt.Errorf("%w: round %d: %w", ledger.ErrDeltaReevaluation, rnd, context.Cause(ctx))
	}
	return l.sc, l.err
}

// TestGetTxnStateChangesForRound checks the handler of GET /v2/deltas/{round}/txn/state-changes:
// the answer in JSON and msgpack, the filters it passes on, and its errors.
func TestGetTxnStateChangesForRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	acct := ledgertesting.RandomAddress()
	txid := transactions.Txid{7}
	sc := ledger.StateChanges{Round: 9, Txns: []ledger.TxnStateChanges{
		{Path: []uint64{3}, TxID: txid, Changes: []ledger.StateChange{
			{App: 5, Type: logic.GlobalState, Key: "g", Before: &basics.TealValue{Type: basics.TealUintType, Uint: 1}, After: &basics.TealValue{Type: basics.TealUintType, Uint: 2}},
			{App: 5, Type: logic.LocalState, Account: acct, Key: "l", Before: &basics.TealValue{Type: basics.TealBytesType, Bytes: "x"}},
			{App: 6, Type: logic.BoxState, Key: "b", After: &basics.TealValue{Type: basics.TealBytesType, Bytes: "box"}},
		}},
		{Path: []uint64{3, 0}, Changes: []ledger.StateChange{
			{App: 6, Type: logic.BoxState, Key: "b", After: &basics.TealValue{Type: basics.TealBytesType, Bytes: "box"}},
		}},
	}}

	type result struct {
		rec *httptest.ResponseRecorder
		f   ledger.StateChangesFilter
		rnd basics.Round
	}
	get := func(l stateChangesLedger, params model.GetTxnStateChangesForRoundParams, configure ...func(*config.Local)) result {
		cfg := config.GetDefaultLocal()
		cfg.EnableTimeTravel = true
		for _, c := range configure {
			c(&cfg)
		}
		var res result
		l.mockLedger = &mockLedger{latest: 10}
		l.got, l.rnd = &res.f, &res.rnd
		h := v2.Handlers{Node: makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
		res.rec = httptest.NewRecorder()
		require.NoError(t, h.GetTxnStateChangesForRound(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), res.rec), 9, params))
		return res
	}

	res := get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{})
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.Equal(t, basics.Round(9), res.rnd)
	require.Equal(t, ledger.StateChangesFilter{MaxBytes: 16 << 20}, res.f)
	require.JSONEq(t, fmt.Sprintf(`{"round": 9, "txns": [
		{"path": [3], "txid": %q, "changes": [
			{"app-id": 5, "app-state-type": "g", "key": "Zw==", "before": {"type": 2, "uint": 1, "bytes": ""}, "after": {"type": 2, "uint": 2, "bytes": ""}},
			{"app-id": 5, "app-state-type": "l", "account": %q, "key": "bA==", "before": {"type": 1, "uint": 0, "bytes": "eA=="}},
			{"app-id": 6, "app-state-type": "b", "key": "Yg==", "after": {"type": 1, "uint": 0, "bytes": "Ym94"}}]},
		{"path": [3, 0], "changes": [
			{"app-id": 6, "app-state-type": "b", "key": "Yg==", "after": {"type": 1, "uint": 0, "bytes": "Ym94"}}]}
	]}`, txid.String(), acct.String()), res.rec.Body.String())

	msgpack := model.GetTxnStateChangesForRoundParamsFormat("msgpack")
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Format: &msgpack})
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.Equal(t, "application/msgpack", res.rec.Header().Get(echo.HeaderContentType))
	var decoded model.TxnStateChangesResponse
	require.NoError(t, protocol.DecodeReflect(res.rec.Body.Bytes(), &decoded))
	require.Equal(t, basics.Round(9), decoded.Round)
	require.Len(t, decoded.Txns, 2)
	require.Equal(t, []byte("l"), decoded.Txns[0].Changes[1].Key)
	// a missing value is a missing field, not a nil one
	var raw map[string]any
	require.NoError(t, protocol.DecodeReflect(res.rec.Body.Bytes(), &raw))
	fields := func(v any) map[string]any { // nested maps decode with interface keys
		out := map[string]any{}
		switch m := v.(type) {
		case map[string]any:
			return m
		case map[any]any:
			for k, x := range m {
				out[fmt.Sprint(k)] = x
			}
		default:
			t.Fatalf("not a map: %T", v)
		}
		return out
	}
	local := fields(fields(raw["txns"].([]any)[0])["changes"].([]any)[1])
	require.Contains(t, local, "before")
	require.Equal(t, []byte("x"), fields(local["before"])["bytes"], "bytes are bin, not base64 text")
	require.NotContains(t, local, "after")
	require.Contains(t, local, "account")
	box := fields(fields(raw["txns"].([]any)[1])["changes"].([]any)[0])
	require.NotContains(t, box, "before")
	require.NotContains(t, box, "account")
	require.NotContains(t, fields(raw["txns"].([]any)[1]), "txid")

	require.NotContains(t, raw, "groups")
	require.NotContains(t, raw, "fee-sink")

	// no changes: an empty list, not null
	res = get(stateChangesLedger{sc: ledger.StateChanges{Round: 9}}, model.GetTxnStateChangesForRoundParams{})
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.JSONEq(t, `{"round": 9, "txns": []}`, res.rec.Body.String())

	// filters
	app := basics.AppIndex(5)
	txidStr := txid.String()
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{App: &app, Txid: &txidStr},
		func(c *config.Local) { c.MaxAPIStateChangesResponseBytes = 0 })
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.Equal(t, ledger.StateChangesFilter{App: 5, TxIDs: []transactions.Txid{txid}}, res.f)
	// txid and txids combine; repeats are the ledger's to drop
	other := transactions.Txid{8}
	txids := []string{other.String(), txidStr}
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Txid: &txidStr, Txids: &txids})
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.Equal(t, []transactions.Txid{txid, other, txid}, res.f.TxIDs)
	bad := "not-a-txid"
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Txid: &bad})
	require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())
	badList := []string{txidStr, bad}
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Txids: &badList})
	require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())
	require.Contains(t, res.rec.Body.String(), bad, "names the bad txid")
	for _, empty := range [][]string{{""}, {}} { // ?txids= : not every transaction
		res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Txids: &empty})
		require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())
	}
	badFormat := model.GetTxnStateChangesForRoundParamsFormat("xml")
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Format: &badFormat})
	require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())

	// include
	inc := []model.GetTxnStateChangesForRoundParamsInclude{"apps", "accounts", "apps"}
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Include: &inc})
	require.Equal(t, http.StatusOK, res.rec.Code, res.rec.Body.String())
	require.Equal(t, ledger.IncludeAccounts|ledger.IncludeApps, res.f.Include)
	for _, bad := range [][]model.GetTxnStateChangesForRoundParamsInclude{{}, {""}, {"accounts", "boxes"}} {
		res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{Include: &bad})
		require.Equal(t, http.StatusBadRequest, res.rec.Code, "%v: %s", bad, res.rec.Body.String())
	}

	// the size cap: the ledger's estimate, and the encoded answer
	res = get(stateChangesLedger{err: ledger.ErrStateChangesTooLarge}, model.GetTxnStateChangesForRoundParams{})
	require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())
	require.Contains(t, res.rec.Body.String(), "app, txid or txids")
	res = get(stateChangesLedger{sc: sc}, model.GetTxnStateChangesForRoundParams{}, func(c *config.Local) { c.MaxAPIStateChangesResponseBytes = 100 })
	require.Equal(t, http.StatusBadRequest, res.rec.Code, res.rec.Body.String())
	require.Contains(t, res.rec.Body.String(), "app, txid or txids")

	// errors
	for _, tc := range []struct {
		err  error
		code int
	}{
		{errors.New("round 9 is older than the in-memory deltas and its block is not stored"), http.StatusNotFound},
		{errors.New("round 0 has no block to evaluate"), http.StatusNotFound},
		{fmt.Errorf("%w: %s", ledger.ErrTxnNotInRound, txid), http.StatusNotFound},
		{fmt.Errorf("%w: round 9: the result differs from the stored changes", ledger.ErrDeltaReevaluation), http.StatusInternalServerError},
		{ledger.ErrReevalBusy, http.StatusServiceUnavailable},
	} {
		res = get(stateChangesLedger{err: tc.err}, model.GetTxnStateChangesForRoundParams{})
		require.Equal(t, tc.code, res.rec.Code, "%v: %s", tc.err, res.rec.Body.String())
		if tc.code == http.StatusServiceUnavailable {
			require.Equal(t, "1", res.rec.Header().Get("Retry-After"))
		}
	}
	res = get(stateChangesLedger{slow: true}, model.GetTxnStateChangesForRoundParams{}, func(c *config.Local) { c.RestWriteTimeoutSeconds = 1 })
	require.Equal(t, http.StatusGatewayTimeout, res.rec.Code, res.rec.Body.String())

	// a ledger without time travel
	h := v2.Handlers{Node: makeMockNodeWithConfig(&mockLedger{latest: 10}, t.Name(), nil, cannedStatusReportGolden, false, config.GetDefaultLocal()), Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
	rec := httptest.NewRecorder()
	require.NoError(t, h.GetTxnStateChangesForRound(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec), 9, model.GetTxnStateChangesForRoundParams{}))
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
}

// TestTxnStateChangesRoute checks that the route is served only with EnableTimeTravel, which
// GET /v2/capabilities reports with state-changes.
func TestTxnStateChangesRoute(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var f ledger.StateChangesFilter
	var rnd basics.Round
	l := stateChangesLedger{mockLedger: &mockLedger{latest: 10}, sc: ledger.StateChanges{Round: 4}, got: &f, rnd: &rnd}
	router := func(timeTravel bool) http.Handler {
		cfg := config.GetDefaultLocal()
		cfg.EnableTimeTravel = timeTravel
		n := makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg)
		return server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000)
	}
	a, b := transactions.Txid{1}, transactions.Txid{2}
	query := "app=12&txids=" + b.String() + "," + a.String()
	get := func(h http.Handler) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/deltas/4/txn/state-changes?"+query, nil))
		return rec
	}

	h := router(true)
	rec := get(h)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, basics.Round(4), rnd)
	require.Equal(t, basics.AppIndex(12), f.App)
	require.Equal(t, []transactions.Txid{b, a}, f.TxIDs, "a comma-separated list")
	// lists the binder hands over that must not reach the ledger as every transaction
	for _, q := range []string{"txids=", "txids=" + a.String() + ",", "txids=" + a.String() + "&txids=" + b.String(), "txids=%20" + a.String()} {
		f, query = ledger.StateChangesFilter{App: 99}, q
		rec = get(h)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", q, rec.Body.String())
		require.Equal(t, basics.AppIndex(99), f.App, "%s reached the ledger", q)
	}
	query = "include=assets,accounts"
	rec = get(h)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, ledger.IncludeAccounts|ledger.IncludeAssets, f.Include)
	for _, q := range []string{"include=", "include=accounts,", "include=accounts&include=apps", "include=boxes"} {
		f, query = ledger.StateChangesFilter{App: 99}, q
		rec = get(h)
		require.Equal(t, http.StatusBadRequest, rec.Code, "%s: %s", q, rec.Body.String())
		require.Equal(t, basics.AppIndex(99), f.App, "%s reached the ledger", q)
	}
	query = "app=12"
	var caps map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(capabilitiesJSON(t, h), &caps))
	require.JSONEq(t, `{"max-response-bytes": 16777216, "include": ["accounts", "assets", "apps"]}`, string(caps["state-changes"]))

	h = router(false)
	rec = get(h)
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.False(t, strings.Contains(string(capabilitiesJSON(t, h)), "state-changes"))
}

// TestGetTxnStateChangesGroups checks how the groups of GET /v2/deltas/{round}/txn/state-changes
// with include are encoded: absent values, empty lists and zero nested structs left out (as in
// every answer of the codec), bytes as bin in msgpack, and
// minimum balances from the round's consensus parameters.
func TestGetTxnStateChangesGroups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	a, b, sink := ledgertesting.RandomAddress(), ledgertesting.RandomAddress(), ledgertesting.RandomAddress()
	gid := crypto.Digest{9}
	before := ledgercore.AccountData{AccountBaseData: ledgercore.AccountBaseData{
		MicroAlgos: basics.MicroAlgos{Raw: 5_000_000}, TotalAssets: 1}}
	after := before
	after.MicroAlgos.Raw = 4_000_000
	after.AuthAddr = b
	sc := ledger.StateChanges{Round: 9, FeeSink: sink, Proto: protocol.ConsensusCurrentVersion, Groups: []ledger.GroupStateChanges{
		{First: 0, Count: 2, GroupID: gid, TxIDs: []transactions.Txid{{1}, {2}},
			Accounts: []ledger.AccountChange{{Addr: a, Before: &before, After: &after}, {Addr: b, After: &before}},
			Holdings: []ledger.HoldingChange{{Addr: a, Asset: 7, Before: &basics.AssetHolding{Amount: 3}}},
			Apps: []ledger.AppParamsChange{{App: 11, Creator: a, After: &basics.AppParams{
				ApprovalProgram: []byte{1}, ClearStateProgram: []byte{2}, GlobalState: basics.TealKeyValue{"k": {Type: basics.TealUintType, Uint: 1}}}}},
			LocalStates: []ledger.LocalStateChange{{Addr: a, App: 11, After: &basics.StateSchema{NumUint: 1}},
				{Addr: b, App: 12, Before: &basics.StateSchema{}}}, // an opt-out of an app with an empty local schema
		},
		{First: 2, Count: 1, TxIDs: []transactions.Txid{{3}},
			Assets: []ledger.AssetParamsChange{{Asset: 8, Creator: a, Before: &basics.AssetParams{Total: 5, UnitName: "U"}}}},
	}}
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	l := stateChangesLedger{mockLedger: &mockLedger{latest: 10}, sc: sc, got: &ledger.StateChangesFilter{}, rnd: new(basics.Round)}
	h := v2.Handlers{Node: makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
	get := func(format string) []byte {
		rec := httptest.NewRecorder()
		f := model.GetTxnStateChangesForRoundParamsFormat(format)
		inc := []model.GetTxnStateChangesForRoundParamsInclude{"accounts", "assets", "apps"}
		require.NoError(t, h.GetTxnStateChangesForRound(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec), 9,
			model.GetTxnStateChangesForRoundParams{Format: &f, Include: &inc}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.Bytes()
	}

	proto := config.Consensus[protocol.ConsensusCurrentVersion]
	minBal := func(ad ledgercore.AccountData) uint64 { return ad.MinBalance(&proto).Raw }
	b64 := func(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
	require.JSONEq(t, fmt.Sprintf(`{"round": 9, "txns": [], "fee-sink": %q, "groups": [
		{"first": 0, "count": 2, "group-id": %q, "txids": [%q, %q],
		 "accounts": [
			{"address": %q,
			 "before": {"amount": 5000000, "min-balance": %d, "status": "Offline",
			            "total-apps-opted-in": 0, "total-assets-opted-in": 1, "total-created-apps": 0, "total-created-assets": 0},
			 "after": {"amount": 4000000, "min-balance": %d, "status": "Offline", "auth-addr": %q,
			           "total-apps-opted-in": 0, "total-assets-opted-in": 1, "total-created-apps": 0, "total-created-assets": 0}},
			{"address": %q,
			 "after": {"amount": 5000000, "min-balance": %d, "status": "Offline",
			           "total-apps-opted-in": 0, "total-assets-opted-in": 1, "total-created-apps": 0, "total-created-assets": 0}}],
		 "holdings": [{"address": %q, "asset-id": 7, "before": {"asset-id": 7, "amount": 3, "is-frozen": false}}],
		 "apps": [{"app-id": 11, "after": {"creator": %q, "approval-program": "AQ==", "clear-state-program": "Ag=="}}],
		 "local-states": [{"address": %q, "app-id": 11, "after": {"id": 11, "schema": {"num-uint": 1, "num-byte-slice": 0}}},
			{"address": %q, "app-id": 12, "before": {"id": 12, "schema": {"num-uint": 0, "num-byte-slice": 0}}}]},
		{"first": 2, "count": 1, "txids": [%q],
		 "assets": [{"asset-id": 8, "before": {"creator": %q, "total": 5, "decimals": 0,
			"unit-name": "U", "unit-name-b64": "VQ=="}}]}
	]}`, sink.String(), b64(gid[:]), transactions.Txid{1}.String(), transactions.Txid{2}.String(),
		a.String(), minBal(before), minBal(after), b.String(), b.String(), minBal(before),
		a.String(), a.String(), a.String(), b.String(), transactions.Txid{3}.String(), a.String()), string(get("json")))

	var raw map[string]any
	require.NoError(t, protocol.DecodeReflect(get("msgpack"), &raw))
	require.Equal(t, []byte(sink.String()), raw["fee-sink"]) // strings decode as bytes here
	g0 := raw["groups"].([]any)[0].(map[any]any)
	require.Equal(t, gid[:], g0["group-id"], "bin")
	app := g0["apps"].([]any)[0].(map[any]any)["after"].(map[any]any)
	require.Equal(t, []byte{1}, app["approval-program"])
	require.NotContains(t, app, "global-state", "global state changes are per transaction")
	var decoded model.TxnStateChangesResponse
	require.NoError(t, protocol.DecodeReflect(get("msgpack"), &decoded))
	require.Len(t, *decoded.Groups, 2)
	require.Nil(t, (*decoded.Groups)[1].Accounts)
	optOut := (*(*decoded.Groups)[0].LocalStates)[1]
	require.NotNil(t, optOut.Before, "an empty schema is still there")
	require.Nil(t, optOut.After)

	// include without groups (an empty block): the codec leaves out the empty list, but the fee
	// sink is there, which tells the answer from one of a node that ignored include
	l.sc = ledger.StateChanges{Round: 9, FeeSink: sink}
	h.Node = makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg)
	require.JSONEq(t, fmt.Sprintf(`{"round": 9, "txns": [], "fee-sink": %q}`, sink.String()), string(get("json")))
}
