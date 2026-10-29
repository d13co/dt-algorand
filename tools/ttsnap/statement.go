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
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"path"
	"strings"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
)

// statementPrefix starts a statement, 30 bytes.
const statementPrefix = "dtalgorand-ttsnap/statement/1\n"

// statementLen is the length of an encoded statement.
const statementLen = len(statementPrefix) + 32 + 8 + 32 + 32

// Statement is what a producer or an attester signs about a network's
// history up to a round: the header hash of the round, and the commitment of
// the history (timetravel.Store.Commitment). It depends on the chain alone,
// so every node with a correct history makes the same one, whatever the
// files of its store. Encoded, it is 134 bytes:
//
//	"dtalgorand-ttsnap/statement/1\n" (30 bytes)
//	genesis hash (32)
//	round (8, big endian)
//	header hash of the round (32)
//	commitment (32)
//
// Its signatures are deterministic Falcon-1024 signatures of these bytes, as
// the AVM's falcon_verify checks them.
type Statement struct {
	GenesisHash crypto.Digest
	Round       basics.Round
	HeaderHash  crypto.Digest
	Commitment  [32]byte
}

// Bytes encodes the statement.
func (s Statement) Bytes() []byte {
	b := make([]byte, 0, statementLen)
	b = append(b, statementPrefix...)
	b = append(b, s.GenesisHash[:]...)
	b = binary.BigEndian.AppendUint64(b, uint64(s.Round))
	b = append(b, s.HeaderHash[:]...)
	return append(b, s.Commitment[:]...)
}

func (s Statement) String() string {
	return fmt.Sprintf("round %d of %s: header %s, commitment %s", s.Round, s.GenesisHash, bookkeeping.BlockHash(s.HeaderHash), hex.EncodeToString(s.Commitment[:]))
}

// parseBlockHash parses a block hash as BlockHash.String writes it.
func parseBlockHash(s string) (crypto.Digest, error) {
	rest, ok := strings.CutPrefix(s, "blk-")
	if !ok {
		return crypto.Digest{}, fmt.Errorf("%q is not a block hash", s)
	}
	return crypto.DigestFromString(rest)
}

// statementDir is where the signatures of the statements about a round are
// published, one file per key: <key id>.sig. statement.hex holds the encoded
// statement.
func statementDir(genesisID string, rnd basics.Round) string {
	return path.Join("snapshots", genesisID, "statements", fmt.Sprint(rnd))
}

func statementSigPath(genesisID string, rnd basics.Round, id string) string {
	return path.Join(statementDir(genesisID, rnd), id+".sig")
}

// statement returns the statement a manifest makes.
func (m *Manifest) statement() (Statement, error) {
	var s Statement
	var err error
	if s.GenesisHash, err = crypto.DigestFromString(m.GenesisHash); err != nil {
		return s, fmt.Errorf("the manifest's genesis hash: %w", err)
	}
	if s.HeaderHash, err = parseBlockHash(m.History.LastHash); err != nil {
		return s, fmt.Errorf("the manifest's last header hash: %w", err)
	}
	if m.Commitment == nil {
		return s, fmt.Errorf("the manifest has no commitment")
	}
	b, err := hex.DecodeString(m.Commitment.Digest)
	if err != nil || len(b) != len(s.Commitment) {
		return s, fmt.Errorf("the manifest's commitment %q is not 32 bytes of hex", m.Commitment.Digest)
	}
	copy(s.Commitment[:], b)
	s.Round = m.History.Last
	return s, nil
}
