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
	"time"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/config"
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
