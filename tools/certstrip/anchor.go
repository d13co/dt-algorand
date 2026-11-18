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
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/util/db"
)

type labelsFlag []string

func (l *labelsFlag) String() string     { return strings.Join(*l, ",") }
func (l *labelsFlag) Set(v string) error { *l = append(*l, strings.TrimSpace(v)); return nil }

func anchorCmd(args []string) error {
	fs, dataDir, stateDir := newFlags("anchor", `Checks the node's own catchpoint label for a round against the network's, then checks that the blocks between the
verify-chain walk and that round link up, and records the anchored round in the state directory. A matching label means the
node's header for that round, and its balances, totals and online stake at that round, are the network's; the hash
chain then makes every block up to it the network's.

Give the network's label for one round with -label, once per source, from two independent sources or more: e.g. the
labels Algorand publishes, and last-catchpoint from another node's /v2/status. The node may be running; its own last
catchpoint (it keeps no other label) must be for that round, made after reset-trie.`)
	var labels labelsFlag
	fs.Var(&labels, "label", "the network's catchpoint label for the round and where it came from, as <source>=<round>#<hash> (e.g. algorand-s3=50000000#ABC...); repeat once per source")
	minSources := fs.Int("min-sources", 2, "the fewest distinct -label sources accepted")
	workers := fs.Int("workers", runtime.NumCPU(), "rows decoded and checked in parallel")
	_ = fs.Parse(args)

	n, err := openNode(*dataDir, *stateDir)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return anchor(ctx, n, labels, *minSources, max(*workers, 1))
}

// parseSources splits -label values into their source names and the one label they must all give.
func parseSources(given []string, minSources int) (label string, sources []string, err error) {
	seen := map[string]bool{}
	for _, g := range given {
		src, l, ok := strings.Cut(g, "=")
		src, l = strings.TrimSpace(src), strings.TrimSpace(l)
		if !ok || src == "" || l == "" {
			return "", nil, fmt.Errorf("-label %q: give it as <source>=<round>#<hash>, naming where it came from", g)
		}
		if seen[src] {
			return "", nil, fmt.Errorf("source %q is given twice: one label per source", src)
		}
		seen[src] = true
		if label == "" {
			label = l
		} else if l != label {
			return "", nil, fmt.Errorf("the given labels differ: %s gives %q, %s gives %q", sources[0], label, src, l)
		}
		sources = append(sources, src)
	}
	if len(sources) < max(minSources, 1) {
		return "", nil, fmt.Errorf("give the network's label from %d sources or more (-label), got %d", max(minSources, 1), len(sources))
	}
	return label, sources, nil
}

func anchor(ctx context.Context, n *node, given []string, minSources, workers int) error {
	label, sources, err := parseSources(given, minSources)
	if err != nil {
		return err
	}
	rnd, _, err := ledgercore.ParseCatchpointLabel(label)
	if err != nil {
		return fmt.Errorf("label %q: %w", label, err)
	}

	reset, err := n.readReset()
	if err != nil {
		return err
	}
	if reset == nil {
		return fmt.Errorf("no %s: run reset-trie (with algod stopped) before letting algod make the label", n.resetState())
	}
	if rnd <= reset.Round {
		return fmt.Errorf("round %d is not past the tracker round %d of reset-trie: the node's label for it would come from the snapshot's trie; use a later catchpoint", rnd, reset.Round)
	}
	st, err := n.readChainState()
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("no %s: run verify-chain first", n.chainState())
	}

	local, err := n.localLabel(rnd)
	if err != nil {
		return err
	}
	if local != label {
		return fmt.Errorf("the node's label for round %d is %q, not the network's %q: the snapshot's blocks or tracker state differ from the network's", rnd, local, label)
	}
	fmt.Printf("the node's label for round %d matches the one given by %s\n", rnd, strings.Join(sources, ", "))

	acc, err := db.MakeAccessor(n.blockDB(), true, false)
	if err != nil {
		return err
	}
	defer acc.Close()
	walked, err := resumeHdr(acc.Handle, st, n.chainState())
	if err != nil {
		return err
	}
	genesisHdr, err := n.genesisHdr()
	if err != nil {
		return err
	}
	w := &walker{db: acc.Handle, genesisHdr: genesisHdr, workers: workers, chunk: 128}
	p := &progress{start: time.Now(), interval: progressInterval}
	each := func(c checkedBlock) error { p.report(c.rnd, false); return nil }
	if rnd >= st.Round {
		// the blocks algod added since the walk must link the walk's tip to the labelled round
		p.from, p.to = st.Round+1, rnd
		if _, err = w.walk(ctx, st.Round+1, rnd, walked, each); err != nil {
			return fmt.Errorf("linking the walked rounds 0..%d to round %d: %w", st.Round, rnd, err)
		}
	} else {
		// the walked tip must descend from the labelled round
		from, err1 := readHdr(acc.Handle, rnd)
		if err1 != nil {
			return err1
		}
		p.from, p.to = rnd+1, st.Round
		last, err1 := w.walk(ctx, rnd+1, st.Round, from, each)
		if err1 != nil {
			return fmt.Errorf("linking round %d to the walked tip %d: %w", rnd, st.Round, err1)
		}
		if last.Hash().String() != st.Hash {
			return fmt.Errorf("round %d now hashes to %v, not %s as walked", st.Round, last.Hash(), st.Hash)
		}
		fmt.Printf("warning: rounds %d..%d were walked but are not anchored; anchor a later round to cover them\n", rnd+1, st.Round)
	}

	err = writeJSON(n.anchor(), anchorState{
		Network:     n.genesis.ID(),
		GenesisHash: n.genesis.Hash().String(),
		Round:       rnd,
		Label:       label,
		Sources:     sources,
		ChainRound:  st.Round,
		ChainHash:   st.Hash,
		Updated:     time.Now().UTC(),
		Tool:        toolVersion(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("anchored rounds 0..%d to the network's label %s (in %s)\n", rnd, label, n.anchor())
	return nil
}

// localLabel returns the catchpoint label the node made for rnd, from its tracker DB. The node keeps only its last
// label (what /v2/status shows as last-catchpoint; storedcatchpoints rows have none), so rnd must be its last
// catchpoint.
func (n *node) localLabel(rnd basics.Round) (string, error) {
	acc, err := db.MakeAccessor(n.trackerDB(), true, false)
	if err != nil {
		return "", err
	}
	defer acc.Close()
	var last sql.NullString
	err = acc.Handle.QueryRow("SELECT strval FROM catchpointstate WHERE id=?", string(trackerdb.CatchpointStateLastCatchpoint)).Scan(&last)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if !last.Valid || last.String == "" {
		return "", fmt.Errorf("the node has made no catchpoint label yet: let it run until it makes one (last-catchpoint in /v2/status)")
	}
	r, _, err := ledgercore.ParseCatchpointLabel(last.String)
	if err != nil {
		return "", fmt.Errorf("the node's last catchpoint label %q: %w", last.String, err)
	}
	if r != rnd {
		return "", fmt.Errorf("the node's last catchpoint is for round %d, not %d: it keeps only its last label, so give the network's label for round %d", r, rnd, r)
	}
	return last.String, nil
}
