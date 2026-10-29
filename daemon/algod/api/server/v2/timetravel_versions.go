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
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"slices"
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
	AppBoxVersions(app basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error)
}

const (
	// DefaultVersions is the page size of the versions endpoints.
	DefaultVersions = 100
	// MaxVersions is the largest page of versions without values.
	MaxVersions = 10_000
	// MaxVersionsWithValues is the largest page of versions with values.
	MaxVersionsWithValues = 1_000
	// MaxVersionsSeeks bounds the lookups of a page with rounds: one per
	// object per requested round. A page always has one round.
	MaxVersionsSeeks = 100_000
	// MaxVersionsReadBytes bounds the values the lookups of a page with
	// rounds read, returned or not: each counts the value of the version in
	// effect and the one before it. It bounds a page with key likewise.
	MaxVersionsReadBytes = 64_000_000
	// MaxVersionsScanned bounds the versions a page with key examines.
	MaxVersionsScanned = 100_000
	// MaxBoxIndexScanned bounds the box index entries a page with prefix
	// steps through.
	MaxBoxIndexScanned = 1_000_000
	// MaxVersionsKeyLen is the longest key or prefix: MaxAppKeyLen, also the
	// longest box name.
	MaxVersionsKeyLen = 64
)

// versionsQuery is the paging every versions endpoint takes.
type versionsQuery struct {
	min, max *basics.Round
	order    *string
	limit    *uint64
	next     *string
	include  []string
	keysOK   bool // the endpoint takes include=keys: it versions app state
	rounds   *[]basics.Round
	key      *string // a state key, in the goal app-arg form
	keyOK    bool    // the endpoint takes key
	prefix   *string // a box name prefix, in the goal app-arg form
	prefixOK bool    // the endpoint takes prefix
}

