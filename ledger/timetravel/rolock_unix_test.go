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

//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package timetravel

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/cockroachdb/pebble/v2/vfs"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// readOnlyMount is a file system that refuses to create files, as a
// read-only mount does, at least where pebble takes its lock.
type readOnlyMount struct{ vfs.FS }

func (readOnlyMount) Lock(name string) (io.Closer, error) {
	return nil, &os.PathError{Op: "open", Path: name, Err: unix.EROFS}
}

// rolockStore writes a store of a few rounds and closes it.
func rolockStore(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "tt")
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	for rnd := uint64(0); rnd < 3; rnd++ {
		require.NoError(t, s.AddRound(rnd, []Change{{Key: TotalsKey, Value: []byte{byte(rnd)}}}, []byte("hdr"), nil))
	}
	require.NoError(t, s.Close())
	return dir
}

func TestReadOnlyOpenOnReadOnlyFS(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dir := rolockStore(t)
	ro := Options{ReadOnly: true, fs: readOnlyMount{vfs.Default}}
	s, err := Open(dir, ro)
	require.NoError(t, err)
	last, ok := s.Last()
	require.True(t, ok)
	require.EqualValues(t, 2, last)
	v, ok, err := s.Get(TotalsKey, 1)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []byte{1}, v)

	// the lock is shared: a second read-only open succeeds too
	s2, err := Open(dir, ro)
	require.NoError(t, err)
	require.NoError(t, s2.Close())
	require.NoError(t, s.Close())

	// a writable open is not affected by the file system's refusals: it
	// fails as on a read-only mount
	_, err = Open(dir, Options{fs: readOnlyMount{vfs.Default}})
	require.ErrorIs(t, err, unix.EROFS)

	// without a LOCK file there is nothing to share: the error is the mount's
	require.NoError(t, os.Remove(filepath.Join(dir, "LOCK")))
	_, err = Open(dir, ro)
	require.ErrorIs(t, err, unix.EROFS)
}

// TestReadOnlyOpenOnReadOnlyFSLocked checks that a read-only open on a
// read-only file system is refused while another process has the store open
// for writing, as an algod does through another mount. POSIX locks never
// conflict within a process, so the read-only open runs in a child: this
// test binary, running TestReadOnlyOpenChild.
func TestReadOnlyOpenOnReadOnlyFSLocked(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dir := rolockStore(t)
	child := func() (string, error) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestReadOnlyOpenChild$", "-test.count=1")
		cmd.Env = append(os.Environ(), "TT_ROLOCK_DIR="+dir)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	s, err := Open(dir, Options{})
	require.NoError(t, err)
	out, err := child()
	require.Error(t, err, "the child opened a store this process has open for writing: %s", out)
	require.Contains(t, out, "shared lock on")
	require.NoError(t, s.Close())
	out, err = child()
	require.NoError(t, err, out)
}

// TestReadOnlyOpenChild opens TT_ROLOCK_DIR read-only on a read-only file
// system, for TestReadOnlyOpenOnReadOnlyFSLocked.
func TestReadOnlyOpenChild(t *testing.T) {
	partitiontest.PartitionTest(t)
	dir := os.Getenv("TT_ROLOCK_DIR")
	if dir == "" {
		t.Skip("run by TestReadOnlyOpenOnReadOnlyFSLocked")
	}
	s, err := Open(dir, Options{ReadOnly: true, fs: readOnlyMount{vfs.Default}})
	require.NoError(t, err)
	require.NoError(t, s.Close())
}
