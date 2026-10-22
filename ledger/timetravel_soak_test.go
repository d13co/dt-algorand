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

//go:build linux

package ledger

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// A time travel node's ledger copied at any instant, as a filesystem snapshot
// (btrfs, ZFS, LVM) of a running node copies it, must open: blocks are
// written before the trackers commit them, and the history is synced before
// the tracker DB commits (historyTracker.commitRound). The soak test checks
// that with a ledger in a child process that keeps adding blocks: it freezes
// the child (SIGSTOP), so that its files stop changing, copies them, and lets
// it go on.

const soakEnv = "TT_SOAK_DIR"

var soakGenHash = crypto.Hash([]byte("time travel snapshot soak"))

// soakGenesis is the soak ledger's genesis, the same in both processes.
func soakGenesis(t *testing.T) (bookkeeping.GenesisBalances, bookkeeping.Block) {
	balances, _, _ := ledgertesting.NewTestGenesis()
	balances.Timestamp = 1_700_000_000
	genBlock, err := bookkeeping.MakeGenesisBlock(protocol.ConsensusFuture, balances, "test", soakGenHash)
	require.NoError(t, err)
	return balances, genBlock
}

func openSoakLedger(t *testing.T, dbName string) *Ledger {
	l, err := openSoakLedgerErr(t, dbName)
	require.NoError(t, err)
	return l
}

func openSoakLedgerErr(t *testing.T, dbName string) (*Ledger, error) {
	balances, genBlock := soakGenesis(t)
	cfg := ttCatchupConfig()
	cfg.Archival = false
	return OpenLedger(logging.TestingLog(t), dbName, false, ledgercore.InitState{Block: genBlock, Accounts: balances.Balances, GenesisHash: soakGenHash}, cfg)
}

// TestTimeTravelSnapshotSoakChild is the child process of
// TestTimeTravelSnapshotSoak: it adds blocks until it is killed.
func TestTimeTravelSnapshotSoakChild(t *testing.T) {
	dir := os.Getenv(soakEnv)
	if dir == "" {
		t.Skip("the child process of TestTimeTravelSnapshotSoak")
	}
	l := openSoakLedger(t, filepath.Join(dir, "node"))
	_, addrs, _ := ledgertesting.NewTestGenesis()
	ev := nextBlock(t, l)
	txn(t, l, ev, &txntest.Txn{Type: "appl", Sender: addrs[0], ApprovalProgram: counterApp, GlobalStateSchema: basics.StateSchema{NumUint: 1}})
	vb := endBlock(t, l, ev)
	app := vb.Block().Payset[0].ApplyData.ApplicationID
	ev = nextBlock(t, l)
	txn(t, l, ev, &txntest.Txn{Type: "pay", Sender: addrs[0], Receiver: app.Address(), Amount: 100_000_000})
	endBlock(t, l, ev)
	for {
		r := uint64(l.Latest() + 1)
		ev := nextBlock(t, l)
		for i := range 8 {
			txn(t, l, ev, &txntest.Txn{Type: "pay", Sender: addrs[1+i], Receiver: addrs[(2+i)%len(addrs)], Amount: r, Note: binary.BigEndian.AppendUint64(nil, r)})
		}
		txn(t, l, ev, &txntest.Txn{Type: "appl", Sender: addrs[0], ApplicationID: app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, r)},
			Boxes: []transactions.BoxRef{{Name: []byte("b")}}})
		endBlock(t, l, ev)
	}
}

