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
// resuming from where the data directory's trackers stand. No block of the
// archive is changed, but SQLite opens it for writing (its -wal and -shm
// files go next to it), so the archive's directory must be writable.
// Afterwards the data directory's own block database gets the last blocks
// algod needs to start; start algod on the directory to keep syncing from
// the network.
package main

import (
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	rtmetrics "runtime/metrics"
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
	memprofile := flag.String("memprofile", "", "write a heap profile to this file on Ctrl-C and, during an import on Linux, whenever resident memory reaches a new high (from 1 GiB, in 256 MiB steps)")
	verbose := flag.Bool("v", false, "also log the ledger's info messages")
	dedup := flag.Bool("dedup", false, "instead of importing, remove the records that change nothing from the time travel store of -d, whose algod must be stopped, and rebuild its box index")
	indexBoxes := flag.Bool("index-boxes", false, "instead of importing, build the box index of the time travel store of -d, whose algod must be stopped: stores written by builds without it need it for /v2/applications/{id}/boxes/versions")
	fillAPI := flag.String("fill", "", "instead of importing, add to the time travel store of -d, whose algod must be stopped, the rounds its account DB is ahead by, with their deltas from this algod API URL and their headers from -blocks")
	holdersIndex := flag.Bool("holders-index", false, "instead of importing, build the asset holders index of the time travel store of -d, whose algod must be stopped and whose config.json sets TimeTravelHoldersIndex, for /v2/assets/{id}/holders")
	flag.Parse()
	modes := 0
	for _, on := range []bool{*dedup, *indexBoxes, *holdersIndex, *fillAPI != ""} {
		if on {
			modes++
		}
	}
	// -fill and the import read -blocks; the other modes don't
	if *dataDir == "" || (*blocks == "") != (*dedup || *indexBoxes || *holdersIndex) || modes > 1 {
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
	// resumes from the trackers' last commit (a -dedup, -index-boxes or -holders-index rerun starts
	// over, a -dedup one skipping what was removed). A broken output pipe does too: piped to
	// grep, Ctrl-C ends grep first, and the SIGPIPE of the next write to stdout or
	// stderr would otherwise kill the process before the profile is written.
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM, syscall.SIGPIPE)
	go func() {
		<-sig
		pprof.StopCPUProfile()
		if *memprofile != "" {
			if err := writeHeapProfile(*memprofile); err != nil {
				fmt.Fprintln(os.Stderr, "ttimport: heap profile:", err)
			}
		}
		os.Exit(1)
	}()
	var err error
	switch {
	case *dedup:
		err = removeNoops(*dataDir)
	case *indexBoxes:
		err = buildBoxIndex(*dataDir)
	case *fillAPI != "":
		err = fill(*blocks, *dataDir, *fillAPI)
	case *holdersIndex:
		err = buildHoldersIndex(*dataDir)
	default:
		err = run(*blocks, *dataDir, *memprofile)
	}
	pprof.StopCPUProfile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ttimport:", err)
		os.Exit(1)
	}
}

func run(blocksPath, dataDir, memprofile string) error {
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
	stopProgress := reportProgress(src.Handle, from, tip, start, memprofile)
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
// DB transaction and history writes of commits (which run alongside the replay and
// each other, so near 100% means the replay waits for commits), resident memory, and
// the rate since start. Resident memory is split into the Go runtime's (with the
// heap's live bytes) and the rest, mostly C allocations: the time travel store's
// block cache and memtables, and SQLite's page caches. With memprofile set, it
// writes a heap profile there whenever resident memory reaches a new high, so
// that one is left after an OOM kill.
func reportProgress(src *sql.DB, from, tip basics.Round, start time.Time, memprofile string) (stop func()) {
	done, finished := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(finished)
		ticker := time.NewTicker(progressInterval)
		defer ticker.Stop()
		prev, prevAt := ledgerMetrics(), start
		nextProfile := uint64(heapProfileFrom)
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
					held, live := goMemory()
					line += fmt.Sprintf(", RSS %.1f GiB (Go %.1f GiB with %.1f GiB live, other %.1f GiB)",
						gib(rss), gib(held), gib(live), gib(rss-min(rss, held)))
					if memprofile != "" && rss >= nextProfile {
						if err := writeHeapProfile(memprofile); err != nil {
							fmt.Fprintln(os.Stderr, "ttimport: heap profile:", err)
						}
						nextProfile = rss + heapProfileStep
					}
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

// -memprofile writes a heap profile when resident memory first reaches
// heapProfileFrom, then each time it grows by heapProfileStep past the last one.
const (
	heapProfileFrom = 1 << 30
	heapProfileStep = 256 << 20
)

func gib(b uint64) float64 { return float64(b) / (1 << 30) }

// goMemory is what the Go runtime holds (heap, stacks and its own metadata, less
// what it returned to the OS), and the heap's live bytes as of the last GC.
func goMemory() (held, live uint64) {
	s := []rtmetrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}, {Name: "/gc/heap/live:bytes"}}
	rtmetrics.Read(s)
	return s[0].Value.Uint64() - s[1].Value.Uint64(), s[2].Value.Uint64()
}

// writeHeapProfile replaces path with a heap profile. It writes a temporary file
// first, so that an OOM kill while writing leaves the previous profile.
func writeHeapProfile(path string) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	err = pprof.Lookup("heap").WriteTo(f, 0)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
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
	defer func() { _ = tx.Rollback() }() // a no-op once committed
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
