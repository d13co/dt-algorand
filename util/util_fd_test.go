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

package util

import (
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestRaiseFdSoftLimitToHard checks that the hard limit GetFdLimits reports
// can be set. On macOS, RLIMIT_NOFILE's own hard limit often can't.
func TestRaiseFdSoftLimitToHard(t *testing.T) {
	partitiontest.PartitionTest(t)

	soft, hard, err := GetFdLimits()
	require.NoError(t, err)
	require.LessOrEqual(t, soft, hard)
	t.Cleanup(func() { require.NoError(t, SetFdSoftLimit(soft)) })

	require.NoError(t, RaiseFdSoftLimit(hard))
	got, _, err := GetFdLimits()
	require.NoError(t, err)
	require.Equal(t, hard, got)

	// asking for more than the OS allows settles for the most it does
	more := hard + 1000
	if more < hard { // hard is RLIM_INFINITY
		more = math.MaxUint64
	}
	require.NoError(t, RaiseFdSoftLimit(more))
	got, _, err = GetFdLimits()
	require.NoError(t, err)
	require.Equal(t, hard, got)
}
