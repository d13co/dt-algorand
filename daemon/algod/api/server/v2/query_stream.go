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

// POST /v2/query answered as a stream of Server-Sent Events, for a request that accepts
// text/event-stream: each log is sent as the called app makes it. Non-standard fork feature, kept
// in its own file to keep upstream merges of handlers.go clean.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
)

// QueryStreamType is the media type of a streamed POST /v2/query answer.
const QueryStreamType = "text/event-stream"

// queryStreamFlushInterval is the least time between two writes of a stream after the first, so
// that events that come close together go out together: each write is a flush, and with gzip the
// end of a compressed block.
const queryStreamFlushInterval = 20 * time.Millisecond

// queryStreamKeepalive is how long a stream may go without a write before it sends a comment, so
// that proxies with an idle timeout keep it open while a call runs. It is checked every third of
// that, so a stream goes at most 4/3 of it without a write.
const queryStreamKeepalive = 15 * time.Second

// queryStreamBacklogMax is the most bytes of events a stream may hold for its client on a node
// without MaxAPIQueryResponseBytes, which otherwise bounds them: a stream whose client falls that
// far behind ends.
const queryStreamBacklogMax = 32 << 20

// queryStreamWritePiece is the most bytes a stream writes under one write deadline: a client must
// take that much in each write margin of the node (requestWriteMargin), or it is gone.
const queryStreamWritePiece = 64 << 10

// errQueryStreamBacklog ends a stream whose client reads too slowly.
var errQueryStreamBacklog = fmt.Errorf("the stream's client is more than %d bytes behind: read faster, or query fewer calls or rounds", queryStreamBacklogMax)

// errQueryStreamClientGone stops the query of a stream that can't be written any more.
var errQueryStreamClientGone = errors.New("the stream's client went away")

// wantsQueryStream reports whether r accepts text/event-stream, with a q above 0.
func wantsQueryStream(r *http.Request) bool {
	for _, v := range r.Header.Values(echo.HeaderAccept) {
		for _, part := range strings.Split(v, ",") {
			mt, params, err := mime.ParseMediaType(part)
			if err != nil || mt != QueryStreamType {
				continue
			}
			if q, ok := params["q"]; ok {
				if f, err := strconv.ParseFloat(q, 64); err != nil || f <= 0 {
					continue
				}
			}
			return true
		}
	}
	return false
}

// streamQuery runs request, admitted, and answers with its events as they come. The answer starts
// with the first event: a request that Query rejects before it runs gets the status and body it
// would get without a stream. Errors after that end the stream with an end event.
func (v2 *Handlers) streamQuery(ctx echo.Context, request simulation.QueryRequest, effort *ledgercore.Effort) error {
	// The stream stops the query when its client is gone or too far behind.
	queryCtx, stop := context.WithCancelCause(request.Context())
	defer stop(nil)
	q := &queryStreamQueue{ready: make(chan struct{}, 1), stop: stop}
	if request.Limits.MaxResponseBytes == 0 {
		q.max = queryStreamBacklogMax
	}
	request = request.WithContext(queryCtx).WithStream(q.push)
	var err error
	done := make(chan struct{})
	go func() {
		defer close(done)
		// Off the handler's goroutine, a panic would crash the node: it is the query's error.
		defer func() {
			if r := recover(); r != nil {
				v2.Log.Errorf("recovered from panic in a streamed query: %v\n%s", r, debug.Stack())
				err = fmt.Errorf("panic: %v", r)
			}
		}()
		_, err = v2.Node.Query(request)
	}()

	select {
	case <-q.ready:
	case <-done:
		if err != nil && q.empty() {
			return v2.answerQueryError(ctx, err)
		}
	}
	cfg := v2.Node.Config()
	// The server's own write deadline, which the stream's writes move but never pass while the
	// query runs.
	var serverDeadline time.Time
	if deadline, ok := requestDeadline(ctx, cfg); ok {
		serverDeadline = deadline.Add(requestWriteMargin(cfg))
	}
	w := startQueryStream(ctx.Response(), effort, requestWriteMargin(cfg), serverDeadline)
	// write writes the events queued. Once that fails, nobody reads the stream: the query stops.
	write := func() {
		w.write(q.take())
		if w.err != nil {
			stop(errQueryStreamClientGone)
		}
	}
	write() // the start event, and the headers, go out now
	keepalive := time.NewTicker(queryStreamKeepalive / 3)
	defer keepalive.Stop()
	for {
		select {
		case <-done:
			// What is left has until the server's own write deadline, or a write margin from now if
			// that is later, and the end event a margin more.
			w.finishBy(serverDeadline)
			w.write(q.take())
			w.end(err)
			switch {
			case w.err != nil || clientGone(ctx):
				v2.Log.Debugf("query stream: client gone: %v", errors.Join(w.err, err))
			case err != nil:
				// As the answer without a stream logs it (returnError).
				v2.Log.Infof("query stream ended with status %d: %v", queryErrorStatus(err), err)
			}
			return nil
		case <-q.ready:
			if wait := queryStreamFlushInterval - time.Since(w.lastWrite); wait > 0 {
				timer := time.NewTimer(wait)
				select {
				case <-timer.C:
				case <-done:
				}
				timer.Stop()
			}
			write()
		case <-keepalive.C:
			if time.Since(w.lastWrite) >= queryStreamKeepalive {
				w.write([]byte(": keepalive\n\n"))
				if w.err != nil {
					stop(errQueryStreamClientGone)
				}
			}
		}
	}
}

