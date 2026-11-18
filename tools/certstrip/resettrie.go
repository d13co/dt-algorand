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
	"cmp"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb/sqlitedriver"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

func resetTrieCmd(args []string) error {
	fs, dataDir, stateDir := newFlags("reset-trie", `With algod stopped, prepares a snapshot's tracker DB to be checked against a catchpoint label:
  - clears the balances trie, so that algod rebuilds it from the accounts at startup: a trie left as the snapshot
    has it would give the network's label even with tampered accounts;
  - rebuilds the creatables index (assetcreators) from the resources table, and the txtail from the verified
    blocks: neither is covered by the label;
  - removes the snapshot's catchpoints (files and records), which algod would otherwise serve to peers, and its
    agreement (crash.sqlite) and state proof (stateproof.sqlite) databases, which are not this node's;
  - drops every trigger and view of the block and tracker DBs, which could change data after it was checked, and
    any catchpoint catchup the snapshot holds (state and staging tables), which algod would resume and finish
    with unchecked blocks and accounts.
Run verify-chain first: the txtail is rebuilt from blocks it verified.`)
	_ = fs.Parse(args)
	n, err := openNode(*dataDir, *stateDir)
	if err != nil {
		return err
	}
	if err = checkRunAs(n.dataDir, os.Geteuid()); err != nil {
		return err
	}
	if err = n.requireStopped(); err != nil {
		return err
	}
	return resetTrie(n)
}

func resetTrie(n *node) error {
	st, err := n.readChainState()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("no %s: run verify-chain first", n.chainState())
	}
	if _, err = os.Stat(n.trackerDB()); err != nil {
		return err
	}
	tt := filepath.Join(cmp.Or(n.dirs.TimeTravelGenesisDir, n.dirs.BlockGenesisDir), config.LedgerFilenamePrefix+".timetravel")
	if _, err = os.Stat(tt); err == nil {
		return fmt.Errorf("%s is a time travel store, which no catchpoint label covers: remove it (or install a checked one with ttsnap) first", tt)
	}
	tracker, err := db.MakeAccessor(n.trackerDB(), false, false)
	if err != nil {
		return err
	}
	defer tracker.Close()
	blocks, err := db.MakeAccessor(n.blockDB(), false, false)
	if err != nil {
		return err
	}
	defer blocks.Close()

	// first, before anything here writes and fires a trigger
	err = blocks.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		return dropObjects(ctx, tx, n.blockDB(), []string{"catchpointblocks"})
	})
	if err != nil {
		return err
	}
	if err = checkBlocksSchema(blocks.Handle); err != nil {
		return err
	}

	var dbRound basics.Round
	err = tracker.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		err0 := dropObjects(ctx, tx, n.trackerDB(), trackerStagingTables)
		if err0 != nil {
			return err0
		}
		dbRound, err0 = sqlitedriver.NewAccountsSQLReaderWriter(tx).AccountsRound()
		if err0 != nil {
			return err0
		}
		if dbRound > st.Round {
			return fmt.Errorf("the tracker DB is at round %d, past the verified rounds 0..%d: run verify-chain again", dbRound, st.Round)
		}
		fmt.Printf("tracker DB at round %d\n", dbRound)

		if _, err0 = tx.ExecContext(ctx, "DELETE FROM accounthashes"); err0 != nil {
			return err0
		}
		fmt.Println("cleared the balances trie: algod rebuilds it at startup")

		for _, q := range []string{
			"DELETE FROM storedcatchpoints",
			"DELETE FROM unfinishedcatchpoints",
			"DELETE FROM catchpointfirststageinfo",
		} {
			if _, err0 = tx.ExecContext(ctx, q); err0 != nil {
				return err0
			}
		}
		// the last label, an unfinished first stage, and above all a catchpoint catchup in progress, which algod
		// would resume at startup and finish by swapping in the staged blocks and accounts
		if _, err0 = tx.ExecContext(ctx, "DELETE FROM catchpointstate"); err0 != nil {
			return err0
		}

		if err0 = rebuildCreatables(ctx, tx); err0 != nil {
			return fmt.Errorf("rebuilding the creatables index: %w", err0)
		}
		if err0 = rebuildTxTail(ctx, tx, blocks, dbRound); err0 != nil {
			return fmt.Errorf("rebuilding the txtail: %w", err0)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err = n.writeReset(dbRound); err != nil {
		return err
	}

	if err = removeSnapshotFiles(n); err != nil {
		return err
	}
	warnParticipationKeys(n)
	fmt.Println("now start algod with Archival (catchpoints tracked, the archival default) and CertificateRetentionRounds 0; once it has made a catchpoint past the walked rounds, run certstrip anchor")
	return nil
}

