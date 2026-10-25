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
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/nonparticipating/public"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// serve sends GET target through the public routes generated from the spec,
// which parse ?round= into the handlers' params.
func serve(h *v2.Handlers, target string) *httptest.ResponseRecorder {
	e := echo.New()
	public.RegisterHandlers(e, h)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec
}

// historyLedger answers past-round lookups with a balance of 1000*round.
type historyLedger struct{ *mockLedger }

func (l historyLedger) LookupLatestAt(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	return basics.AccountData{MicroAlgos: basics.MicroAlgos{Raw: 1000 * uint64(rnd)}}, rnd, basics.MicroAlgos{Raw: 1000 * uint64(rnd)}, nil
}
func (l historyLedger) LookupAssetsAt(basics.Round, basics.Address, basics.AssetIndex, uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	return nil, 0, nil
}
func (l historyLedger) LookupApplicationsAt(basics.Round, basics.Address, basics.AppIndex, uint64, bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	return nil, 0, nil
}
func (l historyLedger) GetCreatorForRound(basics.Round, basics.CreatableIndex, basics.CreatableType) (basics.Address, bool, error) {
	return basics.Address{}, false, nil
}
func (l historyLedger) Totals(basics.Round) (ledgercore.AccountTotals, error) {
	return ledgercore.AccountTotals{}, nil
}

func TestAccountInformationAtRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	ml := &mockLedger{accounts: map[basics.Address]basics.AccountData{addr: {MicroAlgos: basics.MicroAlgos{Raw: 1}}}, latest: 10}
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	get := func(lgr v2.LedgerForAPI, query string) (int, model.AccountResponse) {
		h := v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		rec := serve(&h, "/v2/accounts/"+addr.String()+query)
		var resp model.AccountResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		}
		return rec.Code, resp
	}

	code, resp := get(historyLedger{ml}, "?round=7")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, basics.Round(7), resp.Round)
	require.EqualValues(t, 7000, resp.Amount)

	code, resp = get(historyLedger{ml}, "")
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, basics.Round(10), resp.Round)
	require.EqualValues(t, 1, resp.Amount)

	code, _ = get(historyLedger{ml}, "?round=11")
	require.Equal(t, http.StatusBadRequest, code, "future round")
	code, _ = get(historyLedger{ml}, "?round=x")
	require.Equal(t, http.StatusBadRequest, code)
	code, _ = get(historyLedger{ml}, "?round=")
	require.Equal(t, http.StatusBadRequest, code, "empty round")
	code, _ = get(historyLedger{ml}, "?round=7&round=8")
	require.Equal(t, http.StatusBadRequest, code, "repeated round")
	code, _ = get(ml, "?round=7")
	require.Equal(t, http.StatusBadRequest, code, "ledger without history")
	cfg.EnableTimeTravel = false
	code, _ = get(historyLedger{ml}, "?round=7")
	require.Equal(t, http.StatusBadRequest, code, "time travel disabled")
}

// untrustedLedger has a history found to differ from the chain.
type untrustedLedger struct{ historyLedger }

func (untrustedLedger) HistoryUntrusted() error {
	return fmt.Errorf("%w: round 5 has a different header", ledger.ErrHistoryUntrusted)
}

// TestUntrustedHistory checks that an untrusted history is not served: 503
// for past rounds, while the latest state still is.
func TestUntrustedHistory(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	ml := &mockLedger{accounts: map[basics.Address]basics.AccountData{addr: {MicroAlgos: basics.MicroAlgos{Raw: 1}}}, latest: 10}
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	h := v2.Handlers{Node: makeMockNodeWithConfig(untrustedLedger{historyLedger{ml}}, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}

	rec := serve(&h, "/v2/accounts/"+addr.String()+"?round=7")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	require.Contains(t, rec.Body.String(), "is not served")
	rec = serve(&h, "/v2/accounts/"+addr.String())
	require.Equal(t, http.StatusOK, rec.Code)
}

// supplyLedger answers OnlineCirculation with stake, or err.
type supplyLedger struct {
	historyLedger
	stake uint64
	err   error
}

func (l supplyLedger) OnlineCirculation(basics.Round, basics.Round) (basics.MicroAlgos, error) {
	return basics.MicroAlgos{Raw: l.stake}, l.err
}

// TestSupplyAtRound checks GetSupply at a past round: the online stake where
// the history recorded it, 404 where it did not.
func TestSupplyAtRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	ml := &mockLedger{latest: 10}
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	get := func(lgr v2.LedgerForAPI) (int, model.SupplyResponse) {
		h := v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		rec := serve(&h, "/v2/ledger/supply?round=7")
		var resp model.SupplyResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		}
		return rec.Code, resp
	}

	code, resp := get(supplyLedger{historyLedger: historyLedger{ml}, stake: 42})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, basics.Round(7), resp.CurrentRound)
	require.EqualValues(t, 42, resp.OnlineStake)

	code, _ = get(supplyLedger{historyLedger: historyLedger{ml}, err: fmt.Errorf("%w: balance round 0, vote round 7", ledger.ErrOnlineStakeUnavailable)})
	require.Equal(t, http.StatusNotFound, code)
}

