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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

func cmdPublish(args []string) error {
	fs := flag.NewFlagSet("publish", flag.ExitOnError)
	out := fs.String("o", "", "snapshot tree to add to (served as is over HTTP)")
	key := fs.String("key", "", "private key file (ttsnap keygen) that signs the manifest and the statement")
	full := fs.Bool("full", false, "publish the whole ledger (tracker and block DBs too), not only the history")
	label := fs.String("catchpoint", "", "a catchpoint label the history reaches, for ttsnap catchup when the network's latest catchpoint is past it; one it doesn't reach is left out")
	workers := fs.Int("workers", 0, "goroutines that compute the commitment; 0: one per CPU")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap publish -o <out> -key <key file> [-full] [-catchpoint <label>] <prepared genesis dir>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if fs.NArg() != 1 || *out == "" || *key == "" {
		fs.Usage()
		os.Exit(2)
	}
	signer, err := readSigner(*key)
	if err != nil {
		return err
	}
	m, err := publish(fs.Arg(0), *out, publishOpts{signer: signer, full: *full, catchpoint: *label, workers: *workers}, os.Stderr)
	if errors.Is(err, errPublished) {
		fmt.Println("nothing to publish:", err)
		return nil
	}
	if err != nil {
		return err
	}
	fmt.Printf("published %s\n", filepath.Join(*out, manifestPath(m.GenesisID, m.Kind, m.History.Last)))
	return nil
}

type publishOpts struct {
	signer     *crypto.FalconSigner
	full       bool
	catchpoint string
	workers    int
}

// publish adds the ledger in dir to the snapshot tree out: every file as a
// blob named by its SHA-256 (files already there, such as the tables an
// earlier snapshot shares, are not copied again), the statement about the
// history with the producer's signature, then the signed manifest, then the
// latest pointer. It reports its progress to log, if not nil.
func publish(dir, out string, o publishOpts, log io.Writer) (*Manifest, error) {
	info, nw, err := readInfo(dir, o.full)
	if err != nil {
		return nil, err
	}
	if err = info.check(); err != nil {
		return nil, err
	}
	kind := kindHistory
	if o.full {
		kind = kindFull
	}
	paths, err := snapshotFiles(dir, o.full)
	if err != nil {
		return nil, err
	}
	m := &Manifest{
		Format:      manifestFormat,
		Kind:        kind,
		GenesisID:   nw.id,
		GenesisHash: nw.hash,
		Created:     time.Now().UTC().Format(time.RFC3339),
		Producer:    config.GetCurrentVersion().String(),
		Info:        info,
	}
	// before minutes of hashing: a node that has not advanced, or an older
	// snapshot, publishes nothing
	latest := filepath.Join(out, filepath.FromSlash(latestPath(m.GenesisID, kind)))
	if cur, rerr := os.ReadFile(latest); rerr == nil {
		name, perr := parseLatest(cur, kind)
		if perr != nil {
			return nil, perr
		}
		if rnd, _ := strconv.ParseUint(manifestFileName.FindStringSubmatch(name)[2], 10, 64); basics.Round(rnd) > info.History.Last {
			return nil, fmt.Errorf("the latest %s snapshot is of round %d, this one of round %d: publish only newer snapshots", kind, rnd, info.History.Last)
		}
	} else if !os.IsNotExist(rerr) {
		return nil, rerr
	}
	mp := filepath.Join(out, filepath.FromSlash(manifestPath(m.GenesisID, kind, info.History.Last)))
	if _, serr := os.Stat(mp); serr == nil {
		return nil, fmt.Errorf("%w: %s", errPublished, mp)
	}
	if o.catchpoint != "" {
		if cerr := checkLabel(o.catchpoint, info.History.Last); cerr != nil {
			if log != nil {
				fmt.Fprintf(log, "leaving the catchpoint out: %v\n", cerr)
			}
		} else {
			m.Catchpoint = o.catchpoint
		}
	}
	var total int64
	for _, p := range paths {
		fi, serr := os.Stat(filepath.Join(dir, filepath.FromSlash(p)))
		if serr != nil {
			return nil, serr
		}
		total += fi.Size()
	}
	if log != nil {
		fmt.Fprintf(log, "publishing %d files, %s, of rounds 0..%d\n", len(paths), size(total), info.History.Last)
	}
	var copied atomic.Int64
	prog := startProgress(log, "hashed", total, func() string { return size(copied.Load()) + " copied to new blobs" })
	defer prog.Stop()
	for _, p := range paths {
		f, berr := addBlob(filepath.Join(dir, filepath.FromSlash(p)), out, prog, &copied)
		if berr != nil {
			return nil, berr
		}
		f.Path = p
		m.Files = append(m.Files, f)
	}
	prog.Stop()
	if log != nil {
		fmt.Fprintf(log, "hashed %s in %s, %s of it new\n", size(prog.done.Load()), roundDuration(time.Since(prog.start)), size(copied.Load()))
	}
	if m.Commitment, err = dirCommitment(filepath.Join(dir, historyDir), info.History, o.workers, log); err != nil {
		return nil, err
	}
	stmt, err := m.statement()
	if err != nil {
		return nil, err
	}
	b, err := encodeManifest(m)
	if err != nil {
		return nil, err
	}
	// the statement first: it is about the chain, whatever the manifest
	sdir := filepath.Join(out, filepath.FromSlash(statementDir(m.GenesisID, m.History.Last)))
	if err = os.MkdirAll(sdir, 0o755); err != nil {
		return nil, err
	}
	if err = writeFileSync(filepath.Join(sdir, "statement.hex"), []byte(hex.EncodeToString(stmt.Bytes())+"\n")); err != nil {
		return nil, err
	}
	sig, err := signFile(o.signer, stmt.Bytes())
	if err != nil {
		return nil, err
	}
	if err = writeFileSync(filepath.Join(out, filepath.FromSlash(statementSigPath(m.GenesisID, m.History.Last, keyID(o.signer.PublicKey)))), sig); err != nil {
		return nil, err
	}
	if err = writeSigned(mp, b, o.signer); err != nil {
		return nil, err
	}
	// the pointer last, once the manifest and its blobs are in place
	tmp := latest + ".tmp"
	if err = os.WriteFile(tmp, []byte(manifestName(kind, info.History.Last)+"\n"), 0o644); err != nil {
		return nil, err
	}
	if err = syncFile(tmp); err != nil {
		return nil, err
	}
	if err = os.Rename(tmp, latest); err != nil {
		return nil, err
	}
	return m, syncDir(filepath.Dir(latest))
}

