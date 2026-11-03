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
	"encoding/binary"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-codec/codec"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// simNode wraps mockNode to count simulations, and to make them slow or panic. Set those before
// simulating.
type simNode struct {
	*mockNode
	calls atomic.Int64
	// delay makes every simulation last at least this long.
	delay time.Duration
	// slow makes every simulation last until its request's context is done.
	slow bool
	// panics makes every simulation panic.
	panics bool
}

func (n *simNode) Simulate(request simulation.Request) (simulation.Result, error) {
	n.calls.Add(1)
	// A simulation stops when its request's context is done, as the simulator does.
	ctx := request.Context()
	if n.slow {
		<-ctx.Done()
	}
	select {
	case <-ctx.Done():
		return simulation.Result{}, context.Cause(ctx)
	case <-time.After(n.delay):
	}
	if n.panics {
		panic("injected panic")
	}
	return n.mockNode.Simulate(request)
}

// newSimNode makes a mock node on env's ledger whose status reports the ledger's latest round.
func newSimNode(t *testing.T, env simulationtesting.Environment, configure ...func(*config.Local)) *simNode {
	t.Helper()
	cfg := config.GetDefaultLocal()
	for _, f := range configure {
		f(&cfg)
	}
	status := cannedStatusReportGolden
	status.LastRound = env.Ledger.Latest()
	return &simNode{mockNode: makeMockNodeWithConfig(env.Ledger, t.Name(), nil, status, false, cfg)}
}

// newSimRouter serves node's REST API, as algod does.
func newSimRouter(t *testing.T, n *simNode) *echo.Echo {
	t.Helper()
	return server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000)
}

func encodeSimulateRequest(t *testing.T, request v2.PreEncodedSimulateRequest, format string) []byte {
	t.Helper()
	if format == "json" {
		return []byte(protocol.EncodeJSON(&request))
	}
	return protocol.EncodeReflect(&request)
}

// postSimulate posts body to the simulate endpoint of h, asking for a response in format.
func postSimulate(t *testing.T, h http.Handler, body []byte, format string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/transactions/simulate?format="+format, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSimulateResponse(t *testing.T, rec *httptest.ResponseRecorder, format string) v2.PreEncodedSimulateResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	handle := codec.Handle(protocol.JSONStrictHandle)
	if format == "msgpack" {
		handle = protocol.CodecHandle
	}
	var response v2.PreEncodedSimulateResponse
	require.NoError(t, codec.NewDecoderBytes(rec.Body.Bytes(), handle).Decode(&response))
	return response
}

func simulateOK(t *testing.T, h http.Handler, request v2.PreEncodedSimulateRequest, format string) v2.PreEncodedSimulateResponse {
	t.Helper()
	return decodeSimulateResponse(t, postSimulate(t, h, encodeSimulateRequest(t, request, format), format), format)
}

// roundLoggerApp logs the round of the simulated block.
const roundLoggerApp = `#pragma version 8
global Round
itob
log
int 1`

// requireRoundLogged checks that the round logger app in group logged the round after rnd.
func requireRoundLogged(t *testing.T, group v2.PreEncodedSimulateTxnGroupResult, rnd basics.Round) {
	t.Helper()
	require.Nil(t, group.FailureMessage)
	logs := group.Txns[0].Txn.Logs
	require.NotNil(t, logs)
	require.Equal(t, [][]byte{binary.BigEndian.AppendUint64(nil, uint64(rnd+1))}, *logs)
}

// roundLoggerCall returns a signed app create that logs the round, valid at every round of the test.
func roundLoggerCall(env simulationtesting.Environment, sender simulationtesting.Account, note byte) transactions.SignedTxn {
	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   roundLoggerApp,
		ClearStateProgram: "#pragma version 8\nint 1",
		Note:              []byte{note},
	})
	txn.FirstValid = 0
	txn.LastValid = 1000
	return txn.Txn().Sign(sender.Sk)
}

