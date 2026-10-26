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
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The dtalgorand packages install the public keys of their snapshot producers
// and attesters (see readKeys) at keysPath under the prefix ttsnap is
// installed in: /usr for the .deb, the formula's keg for Homebrew.
const keysPath = "share/dtalgorand/ttsnap/keys"

// debKeys is where the .deb installs them, for a ttsnap installed elsewhere.
const debKeys = "/usr/" + keysPath

// keysLevels is how many directories above an installed keys file are
// checked (see openOwned): those of keysPath and the prefix, for both
// candidates.
var keysLevels = strings.Count(keysPath, "/") + 1

// keysCandidates lists where the installed keys may be, for the executable at
// exe (links resolved): keysPath under the parent of its directory, which is
// /usr/share/... for the .deb's /usr/bin/ttsnap and the keg's share/ for
// Homebrew's libexec/ttsnap, then the .deb's path.
func keysCandidates(exe string) []string {
	paths := []string{filepath.Join(filepath.Dir(filepath.Dir(exe)), filepath.FromSlash(keysPath))}
	if paths[0] != debKeys {
		paths = append(paths, debKeys)
	}
	return paths
}

// installedKeysFor reads the installed keys for the ttsnap executable at exe:
// see keysCandidates and installedKeys.
func installedKeysFor(exe string) ([]Key, string, error) {
	r, err := filepath.EvalSymlinks(exe)
	if err == nil {
		var uid uint32
		uid, err = fileOwner(r)
		if err == nil {
			return installedKeys(keysCandidates(r), uid)
		}
	}
	return nil, "", fmt.Errorf("find the installed keys: %w: pass -keys <keys file>", err)
}

// installedKeys reads the first of paths that exists, if only root or uid,
// ttsnap's owner, can have put it there (see openOwned): a ttsnap run from a
// directory of /tmp must not trust /tmp/share/....
func installedKeys(paths []string, uid uint32) ([]Key, string, error) {
	var refused []string
	for _, p := range paths {
		f, err := openOwned(p, keysLevels, uid)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			refused = append(refused, fmt.Sprintf("%s (%v)", p, err))
			continue
		}
		keys, err := parseKeys(f, p)
		f.Close()
		return keys, p, err
	}
	if len(refused) > 0 {
		return nil, "", fmt.Errorf("refusing the keys file %s: pass -keys <keys file>", strings.Join(refused, ", "))
	}
	return nil, "", fmt.Errorf("no keys file at %s: pass -keys <keys file> (a source tree has installer/dtalgorand/ttsnap_keys)", strings.Join(paths, " or "))
}
