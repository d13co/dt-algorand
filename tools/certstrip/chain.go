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
	"bytes"
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/protocol"
)

// blockRow is a row of the blocks table. cert is read only when the walk needs it.
type blockRow struct {
	rnd     basics.Round
	proto   string
	hdrdata []byte
	blkdata []byte
	cert    []byte
}

// checkedBlock is a row after checkRow. blk and cert are set only for walks that keep blocks.
type checkedBlock struct {
	rnd  basics.Round
	hdr  bookkeeping.BlockHeader
	blk  *bookkeeping.Block
	cert *agreement.Certificate
	// paysetLen is the number of transactions in the block.
	paysetLen int
	err       error
}

// chainError is a failed check of one round.
type chainError struct {
	rnd basics.Round
	msg string
}

func (e chainError) Error() string { return fmt.Sprintf("round %d: %s", e.rnd, e.msg) }

func rowErr(rnd basics.Round, format string, args ...any) error {
	return chainError{rnd: rnd, msg: fmt.Sprintf(format, args...)}
}

// checkRow runs the checks of one row that need nothing else: both blobs decode and are exactly the canonical
// encoding of what they decode to (the block service and REST serve the stored bytes as they are, so extra fields
// or trailing bytes the decoder tolerates must not get through), the header column is the block's header (the
// ledger reads headers from that column alone), round and protocol columns match the header, and the payset
// matches the header's transaction commitments.
func checkRow(r blockRow, keep bool) checkedBlock {
	out := checkedBlock{rnd: r.rnd}
	var blk bookkeeping.Block
	if err := protocol.Decode(r.blkdata, &blk); err != nil {
		out.err = rowErr(r.rnd, "block does not decode: %v", err)
		return out
	}
	if !bytes.Equal(protocol.Encode(&blk), r.blkdata) {
		out.err = rowErr(r.rnd, "block is not canonically encoded (%d bytes stored, %d re-encoded)", len(r.blkdata), len(protocol.Encode(&blk)))
		return out
	}
	if err := protocol.Decode(r.hdrdata, &out.hdr); err != nil {
		out.err = rowErr(r.rnd, "header does not decode: %v", err)
		return out
	}
	if !bytes.Equal(protocol.Encode(&out.hdr), r.hdrdata) {
		out.err = rowErr(r.rnd, "header is not canonically encoded")
		return out
	}
	if !bytes.Equal(protocol.Encode(&blk.BlockHeader), r.hdrdata) {
		out.err = rowErr(r.rnd, "header column %v differs from the block's header %v", out.hdr.Hash(), blk.BlockHeader.Hash())
		return out
	}
	if out.hdr.Round != r.rnd {
		out.err = rowErr(r.rnd, "header is for round %d", out.hdr.Round)
		return out
	}
	if r.proto != string(out.hdr.CurrentProtocol) {
		out.err = rowErr(r.rnd, "proto column %q differs from the header's protocol %q", r.proto, out.hdr.CurrentProtocol)
		return out
	}
	out.paysetLen = len(blk.Payset)
	// the genesis block has no transactions and no commitments to them; link checks it against genesis.json
	if r.rnd != 0 && !blk.ContentsMatchHeader() {
		out.err = rowErr(r.rnd, "payset does not match the header's transaction commitments")
		return out
	}
	if keep {
		out.blk = &blk
		if len(r.cert) > 0 {
			var cert agreement.Certificate
			if err := protocol.Decode(r.cert, &cert); err != nil {
				out.err = rowErr(r.rnd, "certificate does not decode: %v", err)
				return out
			}
			out.cert = &cert
		}
	}
	return out
}

// walker checks a range of rounds of a block DB, in order.
type walker struct {
	db *sql.DB
	// genesisHdr is the encoding of the header of the block the genesis file makes, which round 0 must have, with
	// no transactions.
	genesisHdr []byte
	// workers decode and check rows in parallel; the hash chain is linked on the caller's goroutine.
	workers int
	// chunk is the rounds read per query.
	chunk int
	// keep makes the walk decode certificates and keep blocks for each.
	keep bool
}

