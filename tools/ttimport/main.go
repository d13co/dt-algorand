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

// ttimport builds a time travel node's ledger from an existing archival
// node's block database (ledger.block.sqlite) instead of the network. The
// ledger opens the archive in place as its block database and replays it the
// way algod replays after a restart, without certificate or signature checks,
// resuming from where the data directory's trackers stand. Nothing is written
// to the archive. Afterwards the data directory's own block database gets the
// last blocks algod needs to start; start algod on the directory to keep
// syncing from the network.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/stateproof"
	"github.com/algorand/go-algorand/util/db"
)

const blockDBFile = config.LedgerFilenamePrefix + ".block.sqlite"

func main() {
	blocks := flag.String("blocks", "", "source block database: <archival data dir>/<genesis id>/"+blockDBFile)
	dataDir := flag.String("d", "", "algod data directory to build: genesis.json, and config.json with EnableTimeTravel")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of the run to this file")
	verbose := flag.Bool("v", false, "log the ledger's info messages, such as replay progress every 5 seconds")
	flag.Parse()
	if *blocks == "" || *dataDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	// lock tracking records a stack trace per Lock, ~25% of the import's CPU
	deadlock.Opts.Disable = true
	logging.Base().SetLevel(logging.Warn)
	if *verbose {
		logging.Base().SetLevel(logging.Info)
	}
	if *cpuprofile != "" {
		f, err := os.Create(*cpuprofile)
		if err == nil {
			err = pprof.StartCPUProfile(f)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "ttimport:", err)
			os.Exit(1)
		}
	}
	// The replay can't stop early, so Ctrl-C kills it (keeping the profile); a rerun
	// resumes from the trackers' last commit.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-sig
		pprof.StopCPUProfile()
		os.Exit(1)
	}()
	err := run(*blocks, *dataDir)
	pprof.StopCPUProfile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ttimport:", err)
		os.Exit(1)
	}
}

