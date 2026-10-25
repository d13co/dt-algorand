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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

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
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
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
	dbName := s.install(t, history, tamper)
	l, err := s.openErr(t, dbName)
	if err != nil {
		return nil, dbName, err
	}
	return l, dbName, s.stage(t, l).CompleteCatchup(context.Background())
}

// stage stages the source's catchpoint in l, as the catchpoint service would
// before it completes the catchup.
func (s *ttCatchupSource) stage(t *testing.T, l *Ledger) CatchpointCatchupAccessor {
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
	return acc
}

// install returns the path of a new ledger with a copy of history installed
// (none if empty), which tamper changes.
func (s *ttCatchupSource) install(t *testing.T, history string, tamper func(*timetravel.Store)) string {
	dbName := filepath.Join(t.TempDir(), "node")
	if history != "" {
		ttPath := TimeTravelPath(DirsAndPrefix{ResolvedGenesisDirs: config.ResolvedGenesisDirs{TimeTravelGenesisDir: dbName}})
		require.NoError(t, os.CopyFS(ttPath, os.DirFS(history)))
		st, err := timetravel.Open(ttPath, timetravel.Options{})
		require.NoError(t, err)
		last, ok := st.Last()
		require.True(t, ok)
		// as ttsnap fetch does
		require.NoError(t, st.SetInstalled("history.json", last))
		if tamper != nil {
			tamper(st)
		}
		require.NoError(t, st.Close())
	}
	return dbName
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
		require.ErrorContains(t, l.CheckHistoryForCatchpoint(s.x), "past the catchpoint's balances round", "the ledger is past it now")
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
		require.Greater(t, expiring, 100, "the ledger wrote the rounds after d with their expired stake")
		last, ok := l.history.store.Last()
		require.True(t, ok)
		require.Greater(t, last, uint64(s.x), "the ledger wrote the rounds it replayed and added")
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

	t.Run("forged", func(t *testing.T) {
		// records after the balances round that no replay would compare: an
		// account its round left alone, and one that never existed. The
		// catchup deletes every round after d, and the ledger writes them
		// again from its own evaluation.
		r := s.d + 10
		ghost := basics.Address{0xee, 1}
		forged := trackerdb.BaseAccountData{MicroAlgos: basics.MicroAlgos{Raw: 1_000_000_000_000}}
		keys := [][]byte{
			binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.AccountKey(s.addrs[1])...), uint64(r)),
			binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.AccountKey(ghost)...), uint64(r)),
		}
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			for _, key := range keys {
				_, _, err := st.DB().Get(key)
				require.ErrorIs(t, err, pebble.ErrNotFound, "no round after d changes addrs[1]")
				require.NoError(t, st.DB().Set(key, append([]byte{'F'}, protocol.Encode(&forged)...), pebble.Sync))
			}
		})
		defer l.Close()
		require.NoError(t, err)
		require.NoError(t, l.HistoryUntrusted())
		for r := s.x + 1; r <= src.Latest(); r++ {
			blk, err := src.Block(r)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		testCatchpointFlushRound(l)
		for _, key := range keys {
			_, _, err := l.history.store.DB().Get(key)
			require.ErrorIs(t, err, pebble.ErrNotFound)
		}
		for _, addr := range []basics.Address{s.addrs[1], ghost} {
			want, _, err := src.LookupWithoutRewards(r, addr)
			require.NoError(t, err)
			got, _, err := l.LookupWithoutRewards(r, addr)
			require.NoError(t, err)
			require.Equal(t, want, got, "%v at round %d", addr, r)
		}
		got, _, err := l.LookupWithoutRewards(r, ghost)
		require.NoError(t, err)
		require.Zero(t, got.MicroAlgos.Raw, "the account no round created")
		s.sameHistory(t, l)
	})

	t.Run("sync", func(t *testing.T) {
		// an installed history that the ledger syncs from genesis, without a
		// catchpoint: each round is compared as it is replayed, and a forged
		// value marks the store diverged, durably
		r := basics.Round(10)
		dbName := s.install(t, s.full, func(st *timetravel.Store) {
			key := binary.BigEndian.AppendUint64(append([]byte{'c'}, timetravel.AccountKey(s.addrs[4])...), uint64(r))
			_, closer, err := st.DB().Get(key)
			require.NoError(t, err, "the receiver changes every round")
			closer.Close()
			require.NoError(t, st.DB().Set(key, []byte("Fforged"), pebble.Sync))
		})
		l := s.open(t, dbName)
		require.Positive(t, l.history.foreignThrough, "a store past the blocks is not the node's")
		for rnd := basics.Round(1); rnd <= r+5; rnd++ {
			blk, err := src.Block(rnd)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		_, found, err := l.history.store.Diverged()
		require.NoError(t, err)
		require.True(t, found, "a forged round of a snapshot marks it")
		l.Close()

		l = s.open(t, dbName)
		defer l.Close()
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted, "the mark survives a restart")
		require.ErrorIs(t, l.CheckHistoryForCatchpoint(s.x), ErrHistoryUntrusted)
	})

	t.Run("past", func(t *testing.T) {
		// a ledger past the catchpoint's balances round: the catchup would
		// delete history it serves, refused when it starts and completes
		dbName := s.install(t, s.full, nil)
		l := s.open(t, dbName)
		defer l.Close()
		for rnd := basics.Round(1); rnd <= s.d+1; rnd++ {
			blk, err := src.Block(rnd)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		require.NoError(t, l.HistoryUntrusted(), "the installed rounds the replay checked")
		require.ErrorContains(t, l.CheckHistoryForCatchpoint(s.x), "past the catchpoint's balances round")
		require.ErrorContains(t, s.stage(t, l).CompleteCatchup(context.Background()), "past the catchpoint's balances round")
	})

	t.Run("bad installed round", func(t *testing.T) {
		// a record that can't be read: the whole history taken as installed
		dbName := s.install(t, s.full, func(st *timetravel.Store) {
			require.NoError(t, st.DB().Set([]byte("mx"), []byte("bad"), pebble.Sync))
		})
		l := s.open(t, dbName)
		last, ok := l.history.store.Last()
		require.True(t, ok)
		// synced past the installed rounds, so the store no longer reaches
		// past the block DB
		for rnd := basics.Round(1); rnd <= basics.Round(last)+1; rnd++ {
			blk, err := src.Block(rnd)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		l.WaitForCommit(basics.Round(last) + 1)
		l.Close()
		l = s.open(t, dbName)
		defer l.Close()
		require.Greater(t, l.Latest(), basics.Round(last))
		require.EqualValues(t, last, l.history.foreignThrough)
	})

	t.Run("interrupted", func(t *testing.T) {
		// a truncation cut short (a shutdown) resumes when the store opens,
		// and the catchup completes
		dbName := s.install(t, s.full, nil)
		l := s.open(t, dbName)
		acc := s.stage(t, l)
		ctx, cancel := context.WithCancel(context.Background())
		beforeHistoryTruncate = cancel
		defer func() { beforeHistoryTruncate = nil }()
		require.ErrorIs(t, acc.PrepareCatchpointHistory(ctx), ErrHistoryTruncationInterrupted)
		beforeHistoryTruncate = nil
		_, ok := l.history.store.Last()
		require.True(t, ok)
		require.ErrorContains(t, l.history.store.AddRound(1, nil, nil, nil), "unfinished", "the store waits for the truncation")
		l.Close()

		l = s.open(t, dbName)
		defer l.Close()
		last, ok := l.history.store.Last()
		require.True(t, ok)
		require.Positive(t, l.history.foreignThrough, "still past the ledger's blocks: not the node's yet")
		require.EqualValues(t, s.d, last, "opening the store finished the truncation")
		acc = MakeCatchpointCatchupAccessor(l, l.log)
		require.NoError(t, acc.CompleteCatchup(context.Background()), "the staged catchpoint completes")
		require.Equal(t, s.x, l.Latest())
		require.EqualValues(t, s.d, l.history.foreignThrough, "the truncation lowered the installed rounds to the balances round")
		require.NoError(t, l.HistoryUntrusted())
		for r := s.x + 1; r <= src.Latest(); r++ {
			blk, err := src.Block(r)
			require.NoError(t, err)
			require.NoError(t, l.AddBlock(blk, agreement.Certificate{}))
		}
		testCatchpointFlushRound(l)
		s.sameHistory(t, l)
	})

	t.Run("shadow", func(t *testing.T) {
		// a record key whose object part is no object's key but starts with
		// one: reads of that object would take it for a version of any round
		// at or after its middle bytes. Refused, and marked diverged.
		ghost := basics.Address{0xee, 2}
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			obj := binary.BigEndian.AppendUint64(timetravel.AccountKey(ghost), uint64(s.d+10))
			forged := trackerdb.BaseAccountData{MicroAlgos: basics.MicroAlgos{Raw: 1_000_000_000_000}}
			require.NoError(t, st.DB().Set(binary.BigEndian.AppendUint64(append([]byte{'c'}, obj...), 1), append([]byte{'F'}, protocol.Encode(&forged)...), pebble.Sync))
		})
		defer l.Close()
		require.ErrorIs(t, err, timetravel.ErrCorrupt)
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted)
		require.Zero(t, l.Latest())
	})

	t.Run("reload", func(t *testing.T) {
		// what a reload clears does not stop a catchup, and a reload clears it
		l, _, err := s.restore(t, s.full, nil)
		defer l.Close()
		require.NoError(t, err)
		l.history.mu.Lock()
		l.history.untrusted = fmt.Errorf("%w: a test", ErrHistoryUntrusted)
		l.history.mu.Unlock()
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted)
		require.NotErrorIs(t, l.CheckHistoryForCatchpoint(s.x), ErrHistoryUntrusted)
		require.NoError(t, l.reloadLedger())
		require.NoError(t, l.HistoryUntrusted())

		require.NoError(t, l.history.store.MarkDiverged("a test"))
		require.NoError(t, l.reloadLedger())
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted, "a diverged mark holds")
		require.ErrorIs(t, l.CheckHistoryForCatchpoint(s.x), ErrHistoryUntrusted)
	})

	t.Run("diverged", func(t *testing.T) {
		l, _, err := s.restore(t, s.full, func(st *timetravel.Store) {
			require.NoError(t, st.MarkDiverged("a test"))
		})
		defer l.Close()
		require.ErrorIs(t, err, ErrHistoryUntrusted)
		require.Zero(t, l.Latest())
	})

	t.Run("corrupt", func(t *testing.T) {
		// object keys that prefix one another, which no node writes: refused,
		// and marked diverged
		l, dbName, err := s.restore(t, s.full, func(st *timetravel.Store) {
			obj := timetravel.AccountKey(s.addrs[1])
			for _, k := range [][]byte{obj, append(obj, 1)} {
				require.NoError(t, st.DB().Set(binary.BigEndian.AppendUint64(append([]byte{'c'}, k...), 3), []byte("F"), pebble.Sync))
			}
		})
		require.ErrorIs(t, err, timetravel.ErrCorrupt)
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted)
		require.Zero(t, l.Latest())
		l.Close()

		l = s.open(t, dbName)
		defer l.Close()
		require.ErrorIs(t, l.HistoryUntrusted(), ErrHistoryUntrusted, "the mark survives a restart")
	})
}

