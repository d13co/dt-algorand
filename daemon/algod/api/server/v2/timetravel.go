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
	"github.com/algorand/go-algorand/data/bookkeeping"
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
	countedView
	h   historicalLedger
	rnd basics.Round
}

// countedView is a ledger whose lookups at given rounds count in the
// request's Effort (EnableEffortHeader) when counted is set.
type countedView struct {
	LedgerForAPI
	counted *ledger.CountedLedger
}

// countedLedgerFor returns l with its lookups counted in the request's Effort,
// or l when EnableEffortHeader is off.
func (v2 *Handlers) countedLedgerFor(ctx echo.Context, l LedgerForAPI) countedView {
	view := countedView{LedgerForAPI: l}
	if e := v2.startEffort(ctx); e != nil {
		if c, ok := l.(counter); ok {
			counted := c.Counted(e)
			view.counted = &counted
		}
	}
	return view
}

func (v countedView) LookupAccount(round basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, basics.MicroAlgos, error) {
	if v.counted != nil {
		return v.counted.LookupAccount(round, addr)
	}
	return v.LedgerForAPI.LookupAccount(round, addr)
}

func (v countedView) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	if v.counted != nil {
		return v.counted.LookupAsset(rnd, addr, aidx)
	}
	return v.LedgerForAPI.LookupAsset(rnd, addr, aidx)
}

func (v countedView) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	if v.counted != nil {
		return v.counted.LookupApplication(rnd, addr, aidx)
	}
	return v.LedgerForAPI.LookupApplication(rnd, addr, aidx)
}

func (v countedView) LookupKv(round basics.Round, key string) ([]byte, error) {
	if v.counted != nil {
		return v.counted.LookupKv(round, key)
	}
	return v.LedgerForAPI.LookupKv(round, key)
}

func (v countedView) LookupKeysByPrefix(round basics.Round, keyPrefix string, maxKeyNum uint64) ([]string, error) {
	if v.counted != nil {
		return v.counted.LookupKeysByPrefix(round, keyPrefix, maxKeyNum)
	}
	return v.LedgerForAPI.LookupKeysByPrefix(round, keyPrefix, maxKeyNum)
}

func (v countedView) LookupKvPairsByPrefix(round basics.Round, keyPrefix string, cursor string, limit uint64, maxBytes uint64, includeValues bool) ([]ledgercore.KvPairResult, basics.Round, bool, error) {
	if v.counted != nil {
		return v.counted.LookupKvPairsByPrefix(round, keyPrefix, cursor, limit, maxBytes, includeValues)
	}
	return v.LedgerForAPI.LookupKvPairsByPrefix(round, keyPrefix, cursor, limit, maxBytes, includeValues)
}

func (v countedView) OnlineCirculation(rnd basics.Round, voteRnd basics.Round) (basics.MicroAlgos, error) {
	if v.counted != nil {
		return v.counted.OnlineCirculation(rnd, voteRnd)
	}
	return v.LedgerForAPI.OnlineCirculation(rnd, voteRnd)
}

func (v countedView) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	if v.counted != nil {
		return v.counted.BlockHdr(rnd)
	}
	return v.LedgerForAPI.BlockHdr(rnd)
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
// (the at-round parameter), the ledger as of round R, whose lookups count in
// the request's Effort (EnableEffortHeader).
func (v2 *Handlers) ledgerFor(ctx echo.Context, rnd *basics.Round) (LedgerForAPI, error) {
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
	view := roundView{countedView: v2.countedLedgerFor(ctx, l), h: h, rnd: *rnd}
	if view.counted != nil {
		view.h = *view.counted
	}
	return view, nil
}
