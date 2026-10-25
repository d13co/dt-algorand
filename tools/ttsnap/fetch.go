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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/util/db"
)

// The dtalgorand package installs the public keys of its snapshot producers
// and attesters here (see readKeys).
const defaultKeys = "/usr/share/dtalgorand/ttsnap/keys"

// installedFile records, in the genesis directory, the manifest of the
// snapshot that fetch installed; catchup reads it.
const installedFile = "ttsnap.json"

// stageSuffix names where fetch downloads a target before it is complete.
const stageSuffix = ".ttsnap-partial"

// emptySHA256 is the SHA-256 of no bytes.
const emptySHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

type fetchOpts struct {
	dataDir     string
	base        string // base URL of the snapshot tree
	manifest    string // manifest path in the tree; empty: the latest
	keys        string
	minAttest   int // attester signatures of the statement required
	workers     int // goroutines that compute the commitment
	full        bool
	parallel    int
	stallLimit  time.Duration
	maxAge      time.Duration // refuse an older latest snapshot; 0: any
	progressLog io.Writer
	client      *http.Client
}

func cmdFetch(args []string) error {
	fs := flag.NewFlagSet("fetch", flag.ExitOnError)
	o := fetchOpts{progressLog: os.Stderr}
	fs.StringVar(&o.base, "url", "", "base URL of the snapshot tree")
	fs.StringVar(&o.dataDir, "d", "", "data directory of the new node: genesis.json, and config.json with EnableTimeTravel")
	fs.StringVar(&o.manifest, "manifest", "", "path of a manifest in the tree to install instead of the latest, e.g. snapshots/mainnet-v1.0/history-123.json")
	fs.StringVar(&o.keys, "keys", defaultKeys, "keys file with the producers' and attesters' public keys")
	fs.IntVar(&o.minAttest, "min-attest", 0, "attesters of the keys file that must have signed the snapshot's statement")
	fs.IntVar(&o.workers, "workers", 0, "goroutines that compute the downloaded history's commitment; 0: one per CPU")
	fs.BoolVar(&o.full, "full", false, "install the whole ledger instead of the history alone; needs -trust-producer")
	trust := fs.Bool("trust-producer", false, "accept a full snapshot's state from the producer without checking it against a catchpoint")
	fs.IntVar(&o.parallel, "parallel", 4, "files downloaded at once")
	fs.DurationVar(&o.maxAge, "max-age", 7*24*time.Hour, "refuse a latest snapshot older than this (a mirror can serve a stale latest pointer, which is not signed); 0: any age")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap fetch -url <base URL> -d <data dir> [-keys <keys file>] [-min-attest N] [-full -trust-producer]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if o.base == "" || o.dataDir == "" || fs.NArg() != 0 || o.parallel < 1 || o.minAttest < 0 {
		fs.Usage()
		os.Exit(2)
	}
	if o.full && !*trust {
		return errors.New("a full snapshot's account state is the producer's and is not checked against the chain: pass -trust-producer to install one, or install the history alone and catch up from a catchpoint")
	}
	m, gdir, err := fetch(o)
	if err != nil {
		return err
	}
	fmt.Printf("installed the %s snapshot of %s up to round %d into %s\n", m.Kind, m.GenesisID, m.History.Last, gdir)
	if m.Kind == kindHistory {
		fmt.Printf("next: start algod on %s, then run: ttsnap catchup -d %s\n", o.dataDir, o.dataDir)
	} else {
		fmt.Printf("next: start algod on %s; it continues from round %d\n", o.dataDir, *m.TrackerRound)
	}
	return nil
}

// target is a file to download and where it goes.
type target struct {
	File
	stage string // download path
}

