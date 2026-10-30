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

// Read-only TEAL queries (POST /v2/query). Non-standard fork feature, kept in its own file to keep
// upstream merges of handlers.go clean.

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"unsafe"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/protocol"
)

// PreEncodedQuerySchema mirrors model.ApplicationStateSchema
type PreEncodedQuerySchema struct {
	NumUint      uint64 `codec:"num-uint"`
	NumByteSlice uint64 `codec:"num-byte-slice"`
}

// PreEncodedQueryBox mirrors model.BoxReference
type PreEncodedQueryBox struct {
	App  basics.AppIndex `codec:"app"`
	Name []byte          `codec:"name"`
}

// PreEncodedQueryCall mirrors model.QueryCall
type PreEncodedQueryCall struct {
	Args          [][]byte             `codec:"args,omitempty"`
	Accounts      []string             `codec:"accounts,omitempty"`
	ForeignApps   []basics.AppIndex    `codec:"foreign-apps,omitempty"`
	ForeignAssets []basics.AssetIndex  `codec:"foreign-assets,omitempty"`
	Boxes         []PreEncodedQueryBox `codec:"boxes,omitempty"`
}

// PreEncodedQueryRequest mirrors model.QueryRequest
type PreEncodedQueryRequest struct {
	Program      []byte                 `codec:"program,omitempty"`
	ClearProgram []byte                 `codec:"clear-program,omitempty"`
	GlobalSchema *PreEncodedQuerySchema `codec:"global-schema,omitempty"`
	LocalSchema  *PreEncodedQuerySchema `codec:"local-schema,omitempty"`
	AppID        basics.AppIndex        `codec:"app-id,omitempty"`
	Sender       string                 `codec:"sender,omitempty"`
	Rounds       []basics.Round         `codec:"rounds,omitempty"`
	Protocol     string                 `codec:"protocol,omitempty"`
	Budget       uint64                 `codec:"budget,omitempty"`
	Calls        []PreEncodedQueryCall  `codec:"calls"`
}

// PreEncodedQueryCallResult mirrors model.QueryCallResult
type PreEncodedQueryCallResult struct {
	Logs  [][]byte `codec:"logs"`
	Cost  uint64   `codec:"cost"`
	Error *string  `codec:"error,omitempty"`
}

// PreEncodedQueryRoundResult mirrors model.QueryRoundResult
type PreEncodedQueryRoundResult struct {
	Round    basics.Round                    `codec:"round"`
	Protocol *model.SimulateProtocolOverride `codec:"protocol,omitempty"`
	Error    *string                         `codec:"error,omitempty"`
	Calls    []PreEncodedQueryCallResult     `codec:"calls,omitempty"`
}

// PreEncodedQueryResponse mirrors model.QueryResponse
type PreEncodedQueryResponse struct {
	LastRound basics.Round                 `codec:"last-round"`
	Rounds    []PreEncodedQueryRoundResult `codec:"rounds"`
}

// Query runs read-only TEAL calls at one or more rounds.
// (POST /v2/query)
func (v2 *Handlers) Query(ctx echo.Context, params model.QueryParams) error {
	stat, err := v2.Node.Status()
	if err != nil {
		return internalError(ctx, err, errFailedRetrievingNodeStatus, v2.Log)
	}
	if stat.Catchpoint != "" {
		return serviceUnavailable(ctx, errors.New("query failed as the node was catchpoint catchuping"), errOperationNotAvailableDuringCatchup, v2.Log)
	}
	handle, contentType, err := getCodecHandle((*string)(params.Format))
	if err != nil {
		return badRequest(ctx, err, errFailedParsingFormatOption, v2.Log)
	}

	var body bytes.Buffer
	if _, err = body.ReadFrom(http.MaxBytesReader(nil, ctx.Request().Body, MaxSimulateBytes)); err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	var decoded PreEncodedQueryRequest
	if err = decode(protocol.CodecHandle, body.Bytes(), &decoded); err != nil {
		decoded = PreEncodedQueryRequest{}
		if err = decode(protocol.JSONStrictHandle, body.Bytes(), &decoded); err != nil {
			return badRequest(ctx, err, err.Error(), v2.Log)
		}
	}
	// Check before taking a slot. The node checks again, with the same limits.
	cfg := v2.Node.Config()
	limits := simulation.QueryLimitsFromConfig(cfg)
	if limits.MaxCalls > 0 && len(decoded.Calls) > limits.MaxCalls {
		// Before converting them, which would allocate them again.
		err = fmt.Errorf("too many calls: got %d, the limit is %d", len(decoded.Calls), limits.MaxCalls)
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	request, err := convertQueryRequest(decoded)
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	request.Limits = limits
	if err = request.Validate(cfg.EnableDeveloperAPI); err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}

	// The workers use only the request's context and values, never the echo.Context.
	reqCtx, cancel := requestContext(ctx, cfg)
	defer cancel()
	limiter := v2.SimulateLimiter
	if err = admitSimulate(reqCtx, limiter); err != nil {
		return answerAdmission(ctx, err, v2.Log)
	}
	defer releaseSimulate(limiter)
	effort := v2.startEffort(ctx)
	request = request.WithContext(contextWithEffort(reqCtx, effort)).WithSlots(
		func() bool { return trySimulate(limiter) },
		func() { releaseSimulate(limiter) })

	result, err := v2.Node.Query(request)
	if err != nil {
		if stopped, answerErr := answerStopped(ctx, err, v2.Log); stopped {
			return answerErr
		}
		var invalid simulation.InvalidRequestError
		if errors.As(err, &invalid) || errors.Is(err, simulation.ErrQueryResponseTooLarge) {
			return badRequest(ctx, err, err.Error(), v2.Log)
		}
		return internalError(ctx, err, err.Error(), v2.Log)
	}
	if clientGone(ctx) {
		// There is no one to write the response to.
		return nil
	}

	response := convertQueryResult(result)
	data, err := encode(handle, &response)
	if err != nil {
		return internalError(ctx, err, errFailedToEncodeResponse, v2.Log)
	}
	return writeEncoded(ctx, contentType, data)
}

