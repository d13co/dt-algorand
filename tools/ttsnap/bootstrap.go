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
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/nodecontrol"
	"github.com/algorand/go-algorand/util/tokens"
)

// startNone is the -start value that leaves starting the node to the user.
const startNone = "none"

// The dtalgorand packages' services, which -start defaults to.
const (
	debDataDir   = "/var/lib/dtalgorand"
	debUnit      = "dtalgorand.service"
	debStart     = "systemctl enable --now dtalgorand"
	brewService  = "dtalgorand"
	brewStart    = "brew services start " + brewService
	brewDataPath = "var/dtalgorand" // under the Homebrew prefix
)

// debUnitDirs are where the .deb's unit may be.
var debUnitDirs = []string{"/lib/systemd/system", "/usr/lib/systemd/system", "/etc/systemd/system"}

// bootstrapPhaseEnv names the phase a child ttsnap runs as the data
// directory's owner, when bootstrap runs as root (see runPhase).
const bootstrapPhaseEnv = "TTSNAP_BOOTSTRAP_PHASE"

// exitNodeRunning is the install phase's exit status when algod already
// runs on the data directory, so there is nothing to install or start.
const exitNodeRunning = 3

type bootstrapOpts struct {
	fetch         fetchOpts
	holders       bool          // build the asset holders index
	start         string        // shell command that starts the node; startNone: the user does
	startAsRoot   bool          // start needs root (systemctl)
	wait          time.Duration // how long to wait for algod to answer once started
	pollEvery     time.Duration
	label         string // catchpoint; empty: the network's latest
	snapshotLabel bool
	log           io.Writer // what bootstrap reports; fetch's progress goes to fetch.progressLog
}

func cmdBootstrap(args []string) error {
	fs := flag.NewFlagSet("bootstrap", flag.ExitOnError)
	o := bootstrapOpts{fetch: fetchOpts{progressLog: os.Stderr}, pollEvery: 2 * time.Second, log: os.Stdout}
	fs.StringVar(&o.fetch.base, "url", "", "base URL of the snapshot tree (default: mainnet's or testnet's, by the data directory's genesis.json)")
	fs.StringVar(&o.fetch.dataDir, "d", "", "data directory of the new node: genesis.json, and config.json with EnableTimeTravel")
	fs.StringVar(&o.fetch.keys, "keys", "", "keys file with the producers' and attesters' public keys (default: the package's, as for fetch)")
	fs.IntVar(&o.fetch.minAttest, "min-attest", 0, "attesters of the keys file that must have signed the snapshot's statement")
	fs.IntVar(&o.fetch.workers, "workers", 0, "goroutines that compute the downloaded history's commitment; 0: one per CPU")
	fs.IntVar(&o.fetch.parallel, "parallel", 4, "files downloaded at once")
	fs.DurationVar(&o.fetch.maxAge, "max-age", 7*24*time.Hour, "refuse a latest snapshot older than this; 0: any age")
	fs.BoolVar(&o.holders, "holders-index", true, "build the asset holders index after the download (config.json must set TimeTravelHoldersIndex); -holders-index=false skips it")
	fs.StringVar(&o.start, "start", "", "shell command that starts the node once the history is installed, run from / (as the data directory's owner when bootstrap runs as root), or \""+startNone+"\" to start it yourself while bootstrap waits (default: \""+debStart+"\" for the .deb's "+debDataDir+", \""+brewStart+"\" for Homebrew's)")
	fs.DurationVar(&o.wait, "wait", 30*time.Minute, "how long to wait for the started node's API, which comes up once algod has opened its ledger")
	fs.StringVar(&o.label, "label", "", "catchpoint label to catch up to; default: the network's latest")
	fs.BoolVar(&o.snapshotLabel, "snapshot-label", false, "when the network's latest catchpoint is past the installed history, use the one its producer recorded (the producer's word for the account state)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap bootstrap [-url <base URL>] -d <data dir> [-holders-index=false] [-start <command> | -start none] [fetch and catchup flags]")
		fmt.Fprintln(fs.Output(), "Installs the latest snapshot (fetch), builds the asset holders index, starts the node, waits for its API and starts the catchpoint catchup (catchup). A rerun skips the steps done.")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if o.fetch.dataDir == "" || fs.NArg() != 0 || o.fetch.parallel < 1 || o.fetch.minAttest < 0 || o.wait <= 0 {
		fs.Usage()
		os.Exit(2)
	}
	var err error
	if o.fetch.dataDir, err = filepath.Abs(o.fetch.dataDir); err != nil {
		return err
	}
	if o.fetch.keys != "" {
		if o.fetch.keys, err = filepath.Abs(o.fetch.keys); err != nil {
			return err
		}
	}

	phase := os.Getenv(bootstrapPhaseEnv)
	if phase != "" {
		// a phase runs as the data directory's owner (runPhase), never as root
		// on another user's directory, whoever set the variable
		if owner, perr := phaseOwner(o.fetch.dataDir); perr != nil || owner != nil {
			if perr == nil {
				perr = fmt.Errorf("%s is set: a bootstrap phase runs as the data directory's owner, %s, not as root", bootstrapPhaseEnv, owner.name)
			}
			return perr
		}
	}
	switch phase {
	case "install":
		if err = o.loadKeys(); err != nil {
			return err
		}
		running, ierr := o.install()
		if ierr == nil && running {
			os.Exit(exitNodeRunning)
		}
		return ierr
	case "catchup":
		return o.catchup()
	case "":
	default:
		return fmt.Errorf("%s=%s: not a bootstrap phase", bootstrapPhaseEnv, phase)
	}

	return o.bootstrap(args)
}

