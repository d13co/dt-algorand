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

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// errOnlineStakeUnavailable: the history has no online stake for the round
// (GetSupply's local ledger variable shadows the package).
var errOnlineStakeUnavailable = ledger.ErrOnlineStakeUnavailable

// historicalLedger is implemented by ledgers that can project state at past
// rounds (EnableTimeTravel).
type historicalLedger interface {
	LookupLatestAt(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error)
	LookupAssetsAt(rnd basics.Round, addr basics.Address, assetIDGT basics.AssetIndex, limit uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error)
	LookupApplicationsAt(rnd basics.Round, addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error)
	GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error)
	Totals(rnd basics.Round) (ledgercore.AccountTotals, error)
}

// roundView is the ledger as of a past round: handlers that read the
// "latest" state read the state at rnd instead.
type roundView struct {
	LedgerForAPI
	h   historicalLedger
	rnd basics.Round
}

func (v roundView) Latest() basics.Round { return v.rnd }

func (v roundView) LookupLatest(addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	return v.h.LookupLatestAt(v.rnd, addr)
}

func (v roundView) LookupAssets(addr basics.Address, assetIDGT basics.AssetIndex, limit uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	return v.h.LookupAssetsAt(v.rnd, addr, assetIDGT, limit)
}

func (v roundView) LookupApplications(addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	return v.h.LookupApplicationsAt(v.rnd, addr, appIDGT, limit, includeParams)
}

func (v roundView) GetCreator(cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	return v.h.GetCreatorForRound(v.rnd, cidx, ctype)
}

func (v roundView) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	t, err := v.h.Totals(v.rnd)
	return v.rnd, t, err
}

// ledgerFor returns the node's ledger or, when the request has ?round=R
// (the at-round parameter), the ledger as of round R.
func (v2 *Handlers) ledgerFor(rnd *basics.Round) (LedgerForAPI, error) {
	l := v2.Node.LedgerForAPI()
	if rnd == nil {
		return l, nil
	}
	if *rnd > l.Latest() {
		return nil, errors.New(errRoundGreaterThanTheLatest)
	}
	h, ok := l.(historicalLedger)
	if !ok || !v2.Node.Config().EnableTimeTravel {
		return nil, errors.New("looking up past rounds requires EnableTimeTravel")
	}
	if hu, ok := l.(historyTrust); ok {
		if err := hu.HistoryUntrusted(); err != nil {
			return nil, err
		}
	}
	return roundView{LedgerForAPI: l, h: h, rnd: *rnd}, nil
}

// historyTrust is implemented by the node's ledger: a history found to differ
// from the chain is not served.
type historyTrust interface {
	HistoryUntrusted() error
}

// historyError answers a request whose ledgerFor or versionsRange failed: 503
// for an untrusted history, 400 otherwise.
func (v2 *Handlers) historyError(ctx echo.Context, err error) error {
	if errors.Is(err, ledger.ErrHistoryUntrusted) {
		return serviceUnavailable(ctx, err, err.Error(), v2.Log)
	}
	return badRequest(ctx, err, err.Error(), v2.Log)
}
