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

package test

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// versionsLedger records the last versions lookup and answers with page.
type versionsLedger struct {
	*mockLedger
	page ledger.VersionPage
	err  error

	call string // lookup and its arguments
	r    ledger.VersionRange
	ex   ledger.AccountVersionsExclude
}

func (l *versionsLedger) AccountVersions(addr basics.Address, ex ledger.AccountVersionsExclude, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.ex, l.r = "account "+addr.String(), ex, r
	return l.page, l.err
}
func (l *versionsLedger) AccountAssetVersions(addr basics.Address, aidx basics.AssetIndex, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = "holding "+addr.String(), r
	return l.page, l.err
}
func (l *versionsLedger) AccountAppVersions(addr basics.Address, aidx basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = "local "+addr.String(), r
	return l.page, l.err
}
func (l *versionsLedger) AssetVersions(aidx basics.AssetIndex, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = "asset", r
	return l.page, l.err
}
func (l *versionsLedger) AppVersions(aidx basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = "app", r
	return l.page, l.err
}
func (l *versionsLedger) KvVersions(key string, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = "kv "+key, r
	return l.page, l.err
}
func (l *versionsLedger) AppBoxVersions(app basics.AppIndex, r ledger.VersionRange) (ledger.VersionPage, error) {
	l.call, l.r = fmt.Sprintf("boxes %d", app), r
	return l.page, l.err
}

