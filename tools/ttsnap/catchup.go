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
var catchpointURL = "https://algorand-catchpoints.s3.us-east-2.amazonaws.com/channel/%s/latest.catchpoint"

func cmdCatchup(args []string) error {
	fs := flag.NewFlagSet("catchup", flag.ExitOnError)
	dataDir := fs.String("d", "", "data directory of the running node")
	label := fs.String("label", "", "catchpoint label to catch up to; default: the network's latest from "+fmt.Sprintf(catchpointURL, "<network>"))
	snapshotLabel := fs.Bool("snapshot-label", false, "when the network's latest catchpoint is past the installed history, use the one its producer recorded (the producer's word for the account state)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap catchup -d <data dir> [-label <round>#<hash> | -snapshot-label]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *dataDir == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	return catchup(*dataDir, *label, *snapshotLabel, http.DefaultClient)
}

// catchup checks that the installed history reaches the catchpoint's
// balances round, then asks the node to start the catchup. The node checks
// the history against the catchpoint again before it applies it.
func catchup(dataDir, label string, useSnapshot bool, client *http.Client) error {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	m, ierr := installed(dataDir, genesis)
	if ierr != nil && !os.IsNotExist(ierr) {
		return ierr
	}
	if label == "" {
		if label, err = chooseLabel(ctx, client, genesis, m, useSnapshot); err != nil {
			return withSnapshotLabelHint(err, dataDir)
		}
	}
	if _, _, err = ledgercore.ParseCatchpointLabel(label); err != nil {
		return err
	}
	if m != nil {
		if err = checkLabel(label, m.History.Last); err != nil {
			return fmt.Errorf("%w: fetch a newer history snapshot, or pass an older -label", err)
		}
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

// chooseLabel returns the catchpoint to catch up to: the network's latest,
// which the history of the installed snapshot m must reach. The catchpoint
// the producer recorded in m, which a daily snapshot needs most of the time,
// is used only with useSnapshot: a catchpoint label is what the node's account
// state is checked against, and this one is the producer's word, which no
// attester checks.
func chooseLabel(ctx context.Context, client *http.Client, genesis bookkeeping.Genesis, m *Manifest, useSnapshot bool) (string, error) {
	snapshotLabel := ""
	if m != nil && m.Catchpoint != "" && checkLabel(m.Catchpoint, m.History.Last) == nil {
		snapshotLabel = m.Catchpoint
	}
	network, _, _ := strings.Cut(genesis.ID(), "-")
	u := fmt.Sprintf(catchpointURL, network)
	b, err := getSmall(ctx, client, u, 1024)
	if err != nil {
		return "", fmt.Errorf("looking up the latest catchpoint (or pass -label): %w", err)
	}
	latest := strings.TrimSpace(string(b))
	if m == nil || checkLabel(latest, m.History.Last) == nil {
		fmt.Printf("catchpoint %s: the network's latest, from %s\n", latest, u)
		return latest, nil
	}
	switch {
	case snapshotLabel == "":
		return latest, nil // refused by the caller
	case useSnapshot:
		fmt.Printf("catchpoint %s: the snapshot producer's (the network's latest, %s, is past the history's round %d)\n", snapshotLabel, latest, m.History.Last)
		return snapshotLabel, nil
	}
	return "", fmt.Errorf("%w (catchpoint %s, history to round %d): pass -snapshot-label to use the one the snapshot's producer recorded, %s, or -label with one from a source you trust, or wait for the next snapshot", errLatestPastHistory, latest, m.History.Last, snapshotLabel)
}

// errLatestPastHistory is chooseLabel's refusal of the network's latest
// catchpoint when the snapshot's producer recorded one the history reaches.
var errLatestPastHistory = errors.New("the network's latest catchpoint is past the installed history")

// withSnapshotLabelHint adds to chooseLabel's refusal the command that uses
// the producer's catchpoint, so the choice stays the user's but is one paste
// away.
func withSnapshotLabelHint(err error, dataDir string) error {
	if !errors.Is(err, errLatestPastHistory) {
		return err
	}
	return fmt.Errorf("%w\nto catch up to the producer's catchpoint (this trusts the producer for the account state), run:\n  ttsnap catchup -d %s -snapshot-label", err, shellQuote(dataDir))
}

// shellQuote quotes s for a POSIX shell, unless it needs no quoting.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_-./+:=@%,") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// checkLabel checks that a history whose last round is last reaches the
// balances round of catchpoint label. It takes the lookback of the current
// protocol; the node checks against the catchpoint block's, and more.
func checkLabel(label string, last basics.Round) error {
	rnd, _, err := ledgercore.ParseCatchpointLabel(label)
	if err != nil {
		return err
	}
	if b := rnd.SubSaturate(ledgercore.CatchpointLookback(config.Consensus[protocol.ConsensusCurrentVersion])); last < b {
		return fmt.Errorf("the history ends at round %d, catchpoint %s needs round %d", last, label, b)
	}
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
