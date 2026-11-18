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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/util/db"
)

const (
	stateSaveInterval = 30 * time.Second
	progressInterval  = 10 * time.Second
)

func verifyChainCmd(args []string) error {
	fs, dataDir, stateDir := newFlags("verify-chain", `Walks the block DB in round order from genesis, or from where the last walk stopped (recorded in the state
directory), checking that every block and header is canonically encoded, that the header column is the
block's header, that the payset matches the header's commitments, and that each header links to the previous one.
The node may be running: the walk stops at the tip it finds when it starts.

With -full it instead replays the chain from genesis into a scratch ledger under -work, checking every certificate
against the online stake it rebuilds; it needs the certificates, so run it before stripping them.`)
	workers := fs.Int("workers", runtime.NumCPU(), "rows decoded and checked in parallel")
	to := fs.Uint64("to", 0, "stop at this round instead of the tip")
	restart := fs.Bool("restart", false, "ignore where the last walk stopped and walk from genesis")
	full := fs.Bool("full", false, "replay from genesis checking every certificate (days on mainnet)")
	work := fs.String("work", "", "with -full: directory for the scratch ledger (resumes if it holds one)")
	replaceTracker := fs.Bool("replace-tracker", false, "with -full, once the replay reaches the tip: install the rebuilt tracker DB in place of the node's, which is kept as .bak (algod must be stopped)")
	_ = fs.Parse(args)

	n, err := openNode(*dataDir, *stateDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *full {
		if *work == "" {
			return errors.New("-full needs -work")
		}
		return fullReplay(ctx, n, *work, basics.Round(*to), max(*workers, 1), *replaceTracker)
	}
	if *replaceTracker {
		return errors.New("-replace-tracker needs -full")
	}
	return verifyChain(ctx, n, basics.Round(*to), max(*workers, 1), *restart)
}

func verifyChain(ctx context.Context, n *node, to basics.Round, workers int, restart bool) error {
	acc, err := db.MakeAccessor(n.blockDB(), true, false)
	if err != nil {
		return err
	}
	defer acc.Close()
	if err = checkBlocksSchema(acc.Handle); err != nil {
		return err
	}
	earliest, tip, err := blockRange(acc.Handle)
	if err != nil {
		return err
	}
	if earliest != 0 {
		return fmt.Errorf("the block DB starts at round %d, not genesis: it is not an archive", earliest)
	}
	if to == 0 || to > tip {
		to = tip
	}
	genesisHdr, err := n.genesisHdr()
	if err != nil {
		return err
	}

	var prev *bookkeeping.BlockHeader
	from := basics.Round(0)
	if !restart {
		st, err1 := n.readChainState()
		if err1 != nil {
			return err1
		}
		if st != nil {
			if prev, err = resumeHdr(acc.Handle, st, n.chainState()); err != nil {
				return err
			}
			from = st.Round + 1
		}
	}
	if from > to {
		fmt.Printf("rounds 0..%d were already verified (tip hash %v)\n", prev.Round, prev.Hash())
		return nil
	}

	w := &walker{db: acc.Handle, genesisHdr: genesisHdr, workers: workers, chunk: 128}
	fmt.Printf("verifying rounds %d..%d of %s\n", from, to, n.blockDB())
	p := &progress{start: time.Now(), from: from, to: to, interval: progressInterval}
	lastSave := time.Now()
	var saveErr error
	last, err := w.walk(ctx, from, to, prev, func(c checkedBlock) error {
		p.report(c.rnd, false)
		if time.Since(lastSave) > stateSaveInterval {
			lastSave = time.Now()
			saveErr = n.writeChainState(&c.hdr, false)
		}
		return saveErr
	})
	if last != nil && (prev == nil || last.Round > prev.Round) {
		if serr := n.writeChainState(last, false); serr != nil && err == nil {
			err = serr
		}
	}
	if errors.Is(err, context.Canceled) {
		if last != nil {
			fmt.Printf("stopped after round %d; run again to resume\n", last.Round)
		}
		return nil
	}
	if err != nil {
		if last != nil {
			return fmt.Errorf("%w (rounds 0..%d verified, round %d hashes to %v)", err, last.Round, last.Round, last.Hash())
		}
		return err
	}
	p.report(last.Round, true)
	fmt.Printf("verified rounds 0..%d: tip hash %v (in %s)\n", last.Round, last.Hash(), n.chainState())
	return nil
}
