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
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/protocol"
)

// chainState is how far verify-chain got: rounds 0..Round passed, and Round's header hashes to Hash.
type chainState struct {
	Network     string       `json:"network"`
	GenesisHash string       `json:"genesis-hash"`
	Round       basics.Round `json:"round"`
	Hash        string       `json:"hash"`
	// Full is set when the rounds were checked by a genesis replay (-full), certificates included.
	Full    bool      `json:"full,omitempty"`
	Updated time.Time `json:"updated"`
	Tool    string    `json:"tool"`
}

// anchorState is a round whose catchpoint label matched the network's, linked to a verify-chain walk.
type anchorState struct {
	Network     string       `json:"network"`
	GenesisHash string       `json:"genesis-hash"`
	Round       basics.Round `json:"round"`
	Label       string       `json:"label"`
	// Sources names where the label it matched came from, as the operator gave them.
	Sources []string `json:"sources"`
	// ChainRound and ChainHash are the verify-chain walk the blocks up to Round were linked to.
	ChainRound basics.Round `json:"chain-round"`
	ChainHash  string       `json:"chain-hash"`
	Updated    time.Time    `json:"updated"`
	Tool       string       `json:"tool"`
}

// resetState records a reset-trie: catchpoint labels the node made for rounds past Round come from the trie algod
// rebuilt from the accounts.
type resetState struct {
	Network     string       `json:"network"`
	GenesisHash string       `json:"genesis-hash"`
	Round       basics.Round `json:"round"`
	Updated     time.Time    `json:"updated"`
	Tool        string       `json:"tool"`
}

func toolVersion() string {
	v := config.GetCurrentVersion()
	return strings.TrimSpace(v.String() + " " + v.CommitHash)
}

// writeJSON writes v to path through a temporary file, so a crash leaves the old file or the new one.
func writeJSON(path string, v any) error {
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err = f.Write(append(buf, '\n')); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// readJSON reads path into v, returning false if it doesn't exist.
func readJSON(path string, v any) (bool, error) {
	buf, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if err = json.Unmarshal(buf, v); err != nil {
		return false, fmt.Errorf("%s: %w", path, err)
	}
	return true, nil
}

func (n *node) readChainState() (*chainState, error) {
	var st chainState
	ok, err := readJSON(n.chainState(), &st)
	if err != nil || !ok {
		return nil, err
	}
	if st.GenesisHash != n.genesis.Hash().String() {
		return nil, fmt.Errorf("%s is for another network (%s)", n.chainState(), st.Network)
	}
	return &st, nil
}

func (n *node) writeChainState(hdr *bookkeeping.BlockHeader, full bool) error {
	return writeJSON(n.chainState(), chainState{
		Network:     n.genesis.ID(),
		GenesisHash: n.genesis.Hash().String(),
		Round:       hdr.Round,
		Hash:        hdr.Hash().String(),
		Full:        full,
		Updated:     time.Now().UTC(),
		Tool:        toolVersion(),
	})
}

func (n *node) readAnchor() (*anchorState, error) {
	var st anchorState
	ok, err := readJSON(n.anchor(), &st)
	if err != nil || !ok {
		return nil, err
	}
	if st.GenesisHash != n.genesis.Hash().String() {
		return nil, fmt.Errorf("%s is for another network (%s)", n.anchor(), st.Network)
	}
	return &st, nil
}

func (n *node) writeReset(rnd basics.Round) error {
	return writeJSON(n.resetState(), resetState{
		Network:     n.genesis.ID(),
		GenesisHash: n.genesis.Hash().String(),
		Round:       rnd,
		Updated:     time.Now().UTC(),
		Tool:        toolVersion(),
	})
}

func (n *node) readReset() (*resetState, error) {
	var st resetState
	ok, err := readJSON(n.resetState(), &st)
	if err != nil || !ok {
		return nil, err
	}
	if st.GenesisHash != n.genesis.Hash().String() {
		return nil, fmt.Errorf("%s is for another network (%s)", n.resetState(), st.Network)
	}
	return &st, nil
}

// readHdr returns the header stored for rnd.
func readHdr(db *sql.DB, rnd basics.Round) (*bookkeeping.BlockHeader, error) {
	var buf []byte
	err := db.QueryRow("SELECT hdrdata FROM blocks WHERE rnd=?", rnd).Scan(&buf)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("round %d is not in the block DB", rnd)
	}
	if err != nil {
		return nil, err
	}
	var hdr bookkeeping.BlockHeader
	if err = protocol.Decode(buf, &hdr); err != nil {
		return nil, fmt.Errorf("round %d: %w", rnd, err)
	}
	return &hdr, nil
}

// resumeHdr returns the header the chain state ends at, after checking that the block DB still has it.
func resumeHdr(db *sql.DB, st *chainState, path string) (*bookkeeping.BlockHeader, error) {
	hdr, err := readHdr(db, st.Round)
	if err != nil {
		return nil, err
	}
	if hdr.Hash().String() != st.Hash {
		return nil, fmt.Errorf("round %d of the block DB hashes to %v, not %s as when %s was written: the block DB changed; delete %s to walk again from genesis",
			st.Round, hdr.Hash(), st.Hash, filepath.Base(path), path)
	}
	return hdr, nil
}

// checkBlocksSchema checks that rnd is the blocks table's integer primary key, so that a round has one row.
func checkBlocksSchema(db *sql.DB) error {
	rows, err := db.Query("SELECT name, type, pk FROM pragma_table_info('blocks')")
	if err != nil {
		return err
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var name, typ string
		var pk int
		if err = rows.Scan(&name, &typ, &pk); err != nil {
			return err
		}
		if name == "rnd" {
			found = true
			if !strings.EqualFold(typ, "integer") || pk != 1 {
				return fmt.Errorf("the blocks table's rnd column is %q (primary key position %d), not the integer primary key", typ, pk)
			}
		} else if pk != 0 {
			return fmt.Errorf("the blocks table's primary key includes %s", name)
		}
	}
	if err = rows.Err(); err != nil {
		return err
	}
	if !found {
		return errors.New("the blocks table has no rnd column")
	}
	return nil
}

// blockRange returns the lowest and highest rounds of the block DB.
func blockRange(db *sql.DB) (lo, hi basics.Round, err error) {
	var l, h sql.NullInt64
	if err = db.QueryRow("SELECT MIN(rnd), MAX(rnd) FROM blocks").Scan(&l, &h); err != nil {
		return 0, 0, err
	}
	if !l.Valid {
		return 0, 0, errors.New("the block DB has no blocks")
	}
	return basics.Round(l.Int64), basics.Round(h.Int64), nil
}

// genesisHdr returns the encoding of the header of the genesis block the genesis file makes.
func (n *node) genesisHdr() ([]byte, error) {
	blk, err := n.genesis.Block()
	if err != nil {
		return nil, err
	}
	return protocol.Encode(&blk.BlockHeader), nil
}
