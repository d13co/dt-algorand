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
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merkletrie"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb/sqlitedriver"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

// openArchive opens (creating it the first time) the archival ledger of the data directory, which tracks
// catchpoints and so keeps the balances trie, and closes it.
func openArchive(t *testing.T, dataDir string) {
	g := testGenesis()
	bal, err := g.Balances()
	require.NoError(t, err)
	cfg := config.GetDefaultLocal()
	cfg.Archival = true
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, g.ID(), logging.TestingLog(t))
	require.NoError(t, err)
	l, err := data.LoadLedger(logging.TestingLog(t), ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs},
		false, g.Proto, bal, g.ID(), g.Hash(), cfg)
	require.NoError(t, err)
	l.Close()
}

func trieRoot(t *testing.T, n *node) crypto.Digest {
	store, err := sqlitedriver.Open(n.trackerDB(), false, logging.TestingLog(t))
	require.NoError(t, err)
	defer store.Close()
	var root crypto.Digest
	err = store.Transaction(func(ctx context.Context, tx trackerdb.TransactionScope) error {
		c, err0 := tx.MakeMerkleCommitter(false)
		if err0 != nil {
			return err0
		}
		trie, err0 := merkletrie.MakeTrie(c, trackerdb.TrieMemoryConfig)
		if err0 != nil {
			return err0
		}
		root, err0 = trie.RootHash()
		return err0
	})
	require.NoError(t, err)
	return root
}

// tamperBalance adds to an account's balance in the tracker DB, as a doctored snapshot would.
func tamperBalance(t *testing.T, n *node, addr basics.Address) {
	acc, err := db.MakeAccessor(n.trackerDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	err = acc.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		var buf []byte
		if err0 := tx.QueryRow("SELECT data FROM accountbase WHERE address=?", addr[:]).Scan(&buf); err0 != nil {
			return err0
		}
		var ad trackerdb.BaseAccountData
		if err0 := protocol.Decode(buf, &ad); err0 != nil {
			return err0
		}
		ad.MicroAlgos.Raw += 1_000_000
		_, err0 := tx.Exec("UPDATE accountbase SET data=? WHERE address=?", protocol.Encode(&ad), addr[:])
		return err0
	})
	require.NoError(t, err)
}

func TestResetTrie(t *testing.T) {
	partitiontest.PartitionTest(t)

	for _, tampered := range []bool{false, true} {
		t.Run(map[bool]string{false: "clean", true: "tampered"}[tampered], func(t *testing.T) {
			dataDir := newDataDir(t, testGenesis())
			openArchive(t, dataDir)
			n, err := openNode(dataDir, t.TempDir())
			require.NoError(t, err)
			root := trieRoot(t, n)
			require.False(t, root.IsZero())

			if tampered {
				// left as is, the stored trie hides the change: the label would still match
				tamperBalance(t, n, testAcct)
				openArchive(t, dataDir)
				require.Equal(t, root, trieRoot(t, n))
			}

			// leftovers of the snapshot's node, and traps: a trigger that would undo the reset, a catchpoint catchup
			// that algod would resume and finish with staged blocks and accounts
			execTracker(t, n, "INSERT INTO assetcreators (asset, creator, ctype) VALUES (77, ?, 0)", testAcct[:])
			execTracker(t, n, "CREATE TRIGGER evil AFTER DELETE ON accounthashes BEGIN UPDATE accountbase SET data=data; END")
			execTracker(t, n, "CREATE VIEW evilview AS SELECT 1")
			execTracker(t, n, "INSERT INTO catchpointstate (id, intval) VALUES ('catchpointCatchupState', 4)")
			execTracker(t, n, "CREATE TABLE catchpointbalances (addrid INTEGER PRIMARY KEY NOT NULL, address blob NOT NULL, data blob, normalizedonlinebalance INTEGER)")
			execBlocks(t, n, "CREATE TABLE catchpointblocks (rnd integer primary key, proto text, hdrdata blob, blkdata blob, certdata blob)")
			execBlocks(t, n, "CREATE TRIGGER evilblocks AFTER INSERT ON blocks BEGIN UPDATE blocks SET blkdata=x'00' WHERE rnd=0; END")
			execTracker(t, n, "INSERT INTO storedcatchpoints VALUES (10000, 'f', ?, 1, 0)", testLabel(10000, 1))
			cpDir := filepath.Join(n.dirs.CatchpointGenesisDir, trackerdb.CatchpointDirName)
			require.NoError(t, os.MkdirAll(cpDir, 0o700))
			require.NoError(t, os.WriteFile(filepath.Join(cpDir, "x.catchpoint"), []byte("x"), 0o600))
			crash := filepath.Join(n.dirs.CrashGenesisDir, config.CrashFilename)
			require.NoError(t, os.WriteFile(crash, []byte("x"), 0o600))

			require.ErrorContains(t, resetTrie(n), "run verify-chain first")
			require.NoError(t, runVerify(t, n, 0))
			require.NoError(t, resetTrie(n))
			require.True(t, trieRoot(t, n).IsZero())
			require.Zero(t, trackerRows(t, n, "assetcreators"))
			require.Zero(t, trackerRows(t, n, "storedcatchpoints"))
			require.Zero(t, trackerRows(t, n, "catchpointstate"))
			require.Equal(t, []string{"accountbase", "accounthashes", "accounttotals", "acctrounds", "assetcreators", "catchpointfirststageinfo", "catchpointstate", "kvstore", "onlineaccounts", "onlineroundparamstail", "resources", "stateproofverification", "storedcatchpoints", "txtail", "unfinishedcatchpoints"},
				schemaNames(t, n.trackerDB(), "trigger', 'view', 'table"))
			require.Equal(t, []string{"blocks"}, schemaNames(t, n.blockDB(), "trigger', 'view', 'table"))
			_, err = os.Stat(cpDir)
			require.ErrorIs(t, err, os.ErrNotExist)
			_, err = os.Stat(crash)
			require.ErrorIs(t, err, os.ErrNotExist)
			rs, err := n.readReset()
			require.NoError(t, err)
			require.Equal(t, basics.Round(0), rs.Round)

			// algod rebuilds the trie from the accounts
			openArchive(t, dataDir)
			rebuilt := trieRoot(t, n)
			require.False(t, rebuilt.IsZero())
			if tampered {
				require.NotEqual(t, root, rebuilt)
			} else {
				require.Equal(t, root, rebuilt)
			}
		})
	}
}

