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
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/cockroachdb/pebble/v2/vfs"
	"golang.org/x/sys/unix"
)

// readOnlyFS is the file system of a store opened with Options.ReadOnly.
// Pebble locks a store by creating its LOCK file for writing, which a
// read-only file system (a btrfs read-only snapshot, a read-only mount)
// refuses. There readOnlyFS takes a shared lock on the existing LOCK instead:
// a snapshot has its own LOCK, while a read-only view of the files of a store
// that an algod has open shares that algod's, and is refused as before.
type readOnlyFS struct{ vfs.FS }

func (fs readOnlyFS) Lock(name string) (io.Closer, error) {
	c, err := fs.FS.Lock(name)
	if !errors.Is(err, unix.EROFS) {
		return c, err
	}
	f, oerr := os.Open(name)
	if oerr != nil {
		return nil, err
	}
	spec := unix.Flock_t{Type: unix.F_RDLCK, Whence: io.SeekStart}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &spec); err != nil {
		f.Close()
		return nil, fmt.Errorf("shared lock on %s: %w", name, err)
	}
	return f, nil // closing it releases the lock
}
