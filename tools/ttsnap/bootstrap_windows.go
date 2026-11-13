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

import (
	"errors"
	"os/exec"
)

// phaseOwner: Windows has no root to step down from.
func phaseOwner(string) (*phaseUser, error) { return nil, nil }

func runPhase(*phaseUser, string, []string) (bool, error) {
	return false, errors.New("bootstrap phases run in-process on Windows")
}

func runAs(*exec.Cmd, *phaseUser) error {
	return errors.New("bootstrap runs everything as you on Windows")
}

func shellCommand(command string) *exec.Cmd {
	return exec.Command("cmd", "/C", command)
}