// dirCommitment computes the commitment of the store in dir up to the last
// round of its history hi, checking it is the store hi describes.
func dirCommitment(dir string, hi HistoryInfo, workers int, log io.Writer) (*CommitmentInfo, error) {
	st, err := timetravel.Open(dir, timetravel.Options{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer st.Close()
	c, hdrHash, err := commitmentOf(st, hi.Last, timetravel.CommitmentOptions{Workers: workers}, log)
	if err != nil {
		return nil, err
	}
	if hdrHash.String() != hi.LastHash {
		return nil, fmt.Errorf("round %d's header hash is %s, not %s", hi.Last, hdrHash, hi.LastHash)
	}
	return c, nil
}

// errPublished is returned for a snapshot whose round is published already,
// as when the node has not advanced since the previous run.
var errPublished = errors.New("a snapshot of this round is published already")

// snapshotFiles lists the files a snapshot of dir carries, relative to dir.
// Anything in the history store that the allowlist doesn't know is an error:
// fetch would refuse it.
func snapshotFiles(dir string, full bool) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, historyDir))
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, e := range entries {
		name := e.Name()
		if name == "LOCK" {
			continue
		}
		if !e.Type().IsRegular() || !pebbleFile.MatchString(name) {
			return nil, fmt.Errorf("%s holds %s, which a snapshot can't carry", historyDir, name)
		}
		paths = append(paths, historyDir+"/"+name)
	}
	if full {
		for _, name := range []string{trackerFile, blocksFile} {
			empty, err := walEmpty(filepath.Join(dir, name))
			if err != nil {
				return nil, err
			}
			if !empty {
				return nil, fmt.Errorf("%s has a WAL: run ttsnap prepare first", name)
			}
			paths = append(paths, name)
		}
	}
	return paths, nil
}

// addBlob copies the file at src into out's blobs, unless a blob with its
// content is there already, and returns its size and SHA-256. It counts the
// bytes it hashes into prog and those it copies into copied.
func addBlob(src, out string, prog *progress, copied *atomic.Int64) (File, error) {
	in, err := os.Open(src)
	if err != nil {
		return File{}, err
	}
	defer in.Close()
	h := sha256.New()
	n, err := io.Copy(countingWriter{h, prog}, in)
	if err != nil {
		return File{}, err
	}
	f := File{Size: n, SHA256: hex.EncodeToString(h.Sum(nil))}
	dst := filepath.Join(out, filepath.FromSlash(blobPath(f.SHA256)))
	if fi, err := os.Stat(dst); err == nil && fi.Size() == n {
		return f, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return File{}, err
	}
	tmp := dst + ".tmp"
	// a copy, never a hard link: the blob must not change with its source
	os.Remove(tmp)
	if err := copyFile(src, tmp); err != nil {
		return File{}, err
	}
	if err := checkSHA256(tmp, f.SHA256); err != nil {
		os.Remove(tmp)
		return File{}, fmt.Errorf("%s changed while it was published: %w", src, err)
	}
	copied.Add(n)
	return f, os.Rename(tmp, dst)
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func checkSHA256(path, want string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("SHA-256 %s, expected %s", got, want)
	}
	return nil
}