// fetch downloads, checks and installs a snapshot. Nothing is written to the
// data directory until the manifest's signature and contents are checked;
// files are then downloaded next to their targets (resuming what an earlier
// run left), checked, and renamed into place once all of them are.
func fetch(o fetchOpts) (*Manifest, string, error) {
	if o.client == nil {
		o.client = &http.Client{Transport: &http.Transport{Proxy: http.ProxyFromEnvironment, ResponseHeaderTimeout: time.Minute}}
	}
	if o.stallLimit == 0 {
		o.stallLimit = 2 * time.Minute
	}
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(o.dataDir, config.GenesisJSONFile))
	if err != nil {
		return nil, "", err
	}
	cfg, err := config.LoadConfigFromDisk(o.dataDir)
	if err != nil && !os.IsNotExist(err) {
		return nil, "", err
	}
	if !cfg.EnableTimeTravel {
		return nil, "", errors.New("the data directory's config.json must set EnableTimeTravel")
	}
	if cfg.Archival {
		return nil, "", errors.New("the data directory's config.json must not set Archival: an archival node resets a block DB that does not start at genesis")
	}
	kind := kindHistory
	if o.full {
		kind = kindFull
		if cfg.StorageEngine != "" && cfg.StorageEngine != "sqlite" {
			return nil, "", fmt.Errorf("a full snapshot holds SQLite DBs, the node's StorageEngine is %s", cfg.StorageEngine)
		}
	}

	keys, err := readKeys(o.keys)
	if err != nil {
		return nil, "", err
	}
	ctx := context.Background()
	mpath, err := resolveManifest(ctx, o.client, o.base, genesis.ID(), kind, o.manifest)
	if err != nil {
		return nil, "", err
	}
	murl, err := joinURL(o.base, mpath)
	if err != nil {
		return nil, "", err
	}
	body, err := getSmall(ctx, o.client, murl, 64<<20)
	if err != nil {
		return nil, "", err
	}
	sig, err := getSmall(ctx, o.client, murl+".sig", 64<<10)
	if err != nil {
		return nil, "", err
	}
	producer, err := verifySigFile(sig, manifestMessage(body), keys, roleProducer)
	if err != nil {
		return nil, "", fmt.Errorf("the manifest's signature: %w", err)
	}
	m, err := decodeManifest(body)
	if err != nil {
		return nil, "", err
	}
	if err = m.validate(kind, genesis.ID(), genesis.Hash().String()); err != nil {
		return nil, "", err
	}
	if o.manifest == "" && o.maxAge > 0 {
		created, perr := time.Parse(time.RFC3339, m.Created)
		if perr != nil {
			return nil, "", fmt.Errorf("the manifest's creation time: %w", perr)
		}
		if age := time.Since(created); age > o.maxAge {
			return nil, "", fmt.Errorf("the latest snapshot is %v old, more than -max-age %v: the mirror may serve a stale pointer, which is not signed; pass -max-age 0 or -manifest to install it anyway", age.Round(time.Hour), o.maxAge)
		}
	}

	stmt, err := m.statement()
	if err != nil {
		return nil, "", err
	}
	fmt.Fprintf(o.progressLog, "%s, signed by %s\n", stmt, producer.Name)
	if o.minAttest > 0 {
		signed, problems := o.attestations(ctx, m.GenesisID, stmt, keys)
		for _, p := range problems {
			fmt.Fprintln(o.progressLog, p)
		}
		if len(signed) < o.minAttest {
			return nil, "", fmt.Errorf("%d attesters signed the statement (%s), -min-attest is %d", len(signed), strings.Join(signed, ", "), o.minAttest)
		}
		fmt.Fprintf(o.progressLog, "attested by %s\n", strings.Join(signed, ", "))
	}

	// only now: resolving the dirs creates them
	dirs, err := cfg.EnsureAndResolveGenesisDirs(o.dataDir, genesis.ID(), logging.Base())
	if err != nil {
		return nil, "", err
	}
	histFinal := filepath.Join(dirs.TimeTravelGenesisDir, historyDir)
	histStage := histFinal + stageSuffix
	finals := map[string]string{trackerFile: filepath.Join(dirs.TrackerGenesisDir, trackerFile), blocksFile: filepath.Join(dirs.BlockGenesisDir, blocksFile)}
	record := filepath.Join(dirs.RootGenesisDir, installedFile)
	if _, err = os.Lstat(histFinal); err == nil {
		// an earlier run of this manifest that stopped after moving the
		// history into place finishes here
		if prev, rerr := os.ReadFile(record); rerr == nil && bytes.Equal(prev, body) {
			if kind == kindFull {
				if err = renameStaged(finals); err != nil {
					return nil, "", err
				}
			}
			return m, dirs.RootGenesisDir, nil
		}
		return nil, "", fmt.Errorf("%s already exists: remove it to install a snapshot", histFinal)
	} else if !os.IsNotExist(err) {
		return nil, "", err
	}
	if kind == kindFull {
		for _, p := range finals {
			for _, suffix := range []string{"", "-wal", "-shm"} {
				if _, err = os.Lstat(p + suffix); err == nil {
					return nil, "", fmt.Errorf("%s already exists: a full snapshot is installed into a new data directory", p+suffix)
				} else if !os.IsNotExist(err) {
					return nil, "", err
				}
			}
		}
	}
	if err = ensureDir(histStage); err != nil {
		return nil, "", err
	}
	// what an earlier run left of another snapshot (the latest one changes
	// while a download runs for hours) must not end up in the store
	if err = pruneStage(histStage, m); err != nil {
		return nil, "", err
	}
	var targets []target
	for _, f := range m.Files {
		t := target{File: f}
		if dir, name, ok := strings.Cut(f.Path, "/"); ok && dir == historyDir {
			t.stage = filepath.Join(histStage, name)
		} else {
			t.stage = finals[f.Path] + stageSuffix
		}
		targets = append(targets, t)
	}
	if err = checkSpace(targets); err != nil {
		return nil, "", err
	}
	if err = o.download(ctx, m, targets); err != nil {
		return nil, "", err
	}
	if err = syncDir(histStage); err != nil {
		return nil, "", err
	}

	// what was downloaded opens, is what the manifest says, and its headers
	// chain from this network's genesis block
	hi, nw, err := readHistory(histStage)
	if err != nil {
		return nil, "", fmt.Errorf("the downloaded history: %w", err)
	}
	if nw.hash != m.GenesisHash || nw.id != m.GenesisID || hi.First != 0 || hi.Last != m.History.Last || hi.LastHash != m.History.LastHash || hi.BoxIndexFrom != 0 {
		return nil, "", fmt.Errorf("the downloaded history (network %s, rounds %d..%d ending in %s, box index from %d) is not what the manifest says (network %s, rounds 0..%d ending in %s)", nw.id, hi.First, hi.Last, hi.LastHash, hi.BoxIndexFrom, m.GenesisID, m.History.Last, m.History.LastHash)
	}
	fmt.Fprintln(o.progressLog, "checking the headers")
	if err = checkHeaders(histStage, genesis.Hash(), o.progressLog); err != nil {
		return nil, "", fmt.Errorf("the downloaded history: %w", err)
	}
	// the content, whatever the files: the statement's commitment
	if err = o.canonicalize(histStage, hi, m, path.Base(mpath)); err != nil {
		return nil, "", fmt.Errorf("the downloaded history: %w", err)
	}
	if kind == kindFull {
		rnd, qerr := sqliteQuery(finals[trackerFile]+stageSuffix, 1, "SELECT rnd FROM acctrounds WHERE id = 'acctbase'")
		if qerr != nil {
			return nil, "", qerr
		}
		if rnd[0] != *m.TrackerRound {
			return nil, "", fmt.Errorf("the downloaded tracker DB is at round %d, the manifest says %d", rnd[0], *m.TrackerRound)
		}
		if err = clearCatchpointState(finals[trackerFile] + stageSuffix); err != nil {
			return nil, "", err
		}
		if err = checkpointWAL(finals[trackerFile] + stageSuffix); err != nil {
			return nil, "", err
		}
	}
	// The record first, then the history, then the DBs: a node with the
	// history but no DBs yet is a node at genesis, and a rerun with the same
	// manifest finishes the install.
	if err = writeFileSync(record, body); err != nil {
		return nil, "", err
	}
	if err = os.Rename(histStage, histFinal); err != nil {
		return nil, "", err
	}
	if err = syncDir(filepath.Dir(histFinal)); err != nil {
		return nil, "", err
	}
	if kind == kindFull {
		if err = renameStaged(finals); err != nil {
			return nil, "", err
		}
	}
	return m, dirs.RootGenesisDir, nil
}

