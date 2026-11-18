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
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	storetesting "github.com/algorand/go-algorand/ledger/store/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// certStates reads which of rounds [0, latest] have their certificate stripped.
func certStates(t *testing.T, l *Ledger) []bool {
	var states []bool
	err := l.blockDBs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		latest, err0 := blockdb.BlockLatest(tx)
		require.NoError(t, err0)
		states = make([]bool, latest+1)
		for r := basics.Round(0); r <= latest; r++ {
			states[r], err0 = blockdb.BlockCertStripped(tx, r)
			require.NoError(t, err0)
		}
		return nil
	})
	require.NoError(t, err)
	return states
}

// requireStrippedThrough waits until the certificates of rounds [1, last] are stripped and checks that no other is.
func requireStrippedThrough(t *testing.T, l *Ledger, last basics.Round) {
	t.Helper()
	var states []bool
	require.Eventually(t, func() bool {
		states = certStates(t, l)
		return last == 0 || states[last]
	}, 10*time.Second, 10*time.Millisecond, "round %d never stripped", last)
	for r, st := range states {
		require.Equal(t, r >= 1 && basics.Round(r) <= last, st, "round %d", r)
	}
}

func addBlocksWithCerts(t *testing.T, l *Ledger, blk *bookkeeping.Block, n int) {
	for i := 0; i < n; i++ {
		blk.BlockHeader.Round++
		blk.BlockHeader.TimeStamp += 1000
		require.NoError(t, l.AddBlock(*blk, agreement.Certificate{Round: blk.Round()}))
	}
	l.WaitForCommit(blk.Round())
}

