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

func TestEnvName(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for arg0, dt := range map[string]bool{
		"":                             false,
		"goal":                         false,
		"/usr/bin/algod":               false,
		"/usr/lib/dtalgorand/bin/goal": false,
		"/usr/bin/dtalgod/":            true,
		"dtgoal":                       true,
		"/usr/bin/dtalgod":             true,
		"/opt/homebrew/bin/dtalgocfg":  true,
		"./dtdiagcfg":                  true,
		"../bin/dtalgoh":               true,
	} {
		for _, name := range []string{"ALGORAND_DATA", "ALGORAND_KMD"} {
			want := name
			if dt {
				want = "DT" + name
			}
			require.Equal(t, want, envName(arg0, name), arg0)
		}
	}
}

// With both variables set, the binary reads only the one for its name. Run as the test binary
// (not dt-named), that is ALGORAND_DATA.
func TestDataDirFromEnv(t *testing.T) {
	partitiontest.PartitionTest(t)

	t.Setenv("ALGORAND_DATA", "/stock")
	t.Setenv("DTALGORAND_DATA", "/dt")
	require.Equal(t, "ALGORAND_DATA", DataDirEnvName())
	require.Equal(t, "/stock", DataDirFromEnv())

	t.Setenv("ALGORAND_DATA", "")
	require.Equal(t, "", DataDirFromEnv())
}