// bootstrap runs the phases, as the data directory's owner when ttsnap runs
// as root on another user's data directory, with args as the child's flags.
func (o *bootstrapOpts) bootstrap(args []string) error {
	var err error
	if o.start == "" {
		if o.start, o.startAsRoot, err = defaultStart(o.fetch.dataDir, runtime.GOOS, debUnitDirs, brewPrefixes); err != nil {
			return err
		}
	}
	owner, err := phaseOwner(o.fetch.dataDir)
	if err != nil {
		return err
	}
	if err = checkStartUser(o.start, o.startAsRoot, os.Geteuid()); err != nil {
		return err
	}
	if owner == nil {
		if err = o.loadKeys(); err != nil {
			return err
		}
		return o.run()
	}
	// as root on a data directory another user owns: everything runs as
	// that user but the package's service start (systemctl), which needs root
	child := append(append([]string{}, args...), "-d", o.fetch.dataDir)
	if o.fetch.keys != "" {
		child = append(child, "-keys", o.fetch.keys)
	}
	running, err := runPhase(owner, "install", child)
	if err != nil {
		return err
	}
	if !running {
		if err = o.startNode(owner); err != nil {
			return err
		}
	}
	_, err = runPhase(owner, "catchup", child)
	return err
}

// loadKeys finds the installed keys when -keys is not given, as fetch does.
func (o *bootstrapOpts) loadKeys() error {
	if o.fetch.keys != "" || o.fetch.trusted != nil {
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("find the installed keys: %w: pass -keys <keys file>", err)
	}
	keys, path, err := installedKeysFor(exe)
	if err != nil {
		return err
	}
	o.fetch.keys, o.fetch.trusted = path, keys
	fmt.Fprintf(o.fetch.progressLog, "keys: %s\n", path)
	return nil
}

// run is bootstrap as one user: install, start, catch up.
func (o *bootstrapOpts) run() error {
	running, err := o.install()
	if err != nil {
		return err
	}
	if !running {
		if err = o.startNode(nil); err != nil {
			return err
		}
	}
	return o.catchup()
}

