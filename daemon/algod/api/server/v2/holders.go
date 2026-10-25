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

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// holdersLedger is implemented by ledgers that keep the time travel history.
type holdersLedger interface {
	HoldersIndexState() (timetravel.HoldersState, uint64)
	AssetHoldersAt(rnd basics.Round, aidx basics.AssetIndex, after *basics.Address, limit uint64) (ledger.AssetHoldersPage, error)
}

const (
	// DefaultHolders is the page size of GET /v2/assets/{asset-id}/holders.
	DefaultHolders = 1_000
	// MaxHolders is its largest page.
	MaxHolders = 10_000
)

// GetAssetHolders lists the accounts that hold an asset at a round.
// (GET /v2/assets/{asset-id}/holders)
func (v2 *Handlers) GetAssetHolders(ctx echo.Context, assetID basics.AssetIndex, params model.GetAssetHoldersParams) error {
	cfg := v2.Node.Config()
	l := v2.Node.LedgerForAPI()
	hl, ok := l.(holdersLedger)
	if !ok || !cfg.EnableTimeTravel || !cfg.TimeTravelHoldersIndex {
		err := errors.New("asset holders require EnableTimeTravel and TimeTravelHoldersIndex")
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	if _, n := hl.HoldersIndexState(); n != 1 {
		err := fmt.Errorf("asset holders need a time travel store with N = 1, not %d", n)
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	if hu, ok := l.(historyTrust); ok {
		if err := hu.HistoryUntrusted(); err != nil {
			return v2.historyError(ctx, err)
		}
	}
	limit := uint64(DefaultHolders)
	if params.Limit != nil {
		if *params.Limit == 0 || *params.Limit > MaxHolders {
			err := fmt.Errorf("limit must be between 1 and %d", MaxHolders)
			return badRequest(ctx, err, err.Error(), v2.Log)
		}
		limit = *params.Limit
	}
	var after *basics.Address
	if params.Next != nil {
		a, err := basics.UnmarshalChecksumAddress(*params.Next)
		if err != nil {
			return badRequest(ctx, err, fmt.Sprintf("%s: %v", errUnableToParseNext, err), v2.Log)
		}
		after = &a
	}
	rnd := l.Latest()
	if params.Round != nil {
		if *params.Round > rnd {
			err := errors.New(errRoundGreaterThanTheLatest)
			return badRequest(ctx, err, err.Error(), v2.Log)
		}
		rnd = *params.Round
	}

	page, err := hl.AssetHoldersAt(rnd, assetID, after, limit)
	switch {
	case errors.Is(err, ledger.ErrHistoryUntrusted):
		return serviceUnavailable(ctx, err, err.Error(), v2.Log)
	case errors.Is(err, ledger.ErrNoHoldersIndex):
		return serviceUnavailable(ctx, err, err.Error()+": build it with ttimport -holders-index while the node is stopped", v2.Log)
	case errors.Is(err, ledger.ErrAssetNotFound):
		return notFound(ctx, err, errAssetDoesNotExist, v2.Log)
	case err != nil:
		return internalError(ctx, err, errFailedLookingUpLedger, v2.Log)
	}
	resp := model.AssetHoldersResponse{Round: rnd, Holders: make([]model.AssetHolder, len(page.Holders))}
	for i, h := range page.Holders {
		resp.Holders[i] = model.AssetHolder{Address: h.Address.String(), Amount: h.Holding.Amount, IsFrozen: h.Holding.Frozen}
	}
	if page.More {
		next := page.Next.String()
		resp.NextToken = &next
	}
	return ctx.JSON(http.StatusOK, resp)
}
