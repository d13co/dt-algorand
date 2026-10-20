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

// Simulation of one request at several rounds ("rounds"). Non-standard fork feature, kept in its
// own file to keep upstream merges of handlers.go clean.

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-codec/codec"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/node"
)

// PreEncodedSimulateRoundResult mirrors model.SimulateRoundResult
type PreEncodedSimulateRoundResult struct {
	LastRound        basics.Round                       `codec:"last-round"`
	TxnGroups        []PreEncodedSimulateTxnGroupResult `codec:"txn-groups,omitempty"`
	InitialStates    *model.SimulateInitialStates       `codec:"initial-states,omitempty"`
	ProtocolOverride *model.SimulateProtocolOverride    `codec:"protocol-override,omitempty"`
	Error            *string                            `codec:"error,omitempty"`
}

// effectiveMaxSimulateRounds returns the most entries, duplicates included, a simulate request may
// list in "rounds" for the config value MaxAPISimulateRounds: 0 (rounds disabled) up to
// simulation.MaxSimulateRounds.
func effectiveMaxSimulateRounds(m uint64) int {
	return int(min(m, simulation.MaxSimulateRounds))
}

// EffectiveSimulateRoundsParallelism returns the most per-round simulations of "rounds" requests
// run at once for the config value SimulateRoundsParallelism: 1 up to simulation.MaxSimulateRounds.
func EffectiveSimulateRoundsParallelism(p int) int {
	return min(max(1, p), simulation.MaxSimulateRounds)
}

// fallbackSimulateRoundsLimiter is used by Handlers built without a SimulateRoundsLimiter, so the
// limit stays node-wide.
var fallbackSimulateRoundsLimiter = semaphore.NewWeighted(1)

var fallbackSimulateRoundsLimiterWarning sync.Once

func (v2 *Handlers) simulateRoundsLimiter() *semaphore.Weighted {
	if v2.SimulateRoundsLimiter != nil {
		return v2.SimulateRoundsLimiter
	}
	fallbackSimulateRoundsLimiterWarning.Do(func() {
		v2.Log.Warn("simulate: Handlers has no SimulateRoundsLimiter; rounds requests run one round at a time")
	})
	return fallbackSimulateRoundsLimiter
}

// simulateRounds simulates request once per distinct round of request.Rounds, in a pool of
// workers, and writes one round result per entry of request.Rounds.
func (v2 *Handlers) simulateRounds(ctx echo.Context, handle codec.Handle, contentType string, stat node.StatusReport, request PreEncodedSimulateRequest) error {
	cfg := v2.Node.Config()

	// Problems with the request as a whole fail it before anything is simulated.
	if request.Round != 0 {
		err := errors.New("round and rounds cannot both be set")
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	maxRounds := effectiveMaxSimulateRounds(cfg.MaxAPISimulateRounds)
	if maxRounds == 0 {
		err := errors.New("rounds is disabled on this node (MaxAPISimulateRounds is 0)")
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	if len(request.Rounds) > maxRounds {
		// Every entry gets a result, duplicates included, so the limit counts them all; duplicates
		// only save simulations.
		err := fmt.Errorf("too many rounds: got %d, the limit is %d", len(request.Rounds), maxRounds)
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	// Node.Simulate sets the same limits for every round; set them here so the check covers them.
	base := convertSimulationRequest(request).WithNodeLimits(cfg)
	if err := base.Validate(cfg.EnableDeveloperAPI); err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}

	// Resolve 0 to the latest round when the request started, and simulate each distinct round
	// once. index maps every entry of request.Rounds to its distinct round.
	var distinct []basics.Round
	index := make([]int, len(request.Rounds))
	seen := make(map[basics.Round]int, len(request.Rounds))
	for i, rnd := range request.Rounds {
		if rnd == 0 {
			rnd = stat.LastRound
		}
		j, ok := seen[rnd]
		if !ok {
			j = len(distinct)
			seen[rnd] = j
			distinct = append(distinct, rnd)
		}
		index[i] = j
	}

	// The workers use only the request's context and values, never the echo.Context.
	reqCtx := ctx.Request().Context()
	limiter := v2.simulateRoundsLimiter()
	responses := make([]*PreEncodedSimulateResponse, len(distinct))
	errs := make([]error, len(distinct))
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(EffectiveSimulateRoundsParallelism(cfg.SimulateRoundsParallelism), len(distinct)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range work {
				responses[j], errs[j] = v2.simulateRound(reqCtx, limiter, base, distinct[j])
			}
		}()
	}
	for j := range distinct {
		work <- j
	}
	close(work)
	// Every worker has stopped writing to responses and errs once Wait returns.
	wg.Wait()

	if err := reqCtx.Err(); err != nil {
		// The client is gone: there is no one to write the response to. Returning the error would
		// make echo log a 500 and try to write it.
		v2.Log.Debugf("simulate: rounds request canceled: %v", err)
		return nil
	}

	roundResults := make([]PreEncodedSimulateRoundResult, len(request.Rounds))
	var first *PreEncodedSimulateResponse
	for i, j := range index {
		if errs[j] != nil {
			message := errs[j].Error()
			roundResults[i] = PreEncodedSimulateRoundResult{LastRound: distinct[j], Error: &message}
			continue
		}
		roundResult := responses[j]
		if first == nil {
			first = roundResult
		}
		roundResults[i] = PreEncodedSimulateRoundResult{
			LastRound:        roundResult.LastRound,
			TxnGroups:        roundResult.TxnGroups,
			InitialStates:    roundResult.InitialStates,
			ProtocolOverride: roundResult.ProtocolOverride,
		}
	}

	if first == nil {
		// No round was simulated: report what a result of the request would.
		empty := convertSimulationResult(simulation.Result{
			Version:       simulation.ResultLatestVersion,
			EvalOverrides: base.EvalOverrides(),
			TraceConfig:   base.TraceConfig,
		})
		first = &empty
	}
	response := PreEncodedSimulateResponse{
		Version:         first.Version,
		LastRound:       0,
		TxnGroups:       []PreEncodedSimulateTxnGroupResult{}, // encoded as [], not null
		EvalOverrides:   first.EvalOverrides,
		ExecTraceConfig: first.ExecTraceConfig,
		RoundResults:    &roundResults,
	}
	return v2.writeSimulateResponse(ctx, handle, contentType, &response)
}

// simulateRound simulates base at round in a slot of limiter. An error, a panic included, is that
// round's error.
func (v2 *Handlers) simulateRound(ctx context.Context, limiter *semaphore.Weighted, base simulation.Request, round basics.Round) (response *PreEncodedSimulateResponse, err error) {
	if limiter.Acquire(ctx, 1) != nil {
		return nil, errors.New("request canceled")
	}
	defer func() {
		if r := recover(); r != nil {
			v2.Log.Errorf("recovered from panic simulating round %d: %v\n%s", round, r, debug.Stack())
			response, err = nil, fmt.Errorf("simulating round %d: panic: %v", round, r)
		}
		limiter.Release(1)
	}()

	// A copy per round: base, and the slices it shares with the other rounds, are only read.
	r := base
	r.Round = round
	result, err := v2.Node.Simulate(r)
	if err != nil {
		return nil, err
	}
	converted := convertSimulationResult(result)
	return &converted, nil
}
