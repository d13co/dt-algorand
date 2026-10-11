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
	"archive/tar"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/encoded"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// catchpoint reads (a prefix of) a catchpoint tar stream as served by a relay
// and reports its header plus the average encoded size of accounts/resources/kvs.
func catchpoint(args []string) error {
	fs := flag.NewFlagSet("catchpoint", flag.ExitOnError)
	path := fs.String("file", "", "catchpoint tar (prefix ok)")
	dbPath := fs.String("db", "", "if set, write the sampled state as one full checkpoint into this store")
	fs.Parse(args)
	var changes []timetravel.Change
	f, err := os.Open(*path)
	if err != nil {
		return err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	res := map[string]any{}
	var accts, resources, kvs, acctBytes, kvBytes, chunks int
	for {
		h, err := tr.Next()
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
				res["stopped"] = err.Error()
			}
			break
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			break // truncated prefix
		}
		switch {
		case h.Name == "content.msgpack":
			var hdr ledger.CatchpointFileHeader
			if err := protocol.Decode(b, &hdr); err != nil {
				return err
			}
			res["header"] = hdr
		case strings.HasPrefix(h.Name, "balances."):
			var c ledger.CatchpointSnapshotChunkV6
			if err := protocol.Decode(b, &c); err != nil {
				return err
			}
			chunks++
			for _, br := range c.Balances {
				if *dbPath != "" {
					changes = append(changes, catchpointChanges(br)...)
				}
				accts++
				resources += len(br.Resources)
				acctBytes += len(br.AccountData)
				for _, r := range br.Resources {
					acctBytes += len(r)
				}
			}
			for _, kv := range c.KVs {
				kvs++
				kvBytes += len(kv.Key) + len(kv.Value)
			}
		}
	}
	res["chunks_read"], res["accounts_read"], res["resources_read"], res["kvs_read"] = chunks, accts, resources, kvs
	if accts > 0 {
		res["bytes_per_account_incl_resources"] = float64(acctBytes) / float64(accts)
	}
	if kvs > 0 {
		res["bytes_per_kv"] = float64(kvBytes) / float64(kvs)
	}
	if *dbPath != "" {
		os.RemoveAll(*dbPath)
		s, err := timetravel.Open(*dbPath, timetravel.Options{BlockSize: 128 << 10})
		if err != nil {
			return err
		}
		if err := s.AddRound(0, changes, nil); err != nil {
			return err
		}
		if err := s.DB().Compact(context.Background(), []byte{0}, []byte{0xff}, true); err != nil {
			return err
		}
		disk := s.DB().Metrics().Total().TablesSize
		var logical int
		for _, ch := range changes {
			logical += len(ch.Key) + len(ch.Value) + 10
		}
		res["ckpt_objects"], res["ckpt_logical_bytes_per_account"], res["ckpt_disk_bytes_per_account"] = len(changes), float64(logical)/float64(accts), float64(disk)/float64(accts)
		s.Close()
		os.RemoveAll(*dbPath)
	}
	emit(res)
	return nil
}

// catchpointChanges converts a catchpoint balance record into time travel objects.
func catchpointChanges(br encoded.BalanceRecordV6) []timetravel.Change {
	out := []timetravel.Change{{Key: timetravel.AccountKey(br.Address), Value: br.AccountData}}
	for cidx, raw := range br.Resources {
		var rd trackerdb.ResourcesData
		if err := protocol.Decode(raw, &rd); err != nil {
			continue
		}
		if rd.IsAsset() {
			if rd.IsHolding() {
				h := rd.GetAssetHolding()
				out = append(out, timetravel.Change{Key: timetravel.HoldingKey(br.Address, basics.AssetIndex(cidx)), Value: protocol.Encode(&h)})
			}
			if rd.IsOwning() {
				p := rd.GetAssetParams()
				out = append(out, timetravel.Change{Key: timetravel.AssetParamsKey(basics.AssetIndex(cidx)), Value: append(br.Address[:], protocol.Encode(&p)...)},
					timetravel.Change{Key: timetravel.CreatedKey(br.Address, basics.AssetCreatable, basics.CreatableIndex(cidx)), Value: []byte{}})
			}
		} else {
			if rd.IsHolding() {
				ls := rd.GetAppLocalState()
				out = append(out, timetravel.Change{Key: timetravel.LocalKey(br.Address, basics.AppIndex(cidx)), Value: protocol.Encode(&ls)})
			}
			if rd.IsOwning() {
				p := rd.GetAppParams()
				out = append(out, timetravel.Change{Key: timetravel.AppParamsKey(basics.AppIndex(cidx)), Value: append(br.Address[:], protocol.Encode(&p)...)},
					timetravel.Change{Key: timetravel.CreatedKey(br.Address, basics.AppCreatable, basics.CreatableIndex(cidx)), Value: []byte{}})
			}
		}
	}
	return out
}
