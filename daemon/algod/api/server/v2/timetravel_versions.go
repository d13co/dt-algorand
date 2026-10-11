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
	"strconv"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
)

// versionsLedger is implemented by ledgers that keep the time travel history.
type versionsLedger interface {
	AccountVersions(addr basics.Address, ex ledger.AccountVersionsExclude, r ledger.VersionRange) (ledger.VersionPage, error)
	AccountAssetVersions(addr basics.Address, aidx basics.AssetIndex, r ledger.VersionRange) (ledger.VersionPage, error)
	AccountAppVersions(addr basics.Address, aidx basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error)
	AssetVersions(aidx basics.AssetIndex, r ledger.VersionRange) (ledger.VersionPage, error)
	AppVersions(aidx basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error)
	KvVersions(key string, r ledger.VersionRange) (ledger.VersionPage, error)
}

const (
	// DefaultVersions is the page size of the versions endpoints.
	DefaultVersions = 100
	// MaxVersions is the largest page of versions without values.
	MaxVersions = 10_000
	// MaxVersionsWithValues is the largest page of versions with values.
	MaxVersionsWithValues = 1_000
)

// versionsQuery is the paging every versions endpoint takes.
type versionsQuery struct {
	min, max *basics.Round
	order    *string
	limit    *uint64
	next     *string
	include  []string
}

func strs[T ~string](vals *[]T) []string {
	if vals == nil {
		return nil
	}
	out := make([]string, len(*vals))
	for i, v := range *vals {
		out[i] = string(v)
	}
	return out
}

// versionsRange checks a versions query and returns the history and the
// range to read, and the round responses report.
func (v2 *Handlers) versionsRange(q versionsQuery) (versionsLedger, ledger.VersionRange, basics.Round, error) {
	l := v2.Node.LedgerForAPI()
	vl, ok := l.(versionsLedger)
	if !ok || !v2.Node.Config().EnableTimeTravel {
		return nil, ledger.VersionRange{}, 0, errors.New("versions require EnableTimeTravel")
	}
	r := ledger.VersionRange{Max: l.Latest(), Limit: DefaultVersions}
	for _, inc := range q.include {
		if inc != "values" {
			return nil, r, 0, fmt.Errorf("invalid include value: %s", inc)
		}
		r.Values = true
	}
	if q.max != nil {
		if *q.max > r.Max {
			return nil, r, 0, errors.New(errRoundGreaterThanTheLatest)
		}
		r.Max = *q.max
	}
	if q.min != nil {
		if *q.min > r.Max {
			return nil, r, 0, fmt.Errorf("min-round %d is after max-round %d", *q.min, r.Max)
		}
		r.Min = *q.min
	}
	if q.order != nil {
		switch *q.order {
		case "asc":
		case "desc":
			r.Desc = true
		default:
			return nil, r, 0, fmt.Errorf("invalid order: %s", *q.order)
		}
	}
	maxLimit := uint64(MaxVersions)
	if r.Values {
		maxLimit, r.MaxBytes = MaxVersionsWithValues, maxBoxFetchBytes
	}
	if q.limit != nil {
		if *q.limit == 0 || *q.limit > maxLimit {
			return nil, r, 0, fmt.Errorf("limit must be between 1 and %d", maxLimit)
		}
		r.Limit = *q.limit
	}
	round := r.Max
	if q.next != nil {
		n, err := strconv.ParseUint(*q.next, 10, 64)
		if err != nil {
			return nil, r, 0, fmt.Errorf("%s: %w", errUnableToParseNext, err)
		}
		if r.Desc {
			r.Max = min(r.Max, basics.Round(n))
		} else {
			r.Min = max(r.Min, basics.Round(n))
		}
	}
	return vl, r, round, nil
}

func nextToken(page ledger.VersionPage) *string {
	if !page.More {
		return nil
	}
	s := strconv.FormatUint(uint64(page.Next), 10)
	return &s
}

