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
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/committee"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

// src gives label from two sources.
func src(label string) []string { return []string{"one=" + label, "two=" + label} }

func testLabel(rnd basics.Round, b byte) string {
	return fmt.Sprintf("%d#%s", rnd, crypto.Digest{b}.String())
}

// fakeTracker writes a tracker DB holding only the catchpoint tables anchor reads, with label for rnd.
func (f *fixture) fakeTracker(t *testing.T, rnd basics.Round, label string) {
	acc, err := db.MakeAccessor(filepath.Join(f.dataDir, f.genesis.ID(), trackerDBFile), false, false)
	require.NoError(t, err)
	defer acc.Close()
	for _, q := range []string{
		"CREATE TABLE IF NOT EXISTS storedcatchpoints (round integer primary key, filename text NOT NULL, catchpoint text NOT NULL, filesize size NOT NULL, pinned integer NOT NULL)",
		"CREATE TABLE IF NOT EXISTS catchpointstate (id string primary key, intval integer, strval text)",
	} {
		_, err = acc.Handle.Exec(q)
		require.NoError(t, err)
	}
	// like algod: a file record without the label, which only the last catchpoint state keeps
	_, err = acc.Handle.Exec("INSERT INTO storedcatchpoints VALUES (?, 'f', '', 1, 0)", rnd)
	require.NoError(t, err)
	_, err = acc.Handle.Exec("INSERT OR REPLACE INTO catchpointstate (id, strval) VALUES ('lastCatchpoint', ?)", label)
	require.NoError(t, err)
}

func TestAnchor(t *testing.T) {
	partitiontest.PartitionTest(t)

	ctx := context.Background()
	f := newFixture(t, 50)
	n := f.node(t)
	require.NoError(t, runVerify(t, n, 0))

	// algod catches up past the snapshot and makes a catchpoint for round 80
	f.add(t, 40)
	label := testLabel(80, 1)
	f.fakeTracker(t, 80, label)

	require.ErrorContains(t, anchor(ctx, n, []string{"a=" + label}, 2, 2), "2 sources or more")
	require.ErrorContains(t, anchor(ctx, n, []string{"a=" + label, "a=" + label}, 2, 2), "given twice")
	require.ErrorContains(t, anchor(ctx, n, []string{label, label}, 2, 2), "naming where it came from")
	require.ErrorContains(t, anchor(ctx, n, []string{"a=" + label, "b=" + testLabel(80, 2)}, 2, 2), "the given labels differ")
	require.ErrorContains(t, anchor(ctx, n, src(label), 2, 2), "run reset-trie")

	require.NoError(t, n.writeReset(80))
	require.ErrorContains(t, anchor(ctx, n, src(label), 2, 2), "is not past the tracker round")
	require.NoError(t, n.writeReset(50))

	other := testLabel(80, 3)
	require.ErrorContains(t, anchor(ctx, n, src(other), 2, 2), "not the network's")
	require.ErrorContains(t, anchor(ctx, n, src(testLabel(70, 1)), 2, 2), "the node's last catchpoint is for round 80, not 70")

	require.NoError(t, anchor(ctx, n, src(label), 2, 2))
	a, err := n.readAnchor()
	require.NoError(t, err)
	require.Equal(t, basics.Round(80), a.Round)
	require.Equal(t, label, a.Label)
	require.Equal(t, []string{"one", "two"}, a.Sources)
	require.Equal(t, basics.Round(50), a.ChainRound)
	require.Equal(t, f.blocks[50].Hash().String(), a.ChainHash)
}

func TestAnchorBrokenLink(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 50)
	n := f.node(t)
	require.NoError(t, runVerify(t, n, 0))
	f.add(t, 40)
	// a block between the walk and the labelled round that doesn't link
	blk := f.blocks[60]
	blk.BlockHeader.Seed = committee.Seed{9} // a field PreCheck doesn't look at: round 61 no longer links
	f.reencode(t, blk)
	label := testLabel(80, 1)
	f.fakeTracker(t, 80, label)
	require.NoError(t, n.writeReset(50))
	err := anchor(context.Background(), n, src(label), 2, 2)
	require.ErrorContains(t, err, "round 61: does not follow")
	_, err = os.Stat(n.anchor())
	require.ErrorIs(t, err, os.ErrNotExist)
}

func certCount(t *testing.T, path string) (stripped, kept int, maxRnd basics.Round) {
	acc, err := db.MakeAccessor(path, true, false)
	require.NoError(t, err)
	defer acc.Close()
	err = acc.Handle.QueryRow("SELECT SUM(certdata IS NULL), SUM(certdata IS NOT NULL), MAX(rnd) FROM blocks").Scan(&stripped, &kept, &maxRnd)
	require.NoError(t, err)
	return
}

