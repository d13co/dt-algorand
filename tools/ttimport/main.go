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

// ttimport builds a time travel node's ledger from an existing block database
// (an archival node's ledger.block.sqlite) instead of the network. Blocks are
// applied the way catchup applies them, minus the fetching and the
// certificate and signature checks, which the source node already did. It
// resumes where the destination ledger stands; start algod on the data
// directory afterwards to keep syncing from the network.
package main

import (
	"context"
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

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

func main() {
	blocks := flag.String("blocks", "", "source block database: <archival data dir>/<genesis id>/ledger.block.sqlite")
	dataDir := flag.String("d", "", "algod data directory to build: genesis.json, and config.json with EnableTimeTravel")
	to := flag.Uint64("to", 0, "last round to import (default: the source's last round)")
	cpuprofile := flag.String("cpuprofile", "", "write a CPU profile of the run to this file")
	flag.Parse()
	if *blocks == "" || *dataDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	// lock tracking records a stack trace per Lock, ~25% of the import's CPU
	deadlock.Opts.Disable = true
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
	// Ctrl-C stops after the current block and closes the ledger; a second one kills.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	context.AfterFunc(ctx, stop)
	err := run(ctx, *blocks, *dataDir, basics.Round(*to))
	pprof.StopCPUProfile()
	if err != nil {
		fmt.Fprintln(os.Stderr, "ttimport:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, blocksPath, dataDir string, to basics.Round) error {
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
	log := logging.Base()
	log.SetLevel(logging.Warn)
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), log)
	if err != nil {
		return err
	}
	bal, err := genesis.Balances()
	if err != nil {
		return err
	}
	l, err := data.LoadLedger(log, ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs},
		false, genesis.Proto, bal, genesis.ID(), genesis.Hash(), cfg)
	if err != nil {
		return err
	}
	defer l.Close()

	src, err := db.MakeAccessor(blocksPath, true, false)
	if err != nil {
		return err
	}
	defer src.Close()
	from := l.Latest()
	rows, err := src.Handle.Query("SELECT rnd, blkdata, certdata FROM blocks WHERE rnd > ? ORDER BY rnd", from)
	if err != nil {
		return err
	}
	defer rows.Close()

	hdr, err := l.BlockHdr(from)
	if err != nil {
		return err
	}
	start := time.Now()
	last, lastRnd, lastTxns := start, from, hdr.TxnCounter
	var blockWait, commitWait time.Duration // since the last report
	for ctx.Err() == nil && rows.Next() {
		var rnd basics.Round
		var blkdata, certdata []byte
		if err := rows.Scan(&rnd, &blkdata, &certdata); err != nil {
			return err
		}
		if to != 0 && rnd > to {
			break
		}
		if rnd != l.Latest()+1 {
			return fmt.Errorf("the source has no round %d (next is %d): it must be an archival block database", l.Latest()+1, rnd)
		}
		var blk bookkeeping.Block
		var cert agreement.Certificate
		if err := protocol.Decode(blkdata, &blk); err != nil {
			return fmt.Errorf("round %d: %w", rnd, err)
		}
		if err := protocol.Decode(certdata, &cert); err != nil {
			return fmt.Errorf("round %d certificate: %w", rnd, err)
		}
		if blk.GenesisHash() != genesis.Hash() {
			return fmt.Errorf("round %d belongs to another network (genesis hash %v)", rnd, blk.GenesisHash())
		}
		// Keep the in-memory backlog bounded, as catchup does.
		t0 := time.Now()
		<-l.Wait(rnd.SubSaturate(basics.Round(config.Consensus[blk.CurrentProtocol].MaxBalLookback)))
		t1 := time.Now()
		for l.IsBehindCommittingDeltas() {
			time.Sleep(10 * time.Millisecond)
		}
		blockWait += t1.Sub(t0)
		commitWait += time.Since(t1)
		if err := l.AddBlock(blk, cert); err != nil {
			return fmt.Errorf("round %d: %w", rnd, err)
		}
		if now := time.Now(); now.Sub(last) >= 10*time.Second {
			secs := now.Sub(last).Seconds()
			fmt.Printf("round %d: %.0f rounds/s, %.0f txns/s, waited %.0f%% on block DB, %.0f%% on commits (overall %.0f rounds/s)\n", rnd,
				float64(rnd-lastRnd)/secs, float64(blk.TxnCounter-lastTxns)/secs,
				100*blockWait.Seconds()/secs, 100*commitWait.Seconds()/secs, float64(rnd-from)/now.Sub(start).Seconds())
			last, lastRnd, lastTxns = now, rnd, blk.TxnCounter
			blockWait, commitWait = 0, 0
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	l.WaitForCommit(l.Latest())
	fmt.Printf("imported rounds %d..%d in %v (%.0f rounds/s)\n", from+1, l.Latest(), time.Since(start).Round(time.Second),
		float64(l.Latest()-from)/time.Since(start).Seconds())
	return nil
}