func oneGroupRequest(stxns ...transactions.SignedTxn) v2.PreEncodedSimulateRequest {
	return v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: stxns}}}
}

// simulateDirect calls the handler directly, with the request context ctx.
func simulateDirect(ctx context.Context, t *testing.T, handler *v2.Handlers, request v2.PreEncodedSimulateRequest) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(protocol.EncodeReflect(&request))).WithContext(ctx)
	rec := httptest.NewRecorder()
	err := handler.SimulateTransaction(echo.New().NewContext(req, rec), model.SimulateTransactionParams{})
	return rec, err
}

// TestSimulateBadRequest checks the problems that fail a whole request before anything is simulated.
func TestSimulateBadRequest(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	stxn := roundLoggerCall(env, sender, 1)

	for _, tc := range []struct {
		name      string
		configure func(*config.Local)
		request   v2.PreEncodedSimulateRequest
		err       string
	}{
		{"fix signers", nil, v2.PreEncodedSimulateRequest{FixSigners: true}, "FixSigners requires AllowEmptySignatures to be enabled"},
		{"trace without developer API", nil, v2.PreEncodedSimulateRequest{ExecTraceConfig: simulation.ExecTraceConfig{Enable: true}}, "the local configuration of the node has `EnableDeveloperAPI` turned off, while requesting for execution trace"},
		{"extra budget", nil, v2.PreEncodedSimulateRequest{ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget + 1}, "extra budget 320001 \\u003e simulation extra budget limit 320000"},
		{"extra budget over config limit", func(c *config.Local) { c.MaxAPISimulateExtraOpcodeBudget = 100 }, v2.PreEncodedSimulateRequest{ExtraOpcodeBudget: 101}, "extra budget 101 \\u003e simulation extra budget limit 100"},
		{"no groups", nil, v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{}}, "expected at least 1 transaction group, got 0"},
		{"too many groups", func(c *config.Local) { c.MaxAPISimulateGroups = 1 }, v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{stxn}}, {Txns: []transactions.SignedTxn{stxn}}}}, "too many transaction groups: got 2, the limit is 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configure []func(*config.Local)
			if tc.configure != nil {
				configure = append(configure, tc.configure)
			}
			n := newSimNode(t, env, configure...)
			request := tc.request
			if request.TxnGroups == nil {
				request.TxnGroups = []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{stxn}}}
			}
			rec := postSimulate(t, newSimRouter(t, n), encodeSimulateRequest(t, request, "json"), "json")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.err)
			require.Zero(t, n.calls.Load())
		})
	}

	// A bad format fails the request before anything is simulated.
	n := newSimNode(t, env)
	rec := postSimulate(t, newSimRouter(t, n), encodeSimulateRequest(t, oneGroupRequest(stxn), "msgpack"), "bogus")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), "failed to parse the format option")
	require.Zero(t, n.calls.Load())
}

