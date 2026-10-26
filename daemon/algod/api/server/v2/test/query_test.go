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
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-codec/codec"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/client"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions/logic"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// queryRoundApp logs the round it runs in and its first arg, and rejects the arg "fail".
const queryRoundApp = `#pragma version 10
global Round; itob; log
txna ApplicationArgs 0; log
txna ApplicationArgs 0; byte "fail"; !=`

func assembleQuery(t *testing.T, source string) []byte {
	t.Helper()
	ops, err := logic.AssembleString(source)
	require.NoError(t, err, ops.Errors)
	return ops.Program
}

func encodeQueryRequest(t *testing.T, request v2.PreEncodedQueryRequest, format string) []byte {
	t.Helper()
	if format == "json" {
		return []byte(protocol.EncodeJSON(&request))
	}
	return protocol.EncodeReflect(&request)
}

func postQuery(t *testing.T, h http.Handler, body []byte, format string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/query?format="+format, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeQueryResponse(t *testing.T, rec *httptest.ResponseRecorder, format string) v2.PreEncodedQueryResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	handle := codec.Handle(protocol.JSONStrictHandle)
	if format == "msgpack" {
		handle = protocol.CodecHandle
	}
	var response v2.PreEncodedQueryResponse
	require.NoError(t, codec.NewDecoderBytes(rec.Body.Bytes(), handle).Decode(&response))
	return response
}

func queryCalls(args ...string) []v2.PreEncodedQueryCall {
	calls := make([]v2.PreEncodedQueryCall, len(args))
	for i, a := range args {
		calls[i] = v2.PreEncodedQueryCall{Args: [][]byte{[]byte(a)}}
	}
	return calls
}

func roundBytes(r basics.Round) []byte {
	return binary.BigEndian.AppendUint64(nil, uint64(r))
}

// prepareQueryEnv returns a ledger a few rounds past genesis.
func prepareQueryEnv(t *testing.T) simulationtesting.Environment {
	env := simulationtesting.PrepareSimulatorTest(t)
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	return env
}

// TestQueryREST runs calls at several rounds, in both formats, with a failing call among them.
func TestQueryREST(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	h := newRoundsRouter(t, newRoundsNode(t, env))

	request := v2.PreEncodedQueryRequest{
		Program: assembleQuery(t, queryRoundApp),
		Rounds:  []basics.Round{latest - 1, 0, latest - 1},
		Calls:   queryCalls("a", "fail", "b"),
	}
	for _, format := range []string{"json", "msgpack"} {
		t.Run(format, func(t *testing.T) {
			response := decodeQueryResponse(t, postQuery(t, h, encodeQueryRequest(t, request, format), format), format)
			require.Equal(t, latest, response.LastRound)
			require.Len(t, response.Rounds, 3)
			for i, want := range []basics.Round{latest - 1, latest, latest - 1} {
				rr := response.Rounds[i]
				require.Equal(t, want, rr.Round)
				require.Nil(t, rr.Error)
				require.Nil(t, rr.Protocol)
				require.Len(t, rr.Calls, 3)
				for j, arg := range []string{"a", "fail", "b"} {
					c := rr.Calls[j]
					require.Equal(t, [][]byte{roundBytes(want + 1), []byte(arg)}, c.Logs, "round %d call %d", want, j)
					require.NotZero(t, c.Cost)
					if arg == "fail" {
						require.NotNil(t, c.Error)
						require.Contains(t, *c.Error, "rejected by ApprovalProgram")
					} else {
						require.Nil(t, c.Error)
					}
				}
			}
		})
	}

	// A JSON body as a client writes it: base64 bytes, the field names of the spec.
	body := fmt.Sprintf(`{"program":%q,"calls":[{"args":[%q]}],"budget":1000}`,
		base64.StdEncoding.EncodeToString(request.Program), base64.StdEncoding.EncodeToString([]byte("x")))
	rec := postQuery(t, h, []byte(body), "json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	// Decoded with the types generated from the spec.
	var generated model.QueryResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &generated))
	require.Equal(t, latest, generated.LastRound)
	require.Len(t, generated.Rounds, 1)
	require.Equal(t, latest, generated.Rounds[0].Round)
	require.NotNil(t, generated.Rounds[0].Calls)
	calls := *generated.Rounds[0].Calls
	require.Len(t, calls, 1)
	require.Equal(t, [][]byte{roundBytes(latest + 1), []byte("x")}, calls[0].Logs)
	require.NotZero(t, calls[0].Cost)
	require.Nil(t, calls[0].Error)
}

