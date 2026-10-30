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

package config

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// Only $DTALGORAND_DATA counts; a stock $ALGORAND_DATA is ignored, and only noted.
func TestDataDirFromEnv(t *testing.T) {
	partitiontest.PartitionTest(t)

	t.Setenv("ALGORAND_DATA", "/stock")
	t.Setenv("DTALGORAND_DATA", "/dt")
	require.Equal(t, "/dt", DataDirFromEnv())
	require.Equal(t, "/dt", ResolveDataDir(""))
	require.Equal(t, "/flag", ResolveDataDir("/flag"))
	require.Empty(t, DataDirEnvNote())

	t.Setenv("DTALGORAND_DATA", "")
	require.Empty(t, DataDirFromEnv())
	require.Empty(t, ResolveDataDir(""))
	require.Equal(t, " $ALGORAND_DATA is set but ignored: dtalgorand reads $DTALGORAND_DATA.", DataDirEnvNote())

	t.Setenv("ALGORAND_DATA", "")
	require.Empty(t, DataDirEnvNote())
}