func TestCompact(t *testing.T) {
	partitiontest.PartitionTest(t)

	ctx := context.Background()
	f := newFixture(t, 100)
	n := f.node(t)
	out := n.blockDB() + ".compact"

	// stripping needs an anchor, which must cover the cutoff
	_, err := stripCutoff(n, 100, 10, false)
	require.ErrorContains(t, err, "verify the chain and anchor it first")
	cutoff, err := stripCutoff(n, 100, 10, true)
	require.NoError(t, err)
	require.Equal(t, basics.Round(90), cutoff)
	require.NoError(t, writeJSON(n.anchor(), anchorState{GenesisHash: f.genesis.Hash().String(), Round: 80}))
	_, err = stripCutoff(n, 100, 10, false)
	require.ErrorContains(t, err, "past the anchored round 80")
	require.ErrorContains(t, compact(ctx, n, out, 10, false, false), "past the anchored round 80")
	_, err = os.Stat(out)
	require.ErrorIs(t, err, os.ErrNotExist)
	cutoff, err = stripCutoff(n, 100, 20, false)
	require.NoError(t, err)
	require.Equal(t, basics.Round(80), cutoff)

	require.NoError(t, compact(ctx, n, out, 20, false, false))
	stripped, kept, maxRnd := certCount(t, out)
	require.Equal(t, 80, stripped) // rounds 1..80; genesis has an empty but present certificate
	require.Equal(t, 21, kept)
	require.Equal(t, basics.Round(100), maxRnd)
	// the copy is a valid chain
	require.NoError(t, os.Rename(out, n.blockDB()))
	require.NoError(t, runVerify(t, n, 0))

	// -replace swaps the copy in and keeps the old file
	require.NoError(t, compact(ctx, n, out, 10, true, true))
	stripped, kept, _ = certCount(t, n.blockDB())
	require.Equal(t, 90, stripped)
	require.Equal(t, 11, kept)
	stripped, _, _ = certCount(t, n.blockDB()+".old")
	require.Equal(t, 80, stripped)
	_, err = os.Stat(out)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, compact(ctx, n, out, 10, true, true), ".old exists")

	// the swap renames, so the copy must be next to the block DB
	require.ErrorContains(t, compact(ctx, n, filepath.Join(t.TempDir(), "x"), 10, true, true), "must be in the block DB's directory")
}

func TestStripCutoffFull(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 100)
	n := f.node(t)
	// a walk without certificates is not enough
	require.NoError(t, runVerify(t, n, 0))
	_, err := stripCutoff(n, 100, 10, false)
	require.ErrorContains(t, err, "anchor it first")
	// a genesis replay is, up to where it went
	hdr := f.blocks[50].BlockHeader
	require.NoError(t, n.writeChainState(&hdr, true))
	cutoff, err := stripCutoff(n, 100, 50, false)
	require.NoError(t, err)
	require.Equal(t, basics.Round(50), cutoff)
	_, err = stripCutoff(n, 100, 49, false)
	require.ErrorContains(t, err, "anchor it first")
}

// TestStateNotInDataDir checks that state files shipped in the data directory count for nothing.
func TestStateNotInDataDir(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 10)
	n := f.node(t)
	require.NotContains(t, n.chainState(), f.dataDir)
	for _, name := range []string{"certstrip-chain.json", "chain.json", "anchor.json", "reset.json"} {
		require.NoError(t, writeJSON(filepath.Join(f.dataDir, f.genesis.ID(), name), chainState{GenesisHash: f.genesis.Hash().String(), Round: 10, Hash: f.blocks[10].Hash().String()}))
	}
	st, err := n.readChainState()
	require.NoError(t, err)
	require.Nil(t, st)
	a, err := n.readAnchor()
	require.NoError(t, err)
	require.Nil(t, a)
}

func TestFullReplayFakeCerts(t *testing.T) {
	partitiontest.PartitionTest(t)

	ctx := context.Background()
	f := newFixture(t, 10)
	n := f.node(t)
	work := t.TempDir()
	// the fixture's certificates are empty: the first one fails, after round 0 checked against genesis
	err := fullReplay(ctx, n, work, 0, 2, false)
	require.ErrorContains(t, err, "round 1: certificate")
	st, err := n.readChainState()
	require.NoError(t, err)
	require.Nil(t, st)

	// stripped certificates can't be replayed
	f.exec(t, "UPDATE blocks SET certdata=NULL WHERE rnd>=1")
	err = fullReplay(ctx, n, work, 0, 2, false)
	require.ErrorContains(t, err, "round 1: no certificate")
}

// trackerRows counts the rows of a tracker DB table.
func trackerRows(t *testing.T, n *node, table string) int {
	acc, err := db.MakeAccessor(n.trackerDB(), true, false)
	require.NoError(t, err)
	defer acc.Close()
	var c int
	require.NoError(t, acc.Handle.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&c))
	return c
}

func execTracker(t *testing.T, n *node, q string, args ...any) {
	acc, err := db.MakeAccessor(n.trackerDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	err = acc.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		_, err0 := tx.Exec(q, args...)
		return err0
	})
	require.NoError(t, err)
}