// trackerStagingTables are the tracker DB tables a catchpoint catchup stages accounts in
// (ledger/store/trackerdb/sqlitedriver/catchpoint.go).
var trackerStagingTables = []string{
	"catchpointbalances", "catchpointassetcreators", "catchpointaccounthashes", "catchpointpendinghashes",
	"catchpointresources", "catchpointkvstore", "catchpointonlineaccounts", "catchpointonlineroundparamstail",
	"catchpointstateproofverification",
}

// dropObjects drops every trigger and view of a DB, and the given tables. A trigger could rewrite checked rows
// whenever algod writes; algod creates neither.
func dropObjects(ctx context.Context, tx *sql.Tx, path string, tables []string) error {
	rows, err := tx.QueryContext(ctx, "SELECT type, name FROM sqlite_master WHERE type IN ('trigger', 'view')")
	if err != nil {
		return err
	}
	type object struct{ kind, name string }
	var objs []object
	for rows.Next() {
		var o object
		if err = rows.Scan(&o.kind, &o.name); err != nil {
			rows.Close()
			return err
		}
		objs = append(objs, o)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	for _, o := range objs {
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("DROP %s %q", strings.ToUpper(o.kind), o.name)); err != nil {
			return err
		}
		fmt.Printf("dropped %s %s from %s\n", o.kind, o.name, path)
	}
	for _, t := range tables {
		var n int
		if err = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?", t).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, fmt.Sprintf("DROP TABLE %q", t)); err != nil {
			return err
		}
		fmt.Printf("dropped the catchpoint catchup table %s from %s\n", t, path)
	}
	return nil
}