func (v2 *Handlers) versionsError(ctx echo.Context, err error) error {
	if errors.Is(err, ledger.ErrTooManyVersionObjects) {
		return badRequest(ctx, err, err.Error()+": use exclude or the per-resource versions endpoints", v2.Log)
	}
	return internalError(ctx, err, errFailedLookingUpLedger, v2.Log)
}

// AccountVersions lists the versions of an account's objects.
// (GET /v2/accounts/{address}/versions)
func (v2 *Handlers) AccountVersions(ctx echo.Context, address basics.Address, params model.AccountVersionsParams) error {
	ex, err := parseVersionsExclude(strs(params.Exclude))
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	r.MaxObjects = v2.Node.Config().MaxAPIResourcesPerAccount
	page, err := vl.AccountVersions(address, ex, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, round, page)
}

// AccountAssetVersions lists the versions of an account's asset holding.
// (GET /v2/accounts/{address}/assets/{asset-id}/versions)
func (v2 *Handlers) AccountAssetVersions(ctx echo.Context, address basics.Address, assetID basics.AssetIndex, params model.AccountAssetVersionsParams) error {
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	page, err := vl.AccountAssetVersions(address, assetID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, round, page)
}

// AccountApplicationVersions lists the versions of an account's application local state.
// (GET /v2/accounts/{address}/applications/{application-id}/versions)
func (v2 *Handlers) AccountApplicationVersions(ctx echo.Context, address basics.Address, applicationID basics.AppIndex, params model.AccountApplicationVersionsParams) error {
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	page, err := vl.AccountAppVersions(address, applicationID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, round, page)
}

// parseVersionsExclude parses exclude like AccountInformation does.
func parseVersionsExclude(vals []string) (ledger.AccountVersionsExclude, error) {
	var ex ledger.AccountVersionsExclude
	if len(vals) == 1 {
		switch strings.TrimSpace(vals[0]) {
		case "all":
			ex.All = true
			return ex, nil
		case "none", "":
			return ex, nil
		}
	}
	for _, v := range vals {
		switch v = strings.TrimSpace(v); v {
		case "created-apps-params":
			ex.CreatedAppsParams = true
		case "created-assets-params":
			ex.CreatedAssetsParams = true
		case "all", "none":
			return ex, fmt.Errorf("'%s' cannot be combined with other exclude values", v)
		default:
			return ex, fmt.Errorf("invalid exclude value: %s", v)
		}
	}
	return ex, nil
}