func run(blocksPath, dataDir string) error {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !cfg.EnableTimeTravel {
		return errors.New("the data directory's config.json must set EnableTimeTravel")
	}
	if cfg.Archival {
		return errors.New("the data directory's config.json must not set Archival: the node keeps only the last blocks")
	}
	// The ledger opens <dir>/ledger.block.sqlite, and (like db.MakeAccessor) creates it if missing.
	if filepath.Base(blocksPath) != blockDBFile {
		return fmt.Errorf("the source must be named %s", blockDBFile)
	}
	if _, err = os.Stat(blocksPath); err != nil {
		return err
	}
	src, err := db.MakeAccessor(blocksPath, true, false)
	if err != nil {
		return fmt.Errorf("%s: %w", blocksPath, err)
	}
	defer src.Close()
	// Check the network before a replay that can take days. An empty source fails
	// here too; the ledger would write a genesis block into it.
	var tip basics.Round
	var tipHdr []byte
	err = src.Handle.QueryRow("SELECT rnd, hdrdata FROM blocks ORDER BY rnd DESC LIMIT 1").Scan(&tip, &tipHdr)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("%s has no blocks", blocksPath)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", blocksPath, err)
	}
	var hdr bookkeeping.BlockHeader
	if err = protocol.Decode(tipHdr, &hdr); err != nil {
		return fmt.Errorf("%s round %d: %w", blocksPath, tip, err)
	}
	if hdr.GenesisHash != genesis.Hash() {
		return fmt.Errorf("%s belongs to another network (genesis hash %v)", blocksPath, hdr.GenesisHash)
	}

	log := logging.Base()
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), log)
	if err != nil {
		return err
	}
	srcDir, err := os.Stat(filepath.Dir(blocksPath))
	if err != nil {
		return err
	}
	// the ledger writes its account DB, history and catchpoint dirs in these, and ttimport rewrites the block DB
	for _, dir := range []string{dirs.BlockGenesisDir, dirs.TrackerGenesisDir, dirs.TimeTravelGenesisDir, dirs.CatchpointGenesisDir} {
		if fi, statErr := os.Stat(dir); statErr == nil && os.SameFile(fi, srcDir) {
			return fmt.Errorf("the data directory's ledger would write into the source's directory %s", dir)
		}
	}
	dstPath := filepath.Join(dirs.BlockGenesisDir, blockDBFile)

	bal, err := genesis.Balances()
	if err != nil {
		return err
	}
	// Archival is off, so the ledger neither resets nor prunes the archive: it
	// only writes blocks added to it, and replay adds none.
	replayDirs := dirs
	replayDirs.BlockGenesisDir = filepath.Dir(blocksPath)
	cfg.CatchpointTracking = config.CatchpointTrackingModeUntracked // no catchpoint work during the import
	fmt.Printf("replaying the source up to round %d (-v shows progress); start algod on %s only after this finishes\n", tip, dataDir)
	start := time.Now()
	l, err := data.LoadLedger(log, ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: replayDirs},
		false, genesis.Proto, bal, genesis.ID(), genesis.Hash(), cfg)
	if err != nil {
		return err
	}
	latest, dbRound := l.Latest(), l.LatestTrackerCommitted()
	hdrs := make([]bookkeeping.BlockHeader, 2)
	hdrs[0], err = l.BlockHdr(dbRound)
	if err == nil {
		hdrs[1], err = l.BlockHdr(latest)
	}
	l.Close()
	if err != nil {
		return err
	}
	elapsed := time.Since(start).Round(time.Second)

	// Keep what catchpoint catchup fetches (catchup/catchpointService.go), counted back
	// from the trackers' round, where algod's replay starts, under either round's protocol.
	lo := dbRound
	for _, h := range hdrs {
		proto := config.Consensus[h.CurrentProtocol]
		lo = min(lo, dbRound.SubSaturate(basics.Round(max(proto.MaxTxnLife+proto.DeeperBlockHeaderHistory, proto.MaxBalLookback))))
		if proto.StateProofInterval > 0 {
			lo = min(lo, stateproof.GetOldestExpectedStateProof(&h).SubSaturate(basics.Round(proto.StateProofInterval+proto.StateProofVotersLookback)))
		}
	}
	copied, err := keepBlocks(src.Handle, dstPath, lo, latest)
	if err != nil {
		return fmt.Errorf("copying the last blocks into %s: %w", dstPath, err)
	}
	fmt.Printf("replayed to round %d in %v\n", latest, elapsed)
	if copied {
		fmt.Printf("%s now holds rounds %d..%d\n", dstPath, lo, latest)
	} else {
		fmt.Printf("%s already reaches round %d; left as is\n", dstPath, latest)
	}
	return nil
}

// keepBlocks replaces dst's blocks with src's rounds lo..hi in one transaction,
// unless dst already reaches hi.
func keepBlocks(src *sql.DB, dstPath string, lo, hi basics.Round) (bool, error) {
	dst, err := db.MakeAccessor(dstPath, false, false)
	if err != nil {
		return false, err
	}
	defer dst.Close()
	tx, err := dst.Handle.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	err = blockdb.BlockInit(tx, nil)
	if err != nil {
		return false, err
	}
	dstNext, err := blockdb.BlockNext(tx)
	if err != nil || dstNext > hi {
		return false, err
	}
	_, err = tx.Exec("DELETE FROM blocks")
	if err != nil {
		return false, err
	}
	rows, err := src.Query("SELECT rnd, proto, hdrdata, blkdata, certdata FROM blocks WHERE rnd BETWEEN ? AND ? ORDER BY rnd", lo, hi)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	next := lo
	for rows.Next() {
		var rnd basics.Round
		var proto string
		var hdr, blk, cert []byte
		err = rows.Scan(&rnd, &proto, &hdr, &blk, &cert)
		if err != nil {
			return false, err
		}
		if rnd != next {
			return false, fmt.Errorf("the source has no round %d", next)
		}
		_, err = tx.Exec("INSERT INTO blocks (rnd, proto, hdrdata, blkdata, certdata) VALUES (?, ?, ?, ?, ?)", rnd, proto, hdr, blk, cert)
		if err != nil {
			return false, err
		}
		next++
	}
	err = rows.Err()
	if err != nil {
		return false, err
	}
	if next != hi+1 {
		return false, fmt.Errorf("the source has no round %d", next)
	}
	return true, tx.Commit()
}
