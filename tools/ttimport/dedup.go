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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
)

// removeNoops removes the records that change nothing from the time travel
// store of a data directory whose algod is stopped: stores written before
// the ledger skipped them hold one for the totals and the rewards pool in
// nearly every round (timetravel.Store.RemoveNoops).
func removeNoops(dataDir string) error {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), logging.Base())
	if err != nil {
		return err
	}
	path := ledger.TimeTravelPath(ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs})
	if _, err = os.Stat(path); err != nil {
		return fmt.Errorf("no time travel store: %w", err)
	}
	before, err := dirSize(path)
	if err != nil {
		return err
	}
	s, err := timetravel.Open(path, timetravel.Options{})
	if err != nil {
		return fmt.Errorf("%w (is algod running on %s?)", err, dataDir)
	}
	defer s.Close()
	last, _ := s.Last()
	fmt.Printf("removing no-op records from %s (rounds 0..%d, %.2f GiB)\n", path, last, gib(uint64(before)))

	start, printed := time.Now(), time.Now()
	removed, err := s.RemoveNoops(context.Background(), func(scanned, removed uint64) {
		if time.Since(printed) >= progressInterval {
			printed = time.Now()
			fmt.Printf("scanned %d records, removed %d\n", scanned, removed)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("removed %d records in %v; compacting\n", removed, time.Since(start).Round(time.Second))
	start = time.Now()
	if err = s.CompactCheckpoints(context.Background()); err != nil {
		return err
	}
	if err = s.Close(); err != nil {
		return err
	}
	after, err := dirSize(path)
	if err != nil {
		return err
	}
	fmt.Printf("compacted in %v: %.2f GiB -> %.2f GiB\n", time.Since(start).Round(time.Second), gib(uint64(before)), gib(uint64(after)))
	return nil
}

// dirSize returns the total size of the files in dir.
func dirSize(dir string) (int64, error) {
	var n int64
	err := filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		fi, err := d.Info()
		if err == nil {
			n += fi.Size()
		}
		return err
	})
	return n, err
}
