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
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulateTimeLimit checks that a simulate request stops at RestWriteTimeoutSeconds, and when
// its client goes away.
func TestSimulateTimeLimit(t *testing.T) {
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

	oneSecond := func(cfg *config.Local) { cfg.RestWriteTimeoutSeconds = 1 }
	newHandler := func(n *roundsNode) *v2.Handlers {
		return &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: semaphore.NewWeighted(4)}
	}

	t.Run("single round", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env, oneSecond)
		n.slowAt = map[basics.Round]bool{0: true}
		start := time.Now()
		rec, err := simulateRoundsDirect(context.Background(), t, newHandler(n), single)
		require.NoError(t, err)
		require.GreaterOrEqual(t, time.Since(start), time.Second)
		require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "time limit (RestWriteTimeoutSeconds)")
	})

	t.Run("rounds", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env, oneSecond)
		n.slowAt = map[basics.Round]bool{latest - 1: true}
		rec, err := simulateRoundsDirect(context.Background(), t, newHandler(n), rounds)
		require.NoError(t, err)
		response := decodeSimulateResponse(t, rec, "json")
		results := *response.RoundResults
		require.Len(t, results, 3)
		// The round that finished keeps its result.
		require.Nil(t, results[0].Error)
		require.Equal(t, rounds.Rounds[0], results[0].LastRound)
		require.Len(t, results[0].TxnGroups, 1)
		// The rounds run one after another: the slow one stopped at the time limit, and the one after
		// it never started.
		for _, i := range []int{1, 2} {
			require.NotNil(t, results[i].Error, i)
			require.Equal(t, "not simulated: the request reached the node's time limit (RestWriteTimeoutSeconds)", *results[i].Error)
			require.Equal(t, rounds.Rounds[i], results[i].LastRound)
		}
		require.Equal(t, int64(2), n.calls.Load())
	})

	t.Run("counted from arrival", func(t *testing.T) {
		t.Parallel()
		// A request that arrived 800 ms ago, e.g. with a slow body, has 200 ms left.
		n := newRoundsNode(t, env, oneSecond)
		n.slowAt = map[basics.Round]bool{0: true}
		req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(protocol.EncodeReflect(&single)))
		rec := httptest.NewRecorder()
		c := echo.New().NewContext(req, rec)
		start := time.Now()
		handle := v2.RecordArrival(func(c echo.Context) error {
			c.Set("dtalgorand.arrival", start.Add(-800*time.Millisecond))
			return newHandler(n).SimulateTransaction(c, model.SimulateTransactionParams{})
		})
		require.NoError(t, handle(c))
		require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
		require.Less(t, time.Since(start), 700*time.Millisecond)
	})

	t.Run("no time limit", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env, func(cfg *config.Local) { cfg.RestWriteTimeoutSeconds = 0 })
		n.delay = 1100 * time.Millisecond
		rec, err := simulateRoundsDirect(context.Background(), t, newHandler(n), single)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})

	t.Run("client gone", func(t *testing.T) {
		t.Parallel()
		for _, request := range []v2.PreEncodedSimulateRequest{single, rounds} {
			n := newRoundsNode(t, env)
			n.slowAt = map[basics.Round]bool{0: true, latest - 1: true}
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			start := time.Now()
			rec, err := simulateRoundsDirect(ctx, t, newHandler(n), request)
			require.NoError(t, err)
			require.Less(t, time.Since(start), 5*time.Second)
			require.Zero(t, rec.Body.Len())
			require.False(t, rec.Flushed)
		}
	})
}

// TestSimulateBusy checks that a simulate request that can't get a slot of the node's limiter
// within 250 ms is refused with 503 and Retry-After.
func TestSimulateBusy(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	// the subtests run in parallel, after this function returns
	t.Cleanup(env.Close)
	sender := env.Accounts[0]
	env.TransferAlgos(sender.Addr, sender.Addr, 0)
	latest := env.Ledger.Latest()
	single := oneGroupRequest(roundLoggerCall(env, sender, 1))
	rounds := single
	rounds.Rounds = []basics.Round{latest - 1, latest}

	for name, request := range map[string]v2.PreEncodedSimulateRequest{"single round": single, "rounds": rounds} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newRoundsNode(t, env)
			limiter := semaphore.NewWeighted(2)
			handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: limiter}

			// Every slot is taken.
			require.True(t, limiter.TryAcquire(2))
			start := time.Now()
			rec, err := simulateRoundsDirect(context.Background(), t, handler, request)
			require.NoError(t, err)
			require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
			require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
			require.Equal(t, "1", rec.Header().Get("Retry-After"))
			require.Contains(t, rec.Body.String(), "the node is busy")
			require.Zero(t, n.calls.Load())

			// A slot freed while the request waits is taken.
			time.AfterFunc(50*time.Millisecond, func() { limiter.Release(1) })
			rec, err = simulateRoundsDirect(context.Background(), t, handler, request)
			require.NoError(t, err)
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			require.Empty(t, rec.Header().Get("Retry-After"))

			// Every slot the request took was given back.
			limiter.Release(1)
			require.True(t, limiter.TryAcquire(2))
		})
	}
}

