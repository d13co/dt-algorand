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

// The X-Algod-Effort response header (EnableEffortHeader). Non-standard fork feature, kept in its
// own file to keep upstream merges of handlers.go clean.

import (
	"context"
	"math"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
)

// EffortHeader is the response header that reports the work a request did.
const EffortHeader = "X-Algod-Effort"

// startEffort returns the Effort the request counts its work in, or nil when EnableEffortHeader is
// off. The first call makes it, and has the response carry it in EffortHeader when its headers are
// written: every answer after this call carries it, except 503s, which report that the node was
// too busy to do the work. A request whose client went away writes nothing, so reports nothing.
func (v2 *Handlers) startEffort(ctx echo.Context) *ledgercore.Effort {
	if !v2.Node.Config().EnableEffortHeader {
		return nil
	}
	if e, ok := ctx.Get(effortKey).(*ledgercore.Effort); ok {
		return e
	}
	e := ledgercore.NewEffort()
	ctx.Set(effortKey, e)
	res := ctx.Response()
	res.Before(func() {
		if res.Status != http.StatusServiceUnavailable {
			res.Header().Set(EffortHeader, e.Header())
		}
	})
	return e
}

const effortKey = "dtalgorand.effort"

// effortTraceBudget returns the trace budget of a simulate request: the node's limit, or with an
// Effort, a budget with no limit when the node sets none, so that the trace size is counted.
func effortTraceBudget(limit uint64, e *ledgercore.Effort) *simulation.TraceBudget {
	if limit == 0 && e != nil {
		limit = math.MaxUint64
	}
	return simulation.NewTraceBudget(limit)
}

// counter is a ledger that can count its lookups in an Effort (ledger.Ledger and data.Ledger).
type counter interface {
	Counted(e *ledgercore.Effort) ledger.CountedLedger
}

// contextWithEffort returns ctx carrying e, or ctx when e is nil.
func contextWithEffort(ctx context.Context, e *ledgercore.Effort) context.Context {
	if e == nil {
		return ctx
	}
	return ledgercore.ContextWithEffort(ctx, e)
}