// renameStaged moves the downloaded DBs of a full snapshot into place, the
// block DB first: a tracker DB ahead of the block DB is refused.
func renameStaged(finals map[string]string) error {
	for _, name := range []string{blocksFile, trackerFile} {
		if _, err := os.Lstat(finals[name] + stageSuffix); os.IsNotExist(err) {
			continue // moved by an earlier run
		}
		if err := os.Rename(finals[name]+stageSuffix, finals[name]); err != nil {
			return err
		}
		if err := syncDir(filepath.Dir(finals[name])); err != nil {
			return err
		}
	}
	return nil
}

func writeFileSync(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

// pruneStage removes from the staging dir the files the manifest does not
// list.
func pruneStage(stage string, m *Manifest) error {
	listed := map[string]bool{}
	for _, f := range m.Files {
		if dir, name, ok := strings.Cut(f.Path, "/"); ok && dir == historyDir {
			listed[name] = true
		}
	}
	entries, err := os.ReadDir(stage)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if !listed[e.Name()] {
			if err := os.RemoveAll(filepath.Join(stage, e.Name())); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkHeaders runs verifyHeaders on the store in dir.
func checkHeaders(dir string, genesisHash crypto.Digest, progress io.Writer) error {
	st, err := timetravel.Open(dir, timetravel.Options{ReadOnly: true})
	if err != nil {
		return err
	}
	defer st.Close()
	_, err = verifyHeaders(st, genesisHash, progress)
	return err
}

// ensureDir creates dir, or accepts it if it is a directory already (left by
// an earlier run), but not a symlink to one.
func ensureDir(dir string) error {
	fi, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		return os.Mkdir(dir, 0o700)
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s exists and is not a directory", dir)
	}
	return nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// checkSpace checks that each filesystem has room for what remains to be
// downloaded to it, plus 5% and 1 GiB for the store's own needs.
func checkSpace(targets []target) error {
	need := map[string]uint64{} // by an existing dir on the filesystem
	for _, t := range targets {
		have := int64(0)
		if fi, err := os.Lstat(t.stage); err == nil {
			have = min(fi.Size(), t.Size)
		}
		need[filepath.Dir(t.stage)] += uint64(t.Size - have)
	}
	for dir, n := range need {
		free, ok := freeBytes(dir)
		if !ok {
			continue
		}
		if want := n + n/20 + 1<<30; free < want {
			return fmt.Errorf("%s has %d MiB free, the snapshot needs %d MiB there", dir, free>>20, want>>20)
		}
	}
	return nil
}

// download fetches every target that isn't complete yet, o.parallel at once.
func (o fetchOpts) download(ctx context.Context, m *Manifest, targets []target) error {
	var total int64
	for _, t := range targets {
		total += t.Size
	}
	// the workers and the progress reports write at once
	o.progressLog = &syncWriter{w: o.progressLog}
	fmt.Fprintf(o.progressLog, "downloading %d files, %s, of rounds 0..%d\n", len(targets), size(total), m.History.Last)
	prog := startProgress(o.progressLog, "downloaded", total, nil)
	defer prog.Stop()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	work := make(chan target)
	errs := make(chan error, len(targets))
	var wg sync.WaitGroup
	for range o.parallel {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for t := range work {
				var err error
				for try := range 4 {
					if try > 0 {
						time.Sleep(time.Duration(try) * 2 * time.Second)
					}
					if err = o.downloadFile(ctx, t, prog); err == nil || ctx.Err() != nil {
						break
					}
					fmt.Fprintf(o.progressLog, "%s: %v; retrying\n", t.Path, err)
				}
				if err != nil {
					errs <- fmt.Errorf("%s: %w", t.Path, err)
					cancel()
				}
			}
		}()
	}
	for _, t := range targets {
		select {
		case work <- t:
		case <-ctx.Done():
		}
	}
	close(work)
	wg.Wait()
	close(errs)
	if err := <-errs; err != nil {
		return err
	}
	prog.Stop()
	fmt.Fprintf(o.progressLog, "downloaded %s in %s, %s of it transferred by this run\n", size(total), roundDuration(time.Since(prog.start)), size(prog.moved.Load()))
	return nil
}

// downloadFile brings t.stage to t's content: it resumes a partial file with a
// range request and checks the SHA-256 of the whole file. A file that fails
// the check is removed, so that a retry starts over. It counts into prog what
// it completes.
func (o fetchOpts) downloadFile(ctx context.Context, t target, prog *progress) error {
	var have int64
	fi, err := os.Lstat(t.stage)
	exists := err == nil
	switch {
	case exists && !fi.Mode().IsRegular():
		return fmt.Errorf("%s exists and is not a regular file", t.stage)
	case exists:
		have = fi.Size()
	case !os.IsNotExist(err):
		return err
	}
	if t.Size == 0 {
		// Pebble's markers are empty files: nothing to download
		if t.SHA256 != emptySHA256 {
			return fmt.Errorf("an empty file with SHA-256 %s", t.SHA256)
		}
		return createEmpty(t.stage)
	}
	if have > t.Size {
		have = 0
	}
	h := sha256.New()
	if have > 0 {
		if err = hashPrefix(t.stage, have, h); err != nil {
			return err
		}
	}
	if exists && have == t.Size {
		if hex.EncodeToString(h.Sum(nil)) == t.SHA256 {
			prog.done.Add(t.Size)
			return nil
		}
		have = 0
		h.Reset()
	}
	// what is here counts as done until this attempt fails; a retry
	// downloads it again, or resumes after it
	prog.done.Add(have)
	credited := have
	var body *progressReader
	complete := false
	defer func() {
		if !complete {
			if body != nil {
				credited += body.n
			}
			prog.done.Add(-credited)
		}
	}()

	u, err := joinURL(o.base, blobPath(t.SHA256))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if have > 0 {
		req.Header.Set("Range", fmt.Sprintf("bytes=%d-", have))
	}
	resp, err := o.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusPartialContent && have > 0:
		if !strings.HasPrefix(resp.Header.Get("Content-Range"), fmt.Sprintf("bytes %d-", have)) {
			return fmt.Errorf("GET %s: unexpected Content-Range %q", u, resp.Header.Get("Content-Range"))
		}
	case resp.StatusCode == http.StatusOK:
		have = 0 // no range support: start over
		h.Reset()
		prog.done.Add(-credited)
		credited = 0
	default:
		return fmt.Errorf("GET %s: %s", u, resp.Status)
	}

	f, err := os.OpenFile(t.stage, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err = f.Truncate(have); err != nil {
		return err
	}
	if _, err = f.Seek(have, io.SeekStart); err != nil {
		return err
	}
	// a stalled transfer is cancelled, so that the retry resumes it
	var last atomic.Int64
	last.Store(time.Now().UnixNano())
	stopWatch := make(chan struct{})
	defer close(stopWatch)
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stopWatch:
				return
			case <-tick.C:
				if time.Since(time.Unix(0, last.Load())) > o.stallLimit {
					cancel()
					return
				}
			}
		}
	}()
	body = &progressReader{r: io.LimitReader(resp.Body, t.Size-have+1), last: &last, prog: prog}
	n, err := io.Copy(io.MultiWriter(f, h), body)
	if err != nil {
		return err
	}
	if have+n != t.Size {
		return fmt.Errorf("got %d bytes, expected %d", have+n, t.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != t.SHA256 {
		f.Close()
		os.Remove(t.stage)
		return fmt.Errorf("SHA-256 %s, expected %s", got, t.SHA256)
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}

// progressReader notes when it last read, and counts what it reads.
type progressReader struct {
	r    io.Reader
	last *atomic.Int64
	prog *progress
	n    int64
}

func (p *progressReader) Read(b []byte) (int, error) {
	n, err := p.r.Read(b)
	if n > 0 {
		p.last.Store(time.Now().UnixNano())
		p.prog.add(int64(n))
		p.n += int64(n)
	}
	return n, err
}

func createEmpty(path string) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func hashPrefix(path string, n int64, h hash.Hash) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.CopyN(h, f, n)
	return err
}

// getSmall GETs a document of at most limit bytes.
func getSmall(ctx context.Context, client *http.Client, u string, limit int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("GET %s: %s: %w", u, resp.Status, errNotFound)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", u, resp.Status)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("GET %s: larger than %d bytes", u, limit)
	}
	return b, nil
}

