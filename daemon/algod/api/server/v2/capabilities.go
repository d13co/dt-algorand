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

// The fork features and request limits of this node (GET /v2/capabilities). Non-standard fork
// feature, kept in its own file to keep upstream merges of handlers.go clean.

import (
	"net/http"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/ledger/simulation"
)

// GetCapabilities returns the fork features this node has turned on, and the limits it places on
// simulate and query requests.
// (GET /v2/capabilities)
func (v2 *Handlers) GetCapabilities(ctx echo.Context) error {
	return ctx.JSON(http.StatusOK, capabilities(v2.Node.Config()))
}

// timeLimitSeconds returns limit in whole seconds, at least 1 for a limit under a second, since 0
// means no limit.
func timeLimitSeconds(limit time.Duration) uint64 {
	if limit <= 0 {
		return 0
	}
	return uint64(max(limit/time.Second, 1))
}

// capabilities returns the capabilities of a node configured with cfg. The limits come from
// Request.WithNodeLimits and QueryLimitsFromConfig, clamped with the functions the handlers check
// requests with.
func capabilities(cfg config.Local) model.CapabilitiesResponse {
	limits := simulation.Request{}.WithNodeLimits(cfg)
	var query *model.QueryLimits
	if q := simulation.QueryLimitsFromConfig(cfg); q.MaxCalls > 0 {
		query = &model.QueryLimits{
			MaxCalls:          uint64(q.MaxCalls),
			MaxCallBudget:     uint64(q.MaxCallBudget),
			MaxCallLogBytes:   uint64(q.MaxCallLogBytes),
			MaxCallStateBytes: q.MaxCallStateBytes,
			MaxResponseBytes:  q.MaxResponseBytes,
			MaxRounds:         uint64(q.MaxRounds),
		}
	}
	return model.CapabilitiesResponse{
		TimeTravel:       cfg.EnableTimeTravel,
		DeveloperApi:     cfg.EnableDeveloperAPI,
		TimeLimitSeconds: timeLimitSeconds(requestTimeLimit(cfg)),
		Simulate: model.SimulateLimits{
			MaxTxnGroups:            uint64(simulation.EffectiveMaxGroups(limits.MaxGroups)),
			MaxRounds:               uint64(effectiveMaxSimulateRounds(cfg.MaxAPISimulateRounds)),
			MaxExtraOpcodeBudget:    uint64(simulation.EffectiveMaxExtraOpcodeBudget(limits.MaxExtraOpcodeBudget)),
			ResourceLimitMultiplier: uint64(simulation.EffectiveResourceLimitMultiplier(limits.ResourceLimitMultiplier)),
		},
		Query: query,
	}
}
