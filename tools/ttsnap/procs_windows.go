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

//go:build windows

package main

import "io"

// lockHolder: a pebble store on Windows is locked with LockFileEx, which this
// doesn't read.
func lockHolder(string) (int, bool) { return 0, false }

// processName: not looked up on Windows.
func processName(int) string { return "" }

// processAlive: Windows can't tell, so a process counts as alive.
func processAlive(int) bool { return true }

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// lockDir: Windows has no directory locks; nothing is locked.
func lockDir(string) (io.Closer, error) { return nopCloser{}, nil }
