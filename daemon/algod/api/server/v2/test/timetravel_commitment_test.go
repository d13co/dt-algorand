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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// commitmentLedger answers HistoryCommitment with a digest of the round, or
// err; with block set, it waits for it to be closed first.
type commitmentLedger struct {
	*mockLedger
	err     error
	block   chan struct{}
	started chan struct{}
}

func (l commitmentLedger) HistoryCommitment(ctx context.Context, rnd basics.Round, _ int) (ledger.HistoryCommitment, error) {
	if l.block != nil {
		close(l.started)
		<-l.block
	}
	if l.err != nil {
		return ledger.HistoryCommitment{}, l.err
	}
	var c ledger.HistoryCommitment
	c.Round, c.Objects = uint64(rnd), 42
	c.Digest[0] = byte(rnd)
	c.GenesisHash[0], c.HeaderHash[0] = 1, 2
	return c, nil
}

func TestHistoryCommitmentEndpoint(t *testing.T) {
	partitiontest.PartitionTest(t)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	get := func(l v2.LedgerForAPI, cfg config.Local, target string) *httptest.ResponseRecorder {
		h := v2.Handlers{Node: makeMockNodeWithConfig(l, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
		e := echo.New()
		e.GET(v2.HistoryCommitmentPath+"/:round", h.HistoryCommitment)
		rec := httptest.NewRecorder()
		e.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		return rec
	}
	ml := &mockLedger{latest: 10}

	rec := get(commitmentLedger{mockLedger: ml}, cfg, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var resp v2.HistoryCommitmentResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))
	require.EqualValues(t, 7, resp.Round)
	require.EqualValues(t, 42, resp.Objects)
	require.Equal(t, fmt.Sprintf("07%062x", 0), resp.Commitment)
	require.Regexp(t, "^blk-", resp.HeaderHash)

	rec = get(commitmentLedger{mockLedger: ml}, cfg, v2.HistoryCommitmentPath+"/x")
	require.Equal(t, http.StatusBadRequest, rec.Code)
	rec = get(commitmentLedger{mockLedger: ml, err: fmt.Errorf("%w: diverged", ledger.ErrHistoryUntrusted)}, cfg, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	rec = get(ml, cfg, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusBadRequest, rec.Code, "a ledger without history")
	off := cfg
	off.EnableTimeTravel = false
	rec = get(commitmentLedger{mockLedger: ml}, off, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusBadRequest, rec.Code, "time travel disabled")

	// one at a time
	slow := commitmentLedger{mockLedger: ml, block: make(chan struct{}), started: make(chan struct{})}
	done := make(chan *httptest.ResponseRecorder)
	go func() { done <- get(slow, cfg, v2.HistoryCommitmentPath+"/7") }()
	<-slow.started
	rec = get(commitmentLedger{mockLedger: ml}, cfg, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
	close(slow.block)
	require.Equal(t, http.StatusOK, (<-done).Code)
	rec = get(commitmentLedger{mockLedger: ml}, cfg, v2.HistoryCommitmentPath+"/7")
	require.Equal(t, http.StatusOK, rec.Code, "free again")
}

// TestHistoryCommitmentOutlivesWriteTimeout checks that a commitment that
// takes longer than the server's write timeout is still answered, through the
// gzip middleware algod uses.
func TestHistoryCommitmentOutlivesWriteTimeout(t *testing.T) {
	partitiontest.PartitionTest(t)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	slow := commitmentLedger{mockLedger: &mockLedger{latest: 10}, block: make(chan struct{}), started: make(chan struct{})}
	h := v2.Handlers{Node: makeMockNodeWithConfig(slow, t.Name(), nil, cannedStatusReportGolden, false, cfg), Log: logging.Base(), Shutdown: make(chan struct{})}
	e := echo.New()
	e.Use(middleware.Gzip())
	e.GET(v2.HistoryCommitmentPath+"/:round", h.HistoryCommitment)
	srv := httptest.NewUnstartedServer(e)
	srv.Config.WriteTimeout = 100 * time.Millisecond
	srv.Start()
	defer srv.Close()
	go func() {
		<-slow.started
		time.Sleep(400 * time.Millisecond)
		close(slow.block)
	}()
	resp, err := http.Get(srv.URL + v2.HistoryCommitmentPath + "/7")
	require.NoError(t, err)
	defer resp.Body.Close()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var r v2.HistoryCommitmentResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&r))
	require.EqualValues(t, 7, r.Round)
}
