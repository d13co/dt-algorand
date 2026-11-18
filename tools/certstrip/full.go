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
	"io"
	"os"
	"path/filepath"
	"sync"
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

// fullReplay replays the block DB of n from genesis into a scratch ledger under work, checking each row like
// verify-chain and each certificate against the scratch ledger's online stake, as catchup does: every certificate
// is checked by the votes of the stake that was online, which no snapshot holds for old rounds. It resumes from
// the scratch ledger's latest round.
func fullReplay(ctx context.Context, n *node, work string, to basics.Round, workers int, replaceTracker bool) error {
	if replaceTracker {
		if err := n.requireStopped(); err != nil {
			return err
		}
		if err := checkRunAs(n.dataDir, os.Geteuid()); err != nil {
			return err
		}
	}
	acc, err := db.MakeAccessor(n.blockDB(), true, false)
	if err != nil {
		return err
	}
	defer acc.Close()
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

	if err = os.MkdirAll(work, 0o700); err != nil {
		return err
	}
	l, err := openScratchLedger(n, work)
	if err != nil {
		return err
	}
	closed := false
	defer func() {
		if !closed {
			l.Close()
		}
	}()

	var prev *bookkeeping.BlockHeader
	from := basics.Round(0)
	if latest := l.Latest(); latest > 0 {
		hdr, err1 := l.BlockHdr(latest)
		if err1 != nil {
			return err1
		}
		prev, from = &hdr, latest+1
	}
	if from > to {
		fmt.Printf("the scratch ledger in %s is already at round %d\n", work, from-1)
	} else {
		fmt.Printf("replaying rounds %d..%d of %s into %s, checking every certificate\n", from, to, n.blockDB(), work)
		_, err1 := replay(ctx, l, &walker{db: acc.Handle, genesisHdr: genesisHdr, workers: workers, chunk: 32, keep: true}, from, to, prev)
		// the walk runs ahead of the ledger: what counts is what the ledger took
		l.WaitForCommit(l.Latest())
		if errors.Is(err1, context.Canceled) {
			fmt.Printf("stopped after round %d; run again to resume\n", l.Latest())
			return nil
		}
		if err1 != nil {
			return fmt.Errorf("%w (rounds 0..%d verified)", err1, l.Latest())
		}
	}
	hdr, err := l.BlockHdr(l.Latest())
	if err != nil {
		return err
	}
	if err = n.writeChainState(&hdr, true); err != nil {
		return err
	}
	fmt.Printf("verified rounds 0..%d with their certificates: tip hash %v (in %s)\n", hdr.Round, hdr.Hash(), n.chainState())
	if !replaceTracker {
		return nil
	}
	if hdr.Round != tip {
		return fmt.Errorf("-replace-tracker needs the replay to reach the tip %d, it is at %d", tip, hdr.Round)
	}
	l.Close()
	closed = true
	return installTracker(n, work)
}

func openScratchLedger(n *node, work string) (*data.Ledger, error) {
	bal, err := n.genesis.Balances()
	if err != nil {
		return nil, err
	}
	cfg := config.GetDefaultLocal()
	cfg.Archival = false
	cfg.CatchpointTracking = config.CatchpointTrackingModeUntracked // no catchpoint work during the replay
	cfg.CatchpointInterval = 0
	dirs := config.ResolvedGenesisDirs{
		RootGenesisDir: work, HotGenesisDir: work, ColdGenesisDir: work, TrackerGenesisDir: work, BlockGenesisDir: work,
		CatchpointGenesisDir: work, StateproofGenesisDir: work, CrashGenesisDir: work, TimeTravelGenesisDir: work,
	}
	log := logging.Base()
	log.SetLevel(logging.Warn)
	return data.LoadLedger(log, ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs},
		false, n.genesis.Proto, bal, n.genesis.ID(), n.genesis.Hash(), cfg)
}