func TestVersionsParams(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAPIResourcesPerAccount = 77
	vl := &versionsLedger{mockLedger: &mockLedger{latest: 1000}}
	get := func(lgr v2.LedgerForAPI, cfg config.Local, params model.AccountVersionsParams) (int, model.AccountVersionsResponse) {
		h := v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, h.AccountVersions(ctx, addr, params))
		var resp model.AccountVersionsResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		}
		return rec.Code, resp
	}
	rnd := func(r basics.Round) *basics.Round { return &r }
	u64 := func(u uint64) *uint64 { return &u }
	str := func(s string) *string { return &s }
	order := func(o model.AccountVersionsParamsOrder) *model.AccountVersionsParamsOrder { return &o }
	values := &[]model.AccountVersionsParamsInclude{model.AccountVersionsParamsIncludeValues}

	for _, tc := range []struct {
		params model.AccountVersionsParams
		want   ledger.VersionRange
		ex     ledger.AccountVersionsExclude
		round  basics.Round
	}{
		{model.AccountVersionsParams{}, ledger.VersionRange{Max: 1000, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		{model.AccountVersionsParams{MinRound: rnd(5), MaxRound: rnd(50), Order: order(model.AccountVersionsParamsOrderDesc), Limit: u64(10_000)},
			ledger.VersionRange{Min: 5, Max: 50, Desc: true, Limit: 10_000, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 50},
		{model.AccountVersionsParams{Include: values, Limit: u64(1000)},
			ledger.VersionRange{Max: 1000, Limit: 1000, Values: true, MaxBytes: 1_000_000, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		// next resumes from the token: ascending from it, descending down from it
		{model.AccountVersionsParams{MinRound: rnd(5), Next: str("20")}, ledger.VersionRange{Min: 20, Max: 1000, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		{model.AccountVersionsParams{MaxRound: rnd(50), Order: order(model.AccountVersionsParamsOrderDesc), Next: str("20")},
			ledger.VersionRange{Max: 20, Desc: true, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 20},
		{model.AccountVersionsParams{Exclude: &[]model.AccountVersionsParamsExclude{model.AccountVersionsParamsExcludeAll}},
			ledger.VersionRange{Max: 1000, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{All: true}, 1000},
		{model.AccountVersionsParams{Exclude: &[]model.AccountVersionsParamsExclude{model.AccountVersionsParamsExcludeCreatedAppsParams, model.AccountVersionsParamsExcludeCreatedAssetsParams}},
			ledger.VersionRange{Max: 1000, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{CreatedAppsParams: true, CreatedAssetsParams: true}, 1000},
	} {
		code, resp := get(vl, cfg, tc.params)
		require.Equal(t, http.StatusOK, code)
		require.Equal(t, "account "+addr.String(), vl.call)
		require.Equal(t, tc.want, vl.r)
		require.Equal(t, tc.ex, vl.ex)
		require.Equal(t, tc.round, resp.Round)
	}

	for name, params := range map[string]model.AccountVersionsParams{
		"future max-round":     {MaxRound: rnd(1001)},
		"min after max":        {MinRound: rnd(51), MaxRound: rnd(50)},
		"limit 0":              {Limit: u64(0)},
		"limit too big":        {Limit: u64(10_001)},
		"limit with values":    {Limit: u64(1001), Include: values},
		"bad order":            {Order: order("sideways")},
		"bad include":          {Include: &[]model.AccountVersionsParamsInclude{"params"}},
		"bad next":             {Next: str("x")},
		"next after max":       {MaxRound: rnd(50), Next: str("51")},
		"next before min":      {MinRound: rnd(5), Order: order(model.AccountVersionsParamsOrderDesc), Next: str("4")},
		"all combined":         {Exclude: &[]model.AccountVersionsParamsExclude{model.AccountVersionsParamsExcludeAll, model.AccountVersionsParamsExcludeCreatedAppsParams}},
		"unknown exclude":      {Exclude: &[]model.AccountVersionsParamsExclude{"holdings"}},
		"time travel disabled": {},
	} {
		c := cfg
		if name == "time travel disabled" {
			c.EnableTimeTravel = false
		}
		code, _ := get(vl, c, params)
		require.Equal(t, http.StatusBadRequest, code, name)
	}
	code, _ := get(vl.mockLedger, cfg, model.AccountVersionsParams{})
	require.Equal(t, http.StatusBadRequest, code, "ledger without history")

	vl.err = errors.Join(ledger.ErrTooManyVersionObjects, errors.New("account has more than 77"))
	code, _ = get(vl, cfg, model.AccountVersionsParams{})
	require.Equal(t, http.StatusBadRequest, code)
	vl.err = errors.New("disk on fire")
	code, _ = get(vl, cfg, model.AccountVersionsParams{})
	require.Equal(t, http.StatusInternalServerError, code)
}

func TestVersionsResponses(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	vl := &versionsLedger{mockLedger: &mockLedger{latest: 1000}}
	h := v2.Handlers{Node: makeMockNodeWithConfig(vl, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
	call := func(fn func(echo.Context) error, resp any) {
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, fn(ctx))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), resp))
	}

	ad := ledgercore.AccountData{AccountBaseData: ledgercore.AccountBaseData{MicroAlgos: basics.MicroAlgos{Raw: 5_000_100}, TotalAssets: 1}}
	assetParams := basics.AssetParams{Total: 10, UnitName: "x"}
	appParams := basics.AppParams{ApprovalProgram: []byte{1}, GlobalState: basics.TealKeyValue{"n": {Type: basics.TealUintType, Uint: 3}}}
	vl.page = ledger.VersionPage{More: true, Next: 30, Versions: []ledger.Version{
		{Round: 10, Timestamp: 1700000000, Proto: protocol.ConsensusFuture,
			Account:     &ledger.AccountVersion{Data: &ad, WithoutRewards: basics.MicroAlgos{Raw: 5_000_000}},
			Holdings:    []ledger.HoldingVersion{{Asset: 7, Holding: &basics.AssetHolding{Amount: 3}}, {Asset: 8, Deleted: true}},
			LocalStates: []ledger.LocalStateVersion{{App: 9, State: &basics.AppLocalState{}}},
			Assets:      []ledger.AssetVersion{{Asset: 7, Creator: addr, Params: &assetParams}},
			Apps:        []ledger.AppVersion{{App: 9, Creator: addr, Params: &appParams}}},
		{Round: 20, Timestamp: 1700000004, Proto: protocol.ConsensusFuture,
			Account: &ledger.AccountVersion{Deleted: true}},
	}}
	var acct model.AccountVersionsResponse
	call(func(ctx echo.Context) error { return h.AccountVersions(ctx, addr, model.AccountVersionsParams{}) }, &acct)
	require.Equal(t, basics.Round(1000), acct.Round)
	require.Equal(t, "30", *acct.NextToken)
	require.Len(t, acct.Versions, 2)
	v := acct.Versions[0]
	require.Equal(t, basics.Round(10), v.Round)
	require.Equal(t, int64(1700000000), v.Timestamp)
	require.False(t, v.Base.Deleted)
	require.Equal(t, addr.String(), v.Base.Account.Address)
	require.Equal(t, basics.Round(10), v.Base.Account.Round)
	require.Equal(t, uint64(5_000_100), v.Base.Account.Amount)
	require.Equal(t, uint64(5_000_000), v.Base.Account.AmountWithoutPendingRewards)
	require.Equal(t, uint64(100), v.Base.Account.PendingRewards)
	require.Equal(t, []model.AssetHoldingVersion{
		{AssetId: 7, AssetHolding: &model.AssetHolding{AssetID: 7, Amount: 3}},
		{AssetId: 8, Deleted: true},
	}, *v.Assets)
	require.Equal(t, basics.AppIndex(9), (*v.AppsLocalState)[0].AppId)
	require.Equal(t, basics.AppIndex(9), (*v.AppsLocalState)[0].AppLocalState.Id)
	require.Equal(t, "x", *(*v.CreatedAssets)[0].Asset.Params.UnitName)
	require.Equal(t, addr.String(), (*v.CreatedAssets)[0].Asset.Params.Creator)
	require.Equal(t, uint64(3), (*(*v.CreatedApps)[0].Application.Params.GlobalState)[0].Value.Uint)
	require.Equal(t, &model.AccountBaseVersion{Deleted: true}, acct.Versions[1].Base)
	require.Nil(t, acct.Versions[1].Assets)

	var holding model.AccountVersionsResponse
	call(func(ctx echo.Context) error {
		return h.AccountAssetVersions(ctx, addr, 7, model.AccountAssetVersionsParams{})
	}, &holding)
	require.Equal(t, "holding "+addr.String(), vl.call)
	require.Equal(t, acct, holding)

	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Assets: []ledger.AssetVersion{{Asset: 7, Creator: addr, Params: &assetParams}}},
		{Round: 12, Timestamp: 2, Assets: []ledger.AssetVersion{{Asset: 7, Deleted: true}}},
	}}
	var assets model.AssetVersionsResponse
	call(func(ctx echo.Context) error { return h.AssetVersions(ctx, 7, model.AssetVersionsParams{}) }, &assets)
	require.Nil(t, assets.NextToken)
	require.Len(t, assets.Versions, 2)
	require.Equal(t, basics.AssetIndex(7), assets.Versions[0].Asset.Index)
	require.Equal(t, model.AssetVersion{Round: 12, Timestamp: 2, Deleted: true}, assets.Versions[1])

	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Apps: []ledger.AppVersion{{App: 9, Creator: addr, Params: &appParams}}},
	}}
	var appVersions model.ApplicationVersionsResponse
	call(func(ctx echo.Context) error { return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{}) }, &appVersions)
	require.Equal(t, basics.AppIndex(9), appVersions.Versions[0].Application.Id)

	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Box: &ledger.BoxVersion{Value: []byte("v1")}},
		{Round: 11, Timestamp: 2, Box: &ledger.BoxVersion{Deleted: true}},
	}}
	values := &[]model.ApplicationBoxVersionsParamsInclude{model.ApplicationBoxVersionsParamsIncludeValues}
	var boxes model.BoxVersionsResponse
	call(func(ctx echo.Context) error {
		return h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "str:b", Include: values})
	}, &boxes)
	require.Equal(t, "kv "+apps.MakeBoxKey(9, "b"), vl.call)
	require.Equal(t, []byte("v1"), *boxes.Versions[0].Value)
	require.Equal(t, model.BoxVersion{Round: 11, Timestamp: 2, Deleted: true}, boxes.Versions[1])

	rec := httptest.NewRecorder()
	ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "nope"}))
	require.Equal(t, http.StatusBadRequest, rec.Code, "bad box name")

	vl.page = ledger.VersionPage{More: true, Next: 12, Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Boxes: []ledger.BoxChange{{Name: []byte("a"), Value: []byte("v1")}, {Name: []byte("b"), Deleted: true}}},
		{Round: 11, Timestamp: 2, Boxes: []ledger.BoxChange{{Name: []byte("a"), Value: []byte{}}}},
	}}
	boxesValues := &[]model.ApplicationBoxesVersionsParamsInclude{model.ApplicationBoxesVersionsParamsIncludeValues}
	var feed model.BoxesVersionsResponse
	call(func(ctx echo.Context) error {
		return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Include: boxesValues})
	}, &feed)
	require.Equal(t, "boxes 9", vl.call)
	require.True(t, vl.r.Values)
	require.Equal(t, "12", *feed.NextToken)
	v1, empty := []byte("v1"), []byte{}
	require.Equal(t, []model.BoxesVersion{
		{Round: 10, Timestamp: 1, Boxes: []model.BoxChange{{Name: []byte("a"), Value: &v1}, {Name: []byte("b"), Deleted: true}}},
		{Round: 11, Timestamp: 2, Boxes: []model.BoxChange{{Name: []byte("a"), Value: &empty}}},
	}, feed.Versions, "an empty value is not a deletion")
	var rounds model.BoxesVersionsResponse
	call(func(ctx echo.Context) error {
		return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{})
	}, &rounds)
	require.Nil(t, rounds.Versions[0].Boxes[0].Value, "no values unless included")

	vl.err = fmt.Errorf("lookup: %w before round 7", ledger.ErrNoBoxIndex)
	rec = httptest.NewRecorder()
	ctx = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{}))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "ttimport -index-boxes")
}
