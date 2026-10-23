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
	"context"
	"encoding/hex"
	"errors"
	"net/http"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
)

// HistoryCommitmentPath is the admin endpoint ttsnap attest reads; it is not
// in the REST spec. GET <path>/<round> answers with a HistoryCommitmentResponse.
const HistoryCommitmentPath = "/v2/timetravel/commitment"

// HistoryCommitmentResponse is the commitment of the node's time travel
// history up to Round (see timetravel.Store.Commitment).
type HistoryCommitmentResponse struct {
	GenesisHash string  `json:"genesis-hash"`
	Round       uint64  `json:"round"`
	HeaderHash  string  `json:"header-hash"`
	Commitment  string  `json:"commitment"` // hex
	First       uint64  `json:"first"`
	Objects     uint64  `json:"objects"`
	Versions    uint64  `json:"versions"`
	Headers     uint64  `json:"headers"`
	Expired     uint64  `json:"expired"`
	Seconds     float64 `json:"seconds"`
	// InstalledFrom names the snapshot the history was installed from,
	// which the node's statements must not vouch for; empty if it built it.
	InstalledFrom string `json:"installed-from,omitempty"`
}

// commitmentLedger is implemented by the node's ledger.
type commitmentLedger interface {
	HistoryCommitment(ctx context.Context, rnd basics.Round, workers int) (ledger.HistoryCommitment, error)
}

// commitmentBusy is set while a commitment is computed: one at a time, since
// each one reads the whole history.
var commitmentBusy atomic.Bool

// HistoryCommitment computes the commitment of the history up to the round in
// the path. It takes minutes on mainnet, so it lifts the write timeout; a
// client that goes away cancels it.
func (v2 *Handlers) HistoryCommitment(ctx echo.Context) error {
	rnd, err := strconv.ParseUint(ctx.Param("round"), 10, 64)
	if err != nil {
		return badRequest(ctx, err, "bad round", v2.Log)
	}
	l, ok := v2.Node.LedgerForAPI().(commitmentLedger)
	if !ok || !v2.Node.Config().EnableTimeTravel {
		e := errors.New("the history commitment requires EnableTimeTravel")
		return badRequest(ctx, e, e.Error(), v2.Log)
	}
	if !commitmentBusy.CompareAndSwap(false, true) {
		e := errors.New("another history commitment is being computed")
		return serviceUnavailable(ctx, e, e.Error(), v2.Log)
	}
	defer commitmentBusy.Store(false)
	if derr := http.NewResponseController(ctx.Response()).SetWriteDeadline(time.Time{}); derr != nil {
		v2.Log.Warnf("history commitment: can't lift the write timeout: %v", derr)
	}
	start := time.Now()
	// half the CPUs: the node keeps running meanwhile
	c, err := l.HistoryCommitment(ctx.Request().Context(), basics.Round(rnd), max(1, runtime.GOMAXPROCS(0)/2))
	if errors.Is(err, ledger.ErrHistoryUntrusted) {
		return serviceUnavailable(ctx, err, err.Error(), v2.Log)
	}
	if err != nil {
		return badRequest(ctx, err, err.Error(), v2.Log)
	}
	return ctx.JSON(http.StatusOK, HistoryCommitmentResponse{
		GenesisHash:   c.GenesisHash.String(),
		Round:         c.Round,
		HeaderHash:    c.HeaderHash.String(),
		Commitment:    hex.EncodeToString(c.Digest[:]),
		First:         c.First,
		Objects:       c.Objects,
		Versions:      c.Versions,
		Headers:       c.Headers,
		Expired:       c.Expired,
		Seconds:       time.Since(start).Seconds(),
		InstalledFrom: c.Installed,
	})
}
