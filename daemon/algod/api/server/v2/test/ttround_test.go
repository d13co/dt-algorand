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
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// ttRoundHandlers returns handlers over lgr, with time travel on or off.
func ttRoundHandlers(t *testing.T, lgr v2.LedgerForAPI, timeTravel bool) *v2.Handlers {
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = timeTravel
	return &v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
}

// TestTTRoundHeader checks that every read at a round is marked with the
// round, 404s included, and that reads without round are not.
func TestTTRoundHeader(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	ml := &mockLedger{
		accounts: map[basics.Address]basics.AccountData{addr: {
			AssetParams:    map[basics.AssetIndex]basics.AssetParams{1: {Total: 1}},
			Assets:         map[basics.AssetIndex]basics.AssetHolding{1: {Amount: 1}},
			AppParams:      map[basics.AppIndex]basics.AppParams{1: {}},
			AppLocalStates: map[basics.AppIndex]basics.AppLocalState{1: {}},
		}},
		kvstore: map[string][]byte{apps.MakeBoxKey(1, "a"): []byte("v")},
		latest:  10,
	}
	h := ttRoundHandlers(t, pastLedger{historyLedger: historyLedger{ml}, at: 7, creator: addr}, true)
	a := addr.String()
	for _, path := range []string{
		"/v2/accounts/" + a + "?",
		"/v2/accounts/" + a + "?exclude=all&",
		"/v2/accounts/" + a + "?format=msgpack&",
		"/v2/accounts/" + a + "/assets?",
		"/v2/accounts/" + a + "/assets/1?",
		"/v2/accounts/" + a + "/applications?",
		"/v2/accounts/" + a + "/applications/1?",
		"/v2/assets/1?",
		"/v2/applications/1?",
		"/v2/applications/1/box?name=str:a&",
		"/v2/applications/1/boxes?limit=10&",
		"/v2/ledger/supply?",
	} {
		rec := serve(h, path+"round=7")
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", path, rec.Body.String())
		require.Equal(t, []string{"7"}, rec.Header().Values(v2.TTRoundHeader), path)

		// The latest round: 200 or 404 (the mock holds the asset, app and box
		// only at 7), never marked.
		rec = serve(h, path)
		require.Contains(t, []int{http.StatusOK, http.StatusNotFound}, rec.Code, "%s: %s", path, rec.Body.String())
		require.Empty(t, rec.Header().Values(v2.TTRoundHeader), path)
	}

	// Not found at round 6: the asset, the app and the box exist only at 7.
	for _, path := range []string{
		"/v2/accounts/" + a + "/assets/1?",
		"/v2/accounts/" + a + "/applications/1?",
		"/v2/assets/1?",
		"/v2/applications/1?",
		"/v2/applications/1/box?name=str:a&",
	} {
		rec := serve(h, path+"round=6")
		require.Equal(t, http.StatusNotFound, rec.Code, "%s: %s", path, rec.Body.String())
		require.Equal(t, "6", rec.Header().Get(v2.TTRoundHeader), path)
	}

	// The supply's 404 at a round: the history has no online stake for it.
	sl := supplyLedger{historyLedger: historyLedger{ml}, err: fmt.Errorf("%w: balance round 0, vote round 7", ledger.ErrOnlineStakeUnavailable)}
	rec := serve(ttRoundHandlers(t, sl, true), "/v2/ledger/supply?round=7")
	require.Equal(t, http.StatusNotFound, rec.Code, rec.Body.String())
	require.Equal(t, "7", rec.Header().Get(v2.TTRoundHeader))
}

