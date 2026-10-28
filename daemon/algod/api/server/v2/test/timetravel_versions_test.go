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
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/timetravel"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
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
	keys := &[]model.AccountVersionsParamsInclude{model.AccountVersionsParamsIncludeKeys}
	valuesKeys := &[]model.AccountVersionsParamsInclude{model.AccountVersionsParamsIncludeValues, model.AccountVersionsParamsIncludeKeys}

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
		{model.AccountVersionsParams{Include: keys, Limit: u64(1000)},
			ledger.VersionRange{Max: 1000, Limit: 1000, Keys: true, MaxBytes: 1_000_000, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		{model.AccountVersionsParams{Include: valuesKeys},
			ledger.VersionRange{Max: 1000, Limit: 100, Values: true, Keys: true, MaxBytes: 1_000_000, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		// next resumes from the token: ascending from it, descending down from it
		{model.AccountVersionsParams{MinRound: rnd(5), Next: str("20")}, ledger.VersionRange{Min: 20, Max: 1000, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 1000},
		{model.AccountVersionsParams{MaxRound: rnd(50), Order: order(model.AccountVersionsParamsOrderDesc), Next: str("20")},
			ledger.VersionRange{Max: 20, Desc: true, Limit: 100, MaxObjects: 77}, ledger.AccountVersionsExclude{}, 20},
		// rounds: sorted and deduplicated in page order, bounded by Min and
		// Max, which the response reports
		{model.AccountVersionsParams{Rounds: &[]basics.Round{30, 5, 30, 7, 1000}},
			ledger.VersionRange{Min: 5, Max: 1000, Limit: 100, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{5, 7, 30, 1000}}, ledger.AccountVersionsExclude{}, 1000},
		{model.AccountVersionsParams{Rounds: &[]basics.Round{30, 5, 7}, Order: order(model.AccountVersionsParamsOrderDesc), Include: values},
			ledger.VersionRange{Min: 5, Max: 30, Desc: true, Limit: 100, Values: true, MaxBytes: 1_000_000, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{30, 7, 5}}, ledger.AccountVersionsExclude{}, 30},
		{model.AccountVersionsParams{Rounds: func() *[]basics.Round { rs := make([]basics.Round, 10_001); return &rs }()},
			ledger.VersionRange{Limit: 100, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{0}}, ledger.AccountVersionsExclude{}, 0},
		// next drops the rounds before the token, in page order
		{model.AccountVersionsParams{Rounds: &[]basics.Round{5, 7, 30}, Next: str("7")},
			ledger.VersionRange{Min: 7, Max: 30, Limit: 100, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{7, 30}}, ledger.AccountVersionsExclude{}, 30},
		{model.AccountVersionsParams{Rounds: &[]basics.Round{5, 7, 30}, Next: str("6")},
			ledger.VersionRange{Min: 7, Max: 30, Limit: 100, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{7, 30}}, ledger.AccountVersionsExclude{}, 30},
		{model.AccountVersionsParams{Rounds: &[]basics.Round{5, 7, 30}, Order: order(model.AccountVersionsParamsOrderDesc), Next: str("7")},
			ledger.VersionRange{Min: 5, Max: 7, Desc: true, Limit: 100, MaxObjects: 77, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{7, 5}}, ledger.AccountVersionsExclude{}, 7},
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
		"limit with keys":      {Limit: u64(1001), Include: keys},
		"bad order":            {Order: order("sideways")},
		"bad include":          {Include: &[]model.AccountVersionsParamsInclude{"params"}},
		"bad next":             {Next: str("x")},
		"next after max":       {MaxRound: rnd(50), Next: str("51")},
		"next before min":      {MinRound: rnd(5), Order: order(model.AccountVersionsParamsOrderDesc), Next: str("4")},
		"all combined":         {Exclude: &[]model.AccountVersionsParamsExclude{model.AccountVersionsParamsExcludeAll, model.AccountVersionsParamsExcludeCreatedAppsParams}},
		"unknown exclude":      {Exclude: &[]model.AccountVersionsParamsExclude{"holdings"}},
		"time travel disabled": {},
		"rounds and min-round": {Rounds: &[]basics.Round{5}, MinRound: rnd(1)},
		"rounds and max-round": {Rounds: &[]basics.Round{5}, MaxRound: rnd(10)},
		"future round":         {Rounds: &[]basics.Round{5, 1001}},
		"no rounds":            {Rounds: &[]basics.Round{}},
		"too many rounds": {Rounds: func() *[]basics.Round {
			rs := make([]basics.Round, 10_001)
			for i := range rs {
				rs[i] = basics.Round(i) // latest is 1000: distinct ones count first
			}
			return &rs
		}()},
		"next past rounds":      {Rounds: &[]basics.Round{5, 7}, Next: str("8")},
		"next past rounds desc": {Rounds: &[]basics.Round{5, 7}, Order: order(model.AccountVersionsParamsOrderDesc), Next: str("4")},
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

	// include=keys is for the endpoints that version app state
	h := v2.Handlers{Node: makeMockNodeWithConfig(vl, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
	for name, tc := range map[string]struct {
		fn   func(echo.Context) error
		want int
	}{
		"local state": {func(ctx echo.Context) error {
			return h.AccountApplicationVersions(ctx, addr, 9, model.AccountApplicationVersionsParams{
				Include: &[]model.AccountApplicationVersionsParamsInclude{model.AccountApplicationVersionsParamsIncludeKeys}})
		}, http.StatusOK},
		"app": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{
				Include: &[]model.ApplicationVersionsParamsInclude{model.ApplicationVersionsParamsIncludeValues, model.ApplicationVersionsParamsIncludeKeys}})
		}, http.StatusOK},
		"asset": {func(ctx echo.Context) error {
			return h.AssetVersions(ctx, 7, model.AssetVersionsParams{Include: &[]model.AssetVersionsParamsInclude{"keys"}})
		}, http.StatusBadRequest},
		"holding": {func(ctx echo.Context) error {
			return h.AccountAssetVersions(ctx, addr, 7, model.AccountAssetVersionsParams{Include: &[]model.AccountAssetVersionsParamsInclude{"keys"}})
		}, http.StatusBadRequest},
		"box": {func(ctx echo.Context) error {
			return h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "str:b", Include: &[]model.ApplicationBoxVersionsParamsInclude{"keys"}})
		}, http.StatusBadRequest},
		"boxes": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Include: &[]model.ApplicationBoxesVersionsParamsInclude{"keys"}})
		}, http.StatusBadRequest},
	} {
		vl.r = ledger.VersionRange{}
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, tc.fn(ctx))
		require.Equal(t, tc.want, rec.Code, name)
		if tc.want == http.StatusOK {
			require.True(t, vl.r.Keys, name)
			require.EqualValues(t, 1_000_000, vl.r.MaxBytes, name)
		} else {
			require.Contains(t, rec.Body.String(), "include=keys applies to application state", name)
		}
	}

	// key is for the app state endpoints, prefix for an app's boxes; both
	// are parsed as app args and bound the scan
	key64, key65 := "b64:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 64)), "b64:"+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{1}, 65))
	for name, tc := range map[string]struct {
		fn   func(echo.Context) error
		want ledger.VersionRange // zero: 400
		msg  string
	}{
		"key on app": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("str:count")})
		}, ledger.VersionRange{Max: 1000, Limit: 100, Key: []byte("count"), MaxScanned: 100_000, MaxReadBytes: 64_000_000}, ""},
		"empty key": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("str:")})
		}, ledger.VersionRange{Max: 1000, Limit: 100, Key: []byte{}, MaxScanned: 100_000, MaxReadBytes: 64_000_000}, ""},
		"64-byte key with values": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str(key64),
				Include: &[]model.ApplicationVersionsParamsInclude{model.ApplicationVersionsParamsIncludeValues}})
		}, ledger.VersionRange{Max: 1000, Limit: 100, Values: true, MaxBytes: 1_000_000, Key: bytes.Repeat([]byte{1}, 64), MaxScanned: 100_000, MaxReadBytes: 64_000_000}, ""},
		"key on local state": {func(ctx echo.Context) error {
			return h.AccountApplicationVersions(ctx, addr, 9, model.AccountApplicationVersionsParams{Key: str("int:7")})
		}, ledger.VersionRange{Max: 1000, Limit: 100, Key: []byte{0, 0, 0, 0, 0, 0, 0, 7}, MaxScanned: 100_000, MaxReadBytes: 64_000_000}, ""},
		"prefix on boxes": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("str:u")})
		}, ledger.VersionRange{Max: 1000, Limit: 100, Prefix: []byte("u"), MaxScanned: 1_000_000}, ""},
		"prefix on boxes as of rounds": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("str:u"), Rounds: &[]basics.Round{5}})
		}, ledger.VersionRange{Min: 5, Max: 5, Limit: 100, Prefix: []byte("u"), MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{5},
			MaxObjects: cfg.MaxAPIBoxPerApplication, MaxEntries: 10_000}, ""},
		"key on account": {func(ctx echo.Context) error {
			return h.AccountVersions(ctx, addr, model.AccountVersionsParams{Key: str("str:a")})
		}, ledger.VersionRange{}, "key applies to application state"},
		"key on holding": {func(ctx echo.Context) error {
			return h.AccountAssetVersions(ctx, addr, 7, model.AccountAssetVersionsParams{Key: str("str:a")})
		}, ledger.VersionRange{}, "key applies to application state"},
		"key on asset": {func(ctx echo.Context) error {
			return h.AssetVersions(ctx, 7, model.AssetVersionsParams{Key: str("str:a")})
		}, ledger.VersionRange{}, "key applies to application state"},
		"key on box": {func(ctx echo.Context) error {
			return h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "str:b", Key: str("str:a")})
		}, ledger.VersionRange{}, "key applies to application state"},
		"key on boxes": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Key: str("str:a")})
		}, ledger.VersionRange{}, "key applies to application state"},
		"prefix on app": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"prefix on local state": {func(ctx echo.Context) error {
			return h.AccountApplicationVersions(ctx, addr, 9, model.AccountApplicationVersionsParams{Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"prefix on account": {func(ctx echo.Context) error {
			return h.AccountVersions(ctx, addr, model.AccountVersionsParams{Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"prefix on holding": {func(ctx echo.Context) error {
			return h.AccountAssetVersions(ctx, addr, 7, model.AccountAssetVersionsParams{Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"prefix on asset": {func(ctx echo.Context) error {
			return h.AssetVersions(ctx, 7, model.AssetVersionsParams{Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"prefix on box": {func(ctx echo.Context) error {
			return h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "str:b", Prefix: str("str:a")})
		}, ledger.VersionRange{}, "prefix applies to an application's boxes"},
		"key with rounds": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("str:a"), Rounds: &[]basics.Round{5}})
		}, ledger.VersionRange{}, "key cannot be combined with rounds"},
		"bad key": {func(ctx echo.Context) error {
			return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("nope")})
		}, ledger.VersionRange{}, "key:"},
		"long key": {func(ctx echo.Context) error {
			return h.AccountApplicationVersions(ctx, addr, 9, model.AccountApplicationVersionsParams{Key: str(key65)})
		}, ledger.VersionRange{}, "key is longer than 64 bytes"},
		"bad prefix": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("b64:!")})
		}, ledger.VersionRange{}, "prefix:"},
		"long prefix": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str(key65)})
		}, ledger.VersionRange{}, "prefix is longer than 64 bytes"},
		"empty prefix": {func(ctx echo.Context) error {
			return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("str:")})
		}, ledger.VersionRange{}, "prefix is empty"},
	} {
		vl.r = ledger.VersionRange{}
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, tc.fn(ctx))
		if tc.msg != "" {
			require.Equal(t, http.StatusBadRequest, rec.Code, name)
			require.Contains(t, rec.Body.String(), tc.msg, name)
			continue
		}
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", name, rec.Body.String())
		require.Equal(t, tc.want, vl.r, name)
	}

	// as of rounds, the boxes endpoint merges every box: bounded by
	// MaxAPIBoxPerApplication, and pages by a box count
	boxesAt := func(include *[]model.ApplicationBoxesVersionsParamsInclude) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Rounds: &[]basics.Round{5}, Include: include}))
		return rec
	}
	require.Equal(t, http.StatusOK, boxesAt(nil).Code)
	require.Equal(t, ledger.VersionRange{Min: 5, Max: 5, Limit: 100, MaxSeeks: 100_000, MaxReadBytes: 64_000_000, Rounds: []basics.Round{5}, MaxObjects: cfg.MaxAPIBoxPerApplication, MaxEntries: 10_000}, vl.r)
	require.Equal(t, http.StatusOK, boxesAt(&[]model.ApplicationBoxesVersionsParamsInclude{model.ApplicationBoxesVersionsParamsIncludeValues}).Code)
	require.EqualValues(t, 1_000, vl.r.MaxEntries)
	for _, err := range []error{ledger.ErrTooManyVersionObjects, ledger.ErrRoundTooLarge} {
		vl.err = fmt.Errorf("%w: app 9", err)
		rec := boxesAt(nil)
		require.Equal(t, http.StatusBadRequest, rec.Code)
		require.Contains(t, rec.Body.String(), "use prefix, or GET /v2/applications/9/boxes?round=")
	}
	vl.err = nil
	rec := httptest.NewRecorder()
	require.NoError(t, h.ApplicationBoxesVersions(echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec), 9, model.ApplicationBoxesVersionsParams{}))
	require.Zero(t, vl.r.MaxObjects, "a range of box changes reads the box index")

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
	str := func(s string) *string { return &s }
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

	// changed keys: sorted as the ledger gives them, empty when only other
	// fields changed, absent on deletions, for created apps without params,
	// and unless requested
	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1,
			LocalStates: []ledger.LocalStateVersion{{App: 9, ChangedKeys: [][]byte{[]byte("a"), []byte("b")}}, {App: 10, Deleted: true}},
			Apps:        []ledger.AppVersion{{App: 9, ChangedKeys: [][]byte{}}, {App: 11, Creator: addr}}},
		{Round: 11, Timestamp: 2, Apps: []ledger.AppVersion{{App: 9, Deleted: true}}},
	}}
	keysRec := func(fn func(echo.Context) error) string {
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, fn(ctx))
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		return rec.Body.String()
	}
	accountKeys := &[]model.AccountVersionsParamsInclude{model.AccountVersionsParamsIncludeKeys}
	body := keysRec(func(ctx echo.Context) error {
		return h.AccountVersions(ctx, addr, model.AccountVersionsParams{Include: accountKeys})
	})
	require.Contains(t, body, `"apps-local-state":[{"app-id":9,"changed-keys":["YQ==","Yg=="],"deleted":false},{"app-id":10,"deleted":true}]`)
	require.Contains(t, body, `"created-apps":[{"app-id":9,"changed-keys":[],"deleted":false},{"app-id":11,"deleted":false}]`)
	require.Contains(t, body, `"created-apps":[{"app-id":9,"deleted":true}]`)
	body = keysRec(func(ctx echo.Context) error { return h.AccountVersions(ctx, addr, model.AccountVersionsParams{}) })
	require.NotContains(t, body, "changed-keys")
	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Apps: []ledger.AppVersion{{App: 9, ChangedKeys: [][]byte{{0}, []byte("z")}}}},
		{Round: 11, Timestamp: 2, Apps: []ledger.AppVersion{{App: 9, ChangedKeys: [][]byte{}}}},
		{Round: 12, Timestamp: 3, Apps: []ledger.AppVersion{{App: 9, Deleted: true}}},
	}}
	appKeys := &[]model.ApplicationVersionsParamsInclude{model.ApplicationVersionsParamsIncludeKeys}
	var appKeyVersions model.ApplicationVersionsResponse
	call(func(ctx echo.Context) error {
		return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Include: appKeys})
	}, &appKeyVersions)
	require.Equal(t, &[][]byte{{0}, []byte("z")}, appKeyVersions.Versions[0].ChangedKeys)
	require.Equal(t, &[][]byte{}, appKeyVersions.Versions[1].ChangedKeys)
	require.Nil(t, appKeyVersions.Versions[2].ChangedKeys)
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Include: appKeys})
	})
	require.Contains(t, body, `{"changed-keys":["AA==","eg=="],"deleted":false,"round":10,"timestamp":1}`)
	require.Contains(t, body, `{"changed-keys":[],"deleted":false,"round":11,"timestamp":2}`)
	require.Contains(t, body, `{"deleted":true,"round":12,"timestamp":3}`)

	// key-value: the key's new value, absent when removed or deleted
	vl.page = ledger.VersionPage{More: true, Next: 31, Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, Apps: []ledger.AppVersion{{App: 9, KeyValue: &basics.TealValue{Type: basics.TealUintType, Uint: 5}}}},
		{Round: 11, Timestamp: 2, Apps: []ledger.AppVersion{{App: 9, KeyValue: &basics.TealValue{Type: basics.TealBytesType, Bytes: "hi"}}}},
		{Round: 12, Timestamp: 3, Apps: []ledger.AppVersion{{App: 9}}},
		{Round: 30, Timestamp: 4, Apps: []ledger.AppVersion{{App: 9, Deleted: true}}},
	}}
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("str:n")})
	})
	require.Contains(t, body, `{"deleted":false,"key-value":{"bytes":"","type":2,"uint":5},"round":10,"timestamp":1}`)
	require.Contains(t, body, `{"deleted":false,"key-value":{"bytes":"aGk=","type":1,"uint":0},"round":11,"timestamp":2}`)
	require.Contains(t, body, `{"deleted":false,"round":12,"timestamp":3}`, "removed")
	require.Contains(t, body, `{"deleted":true,"round":30,"timestamp":4}`)
	require.Contains(t, body, `"next-token":"31"`)
	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 10, Timestamp: 1, LocalStates: []ledger.LocalStateVersion{{App: 9, KeyValue: &basics.TealValue{Type: basics.TealUintType, Uint: 1}}}},
	}}
	body = keysRec(func(ctx echo.Context) error {
		return h.AccountApplicationVersions(ctx, addr, 9, model.AccountApplicationVersionsParams{Key: str("str:n")})
	})
	require.Contains(t, body, `"apps-local-state":[{"app-id":9,"deleted":false,"key-value":{"bytes":"","type":2,"uint":1}}]`)
	// a page with a key or prefix can be empty and still have more
	vl.page = ledger.VersionPage{More: true, Next: 500}
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Key: str("str:n")})
	})
	require.JSONEq(t, `{"round":1000,"versions":[],"next-token":"500"}`, body)
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("str:u")})
	})
	require.JSONEq(t, `{"round":1000,"versions":[],"next-token":"500"}`, body)

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

	// as of rounds: changed-round on every object that has one
	at := func(r basics.Round) *basics.Round { return &r }
	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 40, Timestamp: 5,
			Account:     &ledger.AccountVersion{Changed: at(12)},
			Holdings:    []ledger.HoldingVersion{{Asset: 7, Changed: at(31)}},
			LocalStates: []ledger.LocalStateVersion{{App: 9, Changed: at(0)}},
			Assets:      []ledger.AssetVersion{{Asset: 7, Creator: addr, Changed: at(3)}},
			Apps:        []ledger.AppVersion{{App: 9, Creator: addr, Changed: at(4)}}},
		{Round: 41, Timestamp: 6, Account: &ledger.AccountVersion{Deleted: true}},
	}}
	body = keysRec(func(ctx echo.Context) error {
		return h.AccountVersions(ctx, addr, model.AccountVersionsParams{Rounds: &[]basics.Round{40, 41}})
	})
	require.Contains(t, body, `"base":{"changed-round":12,"deleted":false}`)
	require.Contains(t, body, `"assets":[{"asset-id":7,"changed-round":31,"deleted":false}]`)
	require.Contains(t, body, `"apps-local-state":[{"app-id":9,"changed-round":0,"deleted":false}]`, "round 0 is a round")
	require.Contains(t, body, `"created-assets":[{"asset-id":7,"changed-round":3,"deleted":false}]`)
	require.Contains(t, body, `"created-apps":[{"app-id":9,"changed-round":4,"deleted":false}]`)
	require.Contains(t, body, `{"base":{"deleted":true},"round":41,"timestamp":6}`, "never existed: no changed-round")
	require.Equal(t, "account "+addr.String(), vl.call)
	vl.page = ledger.VersionPage{Versions: []ledger.Version{{Round: 40, Timestamp: 5, Assets: []ledger.AssetVersion{{Asset: 7, Deleted: true, Changed: at(33)}}}}}
	body = keysRec(func(ctx echo.Context) error {
		return h.AssetVersions(ctx, 7, model.AssetVersionsParams{Rounds: &[]basics.Round{40}})
	})
	require.Contains(t, body, `{"changed-round":33,"deleted":true,"round":40,"timestamp":5}`)
	vl.page = ledger.VersionPage{Versions: []ledger.Version{{Round: 40, Timestamp: 5, Apps: []ledger.AppVersion{{App: 9, Changed: at(4)}}}}}
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationVersions(ctx, 9, model.ApplicationVersionsParams{Rounds: &[]basics.Round{40}})
	})
	require.Contains(t, body, `{"changed-round":4,"deleted":false,"round":40,"timestamp":5}`)
	vl.page = ledger.VersionPage{Versions: []ledger.Version{{Round: 40, Timestamp: 5, Box: &ledger.BoxVersion{Changed: at(8), Value: []byte("v")}}}}
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationBoxVersions(ctx, 9, model.ApplicationBoxVersionsParams{Name: "str:b", Rounds: &[]basics.Round{40}, Include: values})
	})
	require.Contains(t, body, `{"changed-round":8,"deleted":false,"round":40,"timestamp":5,"value":"dg=="}`)
	vl.page = ledger.VersionPage{Versions: []ledger.Version{
		{Round: 40, Timestamp: 5, Boxes: []ledger.BoxChange{{Name: []byte("a"), Changed: at(8)}, {Name: []byte("b"), Changed: at(39)}}},
		{Round: 41, Timestamp: 6, Boxes: []ledger.BoxChange{}},
	}}
	body = keysRec(func(ctx echo.Context) error {
		return h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Rounds: &[]basics.Round{40, 41}})
	})
	require.Contains(t, body, `"boxes":[{"changed-round":8,"deleted":false,"name":"YQ=="},{"changed-round":39,"deleted":false,"name":"Yg=="}]`)
	require.Contains(t, body, `{"boxes":[],"round":41,"timestamp":6}`, "no boxes at a round")

	vl.err = fmt.Errorf("lookup: %w before round 7", ledger.ErrNoBoxIndex)
	rec = httptest.NewRecorder()
	ctx = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{}))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "ttimport -index-boxes")
	rec = httptest.NewRecorder()
	ctx = echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
	require.NoError(t, h.ApplicationBoxesVersions(ctx, 9, model.ApplicationBoxesVersionsParams{Prefix: str("str:u")}))
	require.Equal(t, http.StatusServiceUnavailable, rec.Code, "a prefix reads the box index too")
}

