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
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"syscall"
)

// fileOwner returns the user that owns path.
func fileOwner(path string) (uint32, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	return owner(path, fi)
}

func owner(path string, fi fs.FileInfo) (uint32, error) {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no owner", path)
	}
	return st.Uid, nil
}

// checkOwner checks that path, described by fi, is owned by root or uid and
// writable by its owner alone.
func checkOwner(path string, fi fs.FileInfo, uid uint32) error {
	o, err := owner(path, fi)
	if err != nil {
		return err
	}
	if o != 0 && o != uid {
		return fmt.Errorf("%s is owned by user %d, not by root or ttsnap's owner (%d)", path, o, uid)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is writable by other users (%v)", path, fi.Mode().Perm())
	}
	return nil
}

// openOwned opens the regular file at path, which must not be a link, and
// checks that it and the levels directories above it are owned by root or uid
// and writable by their owner alone, so that no other user can have put it
// there. The file is checked through the descriptor it returns, so what is
// read is what was checked. A missing file gives an fs.ErrNotExist error.
func openOwned(path string, levels int, uid uint32) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	fi, err := f.Stat()
	if err == nil && !fi.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", path)
	}
	if err == nil {
		err = checkOwner(path, fi, uid)
	}
	for i, d := 0, filepath.Dir(path); err == nil && i < levels; i, d = i+1, filepath.Dir(d) {
		var di fs.FileInfo
		if di, err = os.Stat(d); err == nil {
			err = checkOwner(d, di, uid)
		}
	}
	if err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}
