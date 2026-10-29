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

// openStore opens the time travel store of a data directory whose algod is
// stopped, and returns its path and size.
func openStore(dataDir string) (*timetravel.Store, string, int64, error) {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return nil, "", 0, err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", 0, err
	}
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), logging.Base())
	if err != nil {
		return nil, "", 0, err
	}
	path := ledger.TimeTravelPath(ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs})
	if _, err = os.Stat(path); err != nil {
		return nil, "", 0, fmt.Errorf("no time travel store: %w", err)
	}
	size, err := dirSize(path)
	if err != nil {
		return nil, "", 0, err
	}
	s, err := timetravel.Open(path, timetravel.Options{})
	if err != nil {
		return nil, "", 0, fmt.Errorf("%w (is algod running on %s?)", err, dataDir)
	}
	return s, path, size, nil
}

// removeNoops removes the records that change nothing from the time travel
// store of a data directory whose algod is stopped: stores written before
// the ledger skipped them hold one for the totals and the rewards pool in
// nearly every round (timetravel.Store.RemoveNoops). It rebuilds the box
// index in the same pass.
func removeNoops(dataDir string) error {
	s, path, before, err := openStore(dataDir)
	if err != nil {
		return err
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
	if err = s.CompactRecords(context.Background()); err != nil {
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

// buildBoxIndex builds the box index of the time travel store of a data
// directory whose algod is stopped (timetravel.Store.IndexBoxes).
func buildBoxIndex(dataDir string) error {
	s, path, before, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	last, _ := s.Last()
	if from, ok := s.BoxIndexFrom(); ok {
		fmt.Printf("the box index of %s (rounds 0..%d, %.2f GiB) starts at round %d; rebuilding it\n", path, last, gib(uint64(before)), from)
	} else {
		fmt.Printf("rebuilding the box index of %s (rounds 0..%d, %.2f GiB)\n", path, last, gib(uint64(before)))
	}

	start, printed := time.Now(), time.Now()
	indexed, err := s.IndexBoxes(context.Background(), func(scanned, indexed uint64) {
		if time.Since(printed) >= progressInterval {
			printed = time.Now()
			fmt.Printf("scanned %d, indexed %d box versions\n", scanned, indexed)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("indexed %d box versions in %v; compacting\n", indexed, time.Since(start).Round(time.Second))
	start = time.Now()
	if err = s.CompactBoxIndex(context.Background()); err != nil {
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

// buildHoldersIndex builds the asset holders index of the time travel store
// of a data directory whose algod is stopped (timetravel.Store.IndexHolders).
// Its config must set TimeTravelHoldersIndex: algod deletes the index at
// launch otherwise.
func buildHoldersIndex(dataDir string) error {
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if !cfg.TimeTravelHoldersIndex {
		return fmt.Errorf("the config.json of %s does not set TimeTravelHoldersIndex: algod would delete the index at its next launch", dataDir)
	}
	s, path, before, err := openStore(dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	last, _ := s.Last()
	fmt.Printf("building the asset holders index of %s (rounds 0..%d, %.2f GiB; the index is %s)\n", path, last, gib(uint64(before)), s.HoldersIndexState())

	start, printed := time.Now(), time.Now()
	indexed, err := s.IndexHolders(context.Background(), func(scanned, indexed uint64) {
		if time.Since(printed) >= progressInterval {
			printed = time.Now()
			fmt.Printf("scanned %d holding records, indexed %d holdings\n", scanned, indexed)
		}
	})
	if err != nil {
		return err
	}
	fmt.Printf("indexed %d holdings in %v; compacting\n", indexed, time.Since(start).Round(time.Second))
	start = time.Now()
	if err = s.CompactHoldersIndex(context.Background()); err != nil {
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
