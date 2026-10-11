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
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

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
	flag.Parse()
	if *blocks == "" || *dataDir == "" {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*blocks, *dataDir, basics.Round(*to)); err != nil {
		fmt.Fprintln(os.Stderr, "ttimport:", err)
		os.Exit(1)
	}
}

func run(blocksPath, dataDir string, to basics.Round) error {
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

	start, report := time.Now(), time.Now()
	for rows.Next() {
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
		<-l.Wait(rnd.SubSaturate(basics.Round(config.Consensus[blk.CurrentProtocol].MaxBalLookback)))
		for l.IsBehindCommittingDeltas() {
			time.Sleep(10 * time.Millisecond)
		}
		if err := l.AddBlock(blk, cert); err != nil {
			return fmt.Errorf("round %d: %w", rnd, err)
		}
		if time.Since(report) >= 10*time.Second {
			report = time.Now()
			fmt.Printf("round %d, %.0f rounds/s\n", rnd, float64(rnd-from)/time.Since(start).Seconds())
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