// convertQueryRequest converts a decoded request. Its errors are the client's.
func convertQueryRequest(r PreEncodedQueryRequest) (simulation.QueryRequest, error) {
	request := simulation.QueryRequest{
		Program:      r.Program,
		ClearProgram: r.ClearProgram,
		AppID:        r.AppID,
		Rounds:       r.Rounds,
		Protocol:     r.Protocol,
		Calls:        make([]simulation.QueryCall, len(r.Calls)),
	}
	if r.Budget > uint64(simulation.MaxExtraOpcodeBudgetCeiling) {
		return request, fmt.Errorf("budget %d is over the limit of %d", r.Budget, simulation.MaxExtraOpcodeBudgetCeiling)
	}
	request.Budget = int(r.Budget)
	if r.GlobalSchema != nil {
		request.GlobalSchema = basics.StateSchema{NumUint: r.GlobalSchema.NumUint, NumByteSlice: r.GlobalSchema.NumByteSlice}
	}
	if r.LocalSchema != nil {
		request.LocalSchema = basics.StateSchema{NumUint: r.LocalSchema.NumUint, NumByteSlice: r.LocalSchema.NumByteSlice}
	}
	if r.Sender != "" {
		sender, err := basics.UnmarshalChecksumAddress(r.Sender)
		if err != nil {
			return request, fmt.Errorf("sender: %w", err)
		}
		request.Sender = sender
	}
	for i, c := range r.Calls {
		call := simulation.QueryCall{
			Args:          c.Args,
			ForeignApps:   c.ForeignApps,
			ForeignAssets: c.ForeignAssets,
		}
		for _, a := range c.Accounts {
			addr, err := basics.UnmarshalChecksumAddress(a)
			if err != nil {
				return request, fmt.Errorf("call %d: account %q: %w", i, a, err)
			}
			call.Accounts = append(call.Accounts, addr)
		}
		for _, b := range c.Boxes {
			// A box of the called app has index 0; another app's is its place in foreign-apps, plus 1.
			index := 0
			if b.App != 0 && b.App != r.AppID {
				index = len(call.ForeignApps) + 1
				for j, app := range call.ForeignApps {
					if app == b.App {
						index = j + 1
						break
					}
				}
				if index > len(call.ForeignApps) {
					// Don't append to the request's slice, which call.ForeignApps shares.
					call.ForeignApps = append(call.ForeignApps[:len(call.ForeignApps):len(call.ForeignApps)], b.App)
				}
			}
			call.Boxes = append(call.Boxes, transactions.BoxRef{Index: uint64(index), Name: b.Name})
		}
		request.Calls[i] = call
	}
	return request, nil
}

// convertQueryResult converts a result for encoding. The logs are not copied.
func convertQueryResult(result simulation.QueryResult) PreEncodedQueryResponse {
	response := PreEncodedQueryResponse{
		LastRound: result.LastRound,
		Rounds:    make([]PreEncodedQueryRoundResult, len(result.Rounds)),
	}
	for i, rr := range result.Rounds {
		out := PreEncodedQueryRoundResult{
			Round:    rr.Round,
			Protocol: convertProtocolOverride(rr.Protocol),
		}
		if rr.Error != "" {
			out.Error = &rr.Error
		} else {
			out.Calls = make([]PreEncodedQueryCallResult, len(rr.Calls))
			for j, c := range rr.Calls {
				call := PreEncodedQueryCallResult{
					Logs: make([][]byte, len(c.Logs)),
					Cost: uint64(c.Cost),
				}
				for k, l := range c.Logs {
					if len(l) == 0 {
						call.Logs[k] = []byte{} // encoded as empty bytes, not null
						continue
					}
					// Only read by the encoder, so the bytes of the string can be shared.
					call.Logs[k] = unsafe.Slice(unsafe.StringData(l), len(l))
				}
				if c.Error != "" {
					call.Error = &rr.Calls[j].Error
				}
				out.Calls[j] = call
			}
		}
		response.Rounds[i] = out
	}
	return response
}
