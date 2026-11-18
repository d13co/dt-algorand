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

package ledger

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/util/db"
	"github.com/algorand/go-algorand/util/metrics"
)

// Certificate retention (config.Local.CertificateRetentionRounds) discards the agreement certificate of every block
// more than retention rounds behind the latest committed round, keeping the block.
//
// Stripping always moves upward, so the stripped rounds form one unbroken run from the bottom of the blocks table:
// certdata is NULL for every round in [first, cursor), where first is the lowest round other than genesis. That
// lets the cursor be found at startup with a binary search of ~26 point lookups instead of a scan, and needs no
// schema change. A database stripped out of order by hand is detected by sampling and recovered with one scan.
//
// The stripper is a goroutine of the block queue. After every flush the syncer wakes it; it then strips up to the
// cutoff in transactions of certStripBatch rounds, pausing certStripPause between them so the block writer gets in.
// In the steady state that is one round per flush. When the option is first turned on for an existing archive it is
// a backfill of every round, which may take hours.

const (
	// certStripMinBatch and certStripMaxBatch bound the rounds stripped per transaction. The batch adapts to aim at
	// certStripTargetTx per transaction: rows are 30-90 KB on mainnet, and SQLite rewrites the whole row.
	certStripMinBatch = 16
	certStripMaxBatch = 10_000
	certStripTargetTx = 500 * time.Millisecond

	// certStripSamples is how many evenly spaced rounds below the binary-searched cursor are checked to catch a
	// database stripped out of order.
	certStripSamples = 64

	// certStripProgressLog is how many rounds the backfill strips between progress log lines.
	certStripProgressLog = 1_000_000
)

// certStripPause is the pause between two transactions of a backfill. A variable for tests.
var certStripPause = 50 * time.Millisecond

// skipCertRetentionFloor lets tests open a ledger with a CertificateRetentionRounds below
// config.Local.CertificateRetentionFloor.
var skipCertRetentionFloor atomic.Bool

var ledgerCertStripRounds = metrics.NewCounter("ledger_blockq_certstrip_rounds", "block certificates discarded")
var ledgerCertStripMicros = metrics.NewCounter("ledger_blockq_certstrip_micros", "µs spent discarding block certificates")
var ledgerCertStripErrors = metrics.NewCounter("ledger_blockq_certstrip_errors", "failed attempts to discard block certificates")

type certStripper struct {
	log       logging.Logger
	dbs       db.Pair
	retention basics.Round
	// committed returns the latest round written to the block database.
	committed func() basics.Round

	wake chan struct{}
	done chan struct{}
	// ctx is canceled by close, which stops the stripper and interrupts its transaction
	ctx    context.Context
	cancel context.CancelFunc

	// cursor is the lowest round whose certificate may still be present. Only the stripper goroutine uses it.
	cursor basics.Round
	batch  uint64
}

func newCertStripper(log logging.Logger, dbs db.Pair, retention uint64, committed func() basics.Round) *certStripper {
	ctx, cancel := context.WithCancel(context.Background())
	return &certStripper{
		log:       log,
		dbs:       dbs,
		retention: basics.Round(retention),
		committed: committed,
		wake:      make(chan struct{}, 1),
		done:      make(chan struct{}),
		ctx:       ctx,
		cancel:    cancel,
		batch:     1_000,
	}
}

func (s *certStripper) start() {
	go s.run()
}

// notify tells the stripper that new blocks were committed. It never blocks.
func (s *certStripper) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// close stops the stripper, rolling back the transaction it is in, and waits for it.
func (s *certStripper) close() {
	s.cancel()
	<-s.done
}

// sleep waits for d, returning false if the stripper is stopped first.
func (s *certStripper) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-s.ctx.Done():
		return false
	}
}

func (s *certStripper) run() {
	defer close(s.done)

	for failures := 0; ; failures++ {
		err := s.findCursor()
		if err == nil {
			break
		}
		if s.ctx.Err() != nil {
			return
		}
		s.failed(failures, "cannot find where stripping stopped", err)
		if !s.sleep(time.Minute) {
			return
		}
	}

	failures := 0
	backfillFrom := basics.Round(0)
	for {
		cutoff := s.committed().SubSaturate(s.retention)
		if s.cursor <= cutoff {
			if backfillFrom == 0 && cutoff-s.cursor >= certStripMaxBatch {
				backfillFrom = s.cursor
				s.log.Infof("certificate retention: discarding the certificates of rounds %d to %d in the background", s.cursor, cutoff)
			}
			err := s.stripTo(cutoff)
			if s.ctx.Err() != nil {
				return
			}
			if err != nil {
				s.failed(failures, fmt.Sprintf("discarding certificates from round %d", s.cursor), err)
				failures++
				// back off up to 10 minutes
				if !s.sleep(min(10*time.Second<<min(failures-1, 6), 10*time.Minute)) {
					return
				}
				continue
			}
			failures = 0
			if s.cursor <= cutoff {
				// a backfill: let the block writer in
				if !s.sleep(certStripPause) {
					return
				}
				continue
			}
			if backfillFrom != 0 {
				s.log.Infof("certificate retention: discarded the certificates of rounds %d to %d", backfillFrom, cutoff)
				backfillFrom = 0
			}
		}

		select {
		case <-s.wake:
		case <-s.ctx.Done():
			return
		}
	}
}

// failed counts and logs a failure: the first of a run as an error, so that an option silently not applied shows.
func (s *certStripper) failed(previous int, what string, err error) {
	ledgerCertStripErrors.Inc(nil)
	if previous == 0 {
		s.log.Errorf("certificate retention: %s: %v (retrying)", what, err)
	} else {
		s.log.Warnf("certificate retention: %s: %v (failure %d, retrying)", what, err, previous+1)
	}
}