// globalPutsApp sets global key arg i to arg i+1 for each pair of args. The
// unreachable constants make each version of its params about 15 KB.
var globalPutsApp = "#pragma version 10\n" + `
	txn ApplicationID; bz done
	int 0; store 0
loop:
	load 0; txn NumAppArgs; >=; bnz done
	load 0; txnas ApplicationArgs
	load 0; int 1; +; txnas ApplicationArgs
	app_global_put
	load 0; int 2; +; store 0
	b loop
done:
	int 1; return
` + strings.Repeat("pushbytes 0x"+strings.Repeat("ab", 1000)+"\n", 7)

// TestVersionsKeysPaging pages through include=keys on a real ledger by
// limit, by the 1 MB budget (which counts each version's previous value),
// and in rounds mode, and checks every version's changed keys.
func TestVersionsKeysPaging(t *testing.T) {
	partitiontest.PartitionTest(t)

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender := env.Accounts[0].Addr
	app := env.CreateApp(sender, simulationtesting.AppParams{
		ApprovalProgram:   globalPutsApp,
		ClearStateProgram: "#pragma version 10\nint 1",
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 64},
		ExtraProgramPages: 3,
	})
	// the changed keys of each version, in round order
	wantRounds := []basics.Round{env.Ledger.Latest()}
	wantKeys := [][][]byte{{}}
	put := func(kv ...[]byte) {
		env.Txn(env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender, ApplicationID: app, ApplicationArgs: kv}).SignedTxn())
		var keys [][]byte
		for i := 0; i < len(kv); i += 2 {
			keys = append(keys, kv[i])
		}
		slices.SortFunc(keys, bytes.Compare)
		wantRounds = append(wantRounds, env.Ledger.Latest())
		wantKeys = append(wantKeys, keys)
	}
	key := func(i int) []byte { return fmt.Appendf(nil, "key%029d", i) }   // 32 bytes
	val := func(i int) []byte { return fmt.Appendf(nil, "value%091d", i) } // 96 bytes
	for i := 0; i < 64; i += 8 {                                           // fill the global state, 8 keys a call
		var kv [][]byte
		for j := i; j < i+8; j++ {
			kv = append(kv, key(j), val(j))
		}
		put(kv...)
	}
	for i := range 80 {
		put(key(i*7%64), val(1000+i))
	}

	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	h := &v2.Handlers{Node: makeMockNodeWithConfig(env.Ledger, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
	e := echo.New()
	timetravel.RegisterHandlers(e, h)
	// pages returns every version of the query's pages, and the page sizes
	pages := func(query string) (vs []model.ApplicationVersion, sizes []int) {
		t.Helper()
		next := ""
		for {
			target := fmt.Sprintf("/v2/applications/%d/versions?%s%s", app, query, next)
			rec := httptest.NewRecorder()
			e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			var resp model.ApplicationVersionsResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
			require.NotEmpty(t, resp.Versions, target)
			vs = append(vs, resp.Versions...)
			sizes = append(sizes, len(resp.Versions))
			if resp.NextToken == nil {
				return vs, sizes
			}
			next = "&next=" + *resp.NextToken
		}
	}
	check := func(name string, vs []model.ApplicationVersion, values, asOf bool) {
		t.Helper()
		require.Len(t, vs, len(wantRounds), name)
		for i, v := range vs {
			require.Equal(t, wantRounds[i], v.Round, name)
			require.NotNil(t, v.ChangedKeys, "%s: round %d", name, v.Round)
			require.Equal(t, wantKeys[i], *v.ChangedKeys, "%s: round %d", name, v.Round)
			require.Equal(t, values, v.Application != nil, name)
			if asOf {
				require.Equal(t, &v.Round, v.ChangedRound, name)
			}
		}
	}

	vs, sizes := pages("include=keys&limit=10")
	check("limit", vs, false, false)
	require.Len(t, sizes, (len(wantRounds)+9)/10)

	// a version reads its params and the ones before, about 30 KB: the
	// budget ends pages long before the limit
	vs, sizes = pages("include=keys&limit=1000")
	check("budget", vs, false, false)
	require.Greater(t, len(sizes), 1, "the 1 MB budget ends pages")
	require.Less(t, sizes[0], 60)
	byBudget := sizes

	vs, sizes = pages("include=values,keys&limit=1000")
	check("values", vs, true, false)
	require.Equal(t, byBudget, sizes, "values are read with keys anyway")

	// rounds mode: each requested round is a version, so as of it the
	// version in effect is its own, with the same keys
	rounds := make([]string, len(wantRounds))
	for i, r := range wantRounds {
		rounds[i] = fmt.Sprint(r)
	}
	vs, sizes = pages("include=keys&limit=1000&rounds=" + strings.Join(rounds, ","))
	check("rounds", vs, false, true)
	require.Equal(t, byBudget, sizes, "rounds mode counts the same values")
	vs, _ = pages("include=keys&limit=7&rounds=" + strings.Join(rounds, ","))
	check("rounds limit", vs, false, true)

	// key: one key through the 80 single-key updates. key(0) is set by the
	// first fill call and by updates 0 and 64; each sets a new value
	keyArg := func(k []byte) string { return "key=b64:" + url.QueryEscape(base64.StdEncoding.EncodeToString(k)) }
	wantK := map[basics.Round]string{wantRounds[1]: string(val(0)), wantRounds[1+8]: string(val(1000)), wantRounds[1+8+64]: string(val(1064))}
	for _, q := range []string{"limit=1", "limit=2&order=desc", "limit=1000&include=values,keys"} {
		vs, sizes := pages(keyArg(key(0)) + "&" + q)
		got := map[basics.Round]string{}
		for _, v := range vs {
			require.NotNil(t, v.KeyValue, q)
			b, err := base64.StdEncoding.DecodeString(v.KeyValue.Bytes)
			require.NoError(t, err)
			got[v.Round] = string(b)
			if strings.Contains(q, "keys") {
				require.Contains(t, *v.ChangedKeys, key(0), q)
			}
		}
		require.Equal(t, wantK, got, q)
		require.Len(t, vs, 3, q)
		if q == "limit=1" {
			require.Equal(t, []int{1, 1, 1}, sizes)
		}
	}
	// a key that was never set: one page, no versions
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v2/applications/%d/versions?key=str:nope", app), nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.JSONEq(t, fmt.Sprintf(`{"round":%d,"versions":[]}`, env.Ledger.Latest()), rec.Body.String())
}