// answerQueryError answers a query that failed with err, without a stream.
func (v2 *Handlers) answerQueryError(ctx echo.Context, err error) error {
	if stopped, answerErr := answerStopped(ctx, err, v2.Log); stopped {
		return answerErr
	}
	if queryErrorStatus(err) == http.StatusBadRequest {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	return internalError(ctx, err, err.Error(), v2.Log)
}

// queryErrorStatus is the status of the answer to a query that failed with err, and its message.
func queryErrorStatus(err error) int {
	var invalid simulation.InvalidRequestError
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	case errors.As(err, &invalid), errors.Is(err, simulation.ErrQueryResponseTooLarge), errors.Is(err, errQueryStreamBacklog):
		return http.StatusBadRequest
	}
	return http.StatusInternalServerError
}

// queryStreamQueue holds a stream's events, encoded, until the handler writes them. The request's
// workers push to it and never wait for the client. It holds what MaxAPIQueryResponseBytes bounds,
// each log's bytes and 48 more, as base64 in events: about 4/3 of it at most. Without that limit,
// it holds max bytes at most.
type queryStreamQueue struct {
	mu  deadlock.Mutex
	buf []byte
	// open is set when buf ends in a log event that more logs of the same call may join: those of
	// round, call, from index next.
	open  bool
	round basics.Round
	call  int
	next  int
	// max, if not 0, is the most bytes buf may hold. An event that would not fit is dropped with
	// every later one (over), and stop is called with errQueryStreamBacklog.
	max  int
	over bool
	stop context.CancelCauseFunc
	// ready has a value when buf has events the handler has not taken.
	ready chan struct{}
}

// queryStreamLogs are buffers the workers encode a log in before they lock the queue.
var queryStreamLogs = sync.Pool{New: func() any { return new([]byte) }}

// push encodes e, outside the queue's lock, and adds it at the end of the queue. A log that follows
// the one before it, of the same call, joins its event.
func (q *queryStreamQueue) push(e simulation.QueryEvent) {
	var ok bool
	if e.Kind == simulation.QueryEventLog {
		enc := queryStreamLogs.Get().(*[]byte)
		b := append((*enc)[:0], '"')
		b = base64.StdEncoding.AppendEncode(b, unsafe.Slice(unsafe.StringData(e.Log), len(e.Log)))
		b = append(b, '"')
		ok = q.add(e, b)
		// A buffer that grew large is left to the collector.
		if cap(b) <= queryStreamWritePiece {
			*enc = b
			queryStreamLogs.Put(enc)
		}
	} else {
		ok = q.add(e, appendQueryEvent(nil, e))
	}
	if !ok {
		q.stop(errQueryStreamBacklog)
		return
	}
	select {
	case q.ready <- struct{}{}:
	default:
	}
}