// TestQueryRESTRejected: requests that are invalid as a whole get a 400.
func TestQueryRESTRejected(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	program := assembleQuery(t, queryRoundApp)
	h := newRoundsRouter(t, newRoundsNode(t, env))

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"no calls", fmt.Sprintf(`{"program":%q,"calls":[]}`, base64.StdEncoding.EncodeToString(program)), "at least 1 call"},
		{"no program", `{"calls":[{}]}`, "exactly one of program and app-id"},
		{"future round", fmt.Sprintf(`{"program":%q,"calls":[{}],"rounds":[%d]}`, base64.StdEncoding.EncodeToString(program), latest+1), "after the latest round"},
		{"bad sender", fmt.Sprintf(`{"program":%q,"calls":[{}],"sender":"nope"}`, base64.StdEncoding.EncodeToString(program)), "sender"},
		{"unknown field", fmt.Sprintf(`{"program":%q,"calls":[{}],"allow-unnamed-resources":true}`, base64.StdEncoding.EncodeToString(program)), "allow-unnamed-resources"},
		{"budget over", fmt.Sprintf(`{"program":%q,"calls":[{}],"budget":320001}`, base64.StdEncoding.EncodeToString(program)), "over the limit"},
		{"bad protocol", fmt.Sprintf(`{"program":%q,"calls":[{}],"protocol":"v1"}`, base64.StdEncoding.EncodeToString(program)), "unknown protocol"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := postQuery(t, h, []byte(tc.body), "json")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.want)
		})
	}

	t.Run("disabled", func(t *testing.T) {
		h := newRoundsRouter(t, newRoundsNode(t, env, func(c *config.Local) { c.MaxAPIQueryCalls = 0 }))
		require.Nil(t, getLimits(t, h).Query)
		rec := postQuery(t, h, encodeQueryRequest(t, v2.PreEncodedQueryRequest{Program: program, Calls: queryCalls("a")}, "msgpack"), "msgpack")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "disabled")
	})

	t.Run("response too large", func(t *testing.T) {
		// A call logs 9 bytes in 2 logs, counted as 9 + 2 × 48 = 105.
		h := newRoundsRouter(t, newRoundsNode(t, env, func(c *config.Local) { c.MaxAPIQueryResponseBytes = 250 }))
		require.Equal(t, uint64(250), getLimits(t, h).Query.MaxResponseBytes)
		ok := v2.PreEncodedQueryRequest{Program: program, Calls: queryCalls("a")}
		decodeQueryResponse(t, postQuery(t, h, encodeQueryRequest(t, ok, "msgpack"), "msgpack"), "msgpack")
		tooMany := v2.PreEncodedQueryRequest{Program: program, Calls: queryCalls("a", "b", "c")}
		rec := postQuery(t, h, encodeQueryRequest(t, tooMany, "msgpack"), "msgpack")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "MaxAPIQueryResponseBytes")
	})
}

// queryDirect calls the handler as the router would, under ctx.
func queryDirect(ctx context.Context, t *testing.T, handler *v2.Handlers, request v2.PreEncodedQueryRequest) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(protocol.EncodeReflect(&request))).WithContext(ctx)
	rec := httptest.NewRecorder()
	require.NoError(t, handler.Query(echo.New().NewContext(req, rec), model.QueryParams{}))
	return rec
}

