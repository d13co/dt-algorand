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
	"bufio"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// sseEvent is an event of a stream: its name and data.
type sseEvent struct {
	name string
	data string
	at   time.Time // when it was read
}

// readSSE reads the events of r until it ends, as a client does: comments are skipped.
func readSSE(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	var e sseEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if e.name != "" || e.data != "" {
				e.at = time.Now()
				events = append(events, e)
			}
			e = sseEvent{}
		case strings.HasPrefix(line, ":"):
		case strings.HasPrefix(line, "event: "):
			e.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			e.data += strings.TrimPrefix(line, "data: ")
		default:
			t.Fatalf("unexpected line %q", line)
		}
	}
	require.NoError(t, sc.Err())
	return events
}

// postQueryStream posts a query that accepts a stream, with gzip or without.
func postQueryStream(t *testing.T, srv *httptest.Server, body []byte, gzipped bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v2/query?format=msgpack", bytes.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Accept", "text/event-stream")
	if gzipped {
		// Set by hand, so that the transport doesn't decompress it.
		req.Header.Set("Accept-Encoding", "gzip")
	}
	res, err := srv.Client().Do(req)
	require.NoError(t, err)
	return res
}

// streamBody returns the body of res, decompressed.
func streamBody(t *testing.T, res *http.Response) io.Reader {
	t.Helper()
	if res.Header.Get("Content-Encoding") != "gzip" {
		return res.Body
	}
	zr, err := gzip.NewReader(res.Body)
	require.NoError(t, err)
	return zr
}

