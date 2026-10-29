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
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/nodecontrol"
	"github.com/algorand/go-algorand/util/tokens"
)

// nodeStatement is the statement a node's own history makes about round rnd,
// with what its commitment counted.
type nodeStatement struct {
	Statement
	Info CommitmentInfo
}

// statementOfNode computes the statement of the node in dataDir about round
// rnd: from the running algod's admin API, or from the store of a stopped
// node. rnd 0 means the store's last round, for a stopped node only.
func statementOfNode(dataDir string, genesis bookkeeping.Genesis, rnd basics.Round, workers int, log io.Writer) (nodeStatement, error) {
	ns, apiErr := statementFromAPI(dataDir, genesis, rnd, log)
	if apiErr == nil {
		return ns, nil
	}
	if !errors.Is(apiErr, errNoNode) {
		return ns, apiErr
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return ns, err
	}
	dir, err := cfg.TimeTravelGenesisDir(dataDir, genesis.ID())
	if err != nil {
		return ns, err
	}
	st, err := timetravel.Open(filepath.Join(dir, historyDir), timetravel.Options{ReadOnly: true})
	if err != nil {
		return ns, fmt.Errorf("%v; opening the history of a stopped node: %w", apiErr, err)
	}
	defer st.Close()
	if reason, found, derr := st.Diverged(); derr != nil || found {
		if derr == nil {
			derr = fmt.Errorf("the history is marked as differing from the chain: %s", reason)
		}
		return ns, derr
	}
	if rnd == 0 {
		last, _ := st.Last()
		rnd = basics.Round(last)
	}
	if name, installed, ierr := st.Installed(); ierr != nil || installed {
		if ierr == nil {
			ierr = fmt.Errorf("%w: %s", errInstalled, name)
		}
		return ns, ierr
	}
	c, hdrHash, err := commitmentOf(st, rnd, timetravel.CommitmentOptions{Workers: workers}, log)
	if err != nil {
		return ns, err
	}
	ns.Info = *c
	ns.GenesisHash, ns.Round, ns.HeaderHash = genesis.Hash(), rnd, crypto.Digest(hdrHash)
	b, _ := hex.DecodeString(c.Digest)
	copy(ns.Commitment[:], b)
	return ns, nil
}

// errInstalled: the node's history was installed from a snapshot, so its
// statements would vouch for the producer with the producer's own data.
var errInstalled = errors.New("this node's history was installed from a snapshot: attest from a history the node built itself")

// errNoNode: no algod runs on the data directory.
var errNoNode = errors.New("algod is not running")

func statementFromAPI(dataDir string, genesis bookkeeping.Genesis, rnd basics.Round, log io.Writer) (nodeStatement, error) {
	var ns nodeStatement
	u, err := nodecontrol.MakeNodeController("", dataDir).ServerURL()
	if err != nil {
		return ns, fmt.Errorf("%w (%v)", errNoNode, err)
	}
	if rnd == 0 {
		return ns, errors.New("algod is running: pass -round")
	}
	u.Path = path.Join(v2.HistoryCommitmentPath, fmt.Sprint(rnd))
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, u.String(), nil)
	if err != nil {
		return ns, err
	}
	token, err := tokens.GetAndValidateAPIToken(dataDir, tokens.AlgodAdminTokenFilename)
	if err != nil {
		return ns, err
	}
	req.Header.Set("X-Algo-API-Token", token)
	if log != nil {
		fmt.Fprintf(log, "asking algod at %s for the commitment of round %d (minutes on mainnet)\n", u.Host, rnd)
	}
	resp, err := http.DefaultClient.Do(req) // no timeout: the node reads its whole history
	if err != nil {
		return ns, fmt.Errorf("%w (%v)", errNoNode, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return ns, err
	}
	if resp.StatusCode != http.StatusOK {
		return ns, fmt.Errorf("GET %s: %s: %s", u.Path, resp.Status, body)
	}
	var r v2.HistoryCommitmentResponse
	if err = json.Unmarshal(body, &r); err != nil {
		return ns, err
	}
	if r.GenesisHash != genesis.Hash().String() || r.Round != uint64(rnd) {
		return ns, fmt.Errorf("algod answered for round %d of %s", r.Round, r.GenesisHash)
	}
	if r.InstalledFrom != "" {
		return ns, fmt.Errorf("%w: %s", errInstalled, r.InstalledFrom)
	}
	ns.GenesisHash, ns.Round = genesis.Hash(), rnd
	if ns.HeaderHash, err = parseBlockHash(r.HeaderHash); err != nil {
		return ns, err
	}
	b, err := hex.DecodeString(r.Commitment)
	if err != nil || len(b) != len(ns.Commitment) {
		return ns, fmt.Errorf("algod's commitment %q", r.Commitment)
	}
	copy(ns.Commitment[:], b)
	ns.Info = CommitmentInfo{Digest: r.Commitment, Objects: r.Objects, Versions: r.Versions, Headers: r.Headers, Expired: r.Expired}
	if log != nil {
		fmt.Fprintf(log, "algod computed it in %.0f s\n", r.Seconds)
	}
	return ns, nil
}

