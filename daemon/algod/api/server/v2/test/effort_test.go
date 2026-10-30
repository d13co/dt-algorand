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
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

var effortItem = regexp.MustCompile(`^([a-z-]+)=(\d+)$`)

// effortOf parses the X-Algod-Effort header of rec: nil when it has none.
func effortOf(t *testing.T, rec *httptest.ResponseRecorder) map[string]uint64 {
	t.Helper()
	values := rec.Header().Values(v2.EffortHeader)
	if len(values) == 0 {
		return nil
	}
	require.Len(t, values, 1)
	out := map[string]uint64{}
	for _, item := range regexp.MustCompile(`, `).Split(values[0], -1) {
		m := effortItem.FindStringSubmatch(item)
		require.NotNil(t, m, "item %q of %q", item, values[0])
		n, err := strconv.ParseUint(m[2], 10, 64)
		require.NoError(t, err)
		out[m[1]] = n
	}
	for _, key := range []string{"opcodes", "lookups", "history", "scanned", "response-bytes", "eval-us"} {
		require.Contains(t, out, key, values[0])
	}
	return out
}

func effortOn(c *config.Local) { c.EnableEffortHeader = true }

// TestEffortHeaderSimulate checks the X-Algod-Effort header of simulate answers: on every answer
// after the work started, with the budget consumed and the ledger reads, and not on busy 503s or
// with EnableEffortHeader off.
func TestEffortHeaderSimulate(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	// the subtests run in parallel, after this function returns
	t.Cleanup(env.Close)
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	single := oneGroupRequest(roundLoggerCall(env, sender, 1))
	rounds := single
	rounds.Rounds = []basics.Round{latest - 2, latest - 1, latest}
	newHandler := func(n *roundsNode) *v2.Handlers {
		return &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: semaphore.NewWeighted(4)}
	}

	t.Run("off", func(t *testing.T) {
		t.Parallel()
		rec, err := simulateRoundsDirect(context.Background(), t, newHandler(newRoundsNode(t, env)), single)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Nil(t, effortOf(t, rec))
	})

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		for _, request := range []v2.PreEncodedSimulateRequest{single, rounds} {
			rec, err := simulateRoundsDirect(context.Background(), t, newHandler(newRoundsNode(t, env, effortOn)), request)
			require.NoError(t, err)
			response := decodeSimulateResponse(t, rec, "json")
			var consumed uint64
			groups := response.TxnGroups
			if response.RoundResults != nil {
				groups = nil
				for _, r := range *response.RoundResults {
					groups = append(groups, r.TxnGroups...)
				}
			}
			for _, g := range groups {
				for _, txn := range g.Txns {
					if txn.AppBudgetConsumed != nil {
						consumed += uint64(*txn.AppBudgetConsumed)
					}
					if txn.LogicSigBudgetConsumed != nil {
						consumed += uint64(*txn.LogicSigBudgetConsumed)
					}
				}
			}
			effort := effortOf(t, rec)
			require.NotNil(t, effort)
			require.Positive(t, consumed)
			require.Equal(t, consumed, effort["opcodes"])
			require.Positive(t, effort["lookups"])
			require.Zero(t, effort["history"])
		}
	})

	t.Run("time limit", func(t *testing.T) {
		t.Parallel()
		for _, request := range []v2.PreEncodedSimulateRequest{single, rounds} {
			n := newRoundsNode(t, env, effortOn, func(c *config.Local) { c.RestWriteTimeoutSeconds = 1 })
			n.slowAt = map[basics.Round]bool{0: true, latest - 1: true}
			rec, err := simulateRoundsDirect(context.Background(), t, newHandler(n), request)
			require.NoError(t, err)
			if request.Rounds == nil {
				require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
			} else {
				require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
				require.Positive(t, effortOf(t, rec)["opcodes"], "the rounds that finished")
			}
			require.GreaterOrEqual(t, effortOf(t, rec)["eval-us"], uint64(450_000))
		}
	})

	t.Run("busy", func(t *testing.T) {
		t.Parallel()
		for _, request := range []v2.PreEncodedSimulateRequest{single, rounds} {
			n := newRoundsNode(t, env, effortOn)
			limiter := semaphore.NewWeighted(1)
			require.True(t, limiter.TryAcquire(1))
			handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: limiter}
			rec, err := simulateRoundsDirect(context.Background(), t, handler, request)
			require.NoError(t, err)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Nil(t, effortOf(t, rec))
		}
	})
}

