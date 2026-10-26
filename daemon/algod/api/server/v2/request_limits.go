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
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/logging"
)

// requestTimeLimit returns how long an expensive request may run on a node configured with cfg:
// RestWriteTimeoutSeconds, the server's write deadline, which starts when the request's header was
// read, less a margin to encode and write the answer: a tenth of the timeout, at least a second, and
// at most half the timeout. 0 means no limit.
func requestTimeLimit(cfg config.Local) time.Duration {
	timeout := time.Duration(cfg.RestWriteTimeoutSeconds) * time.Second
	if timeout <= 0 {
		return 0
	}
	margin := max(time.Second, timeout/10)
	if timeout < 2*margin {
		return timeout / 2
	}
	return timeout - margin
}

// arrivalKey is the echo.Context key RecordArrival stores the arrival time of a request under.
const arrivalKey = "dtalgorand.arrival"

// RecordArrival is a middleware that records when a request arrived, once its header was read: the
// moment the server's write deadline starts from. A handler's time limit counts from it, so time
// spent reading a slow request body counts too.
func RecordArrival(next echo.HandlerFunc) echo.HandlerFunc {
	return func(ctx echo.Context) error {
		ctx.Set(arrivalKey, time.Now())
		return next(ctx)
	}
}

// requestDeadline returns when an expensive request must stop: requestTimeLimit after it arrived
// (RecordArrival; now if not recorded). ok is false when there is no limit.
func requestDeadline(ctx echo.Context, cfg config.Local) (deadline time.Time, ok bool) {
	limit := requestTimeLimit(cfg)
	if limit <= 0 {
		return time.Time{}, false
	}
	arrival, recorded := ctx.Get(arrivalKey).(time.Time)
	if !recorded {
		arrival = time.Now()
	}
	return arrival.Add(limit), true
}

// requestContext returns the context an expensive request runs under: the request's own context,
// canceled when the client goes away, with the deadline of requestDeadline. A response written
// after RestWriteTimeoutSeconds is lost, so the work stops before.
func requestContext(ctx echo.Context, cfg config.Local) (context.Context, context.CancelFunc) {
	if deadline, ok := requestDeadline(ctx, cfg); ok {
		return context.WithDeadline(ctx.Request().Context(), deadline)
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

// trySimulate takes a free slot of limiter for more work of an admitted request, without waiting:
// waiting would queue ahead of new requests, which would then be refused as busy.
func trySimulate(limiter *semaphore.Weighted) bool {
	return limiter == nil || limiter.TryAcquire(1)
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

	effort := v2.startEffort(ctx)
	result, err = v2.Node.Simulate(request.WithContext(contextWithEffort(reqCtx, effort)))
	if err != nil {
		if stopped, answerErr := answerStopped(ctx, err, v2.Log); stopped {
			return simulation.Result{}, true, answerErr
		}
	}
	return result, false, err
}

// stateDeltaContextLedger is implemented by ledgers that can stop rebuilding an old round's state
// delta (ledger.Ledger, with time travel).
type stateDeltaContextLedger interface {
	GetStateDeltaForRoundContext(ctx context.Context, rnd basics.Round) (ledgercore.StateDelta, error)
}

// stateDeltaForRound returns the state delta of round rnd. A delta rebuilt from the time travel
// history stops when the client goes away or at the node's time limit, and is refused with 503 when
// every rebuild slot stays busy. answered reports that it already answered the request, with err
// the handler's return value; otherwise err is the lookup's error.
func (v2 *Handlers) stateDeltaForRound(ctx echo.Context, rnd basics.Round) (sd ledgercore.StateDelta, answered bool, err error) {
	l := v2.Node.LedgerForAPI()
	cl, ok := l.(stateDeltaContextLedger)
	if !ok {
		sd, err = l.GetStateDeltaForRound(rnd)
		return sd, false, err
	}
	reqCtx, cancel := requestContext(ctx, v2.Node.Config())
	defer cancel()
	sd, err = cl.GetStateDeltaForRoundContext(contextWithEffort(reqCtx, v2.startEffort(ctx)), rnd)
	if err != nil {
		if errors.Is(err, ledger.ErrReevalBusy) {
			return sd, true, answerBusy(ctx, err, v2.Log)
		}
		if stopped, answerErr := answerStopped(ctx, err, v2.Log); stopped {
			return sd, true, answerErr
		}
	}
	return sd, false, err
}