func (v2 *Handlers) accountVersionsResponse(ctx echo.Context, addr basics.Address, round basics.Round, page ledger.VersionPage) error {
	out := make([]model.AccountVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv := model.AccountVersion{Round: v.Round, Timestamp: v.Timestamp}
		if v.Account != nil {
			base := model.AccountBaseVersion{Deleted: v.Account.Deleted}
			if v.Account.Data != nil {
				proto := config.Consensus[v.Proto]
				acct, err := baseAccount(addr, *v.Account.Data, v.Round, &proto, v.Account.WithoutRewards)
				if err != nil {
					return internalError(ctx, err, errInternalFailure, v2.Log)
				}
				base.Account = &acct
			}
			mv.Base = &base
		}
		if len(v.Holdings) > 0 {
			hs := make([]model.AssetHoldingVersion, len(v.Holdings))
			for i, h := range v.Holdings {
				hs[i] = model.AssetHoldingVersion{AssetId: h.Asset, Deleted: h.Deleted}
				if h.Holding != nil {
					mh := AssetHolding(*h.Holding, h.Asset)
					hs[i].AssetHolding = &mh
				}
			}
			mv.Assets = &hs
		}
		if len(v.LocalStates) > 0 {
			ls := make([]model.ApplicationLocalStateVersion, len(v.LocalStates))
			for i, s := range v.LocalStates {
				ls[i] = model.ApplicationLocalStateVersion{AppId: s.App, Deleted: s.Deleted}
				if s.State != nil {
					ms := AppLocalState(*s.State, s.App)
					ls[i].AppLocalState = &ms
				}
			}
			mv.AppsLocalState = &ls
		}
		if len(v.Assets) > 0 {
			as := make([]model.CreatedAssetVersion, len(v.Assets))
			for i, a := range v.Assets {
				as[i] = model.CreatedAssetVersion{AssetId: a.Asset, Deleted: a.Deleted}
				if a.Params != nil {
					ma := AssetParamsToAsset(a.Creator.String(), a.Asset, a.Params)
					as[i].Asset = &ma
				}
			}
			mv.CreatedAssets = &as
		}
		if len(v.Apps) > 0 {
			as := make([]model.CreatedApplicationVersion, len(v.Apps))
			for i, a := range v.Apps {
				as[i] = model.CreatedApplicationVersion{AppId: a.App, Deleted: a.Deleted}
				if a.Params != nil {
					ma := AppParamsToApplication(a.Creator.String(), a.App, a.Params)
					as[i].Application = &ma
				}
			}
			mv.CreatedApps = &as
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.AccountVersionsResponse{Round: round, NextToken: nextToken(page), Versions: out})
}

// AssetVersions lists the versions of an asset's params.
// (GET /v2/assets/{asset-id}/versions)
func (v2 *Handlers) AssetVersions(ctx echo.Context, assetID basics.AssetIndex, params model.AssetVersionsParams) error {
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	page, err := vl.AssetVersions(assetID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.AssetVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv := model.AssetVersion{Round: v.Round, Timestamp: v.Timestamp}
		if len(v.Assets) != 1 {
			return internalError(ctx, fmt.Errorf("asset %d version at round %d has %d entries", assetID, v.Round, len(v.Assets)), errInternalFailure, v2.Log)
		}
		a := v.Assets[0]
		mv.Deleted = a.Deleted
		if a.Params != nil {
			ma := AssetParamsToAsset(a.Creator.String(), a.Asset, a.Params)
			mv.Asset = &ma
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.AssetVersionsResponse{Round: round, NextToken: nextToken(page), Versions: out})
}

// ApplicationVersions lists the versions of an application's params.
// (GET /v2/applications/{application-id}/versions)
func (v2 *Handlers) ApplicationVersions(ctx echo.Context, applicationID basics.AppIndex, params model.ApplicationVersionsParams) error {
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	page, err := vl.AppVersions(applicationID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.ApplicationVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv := model.ApplicationVersion{Round: v.Round, Timestamp: v.Timestamp}
		if len(v.Apps) != 1 {
			return internalError(ctx, fmt.Errorf("application %d version at round %d has %d entries", applicationID, v.Round, len(v.Apps)), errInternalFailure, v2.Log)
		}
		a := v.Apps[0]
		mv.Deleted = a.Deleted
		if a.Params != nil {
			ma := AppParamsToApplication(a.Creator.String(), a.App, a.Params)
			mv.Application = &ma
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.ApplicationVersionsResponse{Round: round, NextToken: nextToken(page), Versions: out})
}

// ApplicationBoxVersions lists the versions of an application's box.
// (GET /v2/applications/{application-id}/box/versions)
func (v2 *Handlers) ApplicationBoxVersions(ctx echo.Context, applicationID basics.AppIndex, params model.ApplicationBoxVersionsParams) error {
	boxNameBytes, err := apps.NewAppCallBytes(params.Name)
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	boxName, err := boxNameBytes.Raw()
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	vl, r, round, err := v2.versionsRange(versionsQuery{params.MinRound, params.MaxRound, (*string)(params.Order), params.Limit, params.Next, strs(params.Include)})
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	page, err := vl.KvVersions(apps.MakeBoxKey(uint64(applicationID), string(boxName)), r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.BoxVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		if v.Box == nil {
			return internalError(ctx, fmt.Errorf("box version at round %d has no entry", v.Round), errInternalFailure, v2.Log)
		}
		mv := model.BoxVersion{Round: v.Round, Timestamp: v.Timestamp, Deleted: v.Box.Deleted}
		if r.Values && !v.Box.Deleted {
			mv.Value = &v.Box.Value
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.BoxVersionsResponse{Round: round, NextToken: nextToken(page), Versions: out})
}
