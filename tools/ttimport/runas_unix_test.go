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

func TestCheckRunAs(t *testing.T) {
	partitiontest.PartitionTest(t)
	if os.Getuid() == 0 {
		t.Skip("needs a data directory owned by a user other than root")
	}
	dataDir := t.TempDir()
	// as the data directory's owner, or any other user but root
	require.NoError(t, checkRunAs(dataDir, os.Getuid()))
	require.NoError(t, checkRunAs(dataDir, os.Getuid()+1))
	// as root on another user's data directory
	err := checkRunAs(dataDir, 0)
	require.ErrorContains(t, err, "not as root")
	// a missing data directory is left to the mode to report
	require.NoError(t, checkRunAs(filepath.Join(dataDir, "missing"), 0))
}