// replay walks rounds [from, to] with w and adds each to l once its certificate checks out. Certificates are
// checked SeedLookback rounds ahead of the ledger, like catchup's pipeline; each spreads its votes over the vote
// verifier's pool.
func replay(ctx context.Context, l *data.Ledger, w *walker, from, to basics.Round, prev *bookkeeping.BlockHeader) (*bookkeeping.BlockHeader, error) {
	avv := agreement.MakeAsyncVoteVerifier(nil)
	defer avv.Quit()

	type pending struct {
		c    checkedBlock
		done chan error
	}
	params := config.Consensus[prevProto(l, prev)]
	queue := make(chan pending, int(max(params.SeedLookback, 1)))
	var failMu sync.Mutex
	var failure error
	fail := func(err error) {
		failMu.Lock()
		defer failMu.Unlock()
		if failure == nil {
			failure = err
		}
	}
	failed := func() error {
		failMu.Lock()
		defer failMu.Unlock()
		return failure
	}

	p := &progress{start: time.Now(), from: from, to: to, interval: progressInterval}
	var adderDone sync.WaitGroup
	adderDone.Add(1)
	go func() {
		defer adderDone.Done()
		for q := range queue {
			if failed() != nil {
				continue // drain
			}
			if err := <-q.done; err != nil {
				// agreement's errors print whole votes
				msg := err.Error()
				if len(msg) > 600 {
					msg = msg[:600] + "..."
				}
				fail(rowErr(q.c.rnd, "certificate: %s", msg))
				continue
			}
			// like catchup: don't run more than MaxBalLookback rounds ahead of the block DB
			proto := config.Consensus[q.c.hdr.CurrentProtocol]
			<-l.Wait(q.c.rnd.SubSaturate(basics.Round(proto.MaxBalLookback)))
			if err := l.AddBlock(*q.c.blk, *q.c.cert); err != nil {
				fail(rowErr(q.c.rnd, "adding to the scratch ledger: %v", err))
				continue
			}
			p.report(q.c.rnd, false)
		}
	}()

	last, err := w.walk(ctx, from, to, prev, func(c checkedBlock) error {
		if err := failed(); err != nil {
			return err
		}
		if c.rnd == 0 {
			return nil // genesis is in the scratch ledger already
		}
		if c.cert == nil {
			return rowErr(c.rnd, "no certificate: -full needs every certificate; it cannot run once they are stripped")
		}
		proto := config.Consensus[c.hdr.CurrentProtocol]
		q := pending{c: c, done: make(chan error, 1)}
		go func() {
			// the certificate's votes are checked against the seed and stake of rounds SeedLookback back and more
			select {
			case <-l.WaitMem(c.rnd.SubSaturate(basics.Round(proto.SeedLookback))):
			case <-ctx.Done():
				q.done <- ctx.Err()
				return
			}
			q.done <- c.cert.Authenticate(*c.blk, l, avv)
		}()
		select {
		case queue <- q:
		case <-ctx.Done():
			return ctx.Err()
		}
		return nil
	})
	close(queue)
	adderDone.Wait()
	if ferr := failed(); ferr != nil {
		// the adder is behind the walk: its failure is the lower round
		err = ferr
	}
	if err == nil {
		p.report(to, true)
	}
	return last, err
}

func prevProto(l *data.Ledger, prev *bookkeeping.BlockHeader) protocol.ConsensusVersion {
	if prev != nil {
		return prev.CurrentProtocol
	}
	hdr, err := l.BlockHdr(0)
	if err != nil {
		return ""
	}
	return hdr.CurrentProtocol
}

// installTracker puts the scratch ledger's tracker DB in place of the node's, which is kept as .bak.
func installTracker(n *node, work string) error {
	src := filepath.Join(work, trackerDBFile)
	dst := n.trackerDB()
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(src + suffix); err == nil {
			return fmt.Errorf("%s exists: the scratch ledger was not closed cleanly", src+suffix)
		}
	}
	bak := dst + ".bak"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Rename(dst+suffix, bak+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := copyFile(src, dst); err != nil {
		return fmt.Errorf("%w (the node's tracker DB is at %s)", err, bak)
	}
	if err := removeSnapshotFiles(n); err != nil {
		return err
	}
	fmt.Printf("installed the rebuilt tracker DB as %s; the snapshot's is %s. algod rebuilds the balances trie at startup\n", dst, bak)
	return nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, in); err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