var errNotFound = errors.New("not found")

// canonicalize makes the downloaded store answer like any other store with
// the manifest's commitment, or fails: the strict commitment refuses records
// the commitment does not cover (after the last round, outside the store's
// keyspaces), the records it leaves out (no-ops, lone tombstones) are deleted,
// and the box index, which it does not cover either, is rebuilt from the box
// records. The store is then marked as installed from the snapshot name.
func (o *fetchOpts) canonicalize(dir string, hi HistoryInfo, m *Manifest, name string) error {
	st, err := timetravel.Open(dir, timetravel.Options{})
	if err != nil {
		return err
	}
	defer st.Close()
	fmt.Fprintln(o.progressLog, "computing the history's commitment")
	// delete what the commitment leaves out as the scan finds it: the scan
	// reads a snapshot, so the writes don't disturb it
	var mu deadlock.Mutex
	b := st.DB().NewBatch()
	var berr error
	var removed uint64
	nonCanonical := func(k []byte) {
		mu.Lock()
		defer mu.Unlock()
		if berr != nil {
			return
		}
		if berr = b.Delete(k, nil); berr == nil {
			removed++
			if b.Count() >= 1<<16 {
				berr = b.Commit(pebble.NoSync)
				b.Close()
				b = st.DB().NewBatch()
			}
		}
	}
	got, hdrHash, err := commitmentOf(st, hi.Last, timetravel.CommitmentOptions{Workers: o.workers, Strict: true, NonCanonical: nonCanonical}, o.progressLog)
	if err == nil {
		err = berr
	}
	if err == nil {
		err = b.Commit(pebble.Sync)
	}
	b.Close()
	if err != nil {
		return err
	}
	if hdrHash.String() != hi.LastHash {
		return fmt.Errorf("round %d's header hash is %s, not %s", hi.Last, hdrHash, hi.LastHash)
	}
	if *got != *m.Commitment {
		return fmt.Errorf("its commitment is %s (%d objects, %d versions), the manifest's %s (%d objects, %d versions)", got.Digest, got.Objects, got.Versions, m.Commitment.Digest, m.Commitment.Objects, m.Commitment.Versions)
	}
	fmt.Fprintf(o.progressLog, "removed %d records the commitment leaves out; rebuilding the box index\n", removed)
	if _, err = st.IndexBoxes(context.Background(), nil); err != nil {
		return err
	}
	return st.SetInstalled(name, uint64(hi.Last))
}

