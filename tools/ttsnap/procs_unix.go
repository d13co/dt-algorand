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
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// lockHolder tells whether another process holds a POSIX lock on path, as
// algod holds the LOCK file of the history store from when it opens its
// ledger until it exits, and which: pid is 0 or less when the lock doesn't
// say (an open file description lock, a process in another PID namespace).
// This process must not have the store open: closing a descriptor of the
// file releases the locks it holds on it.
func lockHolder(path string) (pid int, held bool) {
	f, err := os.Open(path)
	if err != nil {
		return 0, false
	}
	defer f.Close()
	lk := unix.Flock_t{Type: unix.F_WRLCK, Whence: io.SeekStart}
	if unix.FcntlFlock(f.Fd(), unix.F_GETLK, &lk) != nil || lk.Type == unix.F_UNLCK {
		return 0, false
	}
	return int(lk.Pid), true
}

// processName is the name of the program the process pid runs, or "".
func processName(pid int) string {
	out, err := exec.Command("ps", "-o", "comm=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return filepath.Base(strings.TrimSpace(string(out)))
}

// processAlive tells whether the process pid exists, whoever runs it.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// lockDir locks the directory dir against another lockDir until the lock is
// closed or the process exits.
func lockDir(dir string) (io.Closer, error) {
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, unix.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	return f, nil
}
