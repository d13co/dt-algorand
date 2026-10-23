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

package ledger

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2"
	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/crypto/merklesignature"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// ttCatchupSource is a time travel ledger with a catchpoint of its tracker
// DB at balances round d, blocks up to past the catchpoint round x, and two
// copies of its history: one that ends before d and one that reaches past x.
type ttCatchupSource struct {
	dl       DoubleLedger
	balances bookkeeping.GenesisBalances
	addrs    []basics.Address
	app      basics.AppIndex
	cpFile   string
	d, x     basics.Round
	early    string
	full     string
}

func ttCatchupConfig() config.Local {
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = 4
	return cfg
}

func newTTCatchupSource(t *testing.T) *ttCatchupSource {
	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, ttCatchupConfig())
	s := &ttCatchupSource{dl: dl, balances: genBalances, addrs: addrs}
	src := dl.validator
	s.app = dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: s.app.Address(), Amount: 10_000_000})
	rounds := func(n int) {
		for range n {
			r := src.Latest() + 1
			dl.beginBlock()
			dl.txns(
				&txntest.Txn{Type: "pay", Sender: addrs[3], Receiver: addrs[4], Amount: uint64(r)},
				&txntest.Txn{Type: "appl", Sender: addrs[3], ApplicationID: s.app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))},
					Boxes: []transactions.BoxRef{{Name: []byte("b")}}},
			)
			// accounts go online with keys that expire 20 to 420 rounds
			// later, and offline: the history records expired stake, which
			// the catching up node computes from the catchpoint's online
			// accounts and compares
			switch r % 10 {
			case 0:
				a := addrs[5+int(r/10)%4]
				dl.txn(&txntest.Txn{Type: "keyreg", Sender: a, VoteFirst: r, VoteLast: r + 20 + basics.Round(uint64(r)*37%400),
					VotePK: crypto.OneTimeSignatureVerifier(a), SelectionPK: crypto.VRFVerifier(a), StateProofPK: merklesignature.Commitment{1}, VoteKeyDilution: 1024})
			case 5:
				dl.txn(&txntest.Txn{Type: "keyreg", Sender: addrs[5+int(r/10+2)%4]})
			}
			dl.endBlock()
		}
	}
	checkpoint := func() string {
		testCatchpointFlushRound(src)
		dir := filepath.Join(t.TempDir(), "history")
		require.NoError(t, src.history.store.Checkpoint(dir))
		return dir
	}

	rounds(40)
	s.early = checkpoint()
	rounds(60)

	tmp := t.TempDir()
	s.cpFile = filepath.Join(tmp, "catchpoint.tar.gz")
	testCatchpointFlushRound(src)
	cph := testWriteCatchpoint(t, config.Consensus[protocol.ConsensusFuture], src.trackerDB(), filepath.Join(tmp, "catchpoint.data"), s.cpFile, 0, 0)
	s.d = cph.BalancesRound
	s.x = s.d + catchpointLookback(protocol.ConsensusFuture)
	early, ok := ttStoreLast(t, s.early)
	require.True(t, ok)
	require.Less(t, early, s.d)

	rounds(int(s.x-src.Latest()) + 20)
	s.full = checkpoint()
	full, _ := ttStoreLast(t, s.full)
	require.Greater(t, full, s.x)
	return s
}

