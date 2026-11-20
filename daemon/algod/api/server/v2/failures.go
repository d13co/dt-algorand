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

// Failed simulations kept for replay (EnableFailureCapture, GET /v2/failures). Non-standard fork
// feature.

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/failures"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/protocol"
)

// FailureIDHeader carries the id of a kept failure (GET /v2/failures/{id}) on the answer that failed.
const FailureIDHeader = "X-Algod-Failure-Id"

const (
	errFailuresDisabled    = "failures are kept only with node config EnableFailureCapture"
	errFailuresUnavailable = "failure capture is enabled but its store is not open: see the node's log"
	errFailureNotFound     = "no such failure: never kept, expired or dropped"
	errFailuresNext        = "next is not a token from GET /v2/failures"
)

// defaultFailuresLimit and maxFailuresLimit bound a page of GET /v2/failures.
const (
	defaultFailuresLimit = 100
	maxFailuresLimit     = 1000
)

// FailureStore is implemented by nodes that keep failures (EnableFailureCapture).
type FailureStore interface {
	// FailureStore returns the node's store, or nil when it keeps none.
	FailureStore() *failures.Store
}

// failureStore returns the node's failure store, or nil.
func (v2 *Handlers) failureStore() *failures.Store {
	if fs, ok := v2.Node.(FailureStore); ok {
		return fs.FailureStore()
	}
	return nil
}

// failuresOff answers a failures endpoint on a node that keeps none: 400 when the config
// doesn't ask for it, 503 when it does but the store isn't open (it failed to, or the node, a
// follower, has none).
func (v2 *Handlers) failuresOff(ctx echo.Context) error {
	if v2.Node.Config().EnableFailureCapture {
		return serviceUnavailable(ctx, errors.New("no failure store"), errFailuresUnavailable, v2.Log)
	}
	return badRequest(ctx, errors.New("failure capture is off"), errFailuresDisabled, v2.Log)
}

// requestFormat names the encoding of a request body: JSON begins with an object, msgpack never
// with '{' (a fixint, not a map).
func requestFormat(body []byte) string {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		case '{':
			return "json"
		}
		break
	}
	return "msgpack"
}

// protoAfter returns the consensus version of the block after round rnd, or "" when unknown.
func (v2 *Handlers) protoAfter(rnd basics.Round) protocol.ConsensusVersion {
	hdr, err := v2.Node.LedgerForAPI().BlockHdr(rnd)
	if err != nil {
		return ""
	}
	if hdr.NextProtocolSwitchOn != 0 && hdr.NextProtocolSwitchOn <= rnd+1 {
		return hdr.NextProtocol
	}
	return hdr.CurrentProtocol
}

// captureSimulate keeps a simulate request with a failed group (result) or that the simulator
// refused (refused), and sets FailureIDHeader on the answer. round is the round the request ran on
// when the result doesn't say. body must not change after the call.
func (v2 *Handlers) captureSimulate(ctx echo.Context, body []byte, round basics.Round, result *simulation.Result, refused error) {
	store := v2.failureStore()
	if store == nil {
		return
	}
	r := failures.Record{Kind: failures.KindSimulate, Request: body, Format: requestFormat(body), Version: config.GetCurrentVersion().String()}
	switch {
	case refused != nil:
		r.Error = refused.Error()
		r.Round = round
	case result != nil:
		for i := range result.TxnGroups {
			g := &result.TxnGroups[i]
			if g.FailureMessage == "" {
				continue
			}
			var failedAt []uint64
			for _, p := range g.FailedAt {
				failedAt = append(failedAt, uint64(p))
			}
			r.Groups = append(r.Groups, failures.GroupFailure{Index: i, Message: g.FailureMessage, FailedAt: failedAt})
		}
		if len(r.Groups) == 0 {
			return
		}
		r.Round = result.LastRound
	default:
		return
	}
	r.Proto = v2.protoAfter(r.Round)
	if id := store.Capture(r); id != "" {
		ctx.Response().Header().Set(FailureIDHeader, id)
	}
}

func convertFailure(r failures.Record) model.Failure {
	out := model.Failure{
		Id:    r.ID,
		Time:  r.Time,
		Kind:  string(r.Kind),
		Round: r.Round,
		Error: omitEmpty(r.Error),
	}
	if len(r.Groups) > 0 {
		groups := make([]model.FailureGroup, len(r.Groups))
		for i, g := range r.Groups {
			groups[i] = model.FailureGroup{Index: uint64(g.Index), Message: g.Message}
			if len(g.FailedAt) > 0 {
				failedAt := slices.Clone(g.FailedAt)
				groups[i].FailedAt = &failedAt
			}
		}
		out.Groups = &groups
	}
	if r.Request != nil {
		out.Request = &r.Request
		out.RequestFormat = omitEmpty(r.Format)
	}
	out.ConsensusVersion = omitEmpty(string(r.Proto))
	out.NodeVersion = omitEmpty(r.Version)
	return out
}

// ListFailures lists the failures the node keeps, newest first.
// (GET /v2/failures)
func (v2 *Handlers) ListFailures(ctx echo.Context, params model.ListFailuresParams) error {
	store := v2.failureStore()
	if store == nil {
		return v2.failuresOff(ctx)
	}
	limit := defaultFailuresLimit
	if params.Limit != nil && *params.Limit > 0 {
		limit = int(min(*params.Limit, maxFailuresLimit))
	}
	var before uint64
	if params.Next != nil {
		var err error
		before, err = strconv.ParseUint(*params.Next, 10, 64)
		if err != nil || before == 0 {
			return badRequest(ctx, err, errFailuresNext, v2.Log)
		}
	}
	var kind failures.Kind
	if params.Kind != nil {
		kind = failures.Kind(*params.Kind)
	}
	page, err := store.List(ctx.Request().Context(), before, kind, limit)
	if err != nil {
		return internalError(ctx, err, fmt.Sprintf("listing failures: %v", err), v2.Log)
	}
	out := model.FailuresResponse{Failures: make([]model.Failure, len(page.Records))}
	for i, r := range page.Records {
		out.Failures[i] = convertFailure(r)
	}
	if page.Next != 0 {
		next := strconv.FormatUint(page.Next, 10)
		out.NextToken = &next
	}
	return ctx.JSON(http.StatusOK, out)
}

// GetFailure returns a failure the node keeps, with its request.
// (GET /v2/failures/{id})
func (v2 *Handlers) GetFailure(ctx echo.Context, id string) error {
	store := v2.failureStore()
	if store == nil {
		return v2.failuresOff(ctx)
	}
	r, err := store.Get(ctx.Request().Context(), id)
	if errors.Is(err, failures.ErrNotFound) {
		return notFound(ctx, err, errFailureNotFound, v2.Log)
	}
	if err != nil {
		return internalError(ctx, err, fmt.Sprintf("reading failure %s: %v", id, err), v2.Log)
	}
	return ctx.JSON(http.StatusOK, convertFailure(r))
}