func cmdCommitment(args []string) error {
	fs := flag.NewFlagSet("commitment", flag.ExitOnError)
	dataDir := fs.String("d", "", "data directory of a node")
	rnd := fs.Uint64("round", 0, "round; 0: the last round of a stopped node's history")
	workers := fs.Int("workers", 0, "goroutines that compute the commitment of a stopped node's history; 0: one per CPU")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap commitment -d <data dir> [-round R]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *dataDir == "" || fs.NArg() != 0 {
		fs.Usage()
		os.Exit(2)
	}
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(*dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	ns, err := statementOfNode(*dataDir, genesis, basics.Round(*rnd), *workers, os.Stderr)
	if err != nil {
		return err
	}
	return printJSON(struct {
		Round      basics.Round   `json:"round"`
		HeaderHash string         `json:"header-hash"`
		Commitment CommitmentInfo `json:"commitment"`
		Statement  string         `json:"statement"`
	}{ns.Round, ns.HeaderHash.String(), ns.Info, hex.EncodeToString(ns.Bytes())})
}

func cmdAttest(args []string) error {
	fs := flag.NewFlagSet("attest", flag.ExitOnError)
	key := fs.String("key", "", "private key file (ttsnap keygen -role attester)")
	dataDir := fs.String("d", "", "data directory of a node whose history it built itself (synced from genesis, or imported with ttimport)")
	base := fs.String("url", "", "snapshot tree whose latest history snapshot (or -manifest) to attest")
	mpath := fs.String("manifest", "", "path of the manifest in the tree, e.g. snapshots/mainnet-v1.0/history-123.json")
	rnd := fs.Uint64("round", 0, "round to attest, instead of a snapshot's")
	out := fs.String("o", "", "snapshot tree to write the signature to")
	workers := fs.Int("workers", 0, "goroutines that compute the commitment of a stopped node's history; 0: one per CPU")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap attest -key <key file> -d <data dir> (-url <base URL> [-manifest <path>] | -round R) -o <out>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *key == "" || *dataDir == "" || *out == "" || fs.NArg() != 0 || (*base == "") == (*rnd == 0) {
		fs.Usage()
		os.Exit(2)
	}
	signer, err := readSigner(*key)
	if err != nil {
		return err
	}
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(*dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	p, err := attest(attestOpts{signer: signer, dataDir: *dataDir, genesis: genesis, base: *base, manifest: *mpath,
		round: basics.Round(*rnd), out: *out, workers: *workers, client: http.DefaultClient, log: os.Stderr})
	if err != nil {
		return err
	}
	fmt.Printf("wrote %s\n", p)
	return nil
}

type attestOpts struct {
	signer   *crypto.FalconSigner
	dataDir  string
	genesis  bookkeeping.Genesis
	base     string // snapshot tree; empty: attest round
	manifest string
	round    basics.Round
	out      string
	workers  int
	client   *http.Client
	log      io.Writer
}

// attest signs the statement the node's own history makes about a round:
// the round of a published snapshot, whose statement must be the same, or
// o.round. It writes the signature into the tree o.out and returns its path.
func attest(o attestOpts) (string, error) {
	// a history installed from a snapshot is the producer's before the
	// catchpoint: attesting it would vouch for the producer with its own data
	if _, err := os.Stat(filepath.Join(o.dataDir, o.genesis.ID(), installedFile)); err == nil {
		return "", fmt.Errorf("this node's history was installed from a snapshot (%s): attest from a history the node built itself", installedFile)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	var want *Statement
	rnd := o.round
	if o.base != "" {
		ctx := context.Background()
		mp, err := resolveManifest(ctx, o.client, o.base, o.genesis.ID(), kindHistory, o.manifest)
		if err != nil {
			return "", err
		}
		murl, err := joinURL(o.base, mp)
		if err != nil {
			return "", err
		}
		body, err := getSmall(ctx, o.client, murl, 64<<20)
		if err != nil {
			return "", err
		}
		m, err := decodeManifest(body)
		if err != nil {
			return "", err
		}
		if m.GenesisHash != o.genesis.Hash().String() {
			return "", fmt.Errorf("the snapshot is of network %s, the node's is %s", m.GenesisID, o.genesis.ID())
		}
		s, err := m.statement()
		if err != nil {
			return "", err
		}
		want, rnd = &s, s.Round
		fmt.Fprintf(o.log, "the snapshot says: %s\n", s)
	}
	ns, err := statementOfNode(o.dataDir, o.genesis, rnd, o.workers, o.log)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(o.log, "this node says:    %s\n", ns.Statement)
	if want != nil && *want != ns.Statement {
		return "", errors.New("the node's history differs from the snapshot's: not attesting it")
	}
	msg := ns.Bytes()
	sig, err := signFile(o.signer, msg)
	if err != nil {
		return "", err
	}
	sdir := filepath.Join(o.out, filepath.FromSlash(statementDir(o.genesis.ID(), rnd)))
	if err := os.MkdirAll(sdir, 0o755); err != nil {
		return "", err
	}
	if err := writeFileSync(filepath.Join(sdir, "statement.hex"), []byte(hex.EncodeToString(msg)+"\n")); err != nil {
		return "", err
	}
	p := filepath.Join(o.out, filepath.FromSlash(statementSigPath(o.genesis.ID(), rnd, keyID(o.signer.PublicKey))))
	return p, writeFileSync(p, sig)
}