// writeSigned writes the manifest b to path and its signature to path.sig,
// replacing both atomically, the signature first so that a reader never gets
// a manifest with an older signature.
func writeSigned(path string, b []byte, signer *crypto.FalconSigner) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	sig, err := signFile(signer, manifestMessage(b))
	if err != nil {
		return err
	}
	if err := os.WriteFile(tmp+".sig", sig, 0o644); err != nil {
		return err
	}
	for _, p := range []string{tmp, tmp + ".sig"} {
		if err := syncFile(p); err != nil {
			return err
		}
	}
	if err := os.Rename(tmp+".sig", path+".sig"); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return syncDir(filepath.Dir(path))
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func cmdGC(args []string) error {
	fs := flag.NewFlagSet("gc", flag.ExitOnError)
	out := fs.String("o", "", "snapshot tree")
	keep := fs.Int("keep", 3, "snapshots to keep of each network and kind (the newest)")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap gc -o <out> [-keep N]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *out == "" || *keep < 1 {
		fs.Usage()
		os.Exit(2)
	}
	manifests, blobs, err := gc(*out, *keep)
	if err != nil {
		return err
	}
	fmt.Printf("removed %d snapshots and %d blobs\n", manifests, blobs)
	return nil
}

// gc removes all but the newest keep snapshots of each network and kind, then
// the blobs no remaining manifest lists. A client still downloading a removed
// snapshot fails on its next blob; a rerun of fetch gets the latest one.
func gc(out string, keep int) (manifests, blobs int, _ error) {
	nets, nerr := os.ReadDir(filepath.Join(out, "snapshots"))
	if nerr != nil {
		return 0, 0, nerr
	}
	referenced := map[string]bool{}
	for _, nwDir := range nets {
		dir := filepath.Join(out, "snapshots", nwDir.Name())
		entries, rerr := os.ReadDir(dir)
		if rerr != nil {
			return manifests, blobs, rerr
		}
		byKind := map[string][]uint64{}
		removed, kept := map[uint64]bool{}, map[uint64]bool{}
		for _, e := range entries {
			mm := manifestFileName.FindStringSubmatch(e.Name())
			if mm == nil {
				continue
			}
			rnd, err := strconv.ParseUint(mm[2], 10, 64)
			if err != nil {
				return manifests, blobs, err
			}
			byKind[mm[1]] = append(byKind[mm[1]], rnd)
		}
		for kind, rounds := range byKind {
			sort.Slice(rounds, func(i, j int) bool { return rounds[i] > rounds[j] })
			// the latest pointer names the newest, which is kept; but
			// never remove what it names
			latestName := ""
			if b, rerr := os.ReadFile(filepath.Join(dir, "latest-"+kind)); rerr == nil {
				var perr error
				if latestName, perr = parseLatest(b, kind); perr != nil {
					return manifests, blobs, fmt.Errorf("%s: %w", dir, perr)
				}
			} else if !os.IsNotExist(rerr) {
				return manifests, blobs, rerr
			}
			for i, rnd := range rounds {
				p := filepath.Join(dir, manifestName(kind, basics.Round(rnd)))
				if i >= keep && filepath.Base(p) != latestName {
					if err := os.Remove(p); err != nil {
						return manifests, blobs, err
					}
					os.Remove(p + ".sig")
					manifests++
					removed[rnd] = true
					continue
				}
				kept[rnd] = true
				b, err := os.ReadFile(p)
				if err != nil {
					return manifests, blobs, err
				}
				m, err := decodeManifest(b)
				if err != nil {
					// e.g. cut short by a crash: it lists nothing usable
					fmt.Fprintf(os.Stderr, "ttsnap gc: skipping %s: %v\n", p, err)
					continue
				}
				for _, f := range m.Files {
					referenced[f.SHA256] = true
				}
			}
		}
		// the statements about removed snapshots' rounds; those about
		// other rounds were put there on purpose
		for rnd := range removed {
			if !kept[rnd] {
				if err := os.RemoveAll(filepath.Join(out, filepath.FromSlash(statementDir(nwDir.Name(), basics.Round(rnd))))); err != nil {
					return manifests, blobs, err
				}
			}
		}
	}
	werr := filepath.WalkDir(filepath.Join(out, "blobs"), func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if name := d.Name(); sha256Hex.MatchString(name) && !referenced[name] {
			if err := os.Remove(p); err != nil {
				return err
			}
			blobs++
		}
		return nil
	})
	return manifests, blobs, werr
}