// install downloads and installs the latest snapshot unless one is
// installed, then builds the asset holders index unless it is complete or
// not wanted. It does nothing, and reports running, when algod already
// answers on the data directory: a node that started has a history. An algod
// still opening its ledger (algod.net comes after) holds the store's lock,
// so the index step fails.
func (o *bootstrapOpts) install() (running bool, err error) {
	dataDir := o.fetch.dataDir
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return false, err
	}
	if nodeAnswers(dataDir, genesis) {
		if _, ierr := installed(dataDir, genesis); ierr != nil {
			return false, fmt.Errorf("algod is running on %s, on a history that no snapshot installed (%v): bootstrap is for a new data directory", dataDir, ierr)
		}
		fmt.Fprintf(o.log, "algod is running on %s: nothing to install\n", dataDir)
		return true, nil
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return false, err
	}
	if o.holders && !cfg.TimeTravelHoldersIndex {
		// before the download: algod deletes an index its config doesn't ask for
		return false, fmt.Errorf("the config.json of %s does not set TimeTravelHoldersIndex: set it to build the asset holders index (dtalgocfg set -d %s -p TimeTravelHoldersIndex -v true), or pass -holders-index=false", dataDir, shellQuote(dataDir))
	}
	ttDir, err := cfg.TimeTravelGenesisDir(dataDir, genesis.ID())
	if err != nil {
		return false, err
	}
	hist := filepath.Join(ttDir, historyDir)
	m, ierr := installed(dataDir, genesis)
	if ierr != nil && !os.IsNotExist(ierr) {
		return false, fmt.Errorf("the installed snapshot's record: %w", ierr)
	}
	if ierr == nil && exists(hist) {
		if m.Kind != kindHistory {
			return false, fmt.Errorf("a %s snapshot is installed in %s (ttsnap fetch -full): bootstrap installs a history and catches up from a catchpoint", m.Kind, dataDir)
		}
		fmt.Fprintf(o.log, "the %s snapshot up to round %d is installed: skipping the download\n", m.Kind, m.History.Last)
	} else {
		var gdir string
		m, gdir, err = fetch(o.fetch)
		if err != nil {
			return false, err
		}
		fmt.Fprintf(o.log, "installed the %s snapshot of %s up to round %d into %s\n", m.Kind, m.GenesisID, m.History.Last, gdir)
	}
	switch {
	case o.holders:
		if err = buildHoldersIndex(hist, o.log); err != nil {
			err = fmt.Errorf("the asset holders index: %w (is algod running on %s? It holds the history while it opens its ledger: stop it, then rerun bootstrap)", err, dataDir)
		}
	case cfg.TimeTravelHoldersIndex:
		fmt.Fprintln(o.log, "not building the asset holders index (-holders-index=false): /v2/assets/{id}/holders answers 503 until one is built (ttimport -holders-index, node stopped), and algod warns at start; set TimeTravelHoldersIndex to false to do without it")
	}
	return false, err
}

