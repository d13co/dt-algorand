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
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
)

// The files of a ledger, relative to its genesis directory.
const (
	historyDir  = config.LedgerFilenamePrefix + ".timetravel"
	trackerFile = config.LedgerFilenamePrefix + ".tracker.sqlite"
	blocksFile  = config.LedgerFilenamePrefix + ".block.sqlite"
)

// Snapshot kinds: the history alone, for a node that then catches up from a
// catchpoint, or the whole ledger, whose state the consumer takes on trust.
const (
	kindHistory = "history"
	kindFull    = "full"
)

// manifestFormat is the version of the manifest layout: 2 has the history
// commitment, and is signed with Falcon-1024.
const manifestFormat = 2

// maxFileSize bounds the size a manifest may claim for one file (16 TiB).
const maxFileSize = 1 << 44

// Manifest describes a published snapshot. It is signed, and lists every
// file with its SHA-256; the files are served as content-addressed blobs.
type Manifest struct {
	Format      int    `json:"format"`
	Kind        string `json:"kind"`
	GenesisID   string `json:"genesis-id"`
	GenesisHash string `json:"genesis-hash"`
	Created     string `json:"created"`
	Producer    string `json:"producer"`
	Info
	// Commitment is the history's commitment up to its last round, which
	// fetch recomputes from the downloaded files.
	Commitment *CommitmentInfo `json:"commitment"`
	// Catchpoint is a catchpoint label that the history reaches, which
	// ttsnap catchup uses when the network's latest catchpoint is past it.
	Catchpoint string `json:"catchpoint,omitempty"`
	Files      []File `json:"files"`
}

// CommitmentInfo is a history's commitment (timetravel.Store.Commitment) and
// what it counted.
type CommitmentInfo struct {
	Digest   string `json:"digest"` // hex
	Objects  uint64 `json:"objects"`
	Versions uint64 `json:"versions"`
	Headers  uint64 `json:"headers"`
	Expired  uint64 `json:"expired"`
}

// File is one published file: its path relative to the genesis directory.
type File struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// pebbleFile matches the files of a Pebble store that a snapshot carries:
// tables, WALs, blob files, manifests, options and markers. The LOCK file and
// temporary files are left out.
var pebbleFile = regexp.MustCompile(`^([0-9]{6,}\.(sst|log|blob)|[0-9]{6,}-[0-9]{3}\.log|MANIFEST-[0-9]{6,}|OPTIONS-[0-9]{6,}|marker\.(format-version|manifest)\.[0-9]{6,}\.[A-Za-z0-9-]+|CURRENT)$`)

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// allowedPath reports whether a snapshot of kind may carry the file at p: a
// file of the history store, or for a full snapshot the two SQLite DBs.
func allowedPath(kind, p string) bool {
	if dir, name, ok := strings.Cut(p, "/"); ok {
		return dir == historyDir && pebbleFile.MatchString(name)
	}
	return kind == kindFull && (p == trackerFile || p == blocksFile)
}

// validate checks a manifest from the network before anything is written:
// the network and kind the caller wants, file paths from the allowlist, each
// once, blob IDs and sizes.
func (m *Manifest) validate(kind, genesisID, genesisHash string) error {
	if m.Format != manifestFormat {
		return fmt.Errorf("manifest format %d, this ttsnap reads %d", m.Format, manifestFormat)
	}
	if m.Kind != kind {
		return fmt.Errorf("the manifest is of a %s snapshot, not %s", m.Kind, kind)
	}
	if m.GenesisID != genesisID || m.GenesisHash != genesisHash {
		return fmt.Errorf("the snapshot is of network %s (%s), the data directory's is %s (%s)", m.GenesisID, m.GenesisHash, genesisID, genesisHash)
	}
	if _, err := m.statement(); err != nil {
		return err
	}
	if m.History.First != 0 || m.History.BoxIndexFrom != 0 {
		return fmt.Errorf("the snapshot's history starts at round %d with its box index from round %d; only a full history is installed", m.History.First, m.History.BoxIndexFrom)
	}
	seen := map[string]bool{}
	var history, tracker, blocks bool
	for _, f := range m.Files {
		if !allowedPath(kind, f.Path) {
			return fmt.Errorf("the manifest lists a file that a %s snapshot can't hold: %q", kind, f.Path)
		}
		if seen[f.Path] {
			return fmt.Errorf("the manifest lists %s twice", f.Path)
		}
		seen[f.Path] = true
		if !sha256Hex.MatchString(f.SHA256) {
			return fmt.Errorf("bad SHA-256 %q for %s", f.SHA256, f.Path)
		}
		if f.Size < 0 || f.Size > maxFileSize {
			return fmt.Errorf("bad size %d for %s", f.Size, f.Path)
		}
		switch f.Path {
		case trackerFile:
			tracker = true
		case blocksFile:
			blocks = true
		default:
			history = true
		}
	}
	if !history {
		return errors.New("the manifest lists no history files")
	}
	if kind == kindFull && (!tracker || !blocks || m.TrackerRound == nil) {
		return errors.New("a full snapshot needs the tracker and block DBs and the tracker round")
	}
	return nil
}

// manifestName is the file name of a snapshot's manifest. Manifests and
// their signatures never change once published, so that a cache can't pair a
// manifest with another one's signature (manifest.json.sig).
func manifestName(kind string, last basics.Round) string {
	return fmt.Sprintf("%s-%d.json", kind, last)
}

// manifestPath is where a snapshot's manifest is published, relative to the
// snapshot tree.
func manifestPath(genesisID, kind string, last basics.Round) string {
	return path.Join("snapshots", genesisID, manifestName(kind, last))
}

// latestPath is the pointer to the latest manifest of a kind: a text file
// holding its name. It is not signed; the manifest it names is.
func latestPath(genesisID, kind string) string {
	return path.Join("snapshots", genesisID, "latest-"+kind)
}

var manifestFileName = regexp.MustCompile(`^(history|full)-([0-9]+)\.json$`)

// parseLatest reads a latest pointer: the manifest name, of kind.
func parseLatest(b []byte, kind string) (string, error) {
	name := strings.TrimSpace(string(b))
	mm := manifestFileName.FindStringSubmatch(name)
	if mm == nil || mm[1] != kind {
		return "", fmt.Errorf("the latest %s pointer names %q, not a %s manifest", kind, name, kind)
	}
	return name, nil
}

// blobPath is where the file with this SHA-256 is published.
func blobPath(sha string) string {
	return path.Join("blobs", sha[:2], sha)
}

func encodeManifest(m *Manifest) ([]byte, error) {
	b, err := json.MarshalIndent(m, "", "  ")
	return append(b, '\n'), err
}

func decodeManifest(b []byte) (*Manifest, error) {
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decoding the manifest: %w", err)
	}
	return &m, nil
}