// assembleQueryStream builds the response a query without a stream would have from the events of
// a stream, checking them as a client would, and returns it with the end event.
func assembleQueryStream(t *testing.T, events []sseEvent) (v2.PreEncodedQueryResponse, map[string]any) {
	t.Helper()
	require.NotEmpty(t, events)
	require.Equal(t, "start", events[0].name)
	var start struct {
		LastRound basics.Round   `json:"last-round"`
		Rounds    []basics.Round `json:"rounds"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[0].data), &start))

	type key struct {
		round basics.Round
		call  int
	}
	logs := map[key][][]byte{}
	calls := map[basics.Round][]v2.PreEncodedQueryCallResult{}
	rounds := map[basics.Round]v2.PreEncodedQueryRoundResult{}
	var end map[string]any
	for i, e := range events[1:] {
		require.Nil(t, end, "event after end")
		switch e.name {
		case "log":
			var l struct {
				Round basics.Round `json:"r"`
				Call  int          `json:"c"`
				Index int          `json:"i"`
				Data  [][]byte     `json:"d"`
			}
			require.NoError(t, json.Unmarshal([]byte(e.data), &l))
			k := key{l.Round, l.Call}
			require.Equal(t, len(logs[k]), l.Index, "event %d", i+1)
			logs[k] = append(logs[k], l.Data...)
		case "call":
			var c struct {
				Round basics.Round `json:"r"`
				Call  int          `json:"c"`
				Logs  int          `json:"logs"`
				Cost  uint64       `json:"cost"`
				Error *string      `json:"error"`
			}
			require.NoError(t, json.Unmarshal([]byte(e.data), &c))
			k := key{c.Round, c.Call}
			require.Len(t, logs[k], c.Logs)
			for len(calls[c.Round]) <= c.Call {
				calls[c.Round] = append(calls[c.Round], v2.PreEncodedQueryCallResult{})
			}
			got := logs[k]
			if got == nil {
				got = [][]byte{}
			}
			calls[c.Round][c.Call] = v2.PreEncodedQueryCallResult{Logs: got, Cost: c.Cost, Error: c.Error}
		case "round":
			var r v2.PreEncodedQueryRoundResult
			require.NoError(t, json.Unmarshal([]byte(strings.Replace(e.data, `"r":`, `"round":`, 1)), &r))
			_, dup := rounds[r.Round]
			require.False(t, dup)
			if r.Error == nil {
				r.Calls = calls[r.Round]
			}
			rounds[r.Round] = r
		case "end":
			require.NoError(t, json.Unmarshal([]byte(e.data), &end))
		default:
			t.Fatalf("unexpected event %q", e.name)
		}
	}
	require.NotNil(t, end, "no end event")
	response := v2.PreEncodedQueryResponse{LastRound: start.LastRound}
	if _, failed := end["error"]; !failed {
		for _, rnd := range start.Rounds {
			r, ok := rounds[rnd]
			require.True(t, ok, "round %d", rnd)
			response.Rounds = append(response.Rounds, r)
		}
	}
	return response, end
}

// TestQueryRESTStream: a query that accepts text/event-stream gets its results as events, with gzip
// and without, which add up to the response it gets without a stream.
func TestQueryRESTStream(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	srv := httptest.NewServer(newSimRouter(t, newSimNode(t, env)))
	defer srv.Close()

	request := v2.PreEncodedQueryRequest{
		Program: assembleQuery(t, queryRoundApp),
		Rounds:  []basics.Round{latest - 1, 0, latest - 1},
		Calls:   queryCalls("a", "fail", "b", "c", "d"),
	}
	body := encodeQueryRequest(t, request, "msgpack")
	want := decodeQueryResponse(t, postQuery(t, srv.Config.Handler, body, "json"), "json")

	for _, gzipped := range []bool{false, true} {
		res := postQueryStream(t, srv, body, gzipped)
		require.Equal(t, http.StatusOK, res.StatusCode)
		require.Equal(t, v2.QueryStreamType, res.Header.Get("Content-Type"))
		require.Equal(t, "no", res.Header.Get("X-Accel-Buffering"))
		require.Equal(t, int64(-1), res.ContentLength)
		require.Empty(t, res.Header.Get(v2.EffortHeader))
		if gzipped {
			require.Equal(t, "gzip", res.Header.Get("Content-Encoding"))
		}
		got, end := assembleQueryStream(t, readSSE(t, streamBody(t, res)))
		require.NoError(t, res.Body.Close())
		require.Empty(t, end)
		require.Equal(t, want, got, "gzip %v", gzipped)
	}
}

// TestQueryRESTStreamRejected: a request rejected before it runs gets the status and body it would
// get without a stream.
func TestQueryRESTStreamRejected(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	latest := env.Ledger.Latest()
	srv := httptest.NewServer(newSimRouter(t, newSimNode(t, env)))
	defer srv.Close()

	for name, request := range map[string]v2.PreEncodedQueryRequest{
		"no calls":    {Program: assembleQuery(t, queryRoundApp)},
		"later round": {Program: assembleQuery(t, queryRoundApp), Calls: queryCalls("a"), Rounds: []basics.Round{latest + 1}},
	} {
		res := postQueryStream(t, srv, encodeQueryRequest(t, request, "msgpack"), false)
		b, err := io.ReadAll(res.Body)
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		require.Equal(t, http.StatusBadRequest, res.StatusCode, "%s: %s", name, b)
		require.Contains(t, res.Header.Get("Content-Type"), "application/json", name)
		require.Contains(t, string(b), `"message"`, name)
	}
}

// TestQueryRESTStreamLive: a log is sent as the program makes it, long before the call ends, which
// here is at the node's time limit: the stream ends with its error and the status a query without a
// stream would get. With EnableEffortHeader, the effort is in the end event and the trailer, not the
// headers, which are sent before the work.
func TestQueryRESTStreamLive(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := prepareQueryEnv(t)
	defer env.Close()
	srv := httptest.NewServer(newSimRouter(t, newSimNode(t, env, effortOn, func(c *config.Local) {
		c.RestWriteTimeoutSeconds = 3 // a time limit of 2 s
		c.MaxAPIQueryCallBudget = 1 << 40
	})))
	defer srv.Close()

	request := v2.PreEncodedQueryRequest{
		Program: assembleQuery(t, "#pragma version 10\nbyte \"hello\"; log\nloop: b loop"),
		Calls:   queryCalls("a"),
	}
	start := time.Now()
	res := postQueryStream(t, srv, encodeQueryRequest(t, request, "msgpack"), false)
	require.Equal(t, http.StatusOK, res.StatusCode)
	require.Empty(t, res.Header.Get(v2.EffortHeader))
	events := readSSE(t, res.Body)
	require.NoError(t, res.Body.Close())

	require.Len(t, events, 3)
	require.Equal(t, "log", events[1].name)
	require.Contains(t, events[1].data, fmt.Sprintf("%q", "aGVsbG8=")) // "hello"
	require.Less(t, events[1].at.Sub(start), time.Second)
	require.Equal(t, "end", events[2].name)
	require.GreaterOrEqual(t, events[2].at.Sub(start), 1500*time.Millisecond)
	var end struct {
		Effort string `json:"effort"`
		Error  string `json:"error"`
		Status int    `json:"status"`
	}
	require.NoError(t, json.Unmarshal([]byte(events[2].data), &end))
	require.Equal(t, http.StatusGatewayTimeout, end.Status)
	require.Contains(t, end.Error, "RestWriteTimeoutSeconds")
	require.Contains(t, end.Effort, "opcodes=")
	require.NotContains(t, end.Effort, "opcodes=0,")
	// the trailer is set after the end event: it counts its bytes too
	opcodes, _, _ := strings.Cut(end.Effort, ",")
	require.True(t, strings.HasPrefix(res.Trailer.Get(v2.EffortHeader), opcodes+","), res.Trailer.Get(v2.EffortHeader))
}
