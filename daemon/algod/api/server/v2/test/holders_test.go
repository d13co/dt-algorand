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
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// holdersLedger records the last holders lookup and answers with page.
type holdersLedger struct {
	*mockLedger
	page ledger.AssetHoldersPage
	err  error

	calls int
	rnd   basics.Round
	aidx  basics.AssetIndex
	after *basics.Address
	limit uint64
}

func (l *holdersLedger) AssetHoldersAt(rnd basics.Round, aidx basics.AssetIndex, after *basics.Address, limit uint64) (ledger.AssetHoldersPage, error) {
	l.calls++
	l.rnd, l.aidx, l.after, l.limit = rnd, aidx, after, limit
	return l.page, l.err
}

func TestGetAssetHolders(t *testing.T) {
	partitiontest.PartitionTest(t)

	a, b := ledgertesting.RandomAddress(), ledgertesting.RandomAddress()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel, cfg.TimeTravelHoldersIndex = true, true
	hl := &holdersLedger{mockLedger: &mockLedger{latest: 1000}}
	get := func(lgr v2.LedgerForAPI, cfg config.Local, params model.GetAssetHoldersParams) (int, string, model.AssetHoldersResponse) {
		h := v2.Handlers{Node: makeMockNodeWithConfig(lgr, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		rec := httptest.NewRecorder()
		ctx := echo.New().NewContext(httptest.NewRequest(http.MethodGet, "/", nil), rec)
		require.NoError(t, h.GetAssetHolders(ctx, 7, params))
		var resp model.AssetHoldersResponse
		if rec.Code == http.StatusOK {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
		}
		return rec.Code, rec.Body.String(), resp
	}
	rnd := func(r basics.Round) *basics.Round { return &r }
	u64 := func(u uint64) *uint64 { return &u }
	str := func(s string) *string { return &s }

	// defaults: the latest round, 1,000 holders, from the first
	hl.page = ledger.AssetHoldersPage{Holders: []ledger.AssetHolder{
		{Address: a, Holding: basics.AssetHolding{Amount: 5}},
		{Address: b, Holding: basics.AssetHolding{Frozen: true}},
	}}
	code, body, resp := get(hl, cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, basics.Round(1000), hl.rnd)
	require.Equal(t, basics.AssetIndex(7), hl.aidx)
	require.Nil(t, hl.after)
	require.EqualValues(t, v2.DefaultHolders, hl.limit)
	require.Equal(t, model.AssetHoldersResponse{Round: 1000, Holders: []model.AssetHolder{
		{Address: a.String(), Amount: 5},
		{Address: b.String(), IsFrozen: true},
	}}, resp, "no next-token on the last page")
	require.Contains(t, body, `"amount":0`, "a zero balance is listed")

	// a short page with more to come; the cursor and round are passed on
	hl.page = ledger.AssetHoldersPage{More: true, Next: b, Holders: []ledger.AssetHolder{{Address: a}}}
	code, body, resp = get(hl, cfg, model.GetAssetHoldersParams{Round: rnd(40), Limit: u64(v2.MaxHolders), Next: str(a.String())})
	require.Equal(t, http.StatusOK, code, body)
	require.Equal(t, basics.Round(40), hl.rnd)
	require.Equal(t, &a, hl.after)
	require.EqualValues(t, v2.MaxHolders, hl.limit)
	require.Equal(t, basics.Round(40), resp.Round)
	require.Equal(t, b.String(), *resp.NextToken, "the last address examined")
	hl.page = ledger.AssetHoldersPage{More: true, Next: b}
	code, body, resp = get(hl, cfg, model.GetAssetHoldersParams{Limit: u64(1)})
	require.Equal(t, http.StatusOK, code, body)
	require.Empty(t, resp.Holders)
	require.NotNil(t, resp.Holders, "an empty page lists no holders")
	require.Contains(t, body, `"holders":[]`)
	require.Equal(t, b.String(), *resp.NextToken)

	for name, tc := range map[string]struct {
		params model.GetAssetHoldersParams
		cfg    func(*config.Local)
		lgr    v2.LedgerForAPI
		code   int
		msg    string
	}{
		"option off":           {cfg: func(c *config.Local) { c.TimeTravelHoldersIndex = false }, code: http.StatusBadRequest, msg: "require EnableTimeTravel and TimeTravelHoldersIndex"},
		"time travel off":      {cfg: func(c *config.Local) { c.EnableTimeTravel = false }, code: http.StatusBadRequest, msg: "TimeTravelHoldersIndex"},
		"ledger without index": {lgr: hl.mockLedger, code: http.StatusBadRequest, msg: "TimeTravelHoldersIndex"},
		"limit 0":              {params: model.GetAssetHoldersParams{Limit: u64(0)}, code: http.StatusBadRequest, msg: "limit"},
		"limit too big":        {params: model.GetAssetHoldersParams{Limit: u64(v2.MaxHolders + 1)}, code: http.StatusBadRequest, msg: "limit"},
		"bad next":             {params: model.GetAssetHoldersParams{Next: str("nope")}, code: http.StatusBadRequest, msg: "next"},
		"future round":         {params: model.GetAssetHoldersParams{Round: rnd(1001)}, code: http.StatusBadRequest, msg: "greater than the latest"},
	} {
		c := cfg
		if tc.cfg != nil {
			tc.cfg(&c)
		}
		lgr := tc.lgr
		if lgr == nil {
			lgr = hl
		}
		calls := hl.calls
		code, body, _ := get(lgr, c, tc.params)
		require.Equal(t, tc.code, code, name)
		require.Contains(t, body, tc.msg, name)
		require.Equal(t, calls, hl.calls, "%s: rejected before the lookup", name)
	}

	for _, st := range []timetravel.HoldersState{timetravel.HoldersNone, timetravel.HoldersStale} {
		// as the store's AssetHolders fails, wrapped by the ledger or not
		hl.err = fmt.Errorf("page: %w", indexError(t, st))
		code, body, _ = get(hl, cfg, model.GetAssetHoldersParams{})
		require.Equal(t, http.StatusServiceUnavailable, code)
		require.Contains(t, body, "the asset holders index is "+st.String())
		require.Contains(t, body, "ttimport -holders-index")
	}
	// an untrusted history answers 503 before the lookup, as other time
	// travel endpoints do
	hl.err = nil
	calls := hl.calls
	code, body, _ = get(untrustedHoldersLedger{hl}, cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Contains(t, body, "round 5 has a different header")
	require.Equal(t, calls, hl.calls)

	// found untrusted after the handler's check, by the lookup
	hl.err = fmt.Errorf("%w: round 5 has a different header", ledger.ErrHistoryUntrusted)
	code, body, _ = get(hl, cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusServiceUnavailable, code)
	require.Contains(t, body, "round 5 has a different header")
	hl.err = fmt.Errorf("%w: asset 7 at round 1000", ledger.ErrAssetNotFound)
	code, _, _ = get(hl, cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusNotFound, code)
	hl.err = errors.New("disk on fire")
	code, _, _ = get(hl, cfg, model.GetAssetHoldersParams{})
	require.Equal(t, http.StatusInternalServerError, code)
}

// untrustedHoldersLedger is a holdersLedger whose history is not served.
type untrustedHoldersLedger struct{ *holdersLedger }

func (untrustedHoldersLedger) HistoryUntrusted() error {
	return fmt.Errorf("%w: round 5 has a different header", ledger.ErrHistoryUntrusted)
}

// indexError returns the error a store's AssetHolders returns while its index
// is in state st.
func indexError(t *testing.T, st timetravel.HoldersState) error {
	dir := t.TempDir()
	s, err := timetravel.Open(dir, timetravel.Options{})
	require.NoError(t, err)
	if st == timetravel.HoldersStale {
		// built, then a round added as a build without the index adds it
		_, err = s.IndexHolders(context.Background(), nil)
		require.NoError(t, err)
		require.NoError(t, s.AddRound(0, nil, nil, nil))
		require.NoError(t, s.DB().Set([]byte("mh"), binary.BigEndian.AppendUint64(nil, 0), pebble.Sync))
		require.NoError(t, s.Close())
		s, err = timetravel.Open(dir, timetravel.Options{})
		require.NoError(t, err)
	}
	defer s.Close()
	require.Equal(t, st, s.HoldersIndexState())
	err = s.AssetHolders(7, nil, func([]byte, []byte) bool { return true })
	require.ErrorIs(t, err, ledger.ErrNoHoldersIndex)
	return err
}