// TestSimulateRemovedFields checks that a request setting one of the fields that POST /v2/query
// replaced gets a 400 naming them, in both formats, and that one setting them to their zero value
// is simulated.
func TestSimulateRemovedFields(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	stxn := roundLoggerCall(env, env.Accounts[0], 1)
	latest := env.Ledger.Latest()

	// The request as a map, so that it can carry fields PreEncodedSimulateRequest no longer has.
	body := func(format string, extra map[string]any) []byte {
		request := map[string]any{"txn-groups": []map[string]any{{"txns": []transactions.SignedTxn{stxn}}}}
		maps.Copy(request, extra)
		if format == "json" {
			return protocol.EncodeJSON(request)
		}
		return protocol.EncodeReflect(request)
	}
	for _, format := range []string{"json", "msgpack"} {
		for _, tc := range []struct {
			extra map[string]any
			named string
		}{
			{map[string]any{"rounds": []uint64{uint64(latest)}}, "rounds"},
			{map[string]any{"parallel-groups": true}, "parallel-groups"},
			{map[string]any{"skip-validity-window": true}, "skip-validity-window"},
			{map[string]any{"rewrite-validity-window": true}, "rewrite-validity-window"},
			{map[string]any{"protocol": "v42"}, "protocol"},
			{map[string]any{"rounds": []uint64{uint64(latest)}, "protocol": "latest"}, "rounds, protocol"},
			// values of the wrong type
			{map[string]any{"rounds": "1"}, "rounds"},
			{map[string]any{"parallel-groups": 1}, "parallel-groups"},
			{map[string]any{"skip-validity-window": "yes"}, "skip-validity-window"},
			{map[string]any{"rewrite-validity-window": map[string]any{"a": 1}}, "rewrite-validity-window"},
			{map[string]any{"protocol": 42}, "protocol"},
		} {
			n := newSimNode(t, env)
			rec := postSimulate(t, newSimRouter(t, n), body(format, tc.extra), format)
			require.Equal(t, http.StatusBadRequest, rec.Code, "%s %v: %s", format, tc.extra, rec.Body.String())
			require.Contains(t, rec.Body.String(), "simulate no longer takes "+tc.named+": use POST /v2/query", format)
			require.Zero(t, n.calls.Load())
		}

		// Zero values are no request for a removed feature.
		n := newSimNode(t, env)
		zero := map[string]any{"rounds": []uint64{}, "parallel-groups": false, "skip-validity-window": nil, "rewrite-validity-window": 0, "protocol": ""}
		response := decodeSimulateResponse(t, postSimulate(t, newSimRouter(t, n), body(format, zero), format), format)
		require.Len(t, response.TxnGroups, 1)
		require.Nil(t, response.TxnGroups[0].FailureMessage)
		require.Equal(t, int64(1), n.calls.Load())
	}
}

// TestSimulateLimiter checks how a simulate request uses the node's limiter when it is canceled or
// panics: it never leaves a slot taken.
func TestSimulateLimiter(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	// the subtests run in parallel, after this function returns
	t.Cleanup(env.Close)
	request := oneGroupRequest(roundLoggerCall(env, env.Accounts[0], 1))

	const slots = 4
	newHandler := func(n *simNode) (*v2.Handlers, *semaphore.Weighted) {
		limiter := semaphore.NewWeighted(slots)
		return &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: limiter}, limiter
	}

	t.Run("canceled request", func(t *testing.T) {
		t.Parallel()
		n := newSimNode(t, env)
		handler, limiter := newHandler(n)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec, err := simulateDirect(ctx, t, handler, request)
		require.NoError(t, err)
		require.Zero(t, rec.Body.Len())
		require.False(t, rec.Flushed)
		require.Zero(t, n.calls.Load())
		require.True(t, limiter.TryAcquire(slots))
	})

	t.Run("canceled while every slot is held", func(t *testing.T) {
		t.Parallel()
		n := newSimNode(t, env)
		handler, limiter := newHandler(n)
		require.True(t, limiter.TryAcquire(slots))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			rec, err := simulateDirect(ctx, t, handler, request)
			if err == nil && rec.Body.Len() != 0 {
				err = errors.New("unexpected response body")
			}
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("returned before the cancel: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("canceled request did not return")
		}
		require.Zero(t, n.calls.Load())
		limiter.Release(slots)
	})

	t.Run("panic", func(t *testing.T) {
		t.Parallel()
		n := newSimNode(t, env)
		n.panics = true
		handler, limiter := newHandler(n)
		require.PanicsWithValue(t, "injected panic", func() {
			_, _ = simulateDirect(context.Background(), t, handler, request)
		})
		// The slot was released.
		require.True(t, limiter.TryAcquire(slots))
		limiter.Release(slots)

		n.panics = false
		rec, err := simulateDirect(context.Background(), t, handler, request)
		require.NoError(t, err)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	})
}
