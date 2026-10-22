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

// Time limits and cancellation of expensive requests (simulate, time travel). Non-standard fork
// feature, kept in its own file to keep upstream merges of handlers.go clean.

import (
	"context"
	"errors"
	"net/http"
	"runtime"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/logging"
)

// requestTimeLimit returns how long an expensive request may run on a node configured with cfg:
// RestWriteTimeoutSeconds, less a second to write the answer before the server's write deadline,
// which starts when the request's header was read. 0 means no limit.
func requestTimeLimit(cfg config.Local) time.Duration {
	limit := time.Duration(cfg.RestWriteTimeoutSeconds) * time.Second
	if limit <= 0 {
		return 0
	}
	if limit >= 2*time.Second {
		limit -= time.Second
	}
	return limit
}

// requestContext returns the context an expensive request runs under: the request's own context,
// canceled when the client goes away, with the deadline of requestTimeLimit. A response written
// after RestWriteTimeoutSeconds is lost, so the work stops before.
func requestContext(ctx echo.Context, cfg config.Local) (context.Context, context.CancelFunc) {
	if limit := requestTimeLimit(cfg); limit > 0 {
		return context.WithTimeout(ctx.Request().Context(), limit)
	}
	return context.WithCancel(ctx.Request().Context())
}

// errTimeLimit is the message of a request stopped at its time limit.
const errTimeLimit = "the request reached the node's time limit (RestWriteTimeoutSeconds)"

// clientGone reports whether the client of ctx went away.
func clientGone(ctx echo.Context) bool {
	return ctx.Request().Context().Err() != nil
}

// answerStopped answers a request whose work stopped with err because the client went away or the
// request reached its time limit, and reports whether err was one of those. A client that went
// away gets nothing: writing would fail, and returning an error would make echo log a 500.
func answerStopped(ctx echo.Context, err error, log logging.Logger) (bool, error) {
	switch {
	case clientGone(ctx):
		log.Debugf("request canceled: %v", err)
		return true, nil
	case errors.Is(err, context.DeadlineExceeded):
		return true, returnError(ctx, http.StatusGatewayTimeout, err, errTimeLimit, log)
	case errors.Is(err, context.Canceled):
		// Our own context, canceled while the client is still there: should not happen.
		return true, internalError(ctx, err, err.Error(), log)
	}
	return false, nil
}

// admissionWait is how long a new expensive request waits for capacity before it is refused with
// 503. It is short so that a load balancer can try another node.
const admissionWait = 250 * time.Millisecond

// errBusy is the error of a request refused because the node has no capacity left for it.
var errBusy = errors.New("the node is busy: try again later or on another node")

// answerBusy refuses a request with 503 and Retry-After, which a load balancer takes as "busy, try
// another node", not as a failure of this node.
func answerBusy(ctx echo.Context, err error, log logging.Logger) error {
	ctx.Response().Header().Set("Retry-After", "1")
	return serviceUnavailable(ctx, err, err.Error(), log)
}

// EffectiveSimulateParallelism returns the most simulations that run at once across the node for
// the config value SimulateParallelism: GOMAXPROCS when it is 0 or less.
func EffectiveSimulateParallelism(p int) int {
	if p <= 0 {
		return runtime.GOMAXPROCS(0)
	}
	return p
}

// admitSimulate takes a slot of limiter for a new simulate request, waiting at most admissionWait.
// It returns errBusy when no slot was free by then, and the cause of ctx when ctx ended first. A
// nil limiter admits every request.
func admitSimulate(ctx context.Context, limiter *semaphore.Weighted) error {
	if ctx.Err() != nil {
		return context.Cause(ctx)
	}
	if limiter == nil || limiter.TryAcquire(1) {
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, admissionWait)
	defer cancel()
	if limiter.Acquire(wait, 1) != nil {
		if ctx.Err() != nil {
			return context.Cause(ctx)
		}
		return errBusy
	}
	return nil
}

// acquireSimulate takes a slot of limiter for more work of an admitted request, waiting as long as
// ctx allows.
func acquireSimulate(ctx context.Context, limiter *semaphore.Weighted) error {
	if limiter != nil && limiter.Acquire(ctx, 1) != nil {
		return context.Cause(ctx)
	}
	return nil
}

func releaseSimulate(limiter *semaphore.Weighted) {
	if limiter != nil {
		limiter.Release(1)
	}
}

// answerAdmission answers a request that admitSimulate refused with err.
func answerAdmission(ctx echo.Context, err error, log logging.Logger) error {
	if errors.Is(err, errBusy) {
		return answerBusy(ctx, err, log)
	}
	if stopped, answerErr := answerStopped(ctx, err, log); stopped {
		return answerErr
	}
	return internalError(ctx, err, err.Error(), log)
}

// simulateOnce simulates a request without rounds in a slot of the node's simulate limiter, under
// requestContext. answered reports that it already answered the request (busy, client gone or time
// limit), with err the handler's return value; otherwise err is the simulation's error.
func (v2 *Handlers) simulateOnce(ctx echo.Context, request simulation.Request) (result simulation.Result, answered bool, err error) {
	reqCtx, cancel := requestContext(ctx, v2.Node.Config())
	defer cancel()
	if admitErr := admitSimulate(reqCtx, v2.SimulateLimiter); admitErr != nil {
		return simulation.Result{}, true, answerAdmission(ctx, admitErr, v2.Log)
	}
	defer releaseSimulate(v2.SimulateLimiter)

	budget := simulation.NewTraceBudget(v2.Node.Config().MaxAPISimulateTraceBytes)
	result, err = v2.Node.Simulate(request.WithContext(reqCtx).WithTraceBudget(budget))
	if err != nil {
		if stopped, answerErr := answerStopped(ctx, err, v2.Log); stopped {
			return simulation.Result{}, true, answerErr
		}
		if errors.Is(err, simulation.ErrTraceTooLarge) {
			return simulation.Result{}, true, badRequest(ctx, err, err.Error(), v2.Log)
		}
	}
	return result, false, err
}