// attestations fetches the attesters' signatures of stmt from the snapshot
// tree, and returns the names of the attesters whose signature verifies, and
// what was wrong with the others' (none published is not a problem).
func (o *fetchOpts) attestations(ctx context.Context, genesisID string, stmt Statement, keys []Key) (signed, problems []string) {
	msg := stmt.Bytes()
	for _, k := range keys {
		if k.Role != roleAttester {
			continue
		}
		u, err := joinURL(o.base, statementSigPath(genesisID, stmt.Round, k.ID()))
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		b, err := getSmall(ctx, o.client, u, 64<<10)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err == nil {
			_, err = verifySigFile(b, msg, []Key{k}, roleAttester)
		}
		if err != nil {
			problems = append(problems, fmt.Sprintf("attester %s: %v", k.Name, err))
			continue
		}
		signed = append(signed, k.Name)
	}
	return signed, problems
}

// resolveManifest returns the path, in the snapshot tree at base, of
// manifest if it is not empty, else of the manifest the latest pointer of
// kind names.
func resolveManifest(ctx context.Context, client *http.Client, base, genesisID, kind, manifest string) (string, error) {
	if manifest != "" {
		return manifest, nil
	}
	lurl, err := joinURL(base, latestPath(genesisID, kind))
	if err != nil {
		return "", err
	}
	b, err := getSmall(ctx, client, lurl, 1024)
	if err != nil {
		return "", err
	}
	name, err := parseLatest(b, kind)
	if err != nil {
		return "", err
	}
	return path.Join("snapshots", genesisID, name), nil
}