// TestTimeTravelSnapshotSoak copies a ledger that a child process keeps
// writing, many times at random instants, and checks that every copy opens
// and answers like the ledger it was copied from. Set TT_SNAPSHOT_SOAK=N to
// run it with N copies.
func TestTimeTravelSnapshotSoak(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := 0
	fmt.Sscan(os.Getenv("TT_SNAPSHOT_SOAK"), &n)
	if n == 0 {
		t.Skip("set TT_SNAPSHOT_SOAK to the number of copies to take")
	}

	dir := t.TempDir()
	exe, err := os.Executable()
	require.NoError(t, err)
	logFile, err := os.Create(filepath.Join(dir, "child.log"))
	require.NoError(t, err)
	defer logFile.Close()
	cmd := exec.Command(exe, "-test.run=^TestTimeTravelSnapshotSoakChild$", "-test.timeout=0")
	cmd.Env = append(os.Environ(), soakEnv+"="+dir)
	cmd.Stdout, cmd.Stderr = logFile, logFile
	require.NoError(t, cmd.Start())
	defer cmd.Process.Kill()

	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	_, addrs, _ := ledgertesting.NewTestGenesis()
	addrs = addrs[:4]
	// each copy is checked as soon as it is taken, and removed: the reads are
	// compared with the final ledger at the end
	type read struct {
		snap  int
		rnd   basics.Round
		accts []ledgercore.AccountData
		total ledgercore.AccountTotals
	}
	var reads []read
	withoutWAL := 0
	time.Sleep(3 * time.Second)
	for i := range n {
		time.Sleep(time.Duration(rng.Intn(400)) * time.Millisecond)
		require.NoError(t, cmd.Process.Signal(syscall.SIGSTOP))
		waitStopped(t, cmd.Process.Pid, logFile.Name())
		snap := filepath.Join(dir, "snap")
		require.NoError(t, copyLedgerFiles(filepath.Join(dir, "node"), filepath.Join(snap, "node")))
		require.NoError(t, cmd.Process.Signal(syscall.SIGCONT))

		// the history's WAL is part of the copy: without it the history
		// ends before the tracker DB, and the ledger refuses it
		bare := filepath.Join(dir, "bare")
		require.NoError(t, copyLedgerFiles(filepath.Join(snap, "node"), filepath.Join(bare, "node")))
		logs, err := filepath.Glob(filepath.Join(bare, "node.timetravel", "*.log"))
		require.NoError(t, err)
		for _, p := range logs {
			require.NoError(t, os.Remove(p))
		}
		if l, err := openSoakLedgerErr(t, filepath.Join(bare, "node")); err == nil {
			l.Close()
		} else {
			withoutWAL++
		}
		require.NoError(t, os.RemoveAll(bare))

		l := openSoakLedger(t, filepath.Join(snap, "node"))
		latest, dbRound := l.Latest(), l.LatestTrackerCommitted()
		last, ok := l.history.store.Last()
		require.True(t, ok)
		require.GreaterOrEqual(t, basics.Round(last), dbRound)
		require.NoError(t, l.HistoryUntrusted())
		for range 10 {
			rd := read{snap: i, rnd: basics.Round(1 + rng.Int63n(int64(latest)))}
			for _, addr := range addrs {
				ad, _, err := l.LookupWithoutRewards(rd.rnd, addr)
				require.NoError(t, err, "copy %d round %d", i, rd.rnd)
				rd.accts = append(rd.accts, ad)
			}
			rd.total, err = l.Totals(rd.rnd)
			require.NoError(t, err)
			reads = append(reads, rd)
		}
		l.Close()
		require.NoError(t, os.RemoveAll(snap))
	}
	// the end is a crash too
	require.NoError(t, cmd.Process.Kill())
	cmd.Wait()

	final := openSoakLedger(t, filepath.Join(dir, "node"))
	defer final.Close()
	t.Logf("the child reached round %d", final.Latest())
	for _, rd := range reads {
		for j, addr := range addrs {
			want, _, err := final.LookupWithoutRewards(rd.rnd, addr)
			require.NoError(t, err)
			require.Equal(t, want, rd.accts[j], "copy %d round %d", rd.snap, rd.rnd)
		}
		want, err := final.Totals(rd.rnd)
		require.NoError(t, err)
		require.Equal(t, want, rd.total, "copy %d round %d", rd.snap, rd.rnd)
	}
	t.Logf("%d copies opened and answered %d reads like the final ledger; without the history's WAL, %d of them did not open", n, len(reads), withoutWAL)
	require.Positive(t, withoutWAL, "the copies hold rounds only in the WAL, so the test copies the WAL")
}

// waitStopped waits until the process is stopped by SIGSTOP.
func waitStopped(t *testing.T, pid int, logPath string) {
	var state byte
	for range 10_000 {
		b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
		require.NoError(t, err)
		// pid (comm) state ...: comm may hold spaces, so split after it
		if i := bytes.LastIndexByte(b, ')'); i > 0 && len(b) > i+2 {
			state = b[i+2]
			if state == 'T' {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	logTail, _ := os.ReadFile(logPath)
	if len(logTail) > 4000 {
		logTail = logTail[len(logTail)-4000:]
	}
	t.Fatalf("the child did not stop (state %c); its log ends with:\n%s", state, logTail)
}

// copyLedgerFiles copies the files of the ledger at dbName (dbName.*, and the
// history store's directory) to dst.
func copyLedgerFiles(dbName, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	paths, err := filepath.Glob(dbName + ".*")
	if err != nil {
		return err
	}
	for _, p := range paths {
		target := dst + strings.TrimPrefix(p, dbName)
		fi, err := os.Stat(p)
		if err != nil {
			return err
		}
		if fi.IsDir() {
			if err := os.CopyFS(target, os.DirFS(p)); err != nil {
				return err
			}
			continue
		}
		if err := copyOneFile(p, target); err != nil {
			return err
		}
	}
	return nil
}

func copyOneFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}
