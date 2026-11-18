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
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

var (
	testFeeSink = basics.Address{1}
	testPool    = basics.Address{2}
	testAcct    = basics.Address{3}
)

// paysetRound is the round of the fixture chain with a transaction.
const paysetRound = 3

type fixture struct {
	dataDir  string
	stateDir string
	genesis  bookkeeping.Genesis
	blocks   []bookkeeping.Block
}

func testGenesis() bookkeeping.Genesis {
	alloc := func(a basics.Address, st basics.Status) bookkeeping.GenesisAllocation {
		return bookkeeping.GenesisAllocation{Address: a.String(), State: bookkeeping.GenesisAccountData{Status: st, MicroAlgos: basics.MicroAlgos{Raw: 1_000_000_000_000}}}
	}
	return bookkeeping.Genesis{
		SchemaID:    "v1",
		Network:     "certstriptest",
		Proto:       protocol.ConsensusCurrentVersion,
		Allocation:  []bookkeeping.GenesisAllocation{alloc(testFeeSink, basics.NotParticipating), alloc(testPool, basics.NotParticipating), alloc(testAcct, basics.Offline)},
		FeeSink:     testFeeSink.String(),
		RewardsPool: testPool.String(),
	}
}

// newDataDir writes genesis.json into a new data directory.
func newDataDir(t *testing.T, g bookkeeping.Genesis) string {
	dataDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "genesis.json"), protocol.EncodeJSON(&g), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(dataDir, g.ID()), 0o700))
	return dataDir
}

// newFixture writes a data directory whose block DB holds a valid chain of rounds 0..last, with a transaction in
// paysetRound and fake certificates.
func newFixture(t *testing.T, last basics.Round) *fixture {
	g := testGenesis()
	f := &fixture{dataDir: newDataDir(t, g), stateDir: t.TempDir(), genesis: g}
	gblk, err := g.Block()
	require.NoError(t, err)
	f.blocks = []bookkeeping.Block{gblk}
	for r := basics.Round(1); r <= last; r++ {
		blk := bookkeeping.MakeBlock(f.blocks[r-1].BlockHeader)
		if r == paysetRound {
			stxn := transactions.SignedTxn{Txn: transactions.Transaction{
				Type: protocol.PaymentTx,
				Header: transactions.Header{
					Sender: testAcct, Fee: basics.MicroAlgos{Raw: 1000}, FirstValid: 1, LastValid: 100, GenesisHash: g.Hash(),
				},
				PaymentTxnFields: transactions.PaymentTxnFields{Receiver: testPool, Amount: basics.MicroAlgos{Raw: 5}},
			}}
			stib, err := blk.EncodeSignedTxn(stxn, transactions.ApplyData{SenderRewards: basics.MicroAlgos{Raw: 7}})
			require.NoError(t, err)
			blk.Payset = transactions.Payset{stib}
			blk.TxnCommitments, err = blk.PaysetCommit()
			require.NoError(t, err)
		}
		f.blocks = append(f.blocks, blk)
	}

	acc, err := db.MakeAccessor(f.blockDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	err = acc.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		if err0 := blockdb.BlockInit(tx, f.blocks[:1]); err0 != nil {
			return err0
		}
		for _, blk := range f.blocks[1:] {
			if err0 := blockdb.BlockPut(tx, blk, agreement.Certificate{Round: blk.Round()}); err0 != nil {
				return err0
			}
		}
		return nil
	})
	require.NoError(t, err)
	return f
}

func (f *fixture) blockDB() string {
	return filepath.Join(f.dataDir, f.genesis.ID(), blockDBFile)
}

func (f *fixture) node(t *testing.T) *node {
	n, err := openNode(f.dataDir, f.stateDir)
	require.NoError(t, err)
	return n
}

// exec runs a statement on the fixture's block DB.
func (f *fixture) exec(t *testing.T, q string, args ...any) {
	acc, err := db.MakeAccessor(f.blockDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	_, err = acc.Handle.Exec(q, args...)
	require.NoError(t, err)
}

// add appends rounds to the fixture's chain and block DB, as algod does while catching up.
func (f *fixture) add(t *testing.T, n int) {
	acc, err := db.MakeAccessor(f.blockDB(), false, false)
	require.NoError(t, err)
	defer acc.Close()
	err = acc.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		for range n {
			blk := bookkeeping.MakeBlock(f.blocks[len(f.blocks)-1].BlockHeader)
			f.blocks = append(f.blocks, blk)
			if err0 := blockdb.BlockPut(tx, blk, agreement.Certificate{Round: blk.Round()}); err0 != nil {
				return err0
			}
		}
		return nil
	})
	require.NoError(t, err)
}

func runVerify(t *testing.T, n *node, to basics.Round) error {
	return verifyChain(context.Background(), n, to, 2, false)
}

