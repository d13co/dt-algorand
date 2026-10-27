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

package ledgercore

// The work one REST request did (EnableEffortHeader). Non-standard fork feature.

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// Effort counts the work one request does: opcodes run, ledger objects read, how many of those
// reads the time travel history answered, entries scanned, and the size of the answer. The counters are safe for concurrent use, and every method is a no-op on a nil *Effort, so code
// that counts needs no check for whether anyone is counting.
type Effort struct {
	opcodes       atomic.Uint64
	lookups       atomic.Uint64
	history       atomic.Uint64
	scanned       atomic.Uint64
	responseBytes atomic.Uint64
	start         time.Time
}

// NewEffort returns an Effort whose eval time starts now.
func NewEffort() *Effort {
	return &Effort{start: time.Now()}
}

// AddOpcodes adds n units of app or logicsig budget consumed.
func (e *Effort) AddOpcodes(n uint64) {
	if e != nil && n > 0 {
		e.opcodes.Add(n)
	}
}

// Lookup counts one ledger object read, answered by the time travel history when history is set.
func (e *Effort) Lookup(history bool) {
	if e == nil {
		return
	}
	e.lookups.Add(1)
	if history {
		e.history.Add(1)
	}
}

// History counts a read already counted by Lookup as answered by the time travel history.
func (e *Effort) History() {
	if e != nil {
		e.history.Add(1)
	}
}

// AddScanned adds n entries read by a scan.
func (e *Effort) AddScanned(n uint64) {
	if e != nil && n > 0 {
		e.scanned.Add(n)
	}
}

// SetResponseBytes records the size of the encoded answer, before any compression.
func (e *Effort) SetResponseBytes(n uint64) {
	if e != nil {
		e.responseBytes.Store(n)
	}
}

// EffortCounts is a snapshot of an Effort.
type EffortCounts struct {
	Opcodes, Lookups, History, Scanned, ResponseBytes uint64
	Eval                                              time.Duration
}

// Counts returns the counters so far, and the time since the Effort was made. Zero for nil.
func (e *Effort) Counts() EffortCounts {
	if e == nil {
		return EffortCounts{}
	}
	// history before lookups: each history read counts its lookup first, so a snapshot taken while
	// counting still has History <= Lookups.
	history := e.history.Load()
	return EffortCounts{
		Opcodes:       e.opcodes.Load(),
		Lookups:       e.lookups.Load(),
		History:       history,
		Scanned:       e.scanned.Load(),
		ResponseBytes: e.responseBytes.Load(),
		Eval:          time.Since(e.start),
	}
}

// Header returns the counters as an RFC 9651 dictionary of integers, the value of the
// X-Algod-Effort response header.
func (e *Effort) Header() string {
	c := e.Counts()
	return fmt.Sprintf("opcodes=%d, lookups=%d, history=%d, scanned=%d, response-bytes=%d, eval-us=%d",
		c.Opcodes, c.Lookups, c.History, c.Scanned, c.ResponseBytes, c.Eval.Microseconds())
}

type effortKey struct{}

// ContextWithEffort returns ctx carrying e, for the code that serves the request to count into.
func ContextWithEffort(ctx context.Context, e *Effort) context.Context {
	return context.WithValue(ctx, effortKey{}, e)
}

// EffortFromContext returns the Effort ctx carries, or nil.
func EffortFromContext(ctx context.Context) *Effort {
	if ctx == nil {
		return nil
	}
	e, _ := ctx.Value(effortKey{}).(*Effort)
	return e
}