// TestEffortHeaderReads checks the X-Algod-Effort header of /v2/deltas and of reads at a past
// round, and its absence on reads at the latest round and on busy 503s.
func TestEffortHeaderReads(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	addr := env.Accounts[0].Addr
	env.TransferAlgos(addr, addr, 0)
	round := env.Ledger.Latest()

	handler := func(l v2.LedgerForAPI, configure ...func(*config.Local)) *v2.Handlers {
		cfg := config.GetDefaultLocal()
		cfg.EnableTimeTravel = true
		for _, f := range configure {
			f(&cfg)
		}
		n := makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg)
		return &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
	}
	serve := func(f func(echo.Context) error) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		require.NoError(t, f(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)))
		return rec
	}

	// deltas
	rec := serve(func(c echo.Context) error {
		return handler(deltaLedger{LedgerForAPI: env.Ledger}, effortOn).GetLedgerStateDelta(c, round, model.GetLedgerStateDeltaParams{})
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, uint64(rec.Body.Len()), effortOf(t, rec)["response-bytes"])
	rec = serve(func(c echo.Context) error {
		return handler(deltaLedger{LedgerForAPI: env.Ledger, busy: true}, effortOn).GetLedgerStateDelta(c, round, model.GetLedgerStateDeltaParams{})
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Nil(t, effortOf(t, rec))
	rec = serve(func(c echo.Context) error {
		return handler(deltaLedger{LedgerForAPI: env.Ledger}).GetLedgerStateDelta(c, round, model.GetLedgerStateDeltaParams{})
	})
	require.Nil(t, effortOf(t, rec), "off")

	// an account at a past round, and at the latest
	exclude := []model.AccountInformationParamsExclude{model.AccountInformationParamsExcludeAll}
	past := round - 1
	rec = serve(func(c echo.Context) error {
		return handler(env.Ledger, effortOn).AccountInformation(c, addr, model.AccountInformationParams{Round: &past, Exclude: &exclude})
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	effort := effortOf(t, rec)
	require.Equal(t, uint64(1), effort["lookups"])
	require.Zero(t, effort["history"], "the trackers still hold the round")
	require.Zero(t, effort["response-bytes"], "single objects are billed on lookups")
	require.Equal(t, strconv.FormatUint(uint64(past), 10), rec.Header().Get(v2.TTRoundHeader), "both headers")
	rec = serve(func(c echo.Context) error {
		return handler(env.Ledger, effortOn).AccountInformation(c, addr, model.AccountInformationParams{Exclude: &exclude})
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, effortOf(t, rec), "latest round")

	// an app's boxes at a past round, and at the latest
	limit := uint64(10)
	rec = serve(func(c echo.Context) error {
		return handler(env.Ledger, effortOn).GetApplicationBoxes(c, 1, model.GetApplicationBoxesParams{Round: &past, Limit: &limit})
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, uint64(1), effortOf(t, rec)["lookups"])
	require.Equal(t, strconv.FormatUint(uint64(past), 10), rec.Header().Get(v2.TTRoundHeader), "both headers")
	rec = serve(func(c echo.Context) error {
		return handler(env.Ledger, effortOn).GetApplicationBoxes(c, 1, model.GetApplicationBoxesParams{Limit: &limit})
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Nil(t, effortOf(t, rec), "latest round")
}

// TestEffortHeaderResponseBytes checks that response-bytes is the size of the answer as written,
// before compression: for simulate in both formats, with an execution trace and without, and for a
// versions endpoint. A 504 reports 0.
func TestEffortHeaderResponseBytes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	env.TransferAlgos(sender.Addr, sender.Addr, 0)

	request := oneGroupRequest(roundLoggerCall(env, sender, 1))
	traced := request
	traced.ExecTraceConfig = simulation.ExecTraceConfig{Enable: true, Stack: true}
	router := newRoundsRouter(t, newRoundsNode(t, env, effortOn, func(c *config.Local) { c.EnableDeveloperAPI = true }))
	for _, format := range []string{"json", "msgpack"} {
		var plainBytes uint64
		for _, r := range []v2.PreEncodedSimulateRequest{request, traced} {
			rec := postSimulate(t, router, encodeSimulateRequest(t, r, format), format)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Empty(t, rec.Header().Get("Content-Encoding"))
			got := effortOf(t, rec)["response-bytes"]
			require.Equal(t, uint64(rec.Body.Len()), got, format)
			if !r.ExecTraceConfig.Enable {
				plainBytes = got
				continue
			}
			require.Greater(t, got, plainBytes, "%s: the trace is part of the answer", format)
		}
	}

	// a versions endpoint
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.EnableEffortHeader = true
	vl := &versionsLedger{mockLedger: &mockLedger{latest: 1000}}
	vl.page = ledger.VersionPage{Versions: []ledger.Version{{Round: 10, Timestamp: 1, Account: &ledger.AccountVersion{Deleted: true}}}}
	h := v2.Handlers{Node: makeMockNodeWithConfig(vl, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
	for _, target := range []string{"/", "/?pretty"} {
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, target, nil), rec)
		require.NoError(t, h.AccountVersions(ctx, sender.Addr, model.AccountVersionsParams{}))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.Equal(t, uint64(rec.Body.Len()), effortOf(t, rec)["response-bytes"], target)
	}

	// 504: the error isn't the work's output
	n := newRoundsNode(t, env, effortOn, func(c *config.Local) { c.RestWriteTimeoutSeconds = 1 })
	n.slowAt = map[basics.Round]bool{0: true}
	handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: semaphore.NewWeighted(1)}
	rec, err := simulateRoundsDirect(context.Background(), t, handler, request)
	require.NoError(t, err)
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Zero(t, effortOf(t, rec)["response-bytes"])
}
