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

import "os"

// dtalgorand installs next to stock algorand, so its binaries read their own environment
// variables, never the stock ALGORAND_DATA and ALGORAND_KMD, which may point at a stock node.
const (
	// DataDirEnv names the algod data directory when -d isn't given.
	DataDirEnv = "DTALGORAND_DATA"
	// KmdDirEnv names goal's kmd data directory when -k isn't given.
	KmdDirEnv = "DTALGORAND_KMD"

	stockDataDirEnv = "ALGORAND_DATA"
)

// DataDirFromEnv returns the data directory named by $DTALGORAND_DATA, or "".
func DataDirFromEnv() string {
	return os.Getenv(DataDirEnv)
}

// ResolveDataDir returns flagDir (the -d value) if set, else DataDirFromEnv.
func ResolveDataDir(flagDir string) string {
	if flagDir != "" {
		return flagDir
	}
	return DataDirFromEnv()
}

// DataDirEnvNote explains, for a "data directory not specified" error, that a set
// $ALGORAND_DATA was ignored. It returns "" when it wasn't set, or $DTALGORAND_DATA was.
// Otherwise it returns a sentence with a leading space, to append to the error.
func DataDirEnvNote() string {
	if os.Getenv(stockDataDirEnv) == "" || DataDirFromEnv() != "" {
		return ""
	}
	return " $" + stockDataDirEnv + " is set but ignored: dtalgorand reads $" + DataDirEnv + "."
}
