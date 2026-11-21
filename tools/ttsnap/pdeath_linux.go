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

package main

import (
	"runtime"
	"syscall"
)

// dieWithParent has the kernel kill a process started with the attributes a
// when this one exits, however it does: a phase run as the data directory's
// owner must not go on downloading for hours unattended. SIGKILL, as a
// SIGTERM may be ignored, and the install resumes when rerun. The signal
// comes when the thread that started the process exits, so the caller keeps
// to it until the process is done (the returned function lets go).
func dieWithParent(a *syscall.SysProcAttr) (done func()) {
	a.Pdeathsig = syscall.SIGKILL
	runtime.LockOSThread()
	return runtime.UnlockOSThread
}
