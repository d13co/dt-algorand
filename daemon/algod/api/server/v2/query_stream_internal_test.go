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

package v2

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestQueryStreamQueueBacklog: a queue with a limit never holds more; the event that would not fit
// stops the query, and it and every later one are dropped, so what the queue holds stays whole
// events.
func TestQueryStreamQueueBacklog(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ctx, stop := context.WithCancelCause(context.Background())
	q := &queryStreamQueue{ready: make(chan struct{}, 1), max: 1024, stop: stop}
	log := strings.Repeat("x", 30) // 40 bytes of base64
	for i := range 100 {
		q.push(simulation.QueryEvent{Kind: simulation.QueryEventLog, Round: 7, Call: 1, Index: i, Log: log})
		if ctx.Err() != nil {
			break
		}
	}
	require.ErrorIs(t, context.Cause(ctx), errQueryStreamBacklog)
	require.Equal(t, http.StatusBadRequest, queryErrorStatus(context.Cause(ctx)))
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventCall, Round: 7, Call: 1})
	b := string(q.take())
	require.LessOrEqual(t, len(b), 1024)
	require.Greater(t, len(b), 900)
	require.True(t, strings.HasPrefix(b, `event: log`+"\n"+`data: {"r":7,"c":1,"i":0,"d":["eHh4`), b)
	require.True(t, strings.HasSuffix(b, `"]}`+"\n\n"), b)
	require.NotContains(t, b, "event: call")

	// without a limit, as when MaxAPIQueryResponseBytes bounds the events, nothing is dropped
	q = &queryStreamQueue{ready: make(chan struct{}, 1)}
	for i := range 100 {
		q.push(simulation.QueryEvent{Kind: simulation.QueryEventLog, Round: 7, Call: 1, Index: i, Log: log})
	}
	require.Equal(t, 100, strings.Count(string(q.take()), `"eHh4`))
}

// deadlineRecorder is a response that records the write deadlines set and the writes made.
type deadlineRecorder struct {
	*httptest.ResponseRecorder
	deadlines []time.Time
	writes    []int
}

func (r *deadlineRecorder) SetWriteDeadline(d time.Time) error {
	r.deadlines = append(r.deadlines, d)
	return nil
}

func (r *deadlineRecorder) Write(b []byte) (int, error) {
	r.writes = append(r.writes, len(b))
	return r.ResponseRecorder.Write(b)
}

// TestQueryStreamWriterDeadlines: each piece of a write, and its flush, has a write margin from
// when it starts, up to the server's own deadline. Once the query ended, what is left shares one deadline, the server's own or a
// margin from then if that is later, and the end event has a margin of its own. The end event and
// the trailer report the same effort, whose response-bytes count the whole stream.
func TestQueryStreamWriterDeadlines(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	const margin = 10 * time.Second
	rec := &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	effort := ledgercore.NewEffort()
	w := startQueryStream(echo.NewResponse(rec, echo.New()), effort, margin, time.Now().Add(time.Minute))

	start := time.Now()
	w.write(make([]byte, 2*queryStreamWritePiece+1))
	require.NoError(t, w.err)
	require.Equal(t, []int{queryStreamWritePiece, queryStreamWritePiece, 1}, rec.writes)
	require.Len(t, rec.deadlines, 4) // three pieces and the flush
	for _, d := range rec.deadlines {
		require.WithinDuration(t, start.Add(margin), d, time.Second)
	}

	// the server's deadline is later than a margin from now: it holds for all that is left
	server := time.Now().Add(time.Minute)
	w.finishBy(server)
	w.write(make([]byte, 2*queryStreamWritePiece))
	require.Equal(t, server, rec.deadlines[4])
	require.Len(t, rec.deadlines, 5)
	w.end(nil)
	require.Len(t, rec.deadlines, 7) // the end event and its flush
	require.WithinDuration(t, time.Now().Add(margin), rec.deadlines[6], time.Second)

	// the server's deadline has passed: a margin from now
	rec.deadlines = nil
	w.finishBy(time.Now().Add(-time.Second))
	require.Len(t, rec.deadlines, 1)
	require.WithinDuration(t, time.Now().Add(margin), rec.deadlines[0], time.Second)

	body := rec.Body.String()
	require.Equal(t, uint64(len(body)), w.written)
	end := body[strings.LastIndex(body, "event: end"):]
	trailer := rec.Header().Get(EffortHeader)
	require.Contains(t, end, `"effort":"`+trailer+`"`)
	require.Equal(t, uint64(len(body)), effort.Counts().ResponseBytes)

	// while the query runs, no deadline passes the server's own, however many pieces are written
	rec = &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	limit := time.Now().Add(margin / 2)
	w = startQueryStream(echo.NewResponse(rec, echo.New()), nil, margin, limit)
	w.write(make([]byte, 3*queryStreamWritePiece))
	require.Len(t, rec.deadlines, 4)
	for _, d := range rec.deadlines {
		require.Equal(t, limit, d)
	}
	w.end(nil) // the end event has its margin all the same
	require.WithinDuration(t, time.Now().Add(margin), rec.deadlines[len(rec.deadlines)-1], time.Second)

	// no write deadline on the node: none is set
	rec = &deadlineRecorder{ResponseRecorder: httptest.NewRecorder()}
	w = startQueryStream(echo.NewResponse(rec, echo.New()), nil, 0, time.Time{})
	w.write([]byte("x"))
	w.finishBy(time.Now())
	w.end(nil)
	require.Empty(t, rec.deadlines)
}