// TestSimulateTracedLimits checks the limits of a request with an execution trace: no rounds, and at
// most upstream's extra opcode budget whatever the node allows. Several groups and a past round,
// read from the time travel history, stay allowed.
func TestSimulateTracedLimits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(c *config.Local) {
		c.Archival = false
		c.EnableTimeTravel = true
	})
	defer env.Close()
	sender := env.Accounts[0]
	for range 10 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	// older than the MaxAcctLookback rounds a node without time travel keeps
	past := latest - 8

	n := newRoundsNode(t, env, func(c *config.Local) {
		c.EnableDeveloperAPI = true
		c.EnableTimeTravel = true
		c.MaxAPISimulateExtraOpcodeBudget = 1_000_000
	})
	router := newRoundsRouter(t, n)
	traced := v2.PreEncodedSimulateRequest{
		TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{
			{Txns: []transactions.SignedTxn{roundLoggerCall(env, sender, 1)}},
			{Txns: []transactions.SignedTxn{roundLoggerCall(env, sender, 2)}},
			{Txns: []transactions.SignedTxn{roundLoggerCall(env, sender, 3)}},
		},
		ExecTraceConfig:   simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true},
		ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget,
		Round:             past,
	}
	post := func(request v2.PreEncodedSimulateRequest) *httptest.ResponseRecorder {
		return postSimulate(t, router, encodeSimulateRequest(t, request, "json"), "json")
	}

	// allowed: several groups, upstream's extra budget, a past round
	rec := post(traced)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	response := decodeSimulateResponse(t, rec, "json")
	require.Equal(t, past, response.LastRound)
	require.Len(t, response.TxnGroups, 3)
	for g, group := range response.TxnGroups {
		requireRoundLogged(t, group, past)
		require.NotNil(t, group.Txns[0].TransactionTrace, "group %d", g)
	}

	// over upstream's extra budget: refused when traced, though the node allows it otherwise
	over := traced
	over.ExtraOpcodeBudget = simulation.MaxExtraOpcodeBudget + 1
	rec = post(over)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "320000, the limit for a request with an execution trace")
	over.ExecTraceConfig = simulation.ExecTraceConfig{}
	rec = post(over)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	// rounds
	rounds := traced
	rounds.Round = 0
	rounds.Rounds = []basics.Round{past, latest}
	calls := n.calls.Load()
	rec = post(rounds)
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "exec-trace-config cannot be used with rounds")
	require.Equal(t, calls, n.calls.Load(), "refused before any round is simulated")
}

// deltaLedger serves state deltas the way a time travel ledger rebuilding an old round does:
// busy refuses every rebuild, and slow waits until the request's context is done.
type deltaLedger struct {
	v2.LedgerForAPI
	busy, slow bool
}

func (l deltaLedger) GetStateDeltaForRoundContext(ctx context.Context, rnd basics.Round) (ledgercore.StateDelta, error) {
	switch {
	case l.busy:
		return ledgercore.StateDelta{}, ledger.ErrReevalBusy
	case l.slow:
		<-ctx.Done()
		return ledgercore.StateDelta{}, fmt.Errorf("%w: round %d: %w", ledger.ErrDeltaReevaluation, rnd, context.Cause(ctx))
	}
	return l.LedgerForAPI.GetStateDeltaForRound(rnd)
}

// TestStateDeltaLimits checks that GET /v2/deltas/{round} answers 503 with Retry-After while every
// rebuild slot is busy, 504 at the time limit, and nothing once the client is gone.
func TestStateDeltaLimits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	env.TransferAlgos(env.Accounts[0].Addr, env.Accounts[0].Addr, 0)
	round := env.Ledger.Latest()

	get := func(l deltaLedger, ctx context.Context, configure func(*config.Local)) *httptest.ResponseRecorder {
		cfg := config.GetDefaultLocal()
		if configure != nil {
			configure(&cfg)
		}
		n := makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg)
		handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
		req := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)
		rec := httptest.NewRecorder()
		require.NoError(t, handler.GetLedgerStateDelta(echo.New().NewContext(req, rec), round, model.GetLedgerStateDeltaParams{}))
		return rec
	}

	rec := get(deltaLedger{LedgerForAPI: env.Ledger}, context.Background(), nil)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	rec = get(deltaLedger{LedgerForAPI: env.Ledger, busy: true}, context.Background(), nil)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))

	rec = get(deltaLedger{LedgerForAPI: env.Ledger, slow: true}, context.Background(), func(c *config.Local) { c.RestWriteTimeoutSeconds = 1 })
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())

	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	rec = get(deltaLedger{LedgerForAPI: env.Ledger, slow: true}, ctx, nil)
	require.Zero(t, rec.Body.Len())
	require.False(t, rec.Flushed)
}
