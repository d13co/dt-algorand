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
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/rpcs"
)

// stats prints one CSV row per sampled round: object changes and full value
// bytes per kind, and (if the block file exists) block/header/cert sizes.
func stats(args []string) error {
	fs := flag.NewFlagSet("stats", flag.ExitOnError)
	deltas := fs.String("deltas", "", "sampled delta directory")
	blocks := fs.String("blocks", "", "sampled block directory")
	fs.Parse(args)

	files, err := filepath.Glob(filepath.Join(*deltas, "*.msgp"))
	if err != nil {
		return err
	}
	var rounds []uint64
	for _, f := range files {
		r, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(f), ".msgp"), 10, 64)
		if err == nil {
			rounds = append(rounds, r)
		}
	}
	sort.Slice(rounds, func(i, j int) bool { return rounds[i] < rounds[j] })
	kinds := []byte{timetravel.KindAccount, timetravel.KindHolding, timetravel.KindLocal, timetravel.KindAssetParams, timetravel.KindAppParams, timetravel.KindKv}
	fmt.Print("round")
	for _, k := range kinds {
		fmt.Printf(",n_%c,b_%c", k, k)
	}
	fmt.Println(",n_del,hdr_bytes,txns,block_bytes,blk_hdr_bytes,cert_bytes")
	for _, r := range rounds {
		sd, err := decodeDelta(filepath.Join(*deltas, fmt.Sprintf("%d.msgp", r)))
		if err != nil {
			return err
		}
		n, b := map[byte]int{}, map[byte]int{}
		del := 0
		for _, ch := range timetravel.Changes(sd) {
			n[ch.Key[0]]++
			b[ch.Key[0]] += len(ch.Value)
			if ch.Deleted {
				del++
			}
		}
		fmt.Print(r)
		for _, k := range kinds {
			fmt.Printf(",%d,%d", n[k], b[k])
		}
		hdr := 0
		if sd.Hdr != nil {
			hdr = len(protocol.Encode(sd.Hdr))
		}
		fmt.Printf(",%d,%d,%d", del, hdr, len(sd.Txids))
		if raw, err := os.ReadFile(filepath.Join(*blocks, fmt.Sprintf("%d.msgp", r))); err == nil {
			var ebc rpcs.EncodedBlockCert
			if err := protocol.DecodeReflect(raw, &ebc); err != nil {
				return err
			}
			fmt.Printf(",%d,%d,%d", len(raw), len(protocol.Encode(&ebc.Block.BlockHeader)), len(protocol.Encode(&ebc.Certificate)))
		} else {
			fmt.Print(",,,")
		}
		fmt.Println()
	}
	return nil
}