// parseVersionsBytes parses a key or prefix in the goal app-arg form.
func parseVersionsBytes(param, arg string) ([]byte, error) {
	ab, err := apps.NewAppCallBytes(arg)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", param, err)
	}
	b, err := ab.Raw()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", param, err)
	}
	if len(b) > MaxVersionsKeyLen {
		return nil, fmt.Errorf("%s is longer than %d bytes", param, MaxVersionsKeyLen)
	}
	if b == nil {
		b = []byte{} // nil means no filter
	}
	return b, nil
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
// range to read. Responses report the range's Max as their round: with
// rounds, the largest requested round left after next.
func (v2 *Handlers) versionsRange(q versionsQuery) (versionsLedger, ledger.VersionRange, error) {
	l := v2.Node.LedgerForAPI()
	vl, ok := l.(versionsLedger)
	if !ok || !v2.Node.Config().EnableTimeTravel {
		return nil, ledger.VersionRange{}, errors.New("versions require EnableTimeTravel")
	}
	if hu, ok := l.(historyTrust); ok {
		if err := hu.HistoryUntrusted(); err != nil {
			return nil, ledger.VersionRange{}, err
		}
	}
	r := ledger.VersionRange{Max: l.Latest(), Limit: DefaultVersions}
	for _, inc := range q.include {
		switch inc {
		case "values":
			r.Values = true
		case "keys":
			if !q.keysOK {
				return nil, r, errors.New("include=keys applies to application state")
			}
			r.Keys = true
		default:
			return nil, r, fmt.Errorf("invalid include value: %s", inc)
		}
	}
	if q.rounds != nil && (q.min != nil || q.max != nil) {
		return nil, r, errors.New("rounds cannot be combined with min-round or max-round")
	}
	if q.key != nil {
		if !q.keyOK {
			return nil, r, errors.New("key applies to the versions of an application or of an account's local state of one")
		}
		if q.rounds != nil {
			return nil, r, errors.New("key cannot be combined with rounds")
		}
		k, err := parseVersionsBytes("key", *q.key)
		if err != nil {
			return nil, r, err
		}
		r.Key, r.MaxScanned, r.MaxReadBytes = k, MaxVersionsScanned, MaxVersionsReadBytes
	}
	if q.prefix != nil {
		if !q.prefixOK {
			return nil, r, errors.New("prefix applies to an application's boxes")
		}
		p, err := parseVersionsBytes("prefix", *q.prefix)
		if err != nil {
			return nil, r, err
		}
		if len(p) == 0 {
			return nil, r, errors.New("prefix is empty")
		}
		r.Prefix = p
		if q.rounds == nil {
			r.MaxScanned = MaxBoxIndexScanned
		}
	}
	if q.max != nil {
		if *q.max > r.Max {
			return nil, r, errors.New(errRoundGreaterThanTheLatest)
		}
		r.Max = *q.max
	}
	if q.min != nil {
		if *q.min > r.Max {
			return nil, r, fmt.Errorf("min-round %d is after max-round %d", *q.min, r.Max)
		}
		r.Min = *q.min
	}
	if q.order != nil {
		switch *q.order {
		case "asc":
		case "desc":
			r.Desc = true
		default:
			return nil, r, fmt.Errorf("invalid order: %s", *q.order)
		}
	}
	if q.rounds != nil {
		rs := slices.Compact(slices.Sorted(slices.Values(*q.rounds)))
		switch {
		case len(rs) == 0:
			return nil, r, errors.New("rounds is empty")
		case len(rs) > MaxVersions:
			return nil, r, fmt.Errorf("rounds lists %d distinct rounds, more than %d", len(rs), MaxVersions)
		}
		if rs[len(rs)-1] > r.Max {
			return nil, r, errors.New(errRoundGreaterThanTheLatest)
		}
		if r.Desc {
			slices.Reverse(rs)
		}
		r.Rounds, r.MaxSeeks, r.MaxReadBytes = rs, MaxVersionsSeeks, MaxVersionsReadBytes
	}
	maxLimit := uint64(MaxVersions)
	if r.Values || r.Keys {
		maxLimit, r.MaxBytes = MaxVersionsWithValues, maxBoxFetchBytes
	}
	if q.limit != nil {
		if *q.limit == 0 || *q.limit > maxLimit {
			return nil, r, fmt.Errorf("limit must be between 1 and %d", maxLimit)
		}
		r.Limit = *q.limit
	}
	if q.next != nil {
		n, err := strconv.ParseUint(*q.next, 10, 64)
		if err != nil {
			return nil, r, fmt.Errorf("%s: %w", errUnableToParseNext, err)
		}
		if r.Rounds != nil {
			// the token is the page's first round; drop the rounds before it
			i := slices.IndexFunc(r.Rounds, func(R basics.Round) bool {
				return (r.Desc && R <= basics.Round(n)) || (!r.Desc && R >= basics.Round(n))
			})
			if i < 0 {
				return nil, r, fmt.Errorf("next %s is past the last of rounds", *q.next)
			}
			r.Rounds = r.Rounds[i:]
		} else {
			if r.Desc {
				r.Max = min(r.Max, basics.Round(n))
			} else {
				r.Min = max(r.Min, basics.Round(n))
			}
			// tokens the server made are always in range; this one is from another query
			if r.Min > r.Max {
				return nil, r, fmt.Errorf("next %s is outside min-round to max-round", *q.next)
			}
		}
	}
	if rs := r.Rounds; rs != nil {
		r.Min, r.Max = min(rs[0], rs[len(rs)-1]), max(rs[0], rs[len(rs)-1])
	}
	return vl, r, nil
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
	if errors.Is(err, ledger.ErrNoBoxIndex) {
		return serviceUnavailable(ctx, err, err.Error()+": build it with ttimport -index-boxes while the node is stopped", v2.Log)
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
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: true, rounds: params.Rounds, key: params.Key, prefix: params.Prefix})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	r.MaxObjects = v2.Node.Config().MaxAPIResourcesPerAccount
	page, err := vl.AccountVersions(address, ex, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, r, page)
}

// AccountAssetVersions lists the versions of an account's asset holding.
// (GET /v2/accounts/{address}/assets/{asset-id}/versions)
func (v2 *Handlers) AccountAssetVersions(ctx echo.Context, address basics.Address, assetID basics.AssetIndex, params model.AccountAssetVersionsParams) error {
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: false, rounds: params.Rounds, key: params.Key, prefix: params.Prefix})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	page, err := vl.AccountAssetVersions(address, assetID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, r, page)
}

