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
	"strconv"
	"strings"
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
	"github.com/algorand/go-algorand/util/metrics"
)

const blockDBFile = config.LedgerFilenamePrefix + ".block.sqlite"

func main() {
	blocks := flag.String("blocks", "", "source block database: <archival data dir>/<genesis id>/"+blockDBFile)
	dataDir := flag.String("d", "", "algod data directory to build: genesis.json, and config.json with EnableTimeTravel")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of the run to this file")
	verbose := flag.Bool("v", false, "also log the ledger's info messages")
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
	from := accountsRound(filepath.Join(dirs.TrackerGenesisDir, config.LedgerFilenamePrefix+".tracker.sqlite"))
	fmt.Printf("replaying rounds %d..%d of the source; start algod on %s only after this finishes\n", from+1, tip, dataDir)
	start := time.Now()
	stopProgress := reportProgress(src.Handle, from, tip, start)
	l, err := data.LoadLedger(log, ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: replayDirs},
		false, genesis.Proto, bal, genesis.ID(), genesis.Hash(), cfg)
	stopProgress()
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
	elapsed := time.Since(start)

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
	fmt.Printf("replayed to round %d in %v (%.0f rounds/s)\n", latest, elapsed.Round(time.Second), float64(latest.SubSaturate(from))/elapsed.Seconds())
	if copied {
		fmt.Printf("%s now holds rounds %d..%d\n", dstPath, lo, latest)
	} else {
		fmt.Printf("%s already reaches round %d; left as is\n", dstPath, latest)
	}
	return nil
}

// The ledger metrics the progress lines read, besides the exported round and
// transaction counts: time spent reading blocks (ledger/blockqueue.go), in the
// account DB transaction of commits (ledger/acctupdates.go), and writing the time
// travel history in commits (ledger/timetravel.go).
const (
	blockReadMicros    = "ledger_blockq_getblock_micros"
	accountWriteMicros = "ledger_commitround_micros"
	historyWriteMicros = "ledger_timetravel_write_micros"
)

const progressInterval = 10 * time.Second

// reportProgress prints a line every progressInterval until the returned stop is
// called: the replayed round and its date, rounds/s and txns/s over the interval,
// the share of the interval spent reading blocks (on a goroutine reading a few
// blocks ahead, so near 100% means the replay waits for blocks) and in the account
// DB and history writes of commits (which stop the replay), resident memory, and
// the rate since start.
func reportProgress(src *sql.DB, from, tip basics.Round, start time.Time) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		prev, prevAt := ledgerMetrics(), start
		for {
			select {
			case <-done:
				return
			case now := <-ticker.C:
				cur := ledgerMetrics()
				rnd, ok := cur[metrics.LedgerRound.Name]
				if !ok {
					continue // the replay has not started
				}
				prevRnd, ok := prev[metrics.LedgerRound.Name]
				if !ok {
					prevRnd = float64(from)
				}
				secs := now.Sub(prevAt).Seconds()
				share := func(name string) float64 { return (cur[name] - prev[name]) / 1e6 / secs * 100 }
				line := fmt.Sprintf("round %d/%d", int64(rnd), tip)
				var hdr bookkeeping.BlockHeader
				var buf []byte
				if src.QueryRow("SELECT hdrdata FROM blocks WHERE rnd = ?", int64(rnd)).Scan(&buf) == nil && protocol.Decode(buf, &hdr) == nil {
					line += time.Unix(hdr.TimeStamp, 0).UTC().Format(" (2006-01-02)")
				}
				line += fmt.Sprintf(": %.0f rounds/s, %.0f txns/s, block reads %.0f%%, account writes %.0f%%, history writes %.0f%%",
					(rnd-prevRnd)/secs, (cur[metrics.LedgerTransactionsTotal.Name]-prev[metrics.LedgerTransactionsTotal.Name])/secs,
					share(blockReadMicros), share(accountWriteMicros), share(historyWriteMicros))
				if rss, ok := residentBytes(); ok {
					line += fmt.Sprintf(", RSS %.1f GiB", float64(rss)/(1<<30))
				}
				fmt.Printf("%s (overall %.0f rounds/s)\n", line, (rnd-float64(from))/now.Sub(start).Seconds())
				prev, prevAt = cur, now
			}
		}
	}()
	return func() {
		close(done)
		<-finished
	}
}

func ledgerMetrics() map[string]float64 {
	m := map[string]float64{}
	metrics.DefaultRegistry().AddMetrics(m)
	return m
}

// residentBytes is the process's resident set size, on Linux.
func residentBytes() (uint64, bool) {
	statm, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	f := strings.Fields(string(statm))
	if len(f) < 2 {
		return 0, false
	}
	pages, err := strconv.ParseUint(f[1], 10, 64)
	return pages * uint64(os.Getpagesize()), err == nil
}

// accountsRound is the data directory's account DB round before the ledger opens
// it, where the replay starts; 0 without one. Only the reported rates use it.
func accountsRound(path string) basics.Round {
	if _, err := os.Stat(path); err != nil {
		return 0 // db.MakeAccessor would create it
	}
	acc, err := db.MakeAccessor(path, true, false)
	if err != nil {
		return 0
	}
	defer acc.Close()
	var rnd basics.Round
	if acc.Handle.QueryRow("SELECT rnd FROM acctrounds WHERE id = 'acctbase'").Scan(&rnd) != nil {
		return 0
	}
	return rnd
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
