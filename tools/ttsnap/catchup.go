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
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/nodecontrol"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/tokens"
)

// catchpointURL is where goal node catchup looks up a network's latest
// catchpoint label (cmd/goal/node.go).
const catchpointURL = "https://algorand-catchpoints.s3.us-east-2.amazonaws.com/channel/%s/latest.catchpoint"

func cmdCatchup(args []string) error {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	dataDir := fs.String("d", "", "data directory of the running node")
	label := fs.String("label", "", "catchpoint label to catch up to; default: the network's latest from "+fmt.Sprintf(catchpointURL, "<network>"))
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap catchup -d <data dir> [-label <round>#<hash>]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *dataDir == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	return catchup(*dataDir, *label, http.DefaultClient)
}

// catchup checks that the installed history reaches the catchpoint's
// balances round, then asks the node to start the catchup. The node checks
// the history against the catchpoint again before it applies it.
func catchup(dataDir, label string, client *http.Client) error {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if label == "" {
		network, _, _ := strings.Cut(genesis.ID(), "-")
		b, gerr := getSmall(ctx, client, fmt.Sprintf(catchpointURL, network), 1024)
		if gerr != nil {
			return fmt.Errorf("looking up the latest catchpoint (or pass -label): %w", gerr)
		}
		label = strings.TrimSpace(string(b))
	}
	rnd, _, err := ledgercore.ParseCatchpointLabel(label)
	if err != nil {
		return err
	}
	if m, ierr := installed(dataDir, genesis); ierr == nil {
		// the node checks against the lookback of the protocol it runs, the
		// catchup against the catchpoint block's: this is a first check
		proto := config.Consensus[protocol.ConsensusCurrentVersion]
		lookback := proto.CatchpointLookback
		if lookback == 0 {
			lookback = proto.MaxBalLookback
		}
		b := rnd.SubSaturate(basics.Round(lookback))
		if m.History.Last < b {
			return fmt.Errorf("the installed history ends at round %d, catchpoint %s needs round %d: fetch a newer history snapshot, or pass an older -label", m.History.Last, label, b)
		}
	} else if !os.IsNotExist(ierr) {
		return ierr
	}

	nc := nodecontrol.MakeNodeController("", dataDir)
	u, err := nc.ServerURL()
	if err != nil {
		return fmt.Errorf("finding the node's API (is algod running?): %w", err)
	}
	u.Path = "/v2/catchup/" + url.PathEscape(label)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), nil)
	if err != nil {
		return err
	}
	if token, terr := tokens.GetAndValidateAPIToken(dataDir, tokens.AlgodAdminTokenFilename); terr == nil {
		req.Header.Set("X-Algo-API-Token", token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("POST %s: %s: %s", u.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	fmt.Printf("catching up to %s; follow it with: goal node status -d %s\n", label, dataDir)
	return nil
}

// installed returns the manifest of the snapshot fetch installed.
func installed(dataDir string, genesis bookkeeping.Genesis) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(dataDir, genesis.ID(), installedFile))
	if err != nil {
		return nil, err
	}
	m, err := decodeManifest(b)
	if err != nil {
		return nil, err
	}
	if m.GenesisHash != genesis.Hash().String() {
		return nil, errors.New("the installed snapshot is of another network")
	}
	return m, nil
}