// AccountApplicationVersions lists the versions of an account's application local state.
// (GET /v2/accounts/{address}/applications/{application-id}/versions)
func (v2 *Handlers) AccountApplicationVersions(ctx echo.Context, address basics.Address, applicationID basics.AppIndex, params model.AccountApplicationVersionsParams) error {
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: true, rounds: params.Rounds, key: params.Key, prefix: params.Prefix, keyOK: true})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	page, err := vl.AccountAppVersions(address, applicationID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	return v2.accountVersionsResponse(ctx, address, r, page)
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

func (v2 *Handlers) accountVersionsResponse(ctx echo.Context, addr basics.Address, r ledger.VersionRange, page ledger.VersionPage) error {
	out := make([]model.AccountVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv, err := AccountVersionToModel(addr, v, r.Keys)
		if err != nil {
			return internalError(ctx, err, errInternalFailure, v2.Log)
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.AccountVersionsResponse{Round: r.Max, NextToken: nextToken(page), Versions: out})
}

// AccountVersionToModel is the REST form of a version of an account's
// objects, from the ledger's AccountVersions, AccountAssetVersions or
// AccountAppVersions, with the changed state keys if keys is set.
func AccountVersionToModel(addr basics.Address, v ledger.Version, keys bool) (model.AccountVersion, error) {
	mv := model.AccountVersion{Round: v.Round, Timestamp: v.Timestamp}
	if v.Account != nil {
		base := model.AccountBaseVersion{Deleted: v.Account.Deleted, ChangedRound: v.Account.Changed}
		if v.Account.Data != nil {
			proto := config.Consensus[v.Proto]
			acct, err := BaseAccount(addr, *v.Account.Data, v.Round, &proto, v.Account.WithoutRewards)
			if err != nil {
				return mv, err
			}
			base.Account = &acct
		}
		mv.Base = &base
	}
	if len(v.Holdings) > 0 {
		hs := make([]model.AssetHoldingVersion, len(v.Holdings))
		for i, h := range v.Holdings {
			hs[i] = model.AssetHoldingVersion{AssetId: h.Asset, Deleted: h.Deleted, ChangedRound: h.Changed}
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
			ls[i] = model.ApplicationLocalStateVersion{AppId: s.App, Deleted: s.Deleted, ChangedRound: s.Changed}
			if s.State != nil {
				ms := AppLocalState(*s.State, s.App)
				ls[i].AppLocalState = &ms
			}
			ls[i].ChangedKeys = changedKeysModel(keys, s.Deleted, s.ChangedKeys)
			ls[i].KeyValue = tealValueModel(s.KeyValue)
		}
		mv.AppsLocalState = &ls
	}
	if len(v.Assets) > 0 {
		as := make([]model.CreatedAssetVersion, len(v.Assets))
		for i, a := range v.Assets {
			as[i] = model.CreatedAssetVersion{AssetId: a.Asset, Deleted: a.Deleted, ChangedRound: a.Changed}
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
			as[i] = model.CreatedApplicationVersion{AppId: a.App, Deleted: a.Deleted, ChangedRound: a.Changed}
			if a.Params != nil {
				ma := AppParamsToApplication(a.Creator.String(), a.App, a.Params)
				as[i].Application = &ma
			}
			as[i].ChangedKeys = changedKeysModel(keys, a.Deleted, a.ChangedKeys)
		}
		mv.CreatedApps = &as
	}
	return mv, nil
}

// AssetVersionToModel is the REST form of a version from the ledger's
// AssetVersions.
func AssetVersionToModel(v ledger.Version) (model.AssetVersion, error) {
	mv := model.AssetVersion{Round: v.Round, Timestamp: v.Timestamp}
	if len(v.Assets) != 1 {
		return mv, fmt.Errorf("asset version at round %d has %d entries", v.Round, len(v.Assets))
	}
	a := v.Assets[0]
	mv.Deleted, mv.ChangedRound = a.Deleted, a.Changed
	if a.Params != nil {
		ma := AssetParamsToAsset(a.Creator.String(), a.Asset, a.Params)
		mv.Asset = &ma
	}
	return mv, nil
}

// changedKeysModel is the REST form of a version's changed state keys: absent
// unless requested, on deletions, and for versions that have none (created
// apps without params); empty, not absent, when only other fields changed.
func changedKeysModel(keys, deleted bool, ks [][]byte) *[][]byte {
	if !keys || deleted || ks == nil {
		return nil
	}
	return &ks
}

// ApplicationVersionToModel is the REST form of a version from the ledger's
// AppVersions, with the changed global state keys if keys is set.
func ApplicationVersionToModel(v ledger.Version, keys bool) (model.ApplicationVersion, error) {
	mv := model.ApplicationVersion{Round: v.Round, Timestamp: v.Timestamp}
	if len(v.Apps) != 1 {
		return mv, fmt.Errorf("application version at round %d has %d entries", v.Round, len(v.Apps))
	}
	a := v.Apps[0]
	mv.Deleted, mv.ChangedRound = a.Deleted, a.Changed
	if a.Params != nil {
		ma := AppParamsToApplication(a.Creator.String(), a.App, a.Params)
		mv.Application = &ma
	}
	mv.ChangedKeys = changedKeysModel(keys, a.Deleted, a.ChangedKeys)
	mv.KeyValue = tealValueModel(a.KeyValue)
	return mv, nil
}

// tealValueModel is the REST form of a state key's value (key-value), nil
// if it has none.
func tealValueModel(tv *basics.TealValue) *model.TealValue {
	if tv == nil {
		return nil
	}
	return &model.TealValue{Type: uint64(tv.Type), Bytes: base64.StdEncoding.EncodeToString([]byte(tv.Bytes)), Uint: tv.Uint}
}

// BoxVersionToModel is the REST form of a version from the ledger's
// KvVersions, with the value if values is set.
func BoxVersionToModel(v ledger.Version, values bool) (model.BoxVersion, error) {
	if v.Box == nil {
		return model.BoxVersion{}, fmt.Errorf("box version at round %d has no entry", v.Round)
	}
	mv := model.BoxVersion{Round: v.Round, Timestamp: v.Timestamp, Deleted: v.Box.Deleted, ChangedRound: v.Box.Changed}
	if values && !v.Box.Deleted {
		mv.Value = &v.Box.Value
	}
	return mv, nil
}

// AssetVersions lists the versions of an asset's params.
// (GET /v2/assets/{asset-id}/versions)
func (v2 *Handlers) AssetVersions(ctx echo.Context, assetID basics.AssetIndex, params model.AssetVersionsParams) error {
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: false, rounds: params.Rounds, key: params.Key, prefix: params.Prefix})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	page, err := vl.AssetVersions(assetID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.AssetVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv, err := AssetVersionToModel(v)
		if err != nil {
			return internalError(ctx, fmt.Errorf("asset %d: %w", assetID, err), errInternalFailure, v2.Log)
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.AssetVersionsResponse{Round: r.Max, NextToken: nextToken(page), Versions: out})
}

// ApplicationVersions lists the versions of an application's params.
// (GET /v2/applications/{application-id}/versions)
func (v2 *Handlers) ApplicationVersions(ctx echo.Context, applicationID basics.AppIndex, params model.ApplicationVersionsParams) error {
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: true, rounds: params.Rounds, key: params.Key, prefix: params.Prefix, keyOK: true})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	page, err := vl.AppVersions(applicationID, r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.ApplicationVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv, err := ApplicationVersionToModel(v, r.Keys)
		if err != nil {
			return internalError(ctx, fmt.Errorf("application %d: %w", applicationID, err), errInternalFailure, v2.Log)
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.ApplicationVersionsResponse{Round: r.Max, NextToken: nextToken(page), Versions: out})
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
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: false, rounds: params.Rounds, key: params.Key, prefix: params.Prefix})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	page, err := vl.KvVersions(apps.MakeBoxKey(uint64(applicationID), string(boxName)), r)
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.BoxVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		mv, err := BoxVersionToModel(v, r.Values)
		if err != nil {
			return internalError(ctx, err, errInternalFailure, v2.Log)
		}
		out = append(out, mv)
	}
	return ctx.JSON(http.StatusOK, model.BoxVersionsResponse{Round: r.Max, NextToken: nextToken(page), Versions: out})
}

