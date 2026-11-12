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

package simulation

// Streaming a query's results as its calls run (QueryRequest.WithStream).

import (
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions/logic"
)

// QueryEventKind is what a QueryEvent reports.
type QueryEventKind int

const (
	// QueryEventStart comes first, once the request is valid and its rounds are read: LastRound,
	// and Rounds, the round of each entry of the request's rounds, 0 resolved to LastRound.
	QueryEventStart QueryEventKind = iota
	// QueryEventLog is a log of the called app, as the program makes it: Round, Call, Index and
	// Log. The logs of the apps it calls are not reported, as in QueryCallResult.
	QueryEventLog
	// QueryEventCall is a call that ended, after its logs: Round, Call, Logs (how many it made),
	// Cost and Error.
	QueryEventCall
	// QueryEventRound is a round whose calls all ended: Round, Protocol and Error, as in
	// QueryRoundResult. A round with an Error has no results: its earlier events don't count.
	QueryEventRound
)

// QueryEvent is something that happened while a streamed query ran. Each distinct round of the
// request is reported once, whatever the number of entries it has in the request's rounds.
type QueryEvent struct {
	Kind      QueryEventKind
	LastRound basics.Round
	Rounds    []basics.Round
	Round     basics.Round
	Call      int
	Index     int
	Log       string
	Logs      int
	Cost      int
	Error     string
	Protocol  *ProtocolOverride
}

// WithStream returns r whose results are reported to sink as they come, instead of kept for its
// QueryResult, whose calls then have no logs. sink is called from the request's workers at once,
// so it must be safe for concurrent use, and must not wait: it runs while the workers hold their
// slots. The events of one call come in order; those of different calls interleave.
//
// MaxResponseBytes then bounds the bytes reported: each log once, whatever the number of entries
// its round has in the request's rounds, and a round that fails gives none back, since they were
// sent.
func (r QueryRequest) WithStream(sink func(QueryEvent)) QueryRequest {
	r.stream = sink
	return r
}

// streamLogs reports the logs the called app made since the last ones reported, after an opcode of
// cx, if cx is the called app's program. Each log's bytes count toward the response limit first;
// over it, the request stops.
func (t *queryTracer) streamLogs(cx *logic.EvalContext) {
	if cx.GetCaller() != nil {
		return
	}
	logs := cx.TxnGroup[cx.GroupIndex()].EvalDelta.Logs
	for t.logs < len(logs) {
		log := logs[t.logs]
		if !t.run.chargeBytes(t.round, uint64(len(log))+queryLogOverhead) {
			return
		}
		t.run.req.stream(QueryEvent{Kind: QueryEventLog, Round: t.round.round, Call: t.call, Index: t.logs, Log: log})
		t.logs++
	}
}
