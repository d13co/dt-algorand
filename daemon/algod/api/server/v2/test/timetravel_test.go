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
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

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
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/"+query, nil), rec)
		require.NoError(t, h.AccountInformation(ctx, addr, model.AccountInformationParams{}))
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
	code, _ = get(ml, "?round=7")
	require.Equal(t, http.StatusBadRequest, code, "ledger without history")
	cfg.EnableTimeTravel = false
	code, _ = get(historyLedger{ml}, "?round=7")
	require.Equal(t, http.StatusBadRequest, code, "time travel disabled")
}