// walk checks rounds [from, to]. prev is the header of round from-1, which round from must follow, or nil if from
// is 0. It calls each for every round in order, after its checks pass. It stops at the first failure, which is the
// lowest failing round, or when ctx is done; last is the last round that passed.
func (w *walker) walk(ctx context.Context, from, to basics.Round, prev *bookkeeping.BlockHeader, each func(checkedBlock) error) (last *bookkeeping.BlockHeader, err error) {
	if from > to {
		return prev, nil
	}
	if (from == 0) != (prev == nil) {
		return prev, fmt.Errorf("walk from round %d needs the header of round %d", from, from-1)
	}
	ctx, cancel := context.WithCancel(ctx)
	var wg sync.WaitGroup
	// cancel before waiting: the reader may be blocked handing out a chunk nobody will take
	defer func() {
		cancel()
		wg.Wait()
	}()

	type chunkJob struct {
		rows []blockRow
		out  chan []checkedBlock
	}
	jobs := make(chan chunkJob)
	ordered := make(chan chan []checkedBlock, 2*w.workers)
	readErr := make(chan error, 1)

	for range w.workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				res := make([]checkedBlock, len(j.rows))
				for i, r := range j.rows {
					res[i] = checkRow(r, w.keep)
				}
				j.out <- res
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		defer close(ordered)
		defer close(jobs)
		for lo := from; lo <= to; {
			hi := min(to, lo+basics.Round(w.chunk)-1)
			rows, err0 := w.read(ctx, lo, hi)
			if err0 != nil {
				readErr <- err0
				return
			}
			out := make(chan []checkedBlock, 1)
			select {
			case ordered <- out:
			case <-ctx.Done():
				return
			}
			select {
			case jobs <- chunkJob{rows: rows, out: out}:
			case <-ctx.Done():
				return
			}
			if hi == to {
				return
			}
			lo = hi + 1
		}
	}()

	last = prev
	next := from
	for out := range ordered {
		var res []checkedBlock
		select {
		case res = <-out:
		case <-ctx.Done():
			return last, ctx.Err()
		}
		for _, c := range res {
			if c.rnd != next {
				return last, rowErr(next, "missing from the blocks table (next row is round %d)", c.rnd)
			}
			if c.err != nil {
				return last, c.err
			}
			if err = w.link(last, c); err != nil {
				return last, err
			}
			if each != nil {
				if err = each(c); err != nil {
					return last, err
				}
			}
			hdr := c.hdr
			last = &hdr
			next++
		}
	}
	select {
	case err = <-readErr:
		return last, err
	default:
	}
	if err = ctx.Err(); err != nil {
		return last, err
	}
	if next != to+1 {
		return last, rowErr(next, "missing from the blocks table")
	}
	return last, nil
}

// link checks that c follows prev, or is the genesis block.
func (w *walker) link(prev *bookkeeping.BlockHeader, c checkedBlock) error {
	if prev == nil {
		if c.rnd != 0 {
			return rowErr(c.rnd, "no previous header")
		}
		// checkRow checked that the header column is the block's header; a block is its header and its payset
		if !bytes.Equal(protocol.Encode(&c.hdr), w.genesisHdr) {
			return rowErr(0, "the genesis block %v differs from the one genesis.json makes", c.hdr.Hash())
		}
		if c.paysetLen != 0 {
			return rowErr(0, "the genesis block has transactions")
		}
		return nil
	}
	if err := c.hdr.PreCheck(*prev); err != nil {
		return rowErr(c.rnd, "does not follow round %d (%v): %v", prev.Round, prev.Hash(), err)
	}
	return nil
}

// read returns the rows of rounds [lo, hi]. Leaving certdata, stored after the block, out of the query lets SQLite skip
// the certificate's overflow pages.
func (w *walker) read(ctx context.Context, lo, hi basics.Round) ([]blockRow, error) {
	cols := "rnd, proto, hdrdata, blkdata"
	if w.keep {
		cols += ", certdata"
	}
	q, err := w.db.QueryContext(ctx, "SELECT "+cols+" FROM blocks WHERE rnd>=? AND rnd<=? ORDER BY rnd", lo, hi)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	rows := make([]blockRow, 0, hi-lo+1)
	for q.Next() {
		var r blockRow
		var proto sql.NullString
		dest := []any{&r.rnd, &proto, &r.hdrdata, &r.blkdata}
		if w.keep {
			dest = append(dest, &r.cert)
		}
		if err = q.Scan(dest...); err != nil {
			return nil, err
		}
		r.proto = proto.String
		rows = append(rows, r)
	}
	return rows, q.Err()
}

// progress prints a line at most every interval.
type progress struct {
	start, last time.Time
	from, to    basics.Round
	interval    time.Duration
}

func (p *progress) report(rnd basics.Round, force bool) {
	now := time.Now()
	if !force && now.Sub(p.last) < p.interval {
		return
	}
	p.last = now
	secs := now.Sub(p.start).Seconds()
	rate := float64(rnd-p.from+1) / max(secs, 1e-3)
	eta := ""
	if rnd < p.to && rate > 0 {
		eta = fmt.Sprintf(", %v left", (time.Duration(float64(p.to-rnd)/rate) * time.Second).Round(time.Minute))
	}
	fmt.Printf("round %d/%d (%.0f rounds/s%s)\n", rnd, p.to, rate, eta)
}