func ttStoreLast(t *testing.T, dir string) (basics.Round, bool) {
	st, err := timetravel.Open(dir, timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer st.Close()
	last, ok := st.Last()
	return basics.Round(last), ok
}

// open opens a ledger of the source's network at dbName, on disk.
func (s *ttCatchupSource) open(t *testing.T, dbName string) *Ledger {
	l, err := s.openErr(t, dbName)
	require.NoError(t, err)
	return l
}

func (s *ttCatchupSource) openErr(t *testing.T, dbName string) (*Ledger, error) {
	genHash := s.dl.validator.GenesisHash()
	genBlock, err := bookkeeping.MakeGenesisBlock(protocol.ConsensusFuture, s.balances, "test", genHash)
	require.NoError(t, err)
	return OpenLedger(logging.TestingLog(t), dbName, false, ledgercore.InitState{Block: genBlock, Accounts: s.balances.Balances, GenesisHash: genHash}, ttCatchupConfig())
}

// restore installs a copy of history (none if empty), lets tamper change it,
// opens a new ledger and catches it up from the source's catchpoint as the
// catchpoint service would, and returns the ledger, its path and the error of
// CompleteCatchup, or a nil ledger and the error of opening it.
func (s *ttCatchupSource) restore(t *testing.T, history string, tamper func(*timetravel.Store)) (*Ledger, string, error) {
	dbName := filepath.Join(t.TempDir(), "node")
	if history != "" {
		ttPath := TimeTravelPath(DirsAndPrefix{ResolvedGenesisDirs: config.ResolvedGenesisDirs{TimeTravelGenesisDir: dbName}})
		require.NoError(t, os.CopyFS(ttPath, os.DirFS(history)))
		if tamper != nil {
			st, err := timetravel.Open(ttPath, timetravel.Options{})
			require.NoError(t, err)
			tamper(st)
			require.NoError(t, st.Close())
		}
	}
	l, err := s.openErr(t, dbName)
	if err != nil {
		return nil, dbName, err
	}
	src := s.dl.validator
	ctx := context.Background()
	acc := MakeCatchpointCatchupAccessor(l, l.log)
	require.NoError(t, acc.ResetStagingBalances(ctx, true))
	var progress CatchpointCatchupAccessorProgress
	for _, d := range readCatchpointFile(t, s.cpFile) {
		require.NoError(t, acc.ProcessStagingBalances(ctx, d.headerName, d.data, &progress))
	}
	require.NoError(t, acc.BuildMerkleTrie(ctx, nil))
	blk, err := src.Block(s.x)
	require.NoError(t, err)
	require.NoError(t, acc.StoreBalancesRound(ctx, &blk))
	require.NoError(t, acc.StoreFirstBlock(ctx, &blk, &agreement.Certificate{}))
	for r := s.x - 1; r >= 1 && r+1321 >= s.x; r-- {
		blk, err := src.Block(r)
		require.NoError(t, err)
		require.NoError(t, acc.StoreBlock(ctx, &blk, &agreement.Certificate{}))
	}
	return l, dbName, acc.CompleteCatchup(ctx)
}

// sameHistory compares the two ledgers' answers at old rounds.
func (s *ttCatchupSource) sameHistory(t *testing.T, l *Ledger) {
	src := s.dl.validator
	box := apps.MakeBoxKey(uint64(s.app), "b")
	for _, r := range []basics.Round{1, 20, s.d - 1, s.d, s.d + 1, s.x - 1, s.x, s.x + 5, l.Latest() - 1} {
		for _, addr := range s.addrs[:5] {
			want, _, err := src.LookupWithoutRewards(r, addr)
			require.NoError(t, err)
			got, _, err := l.LookupWithoutRewards(r, addr)
			require.NoError(t, err, "round %d", r)
			require.Equal(t, want, got, "round %d", r)
		}
		want, err := src.Totals(r)
		require.NoError(t, err)
		got, err := l.Totals(r)
		require.NoError(t, err)
		require.Equal(t, want, got, "round %d", r)
		wantHdr, err := src.BlockHdr(r)
		require.NoError(t, err)
		gotHdr, err := l.BlockHdr(r)
		require.NoError(t, err)
		require.Equal(t, wantHdr, gotHdr, "round %d", r)
		wantBox, err := src.LookupKv(r, box)
		require.NoError(t, err)
		gotBox, err := l.LookupKv(r, box)
		require.NoError(t, err)
		require.Equal(t, wantBox, gotBox, "round %d", r)
	}
}

// TestTimeTravelCatchpointCatchup catches a time travel ledger up from a
// catchpoint with a history installed beforehand, and checks the ways a
// history that does not fit is refused.
func TestTimeTravelCatchpointCatchup(t *testing.T) {
	partitiontest.PartitionTest(t)
	// not parallel: the ledgers are large enough

	s := newTTCatchupSource(t)
	defer s.dl.Close()
	src := s.dl.validator

	t.Run("history", func(t *testing.T) {
		l, _, err := s.restore(t, s.full, nil)
		defer l.Close()
		require.NoError(t, err)
		require.Equal(t, s.x, l.Latest())
		require.NoError(t, l.CheckHistoryForCatchpoint(s.x))
		// the installed history reaches past the ledger, which must not
		// serve those rounds before it has them
		_, err = l.BlockHdr(s.x + 1)
		require.Error(t, err, "a round after the ledger's latest")
		require.Nil(t, l.historyAt(s.x+1))
		for r := s.x + 1; r <= src.Latest(); r++ {
			blk, err := src.Block(r)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		testCatchpointFlushRound(l)
		require.NoError(t, l.HistoryUntrusted())
		s.sameHistory(t, l)
		expiring := 0
		for r := s.d + 1; r <= s.x; r++ {
			e, ok, err := l.history.store.ExpiredStake(uint64(r))
			require.NoError(t, err)
			require.True(t, ok, "round %d", r)
			if e > 0 {
				expiring++
			}
		}
		require.Greater(t, expiring, 100, "the replayed rounds must compare nonzero expired stake")
	})

	t.Run("none", func(t *testing.T) {
		l, _, err := s.restore(t, "", nil)
		defer l.Close()
		require.ErrorIs(t, err, ErrHistoryTooShort)
		require.ErrorIs(t, l.CheckHistoryForCatchpoint(s.x), ErrHistoryTooShort)
		require.Zero(t, l.Latest(), "nothing was swapped")
	})

	t.Run("short", func(t *testing.T) {
		l, _, err := s.restore(t, s.early, nil)
		defer l.Close()
		require.ErrorIs(t, err, ErrHistoryTooShort)
		require.ErrorIs(t, l.CheckHistoryForCatchpoint(s.x), ErrHistoryTooShort)
		require.Zero(t, l.Latest())
	})

	t.Run("header", func(t *testing.T) {
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			key := binary.BigEndian.AppendUint64([]byte{'H'}, uint64(s.d))
			rec, closer, err := st.DB().Get(key)
			require.NoError(t, err)
			rec = append([]byte(nil), rec...)
			closer.Close()
			rec[len(rec)-1]++
			require.NoError(t, st.DB().Set(key, rec, pebble.Sync))
		})
		defer l.Close()
		require.ErrorContains(t, err, "another header")
		require.Zero(t, l.Latest())
	})

	t.Run("totals", func(t *testing.T) {
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			key := binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.TotalsKey...), uint64(s.d))
			_, closer, err := st.DB().Get(key)
			require.NoError(t, err, "every round writes the totals")
			closer.Close()
			var totals ledgercore.AccountTotals
			totals.RewardsLevel = 12345
			require.NoError(t, st.DB().Set(key, append([]byte{'F'}, protocol.Encode(&totals)...), pebble.Sync))
		})
		defer l.Close()
		require.ErrorContains(t, err, "other totals")
		require.Zero(t, l.Latest())
	})

	t.Run("genesis", func(t *testing.T) {
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			key := binary.BigEndian.AppendUint64([]byte{'H'}, 0)
			rec, closer, err := st.DB().Get(key)
			require.NoError(t, err)
			rec = append([]byte(nil), rec...)
			closer.Close()
			rec[len(rec)-1]++
			require.NoError(t, st.DB().Set(key, rec, pebble.Sync))
		})
		if l != nil {
			defer l.Close()
		}
		require.ErrorContains(t, err, "another network's genesis block")
	})

	t.Run("value", func(t *testing.T) {
		// a value after the balances round, under its round's digest: the
		// catchup completes, and the replay finds it
		r := s.d + 10
		l, dbName, err := s.restore(t, s.full, func(st *timetravel.Store) {
			key := binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.AccountKey(s.addrs[4])...), uint64(r))
			_, closer, err := st.DB().Get(key)
			require.NoError(t, err, "the receiver changes every round")
			closer.Close()
			require.NoError(t, st.DB().Set(key, []byte("Fforged"), pebble.Sync))
		})
		require.NoError(t, err)
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted)
		_, _, err = l.LookupWithoutRewards(1, s.addrs[4])
		require.Error(t, err, "an untrusted history is not served")
		l.Close()

		l = s.open(t, dbName)
		defer l.Close()
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted, "the mark survives a restart")
	})
}
