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
	"os"
	"os/user"
	"strconv"
	"syscall"
)

// checkRunAs refuses to run as root (euid 0) on a data directory that another
// user owns. The files root writes there would be root's, and the node, which
// runs as that user, could not open them; handing them over afterwards would
// mean root writing, for days, in directories that user controls.
func checkRunAs(dataDir string, euid int) error {
	if euid != 0 {
		return nil
	}
	fi, err := os.Stat(dataDir)
	if err != nil {
		return nil //nolint:nilerr // the mode reports it
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Uid == 0 {
		return nil
	}
	uid := strconv.FormatUint(uint64(st.Uid), 10)
	name, sudoUser := "user "+uid, "'#"+uid+"'"
	if u, err := user.LookupId(uid); err == nil {
		name, sudoUser = u.Username, u.Username
	}
	return fmt.Errorf("%s belongs to %s: run ttimport as that user (sudo -u %s ttimport ...), not as root, so that the node can open what it writes", dataDir, name, sudoUser)
}
