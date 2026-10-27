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

// ttdump explores the time travel store of a stopped node: what any object
// was at any round, its history, listings at past rounds, block headers, and
// the store's raw records and statistics. It opens the store read-only, but
// the store is locked while open, so algod must be stopped; goal's --round
// flags and versions commands ask a running node instead.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

const usage = `usage: ttdump (-d DATADIR | -store DIR) [-json] COMMAND [ARGS] [FLAGS]

commands:
  info                         the store: rounds, settings, disk use by keyspace and kind
  stats [-kind K] [-top N]     scan the records: counts and bytes per keyspace and kind
  get OBJECT [-r R]            the object at round R (default: the last round)
  account ADDR [-r R] [-exclude all|created-apps-params|created-assets-params]
                               the account at R, as GET /v2/accounts/{addr}?round=R
  versions OBJECT [-from R] [-to R] [-desc] [-n N] [-full] [-exclude ...]
                               the object's versions, with the changes between them;
                               OBJECT boxes:APP lists the changes of every box of APP
  ls WHAT [-r R] [-ever] [-after OBJECT] [-n N]
                               accounts | assets | apps | boxes APP | kv [PREFIX] |
                               holdings ADDR | locals ADDR | created ADDR
  header R [R2]                the block header of round R, or a table of R..R2
  raw [-prefix P] [-after HEX] [-n N]
                               the raw records in key order; P is an object, a
                               keyspace (a b c H E m) or hex:PREFIX

` + specHelp + `

Output is text, or with -json the values algod's REST API returns (one JSON
document, or one per line for listings and versions).
`

func main() {
	// lock tracking costs a stack trace per lock and buys nothing here
	deadlock.Opts.Disable = true
	logging.Base().SetLevel(logging.Warn)
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "ttdump:", err)
		var ue usageError
		if errors.As(err, &ue) {
			os.Exit(2)
		}
		os.Exit(1)
	}
}

// usageError is a command line that cannot be run.
type usageError struct{ error }

func usagef(format string, args ...any) error { return usageError{fmt.Errorf(format, args...)} }

// env is what a command works with.
type env struct {
	store       *timetravel.Store
	path        string
	first, last basics.Round
	json        bool
	out         *bufio.Writer
	errOut      io.Writer

	reader *ledger.HistoryReader // made on first use: it needs known protocols
	hdrs   map[basics.Round]bookkeeping.BlockHeader
}

var commands = map[string]func(*env, []string) error{
	"info":     cmdInfo,
	"stats":    cmdStats,
	"get":      cmdGet,
	"account":  cmdAccount,
	"versions": cmdVersions,
	"ls":       cmdLs,
	"header":   cmdHeader,
	"raw":      cmdRaw,
}

func run(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("ttdump", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { fmt.Fprint(stderr, usage) }
	dataDir := fs.String("d", "", "algod data directory")
	storeDir := fs.String("store", "", "time travel store directory (<dir>/<genesis id>/ledger.timetravel)")
	asJSON := fs.Bool("json", false, "print JSON")
	if err := fs.Parse(args); err != nil {
		return usageError{err}
	}
	if fs.NArg() == 0 || (*dataDir == "") == (*storeDir == "") {
		fs.Usage()
		return usagef("give a command, and one of -d and -store")
	}
	cmd, ok := commands[fs.Arg(0)]
	if !ok {
		fs.Usage()
		return usagef("unknown command %q", fs.Arg(0))
	}
	path := *storeDir
	if *dataDir != "" {
		var err error
		if path, err = storePath(*dataDir); err != nil {
			return err
		}
	}
	e, err := openEnv(path)
	if err != nil {
		return err
	}
	defer e.store.Close()
	e.json, e.out, e.errOut = *asJSON, bufio.NewWriter(stdout), stderr
	err = cmd(e, fs.Args()[1:])
	if ferr := e.out.Flush(); err == nil {
		err = ferr
	}
	return err
}

// storePath finds the time travel store of a data directory, as the ledger
// does, and loads the network's consensus protocols as algod does.
func storePath(dataDir string) (string, error) {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return "", err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	config.ApplyShorterUpgradeRoundsForDevNetworks(genesis.Network)
	if err = config.LoadConfigurableConsensusProtocols(dataDir); err != nil {
		return "", err
	}
	// not EnsureAndResolveGenesisDirs, which creates directories and moves files
	dir, err := cfg.TimeTravelGenesisDir(dataDir, genesis.ID())
	if err != nil {
		return "", err
	}
	dirs := config.ResolvedGenesisDirs{TimeTravelGenesisDir: dir}
	return ledger.TimeTravelPath(ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs}), nil
}