// TestTTRoundHeaderRefused checks that an answer refusing the round is not
// marked with it.
func TestTTRoundHeaderRefused(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress().String()
	ml := &mockLedger{latest: 10}
	for name, tc := range map[string]struct {
		h    *v2.Handlers
		path string
		code int
	}{
		"future round":       {ttRoundHandlers(t, historyLedger{ml}, true), "/v2/accounts/" + addr + "?round=11", http.StatusBadRequest},
		"future box page":    {ttRoundHandlers(t, historyLedger{ml}, true), "/v2/applications/1/boxes?round=11", http.StatusBadRequest},
		"time travel off":    {ttRoundHandlers(t, historyLedger{ml}, false), "/v2/assets/1?round=7", http.StatusBadRequest},
		"no history":         {ttRoundHandlers(t, ml, true), "/v2/applications/1?round=7", http.StatusBadRequest},
		"untrusted history":  {ttRoundHandlers(t, untrustedLedger{historyLedger{ml}}, true), "/v2/accounts/" + addr + "?round=7", http.StatusServiceUnavailable},
		"untrusted, exclude": {ttRoundHandlers(t, untrustedLedger{historyLedger{ml}}, true), "/v2/accounts/" + addr + "?exclude=all&round=7", http.StatusServiceUnavailable},
		"future box":         {ttRoundHandlers(t, historyLedger{ml}, true), "/v2/applications/1/box?name=str:a&round=11", http.StatusBadRequest},
		// Malformed requests are refused before the round is looked at.
		"bad box name": {ttRoundHandlers(t, historyLedger{ml}, true), "/v2/applications/1/box?name=nope&round=7", http.StatusBadRequest},
		"bad box next": {ttRoundHandlers(t, historyLedger{ml}, true), "/v2/applications/1/boxes?round=7&next=nope", http.StatusBadRequest},
	} {
		rec := serve(tc.h, tc.path)
		require.Equal(t, tc.code, rec.Code, "%s: %s", name, rec.Body.String())
		require.Empty(t, rec.Header().Values(v2.TTRoundHeader), name)
	}
}

// TestTTRoundHeaderBoxes checks the box pages of a node without time travel,
// which answer the rounds the ledger still holds in memory.
func TestTTRoundHeaderBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)

	ml := &mockLedger{kvstore: map[string][]byte{apps.MakeBoxKey(1, "a"): []byte("v")}, latest: 10, minRound: 5}
	h := ttRoundHandlers(t, ml, false)

	rec := serve(h, "/v2/applications/1/boxes?round=6")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "6", rec.Header().Get(v2.TTRoundHeader))

	rec = serve(h, "/v2/applications/1/boxes?round=4")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "no longer available")
	require.Empty(t, rec.Header().Values(v2.TTRoundHeader))
}

// TestTTRoundHeaderHolders checks the asset holders: marked on the page, the
// 404 and the 500 at a round; not on a refusal of the round, a 503 from the
// index, or without round.
func TestTTRoundHeaderHolders(t *testing.T) {
	partitiontest.PartitionTest(t)

	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel, cfg.TimeTravelHoldersIndex = true, true
	hl := &holdersLedger{mockLedger: &mockLedger{latest: 1000}}
	get := func(cfg config.Local, params model.GetAssetHoldersParams) *httptest.ResponseRecorder {
		h := v2.Handlers{Node: makeMockNodeWithConfig(hl, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, h.GetAssetHolders(ctx, 7, params))
		return rec
	}
	rnd := func(r basics.Round) *basics.Round { return &r }
	at40 := model.GetAssetHoldersParams{Round: rnd(40)}

	rec := get(cfg, at40)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, "40", rec.Header().Get(v2.TTRoundHeader))
	rec = get(cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Values(v2.TTRoundHeader))

	// Refusals of the round. A future round is refused even with a bad limit.
	off := cfg
	off.TimeTravelHoldersIndex = false
	limit0 := uint64(0)
	for name, tc := range map[string]struct {
		cfg    config.Local
		params model.GetAssetHoldersParams
		msg    string
	}{
		"future round": {cfg, model.GetAssetHoldersParams{Round: rnd(1001), Limit: &limit0}, "greater than the latest"},
		"index off":    {off, at40, "TimeTravelHoldersIndex"},
	} {
		rec = get(tc.cfg, tc.params)
		require.Equal(t, http.StatusBadRequest, rec.Code, name)
		require.Contains(t, rec.Body.String(), tc.msg, name)
		require.Empty(t, rec.Header().Values(v2.TTRoundHeader), name)
	}

	for err, code := range map[error]int{
		fmt.Errorf("%w: asset 7 at round 40", ledger.ErrAssetNotFound): http.StatusNotFound,
		errors.New("disk on fire"):                                     http.StatusInternalServerError,
	} {
		hl.err = err
		rec = get(cfg, at40)
		require.Equal(t, code, rec.Code, err)
		require.Equal(t, "40", rec.Header().Get(v2.TTRoundHeader), err)
	}

	for _, err := range []error{
		fmt.Errorf("page: %w", ledger.ErrNoHoldersIndex),
		fmt.Errorf("%w: round 5 has a different header", ledger.ErrHistoryUntrusted),
	} {
		hl.err = err
		rec = get(cfg, at40)
		require.Equal(t, http.StatusServiceUnavailable, rec.Code, err)
		require.Empty(t, rec.Header().Values(v2.TTRoundHeader), err)
	}
}