// removeSnapshotFiles removes the catchpoint files, which algod would serve to peers, and the agreement and state
// proof databases, which belong to the node the snapshot was taken from.
func removeSnapshotFiles(n *node) error {
	dir := filepath.Join(n.dirs.CatchpointGenesisDir, trackerdb.CatchpointDirName)
	if err := os.RemoveAll(dir); err != nil {
		return err
	}
	fmt.Printf("removed %s and the catchpoint records\n", dir)
	for _, f := range []string{
		filepath.Join(n.dirs.StateproofGenesisDir, config.StateProofFileName),
		filepath.Join(n.dirs.CrashGenesisDir, config.CrashFilename),
	} {
		removed := false
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(f + suffix); err == nil {
				removed = true
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		if removed {
			fmt.Printf("removed %s\n", f)
		}
	}
	return nil
}

// warnParticipationKeys points out participation keys in the genesis directories: a snapshot of a participating node
// may carry its keys, and a node voting with another node's keys equivocates. They are not removed, since they may
// be the operator's own.
func warnParticipationKeys(n *node) {
	seen := map[string]bool{}
	var found []string
	for _, dir := range []string{n.dirs.RootGenesisDir, n.dirs.HotGenesisDir, n.dirs.ColdGenesisDir} {
		if seen[dir] {
			continue
		}
		seen[dir] = true
		keys, _ := filepath.Glob(filepath.Join(dir, "*.partkey"))
		found = append(found, keys...)
		if _, err := os.Stat(filepath.Join(dir, config.ParticipationRegistryFilename)); err == nil {
			found = append(found, filepath.Join(dir, config.ParticipationRegistryFilename))
		}
	}
	if len(found) > 0 {
		fmt.Printf("warning: participation keys found, which may have come with the snapshot: remove them unless they are this node's own (and stop algod first):\n  %s\n", strings.Join(found, "\n  "))
	}
}

type creatable struct {
	creator []byte
	ctype   basics.CreatableType
}

// rebuildCreatables rewrites the creatables index from the resources table: a creatable's creator is the account
// holding its params, as when a catchpoint is restored.
func rebuildCreatables(ctx context.Context, tx *sql.Tx) error {
	want := make(map[basics.CreatableIndex]creatable)
	rows, err := tx.QueryContext(ctx, "SELECT a.address, r.aidx, r.data FROM resources r JOIN accountbase a ON a.rowid = r.addrid")
	if err != nil {
		return err
	}
	for rows.Next() {
		var addr, data []byte
		var aidx basics.CreatableIndex
		if err = rows.Scan(&addr, &aidx, &data); err != nil {
			rows.Close()
			return err
		}
		var rd trackerdb.ResourcesData
		if err = protocol.Decode(data, &rd); err != nil {
			rows.Close()
			return fmt.Errorf("resource %d of %v: %w", aidx, addr, err)
		}
		if !rd.IsOwning() {
			continue
		}
		c := creatable{creator: addr, ctype: basics.AssetCreatable}
		if rd.IsApp() {
			c.ctype = basics.AppCreatable
		}
		if _, dup := want[aidx]; dup {
			rows.Close()
			return fmt.Errorf("creatable %d has two creators", aidx)
		}
		want[aidx] = c
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}

	var differ, extra int
	seen := 0
	rows, err = tx.QueryContext(ctx, "SELECT asset, creator, ctype FROM assetcreators")
	if err != nil {
		return err
	}
	for rows.Next() {
		var aidx basics.CreatableIndex
		var creator []byte
		var ctype basics.CreatableType
		if err = rows.Scan(&aidx, &creator, &ctype); err != nil {
			rows.Close()
			return err
		}
		c, ok := want[aidx]
		switch {
		case !ok:
			extra++
		case !bytes.Equal(c.creator, creator) || c.ctype != ctype:
			differ++
			seen++
		default:
			seen++
		}
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	missing := len(want) - seen

	if _, err = tx.ExecContext(ctx, "DELETE FROM assetcreators"); err != nil {
		return err
	}
	ins, err := tx.PrepareContext(ctx, "INSERT INTO assetcreators (asset, creator, ctype) VALUES (?, ?, ?)")
	if err != nil {
		return err
	}
	defer ins.Close()
	for aidx, c := range want {
		if _, err = ins.ExecContext(ctx, aidx, c.creator, c.ctype); err != nil {
			return err
		}
	}
	fmt.Printf("rebuilt the creatables index: %d creatables (the snapshot's differed on %d, lacked %d, had %d extra)\n", len(want), differ, missing, extra)
	return nil
}

// rebuildTxTail rewrites the txtail from the verified blocks before the tracker round, like the txtail migration
// of the tracker DB schema.
func rebuildTxTail(ctx context.Context, tx *sql.Tx, blocks db.Accessor, dbRound basics.Round) error {
	var tail [][]byte
	var first basics.Round
	err := blocks.Atomic(func(ctx context.Context, btx *sql.Tx) error {
		hdr, err0 := blockdb.BlockGetHdr(btx, dbRound)
		if err0 != nil {
			return err0
		}
		proto := config.Consensus[hdr.CurrentProtocol]
		first = max((dbRound + 1).SubSaturate(basics.Round(proto.MaxTxnLife+proto.DeeperBlockHeaderHistory)), 1)
		for rnd := first; rnd <= dbRound; rnd++ {
			blk, err1 := blockdb.BlockGet(btx, rnd)
			if err1 != nil {
				return err1
			}
			t, err1 := trackerdb.TxTailRoundFromBlock(blk)
			if err1 != nil {
				return err1
			}
			enc, _ := t.Encode()
			tail = append(tail, enc)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, "DELETE FROM txtail"); err != nil {
		return err
	}
	if len(tail) == 0 {
		return nil
	}
	if err = sqlitedriver.NewAccountsSQLReaderWriter(tx).TxtailNewRound(ctx, first, tail, first); err != nil {
		return err
	}
	fmt.Printf("rebuilt the txtail from rounds %d..%d\n", first, dbRound)
	return nil
}