func TestCertRetention(t *testing.T) {
	partitiontest.PartitionTest(t)

	skipCertRetentionFloor.Store(true)
	defer skipCertRetentionFloor.Store(false)

	dbPrefix := filepath.Join(t.TempDir(), fmt.Sprintf("%s.%d", t.Name(), crypto.RandUint64()))
	genesisInitState := getInitState()
	cfg := config.GetDefaultLocal()
	cfg.Archival = true

	// an archive without retention keeps every certificate
	l, err := OpenLedger(logging.TestingLog(t), dbPrefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	blk := genesisInitState.Block
	addBlocksWithCerts(t, l, &blk, 300)
	requireStrippedThrough(t, l, 0)
	l.Close()

	// turning retention on backfills, then strips as blocks come
	cfg.CertificateRetentionRounds = 50
	l, err = OpenLedger(logging.TestingLog(t), dbPrefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	requireStrippedThrough(t, l, 250)

	addBlocksWithCerts(t, l, &blk, 30)
	requireStrippedThrough(t, l, 280)

	// a stripped round still serves its block, with an empty certificate
	b, c, err := l.BlockCert(100)
	require.NoError(t, err)
	require.Equal(t, basics.Round(100), b.Round())
	require.Equal(t, agreement.Certificate{}, c)
	_, encCert, err := l.EncodedBlockCert(100)
	require.NoError(t, err)
	require.Empty(t, encCert)
	_, c, err = l.BlockCert(281)
	require.NoError(t, err)
	require.Equal(t, basics.Round(281), c.Round)
	l.Close()

	// turning it off keeps what was stripped and strips nothing more
	cfg.CertificateRetentionRounds = 0
	l, err = OpenLedger(logging.TestingLog(t), dbPrefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	addBlocksWithCerts(t, l, &blk, 20)
	requireStrippedThrough(t, l, 280)
	l.Close()

	// a shorter retention resumes from the cursor
	cfg.CertificateRetentionRounds = 10
	l, err = OpenLedger(logging.TestingLog(t), dbPrefix, false, genesisInitState, cfg)
	require.NoError(t, err)
	defer l.Close()
	requireStrippedThrough(t, l, 340)
}

func TestCertRetentionFloor(t *testing.T) {
	partitiontest.PartitionTest(t)

	cfg := config.GetDefaultLocal()
	cfg.Archival = true
	cfg.CertificateRetentionRounds = 1000
	_, err := OpenLedger(logging.TestingLog(t), t.TempDir()+"/ledger", false, getInitState(), cfg)
	require.ErrorIs(t, err, config.ErrCertificateRetentionTooLow)
}

// stripPattern builds a blocks table of rounds [0, n] and strips the certificates of the given ranges.
func stripPattern(t *testing.T, tx *sql.Tx, n basics.Round, ranges ...[2]basics.Round) {
	blk := getInitState().Block
	require.NoError(t, blockdb.BlockInit(tx, []bookkeeping.Block{blk}))
	for r := basics.Round(1); r <= n; r++ {
		blk.BlockHeader.Round = r
		require.NoError(t, blockdb.BlockPut(tx, blk, agreement.Certificate{Round: r}))
	}
	for _, rg := range ranges {
		_, err := blockdb.BlockStripCerts(context.Background(), tx, rg[0], rg[1])
		require.NoError(t, err)
	}
}

func TestFindCertCursor(t *testing.T) {
	partitiontest.PartitionTest(t)

	const n = 1000
	for _, tc := range []struct {
		name       string
		ranges     [][2]basics.Round
		cursor     basics.Round // from the binary search
		outOfOrder bool
		scanned    basics.Round
	}{
		{name: "none", cursor: 1, scanned: 1},
		{name: "bottom", ranges: [][2]basics.Round{{1, 400}}, cursor: 401, scanned: 401},
		{name: "bottom in two batches", ranges: [][2]basics.Round{{1, 7}, {8, 401}}, cursor: 402, scanned: 402},
		{name: "all", ranges: [][2]basics.Round{{0, n}}, cursor: n + 1, scanned: n + 1},
		// stripped by hand, not from the bottom: the binary search lands above the hole, the samples see it
		{name: "middle only", ranges: [][2]basics.Round{{500, 700}}, cursor: 701, outOfOrder: true, scanned: 1},
		{name: "hole below", ranges: [][2]basics.Round{{1, 100}, {200, 800}}, cursor: 801, outOfOrder: true, scanned: 101},
		// a hole above the cursor is harmless: it is stripped again, which skips the stripped rows
		{name: "hole above", ranges: [][2]basics.Round{{1, 300}, {600, 700}}, cursor: 301, scanned: 301},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dbs, _ := storetesting.DbOpenTest(t, true)
			defer dbs.Close()
			tx, err := dbs.Wdb.Handle.Begin()
			require.NoError(t, err)
			defer tx.Rollback()

			stripPattern(t, tx, n, tc.ranges...)
			st, err := blockdb.BlockCertStripped(tx, 0)
			require.NoError(t, err)
			require.Equal(t, len(tc.ranges) > 0 && tc.ranges[0][0] == 0, st, "genesis")

			cursor, outOfOrder, err := findCertCursor(tx)
			require.NoError(t, err)
			require.Equal(t, tc.cursor, cursor)
			require.Equal(t, tc.outOfOrder, outOfOrder)
			scanned, err := scanCertCursor(context.Background(), tx)
			require.NoError(t, err)
			require.Equal(t, tc.scanned, scanned)
		})
	}
}

// TestCertStripperOutOfOrder checks that the stripper recovers from a database stripped out of order.
func TestCertStripperOutOfOrder(t *testing.T) {
	partitiontest.PartitionTest(t)

	dbs, _ := storetesting.DbOpenTest(t, true)
	defer dbs.Close()
	err := dbs.Wdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		stripPattern(t, tx, 1000, [2]basics.Round{500, 700})
		return nil
	})
	require.NoError(t, err)

	s := newCertStripper(logging.TestingLog(t), dbs, 100, func() basics.Round { return 1000 })
	s.start()
	defer s.close()
	require.Eventually(t, func() bool {
		var done bool
		err := dbs.Rdb.Atomic(func(ctx context.Context, tx *sql.Tx) error {
			first, ok, err0 := blockdb.BlockFirstCertFrom(ctx, tx, 1)
			done = ok && first == 901
			return err0
		})
		require.NoError(t, err)
		return done
	}, 10*time.Second, 10*time.Millisecond)
}
