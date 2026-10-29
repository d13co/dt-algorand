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

//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestInstalledKeys(t *testing.T) {
	partitiontest.PartitionTest(t)
	_, pk := newKey(t)
	line := keyLine("p", roleProducer, pk.PublicKey)

	// Homebrew: bin/ttsnap links to the keg's libexec/ttsnap, whose keys are in
	// the keg's share/dtalgorand.
	prefix, err := filepath.EvalSymlinks(t.TempDir()) // macOS: /var is a link to /private/var
	require.NoError(t, err)
	keg := filepath.Join(prefix, "Cellar", "dtalgorand", "1.0")
	exe := filepath.Join(keg, "libexec", "ttsnap")
	keys := filepath.Join(keg, "share", "dtalgorand", "ttsnap", "keys")
	for f, data := range map[string]string{exe: "", keys: line + "\n"} {
		require.NoError(t, os.MkdirAll(filepath.Dir(f), 0o755))
		require.NoError(t, os.WriteFile(f, []byte(data), 0o644))
	}
	// whatever the umask, as the formula does
	for d := filepath.Dir(keys); d != filepath.Dir(keg); d = filepath.Dir(d) {
		require.NoError(t, os.Chmod(d, 0o755))
	}
	link := filepath.Join(prefix, "bin", "ttsnap")
	require.NoError(t, os.MkdirAll(filepath.Dir(link), 0o755))
	require.NoError(t, os.Symlink(filepath.Join("..", "Cellar", "dtalgorand", "1.0", "libexec", "ttsnap"), link))

	require.Equal(t, []string{keys, debKeys}, keysCandidates(exe))
	for _, e := range []string{link, exe} {
		got, path, err := installedKeysFor(e)
		require.NoError(t, err, e)
		require.Equal(t, keys, path)
		require.Len(t, got, 1)
		require.Equal(t, pk.PublicKey, got[0].PK)
	}

	// the .deb's /usr/bin/ttsnap: its keys are debKeys, listed once
	require.Equal(t, []string{debKeys}, keysCandidates("/usr/bin/ttsnap"))
	require.Equal(t, keysLevels, 4) // keys, ttsnap, dtalgorand, share, and the prefix above them

	// a keys file other users can write, or under a directory they can, is
	// refused: e.g. ttsnap in a directory of /tmp, whose share/ anyone can make
	uid := uint32(os.Getuid())
	paths := []string{keys}
	for _, d := range []string{keys, filepath.Dir(keys), filepath.Join(keg, "share", "dtalgorand"), filepath.Join(keg, "share"), keg} {
		fi, err := os.Stat(d)
		require.NoError(t, err)
		require.NoError(t, os.Chmod(d, fi.Mode().Perm()|0o002))
		_, _, err = installedKeys(paths, uid)
		require.ErrorContains(t, err, d+" is writable by other users")
		require.NoError(t, os.Chmod(d, fi.Mode().Perm()))
	}
	// owned by someone else than root or ttsnap's owner
	if uid != 0 {
		_, _, err = installedKeys(paths, uid+1)
		require.ErrorContains(t, err, "is owned by user")
	}
	// a link, even to a good file: the check reads the file it opens
	moved := keys + ".real"
	require.NoError(t, os.Rename(keys, moved))
	require.NoError(t, os.Symlink(moved, keys))
	_, _, err = installedKeys(paths, uid)
	require.ErrorContains(t, err, "refusing the keys file")
	require.NoError(t, os.Remove(keys))
	require.NoError(t, os.Rename(moved, keys))

	_, path, err := installedKeys(paths, uid)
	require.NoError(t, err)
	require.Equal(t, keys, path)

	// a build without the keys: the error lists where it looked
	missing := []string{filepath.Join(prefix, "a"), filepath.Join(prefix, "b")}
	_, _, err = installedKeys(missing, uid)
	require.ErrorContains(t, err, "no keys file at "+missing[0]+" or "+missing[1])
	require.ErrorContains(t, err, "-keys")
}