// joinURL appends a path of the snapshot tree to its base URL. The path must
// stay inside the tree.
func joinURL(base, p string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("%s: only http and https URLs", base)
	}
	clean := path.Clean("/" + p)
	if clean != "/"+p || strings.Contains(p, "..") {
		return "", fmt.Errorf("bad path %q in the snapshot tree", p)
	}
	u.Path = strings.TrimSuffix(u.Path, "/") + clean
	return u.String(), nil
}

// clearCatchpointState removes from a full snapshot's tracker DB the
// producer's catchpoint work: catchpoint files are not shipped, and a node
// resuming an unfinished one would look for its data file.
func clearCatchpointState(path string) error {
	acc, err := db.MakeAccessor(path, false, false)
	if err != nil {
		return err
	}
	defer acc.Close()
	stmts := []string{"DELETE FROM unfinishedcatchpoints", "DELETE FROM catchpointfirststageinfo", "DELETE FROM storedcatchpoints"}
	var keys []string
	for _, k := range []trackerdb.CatchpointState{trackerdb.CatchpointStateWritingFirstStageInfo, trackerdb.CatchpointStateWritingCatchpoint, trackerdb.CatchpointStateLastCatchpoint} {
		keys = append(keys, strconv.Quote(string(k)))
	}
	stmts = append(stmts, "DELETE FROM catchpointstate WHERE id IN ("+strings.ReplaceAll(strings.Join(keys, ","), `"`, "'")+")")
	for _, s := range stmts {
		if _, err := acc.Handle.Exec(s); err != nil && !strings.Contains(err.Error(), "no such table") {
			return fmt.Errorf("%s: %s: %w", path, s, err)
		}
	}
	return nil
}
