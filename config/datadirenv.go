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
	"os"
	"path/filepath"
	"strings"
)

// DataDirEnvName is the environment variable that names the data directory when -d isn't
// given: ALGORAND_DATA, or DTALGORAND_DATA under a dt name (see EnvName).
func DataDirEnvName() string {
	return EnvName("ALGORAND_DATA")
}

// DataDirFromEnv returns the data directory named by the DataDirEnvName variable, or "".
func DataDirFromEnv() string {
	return os.Getenv(DataDirEnvName())
}

// EnvName returns the environment variable a binary reads in place of the stock variable name.
// dtalgorand installs its binaries under their stock names and runs them through dt-prefixed
// links (dtgoal, dtalgod, ...). Run under such a name, a binary reads DT<name> and ignores
// <name>, which may point at a stock node on the same host. Any binary named dt* does this, and
// it goes by argv[0] alone: run by its stock name, a dtalgorand binary reads the stock variables.
func EnvName(name string) string {
	return envName(os.Args[0], name)
}

func envName(arg0, name string) string {
	if strings.HasPrefix(filepath.Base(arg0), "dt") {
		return "DT" + name
	}
	return name
}