func TestRebuildTxTail(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 10)
	blocks, err := db.MakeAccessor(f.blockDB(), true, false)
	require.NoError(t, err)
	defer blocks.Close()
	tracker, err := db.MakeAccessor(filepath.Join(t.TempDir(), "t.sqlite"), false, false)
	require.NoError(t, err)
	defer tracker.Close()

	err = tracker.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		_, err0 := tx.Exec("CREATE TABLE txtail (rnd INTEGER PRIMARY KEY NOT NULL, data BLOB NOT NULL)")
		require.NoError(t, err0)
		_, err0 = tx.Exec("INSERT INTO txtail VALUES (99, x'80')")
		require.NoError(t, err0)
		return rebuildTxTail(ctx, tx, blocks, 8)
	})
	require.NoError(t, err)

	rows, err := tracker.Handle.Query("SELECT rnd, data FROM txtail ORDER BY rnd")
	require.NoError(t, err)
	defer rows.Close()
	want := basics.Round(1)
	for rows.Next() {
		var rnd basics.Round
		var buf []byte
		require.NoError(t, rows.Scan(&rnd, &buf))
		require.Equal(t, want, rnd)
		var tail trackerdb.TxTailRound
		require.NoError(t, protocol.Decode(buf, &tail))
		require.Equal(t, f.blocks[rnd].BlockHeader, tail.Hdr)
		if rnd == paysetRound {
			stxn, _, err := f.blocks[rnd].DecodeSignedTxn(f.blocks[rnd].Payset[0])
			require.NoError(t, err)
			require.Equal(t, []basics.Round{100}, tail.LastValid)
			require.Equal(t, stxn.ID(), tail.TxnIDs[0])
		} else {
			require.Empty(t, tail.TxnIDs)
		}
		want++
	}
	require.NoError(t, rows.Err())
	require.Equal(t, basics.Round(9), want)
}

func execBlocks(t *testing.T, n *node, q string) {
	acc, err := db.MakeAccessor(n.blockDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	_, err = acc.Handle.Exec(q)
	require.NoError(t, err)
}

// schemaNames lists the names of a DB's schema objects of the given types (quoted for an IN list).
func schemaNames(t *testing.T, path, types string) []string {
	acc, err := db.MakeAccessor(path, true, false)
	require.NoError(t, err)
	defer acc.Close()
	rows, err := acc.Handle.Query("SELECT name FROM sqlite_master WHERE type IN ('" + types + "') AND name NOT LIKE 'sqlite_%' ORDER BY name")
	require.NoError(t, err)
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		require.NoError(t, rows.Scan(&name))
		names = append(names, name)
	}
	require.NoError(t, rows.Err())
	return names
}

func TestResetTrieRefusesTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 3)
	n := f.node(t)
	require.NoError(t, runVerify(t, n, 0))
	f.fakeTracker(t, 1, testLabel(1, 1))
	require.NoError(t, os.MkdirAll(filepath.Join(f.dataDir, f.genesis.ID(), "ledger.timetravel"), 0o700))
	require.ErrorContains(t, resetTrie(n), "time travel store")
}