// TestRoutesTakeRound checks that every route documenting the at-round
// parameter hands it to the handler: without EnableTimeTravel, ?round= is
// refused for that reason.
func TestRoutesTakeRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress().String()
	ml := &mockLedger{latest: 10}
	h := v2.Handlers{Node: makeMockNodeWithConfig(historyLedger{ml}, t.Name(), nil, cannedStatusReportGolden, false, config.GetDefaultLocal()), Log: logging.Base(), Shutdown: make(chan struct{})}
	for _, target := range []string{
		"/v2/accounts/" + addr + "?round=7",
		"/v2/accounts/" + addr + "/assets?round=7",
		"/v2/accounts/" + addr + "/assets/1?round=7",
		"/v2/accounts/" + addr + "/applications?round=7",
		"/v2/accounts/" + addr + "/applications/1?round=7",
		"/v2/assets/1?round=7",
		"/v2/applications/1?round=7",
		"/v2/applications/1/box?name=str:a&round=7",
		"/v2/ledger/supply?round=7",
	} {
		rec := serve(&h, target)
		require.Equal(t, http.StatusBadRequest, rec.Code, target)
		require.Contains(t, rec.Body.String(), "requires EnableTimeTravel", target)
	}
}

// pastLedger holds an asset, an app and a box only at round at, created by
// creator: a handler that looks them up at another round finds nothing.
type pastLedger struct {
	historyLedger
	at      basics.Round
	creator basics.Address
}

// LookupAccount reports the round asked for, as Ledger.LookupAccount does
// (exclude=all reads the account through it).
func (l pastLedger) LookupAccount(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, basics.MicroAlgos, error) {
	ad, _, withoutRewards, err := l.mockLedger.LookupAccount(rnd, addr)
	return ad, rnd, withoutRewards, err
}
func (l pastLedger) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	if rnd != l.at {
		return ledgercore.AssetResource{}, nil
	}
	return l.mockLedger.LookupAsset(rnd, addr, aidx)
}
func (l pastLedger) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	if rnd != l.at {
		return ledgercore.AppResource{}, nil
	}
	return l.mockLedger.LookupApplication(rnd, addr, aidx)
}
func (l pastLedger) LookupKv(rnd basics.Round, key string) ([]byte, error) {
	if rnd != l.at {
		return nil, nil
	}
	return l.mockLedger.LookupKv(rnd, key)
}
func (l pastLedger) GetCreatorForRound(rnd basics.Round, _ basics.CreatableIndex, _ basics.CreatableType) (basics.Address, bool, error) {
	return l.creator, rnd == l.at, nil
}
func (l pastLedger) OnlineCirculation(basics.Round, basics.Round) (basics.MicroAlgos, error) {
	return basics.MicroAlgos{}, nil
}
func (l pastLedger) LookupAssetsAt(rnd basics.Round, _ basics.Address, _ basics.AssetIndex, _ uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	return nil, rnd, nil
}
func (l pastLedger) LookupApplicationsAt(rnd basics.Round, _ basics.Address, _ basics.AppIndex, _ uint64, _ bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	return nil, rnd, nil
}

// TestRoutesAtRound checks that every route documenting the at-round
// parameter answers at that round with EnableTimeTravel, and reports it in
// the response's round, if it has one.
func TestRoutesAtRound(t *testing.T) {
	partitiontest.PartitionTest(t)

	addr := ledgertesting.RandomAddress()
	ml := &mockLedger{
		accounts: map[basics.Address]basics.AccountData{addr: {
			AssetParams:    map[basics.AssetIndex]basics.AssetParams{1: {Total: 1}},
			Assets:         map[basics.AssetIndex]basics.AssetHolding{1: {Amount: 1}},
			AppParams:      map[basics.AppIndex]basics.AppParams{1: {}},
			AppLocalStates: map[basics.AppIndex]basics.AppLocalState{1: {}},
		}},
		kvstore: map[string][]byte{apps.MakeBoxKey(1, "a"): []byte("v")},
		latest:  10,
	}
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	lgr := pastLedger{historyLedger: historyLedger{ml}, at: 7, creator: addr}
	h := v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
	a := addr.String()
	for _, target := range []string{
		"/v2/accounts/" + a + "?round=7",
		"/v2/accounts/" + a + "?exclude=all&round=7",
		"/v2/accounts/" + a + "/assets?round=7",
		"/v2/accounts/" + a + "/assets/1?round=7",
		"/v2/accounts/" + a + "/applications?round=7",
		"/v2/accounts/" + a + "/applications/1?round=7",
		"/v2/assets/1?round=7",
		"/v2/applications/1?round=7",
		"/v2/applications/1/box?name=str:a&round=7",
		"/v2/ledger/supply?round=7",
	} {
		rec := serve(&h, target)
		require.Equal(t, http.StatusOK, rec.Code, "%s: %s", target, rec.Body.String())
		var resp map[string]any
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp), target)
		for _, field := range []string{"round", "current_round"} {
			if r, ok := resp[field]; ok {
				require.EqualValues(t, 7, r, "%s: %s", target, field)
			}
		}
	}
}