// TestHistoryVerifyReplayed checks what a replayed round that differs from
// the store does: the node's own round holds the history back until a
// reload, an installed one marks the store diverged. Not parallel: it sets
// verifyStored.
func TestHistoryVerifyReplayed(t *testing.T) {
	partitiontest.PartitionTest(t)

	defer func(d time.Duration) { verifyRetryDelay = d }(verifyRetryDelay)
	verifyRetryDelay = 0
	key := timetravel.KvKey("x")
	setup := func(t *testing.T) (*historyTracker, historyRound, historyRound) {
		st, err := timetravel.Open(t.TempDir(), timetravel.Options{})
		require.NoError(t, err)
		t.Cleanup(func() { st.Close() })
		ch := []timetravel.Change{{Key: key, Value: []byte("v")}}
		for r := range 4 {
			require.NoError(t, st.AddRound(uint64(r), ch, []byte("h"), nil))
		}
		ht := &historyTracker{store: st, path: "test", log: logging.TestingLog(t)}
		same := historyRound{rnd: 2, changes: ch, hdr: []byte("h")}
		forged := same
		forged.changes = []timetravel.Change{{Key: key, Value: []byte("w")}}
		return ht, same, forged
	}
	diverged := func(t *testing.T, ht *historyTracker) bool {
		_, found, err := ht.store.Diverged()
		require.NoError(t, err)
		return found
	}

	t.Run("same", func(t *testing.T) {
		ht, same, _ := setup(t)
		ht.verifyReplayed(same)
		require.NoError(t, ht.untrusted)
	})

	t.Run("own", func(t *testing.T) {
		// the node's own round: the history from it on is deleted, and the
		// ledger writes it again from its evaluation
		ht, _, forged := setup(t)
		ht.verifyReplayed(forged)
		require.NoError(t, ht.untrusted)
		require.False(t, diverged(t, ht))
		last, _ := ht.store.Last()
		require.EqualValues(t, 1, last)
		require.NoError(t, ht.store.AddRound(2, forged.changes, forged.hdr, nil))
		v, ok, err := ht.store.Get(key, 2)
		require.NoError(t, err)
		require.True(t, ok)
		require.Equal(t, "w", string(v), "the evaluation's value")
	})

	t.Run("own corrupt", func(t *testing.T) {
		// the repair finds a store no node writes: marked
		ht, _, forged := setup(t)
		require.NoError(t, ht.store.DB().Set([]byte{'c', 'z', 0, 0, 0, 0, 0, 0, 0, 1}, []byte("F"), nil))
		ht.verifyReplayed(forged)
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		require.True(t, diverged(t, ht))
	})

	t.Run("installed", func(t *testing.T) {
		ht, _, forged := setup(t)
		ht.foreignThrough = 3
		ht.verifyReplayed(forged)
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		require.True(t, diverged(t, ht))
		require.ErrorIs(t, ht.diverged(), ErrHistoryUntrusted)
	})

	t.Run("own unmarked corrupt", func(t *testing.T) {
		// the repair refuses the store before it marks it: held back
		ht, _, forged := setup(t)
		require.NoError(t, ht.store.DB().Set([]byte("mt"), []byte("x"), nil))
		ht.verifyReplayed(forged)
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		require.False(t, diverged(t, ht))
		require.True(t, ht.repairFailed)
	})

	t.Run("repair failed", func(t *testing.T) {
		// no second repair in a load once one failed
		ht, _, forged := setup(t)
		ht.repairFailed = true
		ht.verifyReplayed(forged)
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		last, _ := ht.store.Last()
		require.EqualValues(t, 3, last, "not truncated")
	})

	t.Run("repair fails", func(t *testing.T) {
		st, err := timetravel.Open(t.TempDir(), timetravel.Options{CheckpointInterval: 2})
		require.NoError(t, err)
		t.Cleanup(func() { st.Close() })
		ch := []timetravel.Change{{Key: key, Value: []byte("v")}}
		for r := range 4 {
			require.NoError(t, st.AddRound(uint64(r), ch, []byte("h"), nil))
		}
		ht := &historyTracker{store: st, path: "test", log: logging.TestingLog(t)}
		ht.verifyReplayed(historyRound{rnd: 2, changes: []timetravel.Change{{Key: key, Value: []byte("w")}}, hdr: []byte("h")})
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		require.ErrorContains(t, ht.untrusted, "deleting the history from that round failed")
		require.True(t, ht.repairFailed)
	})

	readErrors := func(n int) *int {
		calls := 0
		verifyStored = func(st *timetravel.Store, r historyRound) error {
			calls++
			if calls <= n {
				return errors.New("an I/O error")
			}
			return st.Verify(uint64(r.rnd), r.changes, r.hdr, r.expired)
		}
		return &calls
	}
	defer func(f func(*timetravel.Store, historyRound) error) { verifyStored = f }(verifyStored)

	t.Run("read error once", func(t *testing.T) {
		ht, same, _ := setup(t)
		ht.foreignThrough = 3
		calls := readErrors(1)
		ht.verifyReplayed(same)
		require.Equal(t, 2, *calls, "retried")
		require.NoError(t, ht.untrusted)
	})

	t.Run("read errors", func(t *testing.T) {
		ht, same, _ := setup(t)
		ht.foreignThrough = 3
		calls := readErrors(2)
		ht.verifyReplayed(same)
		require.Equal(t, 2, *calls)
		require.ErrorIs(t, ht.untrusted, ErrHistoryUntrusted)
		require.False(t, diverged(t, ht), "a read error is not a difference")
	})
}