func TestVerifyChain(t *testing.T) {
	partitiontest.PartitionTest(t)

	f := newFixture(t, 300)
	n := f.node(t)

	// a walk stopped early resumes where it stopped
	require.NoError(t, runVerify(t, n, 100))
	st, err := n.readChainState()
	require.NoError(t, err)
	require.Equal(t, basics.Round(100), st.Round)
	require.Equal(t, f.blocks[100].Hash().String(), st.Hash)
	require.False(t, st.Full)

	require.NoError(t, runVerify(t, n, 0))
	st, err = n.readChainState()
	require.NoError(t, err)
	require.Equal(t, basics.Round(300), st.Round)
	require.Equal(t, f.blocks[300].Hash().String(), st.Hash)

	// nothing left to do
	require.NoError(t, runVerify(t, n, 0))

	// a block DB changed under the state file is refused
	f.exec(t, "UPDATE blocks SET hdrdata=? WHERE rnd=300", protocol.Encode(&f.blocks[299].BlockHeader))
	require.ErrorContains(t, runVerify(t, n, 0), "the block DB changed")
}

// reencode stores blk's header and block for its round.
func (f *fixture) reencode(t *testing.T, blk bookkeeping.Block) {
	f.exec(t, "UPDATE blocks SET hdrdata=?, blkdata=? WHERE rnd=?", protocol.Encode(&blk.BlockHeader), protocol.Encode(&blk), blk.Round())
}

func TestVerifyChainTampered(t *testing.T) {
	partitiontest.PartitionTest(t)

	for _, tc := range []struct {
		name   string
		tamper func(t *testing.T, f *fixture)
		round  basics.Round
		msg    string
	}{
		{name: "header column only", round: 4, msg: "header column", tamper: func(t *testing.T, f *fixture) {
			hdr := f.blocks[4].BlockHeader
			hdr.TimeStamp++
			f.exec(t, "UPDATE blocks SET hdrdata=? WHERE rnd=4", protocol.Encode(&hdr))
		}},
		{name: "trailing bytes", round: 4, tamper: func(t *testing.T, f *fixture) {
			f.exec(t, "UPDATE blocks SET blkdata=? WHERE rnd=4", append(protocol.Encode(&f.blocks[4]), 0xc0))
		}},
		{name: "non-canonical encoding", round: 4, msg: "not canonically encoded", tamper: func(t *testing.T, f *fixture) {
			enc := protocol.Encode(&f.blocks[4])
			// the round as a uint8 instead of a positive fixint: decodes the same, encodes differently
			old := []byte("\xa3rnd\x04")
			require.Equal(t, 1, bytes.Count(enc, old))
			f.exec(t, "UPDATE blocks SET blkdata=? WHERE rnd=4", bytes.Replace(enc, old, []byte("\xa3rnd\xcc\x04"), 1))
		}},
		{name: "gap", round: 5, msg: "missing", tamper: func(t *testing.T, f *fixture) {
			f.exec(t, "DELETE FROM blocks WHERE rnd=5")
		}},
		{name: "broken branch", round: 6, msg: "does not follow", tamper: func(t *testing.T, f *fixture) {
			blk := f.blocks[6]
			blk.Branch = bookkeeping.BlockHash{9}
			f.reencode(t, blk)
		}},
		{name: "payset", round: paysetRound, msg: "payset", tamper: func(t *testing.T, f *fixture) {
			blk := f.blocks[paysetRound]
			stxn, ad, err := blk.DecodeSignedTxn(blk.Payset[0])
			require.NoError(t, err)
			stxn.Txn.Amount.Raw++
			blk.Payset[0], err = blk.EncodeSignedTxn(stxn, ad)
			require.NoError(t, err)
			f.reencode(t, blk)
		}},
		{name: "apply data", round: paysetRound, msg: "payset", tamper: func(t *testing.T, f *fixture) {
			blk := f.blocks[paysetRound]
			stxn, ad, err := blk.DecodeSignedTxn(blk.Payset[0])
			require.NoError(t, err)
			ad.ClosingAmount.Raw = 1
			blk.Payset[0], err = blk.EncodeSignedTxn(stxn, ad)
			require.NoError(t, err)
			f.reencode(t, blk)
		}},
		{name: "proto column", round: 7, msg: "proto column", tamper: func(t *testing.T, f *fixture) {
			f.exec(t, "UPDATE blocks SET proto='other' WHERE rnd=7")
		}},
		{name: "wrong genesis", round: 0, msg: "genesis", tamper: func(t *testing.T, f *fixture) {
			g := f.genesis
			g.Allocation[2].State.MicroAlgos.Raw++
			require.NoError(t, os.WriteFile(filepath.Join(f.dataDir, "genesis.json"), protocol.EncodeJSON(&g), 0o600))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, 10)
			tc.tamper(t, f)
			err := runVerify(t, f.node(t), 0)
			require.Error(t, err)
			require.ErrorContains(t, err, fmt.Sprintf("round %d: ", tc.round))
			if tc.msg != "" {
				require.ErrorContains(t, err, tc.msg)
			}
			// the state file stops before the bad round
			st, err := f.node(t).readChainState()
			require.NoError(t, err)
			if tc.round == 0 {
				require.Nil(t, st)
			} else {
				require.Equal(t, tc.round-1, st.Round)
			}
		})
	}
}