// add adds e, encoded as enc, at the end of the queue: a log as the quoted string of its event's
// list, any other event whole. It returns false when the queue is full.
func (q *queryStreamQueue) add(e simulation.QueryEvent, enc []byte) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	// 64 bytes more than cover the head and the end of a log event.
	if q.over || q.max > 0 && len(q.buf)+len(enc)+64 > q.max {
		q.over = true
		return false
	}
	if e.Kind != simulation.QueryEventLog {
		q.closeLogs()
		q.buf = append(q.buf, enc...)
		return true
	}
	if q.open && e.Round == q.round && e.Call == q.call && e.Index == q.next {
		q.buf = append(q.buf, ',')
	} else {
		q.closeLogs()
		q.buf = append(q.buf, `event: log`+"\n"+`data: {"r":`...)
		q.buf = strconv.AppendUint(q.buf, uint64(e.Round), 10)
		q.buf = append(q.buf, `,"c":`...)
		q.buf = strconv.AppendInt(q.buf, int64(e.Call), 10)
		q.buf = append(q.buf, `,"i":`...)
		q.buf = strconv.AppendInt(q.buf, int64(e.Index), 10)
		q.buf = append(q.buf, `,"d":[`...)
		q.open, q.round, q.call = true, e.Round, e.Call
	}
	q.buf = append(q.buf, enc...)
	q.next = e.Index + 1
	return true
}

// closeLogs ends the open log event, if any.
func (q *queryStreamQueue) closeLogs() {
	if q.open {
		q.buf = append(q.buf, "]}\n\n"...)
		q.open = false
	}
}

// take returns the events queued, and empties the queue.
func (q *queryStreamQueue) take() []byte {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.closeLogs()
	b := q.buf
	q.buf = nil
	return b
}

// empty reports whether nothing was queued since the last take.
func (q *queryStreamQueue) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.buf) == 0
}

// The data of the events, but for log's, which the queue encodes by hand. The API spec defines them
// as QueryStreamStart and the others; TestQueryStreamEventsMatchSpec holds the two together.

// queryStreamStart is the data of the start event.
type queryStreamStart struct {
	LastRound basics.Round   `json:"last-round"`
	Rounds    []basics.Round `json:"rounds"`
}

// queryStreamCall is the data of a call event.
type queryStreamCall struct {
	Round basics.Round `json:"r"`
	Call  int          `json:"c"`
	Logs  int          `json:"logs"`
	Cost  int          `json:"cost"`
	Error string       `json:"error,omitempty"`
}

// queryStreamRound is the data of a round event.
type queryStreamRound struct {
	Round    basics.Round                 `json:"r"`
	Protocol *model.QueryProtocolOverride `json:"protocol,omitempty"`
	Error    string                       `json:"error,omitempty"`
}

// queryStreamEnd is the data of the end event. Status is the status the answer would have without
// a stream.
type queryStreamEnd struct {
	Effort string `json:"effort,omitempty"`
	Error  string `json:"error,omitempty"`
	Status int    `json:"status,omitempty"`
}

// appendQueryEvent appends e, any event but a log, to b.
func appendQueryEvent(b []byte, e simulation.QueryEvent) []byte {
	switch e.Kind {
	case simulation.QueryEventStart:
		rounds := e.Rounds
		if rounds == nil {
			rounds = []basics.Round{}
		}
		return appendSSE(b, "start", queryStreamStart{LastRound: e.LastRound, Rounds: rounds})
	case simulation.QueryEventCall:
		return appendSSE(b, "call", queryStreamCall{Round: e.Round, Call: e.Call, Logs: e.Logs, Cost: e.Cost, Error: e.Error})
	case simulation.QueryEventRound:
		return appendSSE(b, "round", queryStreamRound{Round: e.Round, Protocol: convertProtocolOverride(e.Protocol), Error: e.Error})
	}
	return b
}

// appendSSE appends an event named name, whose data is v in JSON, to b.
func appendSSE(b []byte, name string, v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		// The event types encode without error.
		panic(err)
	}
	b = append(b, "event: "...)
	b = append(b, name...)
	b = append(b, "\ndata: "...)
	b = append(b, data...)
	return append(b, "\n\n"...)
}