// stripTo strips one batch of rounds from the cursor, up to cutoff, and moves the cursor past it.
func (s *certStripper) stripTo(cutoff basics.Round) error {
	lo := s.cursor
	hi := min(cutoff, lo+basics.Round(s.batch)-1)
	start := time.Now()
	var n int64
	err := s.dbs.Wdb.AtomicContext(s.ctx, func(ctx context.Context, tx *sql.Tx) error {
		// a batch may outlast the default one-second warning on a slow disk; certStripTargetTx keeps it near
		_, err0 := db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(10*time.Second))
		if err0 != nil {
			return err0
		}
		n, err0 = blockdb.BlockStripCerts(ctx, tx, lo, hi)
		return err0
	}, nil)
	elapsed := time.Since(start)
	ledgerCertStripMicros.AddMicrosecondsSince(start, nil)
	if err != nil {
		return err
	}
	ledgerCertStripRounds.AddUint64(uint64(n), nil)

	// adapt the batch to the time it took
	switch {
	case elapsed < certStripTargetTx/4:
		s.batch = min(s.batch*2, certStripMaxBatch)
	case elapsed > certStripTargetTx*2:
		s.batch = max(s.batch/2, certStripMinBatch)
	}

	if hi/certStripProgressLog != lo/certStripProgressLog {
		s.log.Infof("certificate retention: discarded certificates up to round %d", hi)
	}
	s.cursor = hi + 1
	return nil
}

// findCursor sets the cursor to the lowest round whose certificate may still be present.
func (s *certStripper) findCursor() error {
	var cursor basics.Round
	var outOfOrder bool
	err := s.dbs.Rdb.AtomicContext(s.ctx, func(ctx context.Context, tx *sql.Tx) error {
		var err0 error
		cursor, outOfOrder, err0 = findCertCursor(tx)
		return err0
	}, nil)
	if err != nil {
		return err
	}
	if outOfOrder {
		s.log.Warnf("certificate retention: the blocks below round %d were not stripped in order; scanning for the first certificate", cursor)
		start := time.Now()
		err = s.dbs.Rdb.AtomicContext(s.ctx, func(ctx context.Context, tx *sql.Tx) error {
			// the scan can take minutes on an archival node; close interrupts it
			_, err0 := db.ResetTransactionWarnDeadline(ctx, tx, time.Now().Add(time.Hour))
			if err0 != nil {
				return err0
			}
			cursor, err0 = scanCertCursor(ctx, tx)
			return err0
		}, nil)
		if err != nil {
			return err
		}
		s.log.Infof("certificate retention: the first certificate is at round %d (scan took %v)", cursor, time.Since(start))
	}
	s.cursor = cursor
	return nil
}

// certCursorFloor returns the lowest round stripping applies to: the earliest block, other than genesis.
func certCursorFloor(tx *sql.Tx) (lo, latest basics.Round, err error) {
	earliest, err := blockdb.BlockEarliest(tx)
	if err != nil {
		return 0, 0, err
	}
	latest, err = blockdb.BlockLatest(tx)
	if err != nil {
		return 0, 0, err
	}
	// round 0 is genesis, whose certificate is empty anyway
	return max(earliest, 1), latest, nil
}

// findCertCursor binary-searches the blocks table for the lowest round whose certificate is present, relying on the
// stripped rounds being one run from the bottom. It returns latest+1 if every round is stripped. outOfOrder is true
// if a sample below the found cursor still has its certificate, meaning stripping was done out of order and the
// cursor must come from scanCertCursor instead.
func findCertCursor(tx *sql.Tx) (cursor basics.Round, outOfOrder bool, err error) {
	lo, latest, err := certCursorFloor(tx)
	if err != nil {
		return 0, false, err
	}
	if lo > latest {
		return lo, false, nil
	}

	stripped := func(r basics.Round) (bool, error) {
		st, err0 := blockdb.BlockCertStripped(tx, r)
		var noEntry ledgercore.ErrNoEntry
		if errors.As(err0, &noEntry) {
			// a gap in the blocks table: treat as present, so the strip goes over it
			return false, nil
		}
		return st, err0
	}

	// find the first round in [lo, latest+1) that is not stripped
	a, b := lo, latest+1
	for a < b {
		mid := a + (b-a)/2
		st, err0 := stripped(mid)
		if err0 != nil {
			return 0, false, err0
		}
		if st {
			a = mid + 1
		} else {
			b = mid
		}
	}
	cursor = a

	// every round in [lo, cursor) should be stripped; check a spread of them
	if span := uint64(cursor - lo); span > 0 {
		step := max(span/certStripSamples, 1)
		for off := uint64(0); off < span; off += step {
			st, err0 := stripped(lo + basics.Round(off))
			if err0 != nil {
				return 0, false, err0
			}
			if !st {
				return cursor, true, nil
			}
		}
	}
	return cursor, false, nil
}

// scanCertCursor returns the lowest round whose certificate is present by scanning, or latest+1 if every round is
// stripped.
func scanCertCursor(ctx context.Context, tx *sql.Tx) (basics.Round, error) {
	lo, latest, err := certCursorFloor(tx)
	if err != nil {
		return 0, err
	}
	first, ok, err := blockdb.BlockFirstCertFrom(ctx, tx, lo)
	if err != nil {
		return 0, err
	}
	if !ok {
		return max(lo, latest+1), nil
	}
	return first, nil
}
