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
	"sync"
	"syscall"

	"github.com/cockroachdb/pebble/v2/vfs"
	"golang.org/x/sys/unix"

	"github.com/algorand/go-deadlock"
)

// readOnlyFS is the file system of a store opened with Options.ReadOnly.
// Pebble locks a store by creating its LOCK file for writing and locking it
// exclusively. A read-only file system (a btrfs read-only snapshot, a
// read-only mount) refuses to create it, and so does a LOCK file the user may
// read but not write (a store another user owns); and the exclusive lock
// fails while another process holds a shared one. Then readOnlyFS takes a
// shared lock on the existing LOCK instead: a snapshot has its own LOCK, while
// a read-only view of the files of a store that an algod has open shares that
// algod's, and is refused as before. The process must not also open the store
// through another path that pebble locks itself (a writable mount of the
// same files): a process's locks never conflict with each other.
type readOnlyFS struct{ vfs.FS }

func (fs readOnlyFS) Lock(name string) (io.Closer, error) {
	c, err := fs.FS.Lock(name)
	if !errors.Is(err, unix.EROFS) && !errors.Is(err, unix.EACCES) && !errors.Is(err, unix.EPERM) && !errors.Is(err, unix.EAGAIN) {
		return c, err
	}
	c, serr := sharedLock(name)
	if serr != nil {
		return nil, fmt.Errorf("%w; and for a shared lock: %w", err, serr)
	}
	return c, nil
}

// fileID identifies a file, whatever the path to it.
type fileID struct{ dev, ino uint64 }

func fileIDOf(fi os.FileInfo) (fileID, bool) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return fileID{}, false
	}
	return fileID{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true //nolint:unconvert // types differ by platform
}

// sharedLocks are the shared locks this process holds. POSIX locks belong to
// the process, not to a descriptor, and closing any descriptor of a file
// releases all of them: so the descriptors of a LOCK file stay open until the
// last store that shares its lock closes.
var sharedLocks struct {
	mu    deadlock.Mutex
	files map[fileID]*sharedLockFile
}

type sharedLockFile struct {
	fds  []*os.File // the first holds the lock
	refs int
}

// sharedLockRef is one store's share of a LOCK file's shared lock.
type sharedLockRef struct {
	id   fileID
	once sync.Once
}

func (r *sharedLockRef) Close() error {
	var err error
	r.once.Do(func() {
		sharedLocks.mu.Lock()
		defer sharedLocks.mu.Unlock()
		lf := sharedLocks.files[r.id]
		if lf.refs--; lf.refs > 0 {
			return
		}
		delete(sharedLocks.files, r.id)
		for _, f := range lf.fds { // the first close releases the lock
			err = errors.Join(err, f.Close())
		}
	})
	return err
}

// sharedLock takes a shared lock on the existing file name, or shares the one
// this process holds.
func sharedLock(name string) (io.Closer, error) {
	sharedLocks.mu.Lock()
	defer sharedLocks.mu.Unlock()
	// find a lock this process holds before opening the file: closing a
	// descriptor of it would release the lock
	fi, err := os.Stat(name)
	if err != nil {
		return nil, err
	}
	id, ok := fileIDOf(fi)
	if !ok {
		return nil, fmt.Errorf("no file identity for %s", name)
	}
	if lf := sharedLocks.files[id]; lf != nil {
		lf.refs++
		return &sharedLockRef{id: id}, nil
	}
	f, err := os.Open(name)
	if err != nil {
		return nil, err
	}
	if fi, err = f.Stat(); err == nil {
		if fid, ok := fileIDOf(fi); ok && fid != id { // replaced since
			id = fid
			if lf := sharedLocks.files[id]; lf != nil {
				// closing f now would release the lock: it goes with the others
				lf.fds = append(lf.fds, f)
				lf.refs++
				return &sharedLockRef{id: id}, nil
			}
		}
	}
	spec := unix.Flock_t{Type: unix.F_RDLCK, Whence: io.SeekStart}
	if err := unix.FcntlFlock(f.Fd(), unix.F_SETLK, &spec); err != nil {
		f.Close()
		return nil, fmt.Errorf("shared lock on %s: %w", name, err)
	}
	if sharedLocks.files == nil {
		sharedLocks.files = map[fileID]*sharedLockFile{}
	}
	sharedLocks.files[id] = &sharedLockFile{fds: []*os.File{f}, refs: 1}
	return &sharedLockRef{id: id}, nil
}