// TestQueryStreamEventsMatchSpec: the data of each event the handler encodes is that of its
// definition in the API spec (QueryStreamStart and the others), with no field the spec lacks.
func TestQueryStreamEventsMatchSpec(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	q := &queryStreamQueue{ready: make(chan struct{}, 1)}
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventStart, LastRound: 9, Rounds: []basics.Round{9, 7}})
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventLog, Round: 7, Call: 1, Index: 2, Log: "a"})
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventLog, Round: 7, Call: 1, Index: 3, Log: ""})
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventCall, Round: 7, Call: 1, Logs: 4, Cost: 5, Error: "failed"})
	q.push(simulation.QueryEvent{Kind: simulation.QueryEventRound, Round: 7, Error: "unreadable",
		Protocol: &simulation.ProtocolOverride{Applied: true}})
	stream := appendSSE(q.take(), "end", queryStreamEnd{Effort: "opcodes=1", Error: "late", Status: http.StatusGatewayTimeout})

	var got []any
	for event := range strings.SplitSeq(strings.TrimSuffix(string(stream), "\n\n"), "\n\n") {
		name, data, ok := strings.Cut(strings.TrimPrefix(event, "event: "), "\ndata: ")
		require.True(t, ok, event)
		var v any
		switch name {
		case "start":
			v = &model.QueryStreamStart{}
		case "log":
			v = &model.QueryStreamLog{}
		case "call":
			v = &model.QueryStreamCall{}
		case "round":
			v = &model.QueryStreamRound{}
		case "end":
			v = &model.QueryStreamEnd{}
		default:
			require.Fail(t, "unknown event", name)
		}
		dec := json.NewDecoder(strings.NewReader(data))
		dec.DisallowUnknownFields()
		require.NoError(t, dec.Decode(v), event)
		got = append(got, v)
	}
	str := func(s string) *string { return &s }
	status := http.StatusGatewayTimeout
	require.Len(t, got, 5)
	require.Equal(t, &model.QueryStreamStart{LastRound: 9, Rounds: []basics.Round{9, 7}}, got[0])
	require.Equal(t, &model.QueryStreamLog{R: 7, C: 1, I: 2, D: [][]byte{[]byte("a"), {}}}, got[1])
	require.Equal(t, &model.QueryStreamCall{R: 7, C: 1, Logs: 4, Cost: 5, Error: str("failed")}, got[2])
	round := got[3].(*model.QueryStreamRound)
	require.EqualValues(t, 7, round.R)
	require.Equal(t, str("unreadable"), round.Error)
	require.NotNil(t, round.Protocol)
	require.Equal(t, &model.QueryStreamEnd{Effort: str("opcodes=1"), Error: str("late"), Status: &status}, got[4])
}
