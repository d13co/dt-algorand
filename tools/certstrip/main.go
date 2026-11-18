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

// certstrip verifies an archival node's block history without its agreement certificates, so that the
// certificates can be discarded (config.Local.CertificateRetentionRounds) from an archive that came from an
// untrusted snapshot. See README.md.
//
//	certstrip verify-chain -d DATA    walk the block DB: encodings, payset commitments, the hash chain to genesis
//	certstrip reset-trie -d DATA      make algod rebuild the snapshot's balances trie and derived tables
//	certstrip anchor -d DATA -label L -label L
//	                                  match the node's catchpoint label to the network's and link it to the walk
//	certstrip compact -d DATA         copy the block DB without old certificates, to reclaim the space
//	certstrip verify-chain -full -d DATA -work DIR
//	                                  replay from genesis, checking every certificate (days on mainnet)
package main

import (
	"crypto/sha256"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/logging"
)

const (
	blockDBFile   = config.LedgerFilenamePrefix + ".block.sqlite"
	trackerDBFile = config.LedgerFilenamePrefix + ".tracker.sqlite"

	// The state files live in the state directory (-state-dir), never in the data directory: that comes from the
	// snapshot, which could ship state files claiming checks that never ran.
	//
	// chainStateFile records how far verify-chain got.
	chainStateFile = "chain.json"
	// anchorFile records the round anchor matched to the network's catchpoint label.
	anchorFile = "anchor.json"
	// resetFile records the tracker round reset-trie ran at.
	resetFile = "reset.json"
)

func usage() {
	fmt.Fprintf(os.Stderr, `usage: certstrip <command> -d <algod data dir> [flags]

commands:
  verify-chain  walk the block DB from genesis (or where the last walk stopped), checking every block's encoding,
                payset commitments and hash chain; with -full, replay from genesis checking every certificate
  reset-trie    (algod stopped) make algod rebuild the balances trie, and rebuild the creatables index and txtail
                from verified data; remove the snapshot's catchpoints and agreement/state proof databases
  anchor        check the node's catchpoint label against the network's (-label, from two sources or more) and
                link the blocks above the walk to it
  compact       (algod stopped) copy the block DB without the certificates of old rounds, then optionally swap it in

run "certstrip <command> -h" for a command's flags.
`)
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	var err error
	args := os.Args[2:]
	switch os.Args[1] {
	case "verify-chain":
		err = verifyChainCmd(args)
	case "reset-trie":
		err = resetTrieCmd(args)
	case "anchor":
		err = anchorCmd(args)
	case "compact":
		err = compactCmd(args)
	case "-h", "-help", "--help", "help":
		usage()
		return
	default:
		usage()
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "certstrip:", err)
		os.Exit(1)
	}
}

// node is an algod data directory and the files of its genesis.
type node struct {
	dataDir string
	genesis bookkeeping.Genesis
	cfg     config.Local
	dirs    config.ResolvedGenesisDirs
	// stateDir holds certstrip's state files for this node.
	stateDir string
}

func (n *node) blockDB() string    { return filepath.Join(n.dirs.BlockGenesisDir, blockDBFile) }
func (n *node) trackerDB() string  { return filepath.Join(n.dirs.TrackerGenesisDir, trackerDBFile) }
func (n *node) chainState() string { return filepath.Join(n.stateDir, chainStateFile) }
func (n *node) anchor() string     { return filepath.Join(n.stateDir, anchorFile) }
func (n *node) resetState() string { return filepath.Join(n.stateDir, resetFile) }

// defaultStateDir is $XDG_STATE_HOME/certstrip, or ~/.local/state/certstrip.
func defaultStateDir() (string, error) {
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "certstrip"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("no home directory for the state files: set -state-dir: %w", err)
	}
	return filepath.Join(home, ".local", "state", "certstrip"), nil
}

// openNode opens the data directory. The node's state files go in a directory of stateRoot (default
// defaultStateDir) named by the network and the block DB's absolute path, so that two nodes on one host don't share
// them.
func openNode(dataDir, stateRoot string) (*node, error) {
	dataDir = config.ResolveDataDir(dataDir)
	if dataDir == "" {
		return nil, errors.New("-d is required." + config.DataDirEnvNote())
	}
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return nil, err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), logging.Base())
	if err != nil {
		return nil, err
	}
	n := &node{dataDir: dataDir, genesis: genesis, cfg: cfg, dirs: dirs}
	if _, err = os.Stat(n.blockDB()); err != nil {
		return nil, err
	}
	if stateRoot == "" {
		if stateRoot, err = defaultStateDir(); err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(n.blockDB())
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(abs))
	n.stateDir = filepath.Join(stateRoot, fmt.Sprintf("%s-%x", genesis.ID(), sum[:8]))
	return n, nil
}

// requireStopped fails if the algod.pid file of the data directory names a running process.
func (n *node) requireStopped() error {
	buf, err := os.ReadFile(filepath.Join(n.dataDir, "algod.pid"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	// a pid file algod didn't write, or naming no process, means algod isn't running
	if pid, convErr := strconv.Atoi(strings.TrimSpace(string(buf))); convErr == nil && pid > 0 {
		if proc, findErr := os.FindProcess(pid); findErr == nil {
			if err = proc.Signal(syscall.Signal(0)); err == nil || errors.Is(err, syscall.EPERM) {
				return fmt.Errorf("algod (pid %d, from %s) is running: stop it first", pid, filepath.Join(n.dataDir, "algod.pid"))
			}
		}
	}
	return nil
}

// newFlags returns a flag set for a command, with -d and -state-dir.
func newFlags(name, desc string) (*flag.FlagSet, *string, *string) {
	fs := flag.NewFlagSet(name, flag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: certstrip %s -d <algod data dir> [flags]\n\n%s\n\n", name, desc)
		fs.PrintDefaults()
	}
	dataDir := fs.String("d", "", "algod data directory (default $"+config.DataDirEnv+")")
	stateDir := fs.String("state-dir", "", "where certstrip keeps what it verified, outside the data directory (default $XDG_STATE_HOME/certstrip or ~/.local/state/certstrip)")
	return fs, dataDir, stateDir
}
