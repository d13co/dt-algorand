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

// ttsnap publishes snapshots of a time travel node's history and installs
// them on new time travel nodes, which then catch up from a catchpoint instead
// of syncing from genesis. See README.md.
package main

import (
	"fmt"
	"os"

	"github.com/algorand/go-algorand/logging"
)

const usageText = `ttsnap: snapshots of a time travel node's history

Producer (on a writable copy of a node's genesis directory, e.g. a btrfs snapshot):
  ttsnap prepare [-full] <genesis dir>             make the copy self-contained, print what it holds
  ttsnap publish -o <out> -key <ssh key> [-full] <genesis dir>
                                                   add it to a static snapshot tree, signed
  ttsnap gc -o <out> [-keep N]                     drop older snapshots and unreferenced blobs
  ttsnap info [-full] <genesis dir>                print what a genesis directory holds

Consumer:
  ttsnap fetch -url <base URL> -d <data dir> [-keys <allowed signers>] [-full -trust-producer]
                                                   download, verify and install the latest snapshot
  ttsnap catchup -d <data dir> [-label <catchpoint>]
                                                   start the catchpoint catchup on the running node
  ttsnap verify -d <data dir> [-deltas <algod URL> -sample N]
                                                   check the history of a stopped node

Run "ttsnap <command> -h" for a command's flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usageText)
		os.Exit(2)
	}
	logging.Base().SetLevel(logging.Warn)
	commands := map[string]func([]string) error{
		"prepare": cmdPrepare,
		"publish": cmdPublish,
		"gc":      cmdGC,
		"info":    cmdInfo,
		"fetch":   cmdFetch,
		"catchup": cmdCatchup,
		"verify":  cmdVerify,
	}
	cmd, ok := commands[os.Args[1]]
	if !ok {
		fmt.Fprint(os.Stderr, usageText)
		if os.Args[1] == "-h" || os.Args[1] == "--help" || os.Args[1] == "help" {
			return
		}
		os.Exit(2)
	}
	if err := cmd(os.Args[2:]); err != nil {
		fmt.Fprintf(os.Stderr, "ttsnap %s: %v\n", os.Args[1], err)
		os.Exit(1)
	}
}
