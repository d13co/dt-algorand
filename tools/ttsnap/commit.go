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

package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// commitmentOf computes the commitment of the store up to rnd, with the
// header hash of rnd, reporting its progress to log if not nil. o.Progress is
// set here.
func commitmentOf(st *timetravel.Store, rnd basics.Round, o timetravel.CommitmentOptions, log io.Writer) (*CommitmentInfo, bookkeeping.BlockHash, error) {
	hdr, err := storeHeader(st, uint64(rnd))
	if err != nil {
		return nil, bookkeeping.BlockHash{}, err
	}
	var mu deadlock.Mutex
	start, reported := time.Now(), time.Now()
	progress := func(read uint64) {
		if log == nil {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if time.Since(reported) < progressEvery {
			return
		}
		reported = time.Now()
		fmt.Fprintf(log, "commitment: read %s, %s/s\n", size(int64(read)), size(int64(float64(read)/time.Since(start).Seconds())))
	}
	o.Progress = progress
	c, err := st.Commitment(context.Background(), uint64(rnd), o)
	if err != nil {
		return nil, bookkeeping.BlockHash{}, err
	}
	if log != nil {
		fmt.Fprintf(log, "commitment of rounds %d..%d: %d objects, %d versions, %d headers, %d expired stake records, in %s\n",
			c.First, c.Round, c.Objects, c.Versions, c.Headers, c.Expired, roundDuration(time.Since(start)))
	}
	return &CommitmentInfo{
		Digest:   hex.EncodeToString(c.Digest[:]),
		Objects:  c.Objects,
		Versions: c.Versions,
		Headers:  c.Headers,
		Expired:  c.Expired,
	}, hdr.Hash(), nil
}