func openEnv(path string) (*env, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no time travel store: %w", err)
	}
	s, err := timetravel.Open(path, timetravel.Options{ReadOnly: true, CacheSize: 64 << 20})
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w (is algod running on it? ttdump needs it stopped; goal's --round and versions commands ask a running node)", path, err)
	}
	e := &env{store: s, path: path, hdrs: map[basics.Round]bookkeeping.BlockHeader{}}
	last, ok := s.Last()
	first, okFirst, err := s.First()
	if err == nil && (!ok || !okFirst) {
		err = errors.New("the time travel store is empty")
	}
	if err != nil {
		s.Close()
		return nil, err
	}
	e.first, e.last = basics.Round(first), basics.Round(last)
	return e, nil
}

// historyReader returns the store's ledger.HistoryReader.
func (e *env) historyReader() (*ledger.HistoryReader, error) {
	if e.reader == nil {
		r, err := ledger.NewHistoryReader(e.store)
		if err != nil {
			return nil, err
		}
		e.reader = r
	}
	return e.reader, nil
}

// header returns the decoded header of round rnd.
func (e *env) header(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	if h, ok := e.hdrs[rnd]; ok {
		return h, nil
	}
	var hdr bookkeeping.BlockHeader
	b, ok, err := e.store.Header(uint64(rnd))
	if err == nil && !ok {
		err = fmt.Errorf("no header for round %d", rnd)
	}
	if err == nil {
		err = protocol.Decode(b, &hdr)
	}
	if err != nil {
		return hdr, err
	}
	if len(e.hdrs) > 1024 {
		clear(e.hdrs)
	}
	e.hdrs[rnd] = hdr
	return hdr, nil
}

// consensus returns the consensus parameters of round rnd.
func (e *env) consensus(rnd basics.Round) (bookkeeping.BlockHeader, config.ConsensusParams, error) {
	hdr, err := e.header(rnd)
	if err != nil {
		return hdr, config.ConsensusParams{}, err
	}
	proto, ok := config.Consensus[hdr.CurrentProtocol]
	if !ok {
		return hdr, proto, fmt.Errorf("round %d has an unknown consensus protocol %s: load the network's consensus.json (-d does)", rnd, hdr.CurrentProtocol)
	}
	return hdr, proto, nil
}

// when is the time of round rnd's block, or "" if its header is missing.
func (e *env) when(rnd basics.Round) string {
	hdr, err := e.header(rnd)
	if err != nil {
		return ""
	}
	return time.Unix(hdr.TimeStamp, 0).UTC().Format(time.DateTime)
}

// checkRound refuses a round the store does not hold.
func (e *env) checkRound(rnd basics.Round) error {
	if rnd < e.first || rnd > e.last {
		return fmt.Errorf("round %d is outside the history (%d..%d)", rnd, e.first, e.last)
	}
	return nil
}

// roundFlag is a round flag that defaults to the last round.
type roundFlag struct {
	rnd basics.Round
	set bool
}

func (r *roundFlag) String() string { return strconv.FormatUint(uint64(r.rnd), 10) }
func (r *roundFlag) Set(s string) error {
	v, err := strconv.ParseUint(s, 10, 64)
	r.rnd, r.set = basics.Round(v), true
	return err
}

// or returns the round, or def if the flag was not given.
func (r *roundFlag) or(def basics.Round) basics.Round {
	if r.set {
		return r.rnd
	}
	return def
}

// newFlags returns a command's flag set, with -json too.
func (e *env) newFlags(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(e.errOut)
	fs.BoolVar(&e.json, "json", e.json, "print JSON")
	return fs
}

// parse parses a command's arguments, flags and positional arguments in any
// order, and requires between min and max positional ones.
func parse(fs *flag.FlagSet, args []string, min, max int) ([]string, error) {
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, usageError{err}
		}
		args = fs.Args()
		if len(args) == 0 {
			break
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
	if len(pos) < min || len(pos) > max {
		return nil, usagef("%s takes %d to %d arguments, not %d", fs.Name(), min, max, len(pos))
	}
	return pos, nil
}

// more tells, on stderr after the listing, how to continue a truncated one.
func (e *env) more(after string) {
	e.out.Flush()
	fmt.Fprintf(e.errOut, "more: -after %s\n", after)
}

// emit writes v as one JSON line.
func (e *env) emit(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = e.out.Write(b)
	return err
}