// buildHoldersIndex builds the asset holders index of the history at dir,
// which fetch does not install, unless it is complete.
func buildHoldersIndex(dir string, log io.Writer) error {
	st, err := timetravel.Open(dir, timetravel.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	if st.HoldersIndexState() == timetravel.HoldersComplete {
		fmt.Fprintln(log, "the asset holders index is complete")
		return nil
	}
	fmt.Fprintln(log, "building the asset holders index (about 5 minutes on mainnet)")
	start, printed := time.Now(), time.Now()
	indexed, err := st.IndexHolders(context.Background(), func(scanned, indexed uint64) {
		if time.Since(printed) >= progressEvery {
			printed = time.Now()
			fmt.Fprintf(log, "scanned %d holding records, indexed %d holdings\n", scanned, indexed)
		}
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(log, "indexed %d holdings in %v; compacting\n", indexed, time.Since(start).Round(time.Second))
	if err = st.CompactHoldersIndex(context.Background()); err != nil {
		return err
	}
	return st.Close()
}

// startNode runs the start command, if any: as owner unless it is nil or
// the command needs root (the package's systemctl, whose unit runs algod as
// the owner).
func (o *bootstrapOpts) startNode(owner *phaseUser) error {
	if o.start == startNone {
		fmt.Fprintf(o.log, "start the node on %s now: waiting up to %v for its API\n", o.fetch.dataDir, o.wait)
		return nil
	}
	fmt.Fprintf(o.log, "starting the node: %s\n", o.start)
	cmd := shellCommand(o.start)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	var err error
	if owner != nil && !o.startAsRoot {
		err = runAs(cmd, owner)
	} else {
		cmd.Stdin = os.Stdin
		err = cmd.Run()
	}
	if err != nil {
		return fmt.Errorf("%s: %w", o.start, err)
	}
	return nil
}

// catchup waits for algod to answer, then starts the catchpoint catchup,
// unless one runs or the node caught up already.
func (o *bootstrapOpts) catchup() error {
	dataDir := o.fetch.dataDir
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	m, err := installed(dataDir, genesis)
	if err != nil {
		return fmt.Errorf("no installed snapshot: %w", err)
	}
	if !nodeAnswers(dataDir, genesis) {
		fmt.Fprintln(o.log, "waiting for algod to open its ledger (a minute or more on mainnet)")
		deadline := time.Now().Add(o.wait)
		for !nodeAnswers(dataDir, genesis) {
			if time.Now().After(deadline) {
				return fmt.Errorf("algod did not answer on %s within %v: check its node.log, then rerun bootstrap", dataDir, o.wait)
			}
			time.Sleep(o.pollEvery)
		}
	}
	st, err := nodeStatus(dataDir)
	if err != nil {
		return err
	}
	switch {
	case st.Catchpoint != "":
		fmt.Fprintf(o.log, "algod is catching up to %s already; follow it with: %s node status -d %s\n", st.Catchpoint, goalCommand(), shellQuote(dataDir))
		return nil
	case st.LastRound > m.History.Last/2:
		// a catchup left it at the catchpoint's round, up to a catchpoint
		// interval before the snapshot's, and only a catchup gets a node this
		// far: a sync from genesis takes weeks. Another would start over.
		fmt.Fprintf(o.log, "algod is at round %d (the snapshot's is %d): it caught up already\n", st.LastRound, m.History.Last)
		return nil
	}
	return catchup(dataDir, o.label, o.snapshotLabel, http.DefaultClient)
}

// defaultStart returns the command that starts the node of the dtalgorand
// package whose data directory is dataDir, and whether it needs root.
func defaultStart(dataDir, goos string, unitDirs, brewPrefixes []string) (string, bool, error) {
	switch goos {
	case "linux":
		if sameDir(dataDir, debDataDir) {
			for _, d := range unitDirs {
				if exists(filepath.Join(d, debUnit)) {
					return debStart, true, nil
				}
			}
		}
	case "darwin":
		for _, p := range brewPrefixes {
			if sameDir(dataDir, filepath.Join(p, filepath.FromSlash(brewDataPath))) {
				return brewStart, false, nil
			}
		}
	}
	return "", false, fmt.Errorf("%s is not a dtalgorand package's data directory: pass -start with the command that starts its algod (e.g. -start 'goal node start -d %s'), or -start %s to start it yourself while bootstrap waits", dataDir, dataDir, startNone)
}

// checkStartUser refuses a start that cannot work as the user euid.
func checkStartUser(start string, asRoot bool, euid int) error {
	switch {
	case asRoot && euid != 0:
		return fmt.Errorf("starting the node (%s) needs root: run sudo ttsnap bootstrap ..., which installs as the data directory's owner and starts the service as root, or pass -start %s and start it yourself while bootstrap waits", start, startNone)
	case start == brewStart && euid == 0:
		return errors.New("brew services runs as you, not root: run ttsnap bootstrap without sudo")
	}
	return nil
}

// brewPrefixes are Homebrew's default prefixes, on Apple silicon and on
// Intel. They are looked at rather than asking brew, which bootstrap would
// run as root under sudo.
var brewPrefixes = []string{"/opt/homebrew", "/usr/local"}

// sameDir tells whether a and b name the same directory, links resolved.
func sameDir(a, b string) bool {
	if ra, err := filepath.EvalSymlinks(a); err == nil {
		a = ra
	}
	if rb, err := filepath.EvalSymlinks(b); err == nil {
		b = rb
	}
	return filepath.Clean(a) == filepath.Clean(b)
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// nodeAnswers tells whether algod runs on dataDir: its algod.net names an
// address where a node of genesis answers. algod.net outlives a crash, and
// another node may have that address now.
func nodeAnswers(dataDir string, genesis bookkeeping.Genesis) bool {
	u, err := nodecontrol.MakeNodeController("", dataDir).ServerURL()
	if err != nil {
		return false
	}
	u.Path = "/versions"
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return false
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	var v struct {
		GenesisHash []byte `json:"genesis_hash_b64"`
	}
	if resp.StatusCode != http.StatusOK || json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&v) != nil {
		return false
	}
	h := genesis.Hash()
	return bytes.Equal(v.GenesisHash, h[:])
}

// phaseUser is the user a bootstrap phase runs as (see runPhase).
type phaseUser struct {
	name, home string
	uid, gid   uint32
	groups     []uint32
}

// nodeState is what bootstrap reads of GET /v2/status.
type nodeState struct {
	LastRound  basics.Round `json:"last-round"`
	Catchpoint string       `json:"catchpoint"`
}

func nodeStatus(dataDir string) (nodeState, error) {
	var st nodeState
	u, err := nodecontrol.MakeNodeController("", dataDir).ServerURL()
	if err != nil {
		return st, err
	}
	u.Path = "/v2/status"
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return st, err
	}
	if token, terr := tokens.GetAndValidateAPIToken(dataDir, tokens.AlgodTokenFilename); terr == nil {
		req.Header.Set("X-Algo-API-Token", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return st, fmt.Errorf("GET %s: %s: %s", u.Path, resp.Status, strings.TrimSpace(string(body)))
	}
	if err = json.Unmarshal(body, &st); err != nil {
		return st, fmt.Errorf("GET %s: %w", u.Path, err)
	}
	return st, nil
}