// queryStreamWriter writes a stream's events to its response.
type queryStreamWriter struct {
	res    *echo.Response
	rc     *http.ResponseController
	effort *ledgercore.Effort
	// margin, if not 0, is how long the client has to take each queryStreamWritePiece bytes: the
	// write deadline moves that far ahead before each. The server's own deadline, which counts from
	// the request, would cut a stream whose client is slow at the end with no end event; this one
	// cuts a client that stalls, whenever it does. It never moves past limit, the server's own
	// deadline, so a client that takes each piece just in time holds the request no longer than
	// before. Once fixed is set (finishBy), it stays.
	margin time.Duration
	limit  time.Time
	fixed  bool
	// written counts the bytes written, before compression. lastWrite is when the last write was,
	// and err the first write error: once the client is gone, nothing more is written.
	written   uint64
	lastWrite time.Time
	err       error
}

// startQueryStream writes the headers of a stream. The effort header would report the work done
// before the first event: the effort goes in the end event, and in a trailer, instead.
func startQueryStream(res *echo.Response, effort *ledgercore.Effort, margin time.Duration, limit time.Time) *queryStreamWriter {
	h := res.Header()
	h.Set(echo.HeaderContentType, QueryStreamType)
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no") // nginx: don't buffer
	if effort != nil {
		h.Set("Trailer", EffortHeader)
		res.Before(func() { h.Del(EffortHeader) })
	}
	res.WriteHeader(http.StatusOK)
	return &queryStreamWriter{res: res, rc: http.NewResponseController(res), effort: effort, margin: margin, limit: limit}
}

// extend moves the write deadline a margin ahead of now, up to limit, unless it is fixed.
func (w *queryStreamWriter) extend() {
	if w.margin > 0 && !w.fixed {
		deadline := time.Now().Add(w.margin)
		if !w.limit.IsZero() && deadline.After(w.limit) {
			deadline = w.limit
		}
		// A response that has no deadline to set (a test's recorder) has none to move.
		_ = w.rc.SetWriteDeadline(deadline)
	}
}

// finishBy gives the writes from now on until deadline, or a margin from now if that is later,
// however many pieces they are.
func (w *queryStreamWriter) finishBy(deadline time.Time) {
	if w.margin <= 0 {
		return
	}
	if soonest := time.Now().Add(w.margin); deadline.Before(soonest) {
		deadline = soonest
	}
	_ = w.rc.SetWriteDeadline(deadline)
	w.fixed = true
}

// write writes b, a piece at a time, and flushes it.
func (w *queryStreamWriter) write(b []byte) {
	if len(b) == 0 {
		return
	}
	w.written += uint64(len(b))
	w.lastWrite = time.Now()
	for len(b) > 0 && w.err == nil {
		piece := b[:min(len(b), queryStreamWritePiece)]
		b = b[len(piece):]
		w.extend()
		_, w.err = w.res.Write(piece)
	}
	if w.err == nil {
		w.extend()
		w.err = w.rc.Flush()
	}
}

// end writes the end event, with err, the query's error, if any, under a write deadline of its
// own. Its effort is the trailer's: both count the end event in response-bytes.
func (w *queryStreamWriter) end(err error) {
	var e queryStreamEnd
	if err != nil {
		e.Status = queryErrorStatus(err)
		e.Error = err.Error()
		if e.Status == http.StatusGatewayTimeout {
			e.Error = errTimeLimit
		}
	}
	event := appendSSE(nil, "end", e)
	if w.effort != nil {
		// The event holds its own size: count it until its digits settle.
		counts := w.effort.Counts()
		for range 4 {
			counts.ResponseBytes = w.written + uint64(len(event))
			e.Effort = counts.Header()
			event = appendSSE(event[:0], "end", e)
			if counts.ResponseBytes == w.written+uint64(len(event)) {
				break
			}
		}
		w.effort.SetResponseBytes(counts.ResponseBytes)
		w.res.Header().Set(EffortHeader, e.Effort)
	}
	w.fixed, w.limit = false, time.Time{}
	w.write(event)
}
