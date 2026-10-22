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

// EffectiveSimulateRoundsParallelism returns the most rounds of one "rounds" request simulated at
// once for the config value SimulateRoundsParallelism: 1 up to simulation.MaxSimulateRounds.
func EffectiveSimulateRoundsParallelism(p int) int {
	return min(max(1, p), simulation.MaxSimulateRounds)
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
	// The rounds share one trace budget.
	base := convertSimulationRequest(request).WithNodeLimits(cfg).
		WithTraceBudget(simulation.NewTraceBudget(cfg.MaxAPISimulateTraceBytes))
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
	reqCtx, cancel := requestContext(ctx, cfg)
	defer cancel()
	limiter := v2.SimulateLimiter
	if err := admitSimulate(reqCtx, limiter); err != nil {
		return answerAdmission(ctx, err, v2.Log)
	}
	defer releaseSimulate(limiter)
	responses := make([]*PreEncodedSimulateResponse, len(distinct))
	errs := make([]error, len(distinct))
	work := make(chan int)
	// waiting is canceled once every round has been handed out, so that the workers waiting for a
	// slot stop: there is nothing left for them.
	waiting, stopWaiting := context.WithCancel(reqCtx)
	defer stopWaiting()
	var wg sync.WaitGroup
	for w := range min(EffectiveSimulateRoundsParallelism(cfg.SimulateRoundsParallelism), len(distinct)) {
		wg.Add(1)
		if w == 0 {
			// The first worker runs its rounds in the request's admission slot, so the request
			// always progresses.
			go func() {
				defer wg.Done()
				for j := range work {
					responses[j], errs[j] = v2.simulateRound(reqCtx, base, distinct[j])
				}
			}()
			continue
		}
		// The others take a slot before each round they take, so that a worker never holds a round
		// while it waits.
		go func() {
			defer wg.Done()
			for acquireSimulate(waiting, limiter) == nil {
				j, ok := <-work
				if ok {
					responses[j], errs[j] = v2.simulateRound(reqCtx, base, distinct[j])
				}
				releaseSimulate(limiter)
				if !ok {
					return
				}
			}
		}()
	}
	for j := range distinct {
		work <- j
	}
	close(work)
	stopWaiting()
	// Every worker has stopped writing to responses and errs once Wait returns.
	wg.Wait()

	if clientGone(ctx) {
		// There is no one to write the response to. Returning the error would make echo log a 500
		// and try to write it.
		v2.Log.Debugf("simulate: rounds request canceled: %v", ctx.Request().Context().Err())
		return nil
	}

	for _, err := range errs {
		if errors.Is(err, simulation.ErrTraceTooLarge) {
			// The budget is shared: the request as a whole asked for too much.
			return badRequest(ctx, err, err.Error(), v2.Log)
		}
	}

	// At the time limit, the rounds that finished keep their results.
	roundResults := make([]PreEncodedSimulateRoundResult, len(request.Rounds))
	var first *PreEncodedSimulateResponse
	for i, j := range index {
		if errs[j] != nil {
			message := errs[j].Error()
			if errors.Is(errs[j], context.DeadlineExceeded) {
				message = "not simulated: " + errTimeLimit
			}
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

// simulateRound simulates base at round under ctx, in a slot of the node's limiter its caller holds.
// An error, a panic included, is that round's error.
func (v2 *Handlers) simulateRound(ctx context.Context, base simulation.Request, round basics.Round) (response *PreEncodedSimulateResponse, err error) {
	defer func() {
		if r := recover(); r != nil {
			v2.Log.Errorf("recovered from panic simulating round %d: %v\n%s", round, r, debug.Stack())
			response, err = nil, fmt.Errorf("simulating round %d: panic: %v", round, r)
		}
	}()
	if ctx.Err() != nil {
		// Don't start a round the request no longer waits for.
		return nil, context.Cause(ctx)
	}

	// A copy per round: base, and the slices it shares with the other rounds, are only read.
	r := base.WithContext(ctx)
	r.Round = round
	result, err := v2.Node.Simulate(r)
	if err != nil {
		return nil, err
	}
	converted := convertSimulationResult(result)
	return &converted, nil
}