// BoxesVersionToModel is the REST form of a version from the ledger's
// AppBoxVersions, with the values if values is set.
func BoxesVersionToModel(v ledger.Version, values bool) model.BoxesVersion {
	mv := model.BoxesVersion{Round: v.Round, Timestamp: v.Timestamp, Boxes: make([]model.BoxChange, len(v.Boxes))}
	for i, b := range v.Boxes {
		mv.Boxes[i] = model.BoxChange{Name: b.Name, Deleted: b.Deleted, ChangedRound: b.Changed}
		if values && !b.Deleted {
			mv.Boxes[i].Value = &v.Boxes[i].Value
		}
	}
	return mv
}

// ApplicationBoxesVersions lists the changes of an application's boxes.
// (GET /v2/applications/{application-id}/boxes/versions)
func (v2 *Handlers) ApplicationBoxesVersions(ctx echo.Context, applicationID basics.AppIndex, params model.ApplicationBoxesVersionsParams) error {
	vl, r, err := v2.versionsRange(versionsQuery{min: params.MinRound, max: params.MaxRound, order: (*string)(params.Order), limit: params.Limit, next: params.Next, include: strs(params.Include), keysOK: false, rounds: params.Rounds, key: params.Key, prefix: params.Prefix, prefixOK: true})
	if err != nil {
		return v2.historyError(ctx, err)
	}
	if r.Rounds != nil {
		// as of a round, every box the app ever had is merged
		r.MaxObjects = v2.Node.Config().MaxAPIBoxPerApplication
		r.MaxEntries = MaxVersions
		if r.Values {
			r.MaxEntries = MaxVersionsWithValues
		}
	}
	page, err := vl.AppBoxVersions(applicationID, r)
	if errors.Is(err, ledger.ErrTooManyVersionObjects) || errors.Is(err, ledger.ErrRoundTooLarge) {
		hint := "prefix, or "
		if r.Prefix != nil {
			hint = ""
		}
		return badRequest(ctx, err, fmt.Sprintf("%v: use %sGET /v2/applications/%d/boxes?round=", err, hint, applicationID), v2.Log)
	}
	if err != nil {
		return v2.versionsError(ctx, err)
	}
	out := make([]model.BoxesVersion, 0, len(page.Versions))
	for _, v := range page.Versions {
		out = append(out, BoxesVersionToModel(v, r.Values))
	}
	return ctx.JSON(http.StatusOK, model.BoxesVersionsResponse{Round: r.Max, NextToken: nextToken(page), Versions: out})
}