// TestQueryRESTSlots: a query is refused with 503 while every slot of the node is taken, its extra
// workers take slots too, and every slot is given back.
func TestQueryRESTSlots(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	n := newRoundsNode(t, env, func(c *config.Local) { c.QueryParallelism = 8 })
	const slots = 3
	limiter := semaphore.NewWeighted(slots)
	handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: limiter}
	request := v2.PreEncodedQueryRequest{
		Program: assembleQuery(t, queryRoundApp),
		Rounds:  []basics.Round{latest, latest - 1, latest - 2},
		Calls:   queryCalls("a", "b", "c", "d", "e", "f", "g", "h"),
	}

	require.True(t, limiter.TryAcquire(slots))
	start := time.Now()
	rec := queryDirect(context.Background(), t, handler, request)
	require.GreaterOrEqual(t, time.Since(start), 250*time.Millisecond)
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, rec.Body.String())
	require.Equal(t, "1", rec.Header().Get("Retry-After"))

	// With one slot, the request runs in it alone.
	limiter.Release(1)
	rec = queryDirect(context.Background(), t, handler, request)
	response := decodeQueryResponse(t, rec, "json")
	require.Len(t, response.Rounds, 3)

	// With all of them, its workers share them; all are given back.
	limiter.Release(slots - 1)
	rec = queryDirect(context.Background(), t, handler, request)
	require.Equal(t, response, decodeQueryResponse(t, rec, "json"))
	require.True(t, limiter.TryAcquire(slots), "every slot was given back")
}

// TestQueryRESTTimeLimit: a query that runs past the node's time limit gets a 504.
func TestQueryRESTTimeLimit(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	h := newRoundsRouter(t, newRoundsNode(t, env, func(c *config.Local) {
		c.RestWriteTimeoutSeconds = 1
		c.MaxAPIQueryCallBudget = 1 << 40
	}))
	request := v2.PreEncodedQueryRequest{Program: assembleQuery(t, "#pragma version 10\nloop: b loop"), Calls: queryCalls("a")}
	start := time.Now()
	rec := postQuery(t, h, encodeQueryRequest(t, request, "msgpack"), "msgpack")
	require.Equal(t, http.StatusGatewayTimeout, rec.Code, rec.Body.String())
	require.Less(t, time.Since(start), 5*time.Second)
}

// TestQueryRESTEffort: with EnableEffortHeader, a query reports its opcodes and lookups.
func TestQueryRESTEffort(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	h := newRoundsRouter(t, newRoundsNode(t, env, effortOn))
	program := fmt.Sprintf("#pragma version 10\naddr %s; balance; itob; log; int 1", env.Accounts[1].Addr)
	request := v2.PreEncodedQueryRequest{Program: assembleQuery(t, program), Calls: queryCalls("a", "b")}
	rec := postQuery(t, h, encodeQueryRequest(t, request, "msgpack"), "msgpack")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	effort := effortOf(t, rec)
	require.NotNil(t, effort)
	require.NotZero(t, effort["opcodes"])
	require.NotZero(t, effort["lookups"])
}

// TestQueryRESTClient queries through the REST client, as tools do.
func TestQueryRESTClient(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	srv := httptest.NewServer(newRoundsRouter(t, newRoundsNode(t, env)))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	c := client.MakeRestClient(*u, "")

	limits, err := c.SimulateLimits()
	require.NoError(t, err)
	require.NotNil(t, limits.Query)
	require.Equal(t, uint64(1024), limits.Query.MaxCalls)

	rounds := []basics.Round{latest - 1}
	args := [][]byte{[]byte("a")}
	calls := []model.QueryCall{{Args: &args}, {Args: &[][]byte{[]byte("fail")}}}
	program := assembleQuery(t, queryRoundApp)
	response, err := c.Query(model.QueryRequest{Program: &program, Rounds: &rounds, Calls: calls})
	require.NoError(t, err)
	require.Equal(t, latest, response.LastRound)
	require.Len(t, response.Rounds, 1)
	got := *response.Rounds[0].Calls
	require.Equal(t, [][]byte{roundBytes(latest), []byte("a")}, got[0].Logs)
	require.Nil(t, got[0].Error)
	require.NotNil(t, got[1].Error)

	_, err = c.Query(model.QueryRequest{Calls: calls})
	require.ErrorContains(t, err, "exactly one of program and app-id")
}
