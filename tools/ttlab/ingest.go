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
	"context"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"time"

	"github.com/cockroachdb/pebble/v2"
	"github.com/cockroachdb/pebble/v2/sstable"

	"github.com/algorand/go-algorand/ledger/timetravel"
)

type query struct {
	obj []byte
	rnd uint64
}

func compressionByName(name string) (*sstable.CompressionProfile, error) {
	switch name {
	case "zstd":
		return sstable.ZstdCompression, nil
	case "snappy":
		return sstable.SnappyCompression, nil
	case "minlz":
		return sstable.MinLZCompression, nil
	case "none":
		return sstable.NoCompression, nil
	}
	return nil, fmt.Errorf("unknown compression %q", name)
}

func ingest(args []string) error {
	fs := flag.NewFlagSet("ingest", flag.ExitOnError)
	data := fs.String("data", "", "delta directory")
	from := fs.Uint64("from", 0, "first round")
	to := fs.Uint64("to", 0, "last round")
	dbPath := fs.String("db", "", "store directory (recreated)")
	n := fs.Uint64("n", 1, "checkpoint interval")
	inc := fs.Bool("inc", false, "incremental patches")
	comp := fs.String("comp", "zstd", "zstd|snappy|minlz|none")
	block := fs.Int("block", 32<<10, "sstable block size")
	hdrs := fs.Bool("hdr", false, "store block headers")
	warm := fs.Uint64("warm", 0, "rounds excluded from steady-state accounting")
	nq := fs.Int("q", 20000, "read queries per class")
	cacheMB := fs.Int64("cache", 256, "block cache MB")
	keep := fs.Bool("keep", false, "keep the store")
	fs.Parse(args)

	cp, err := compressionByName(*comp)
	if err != nil {
		return err
	}
	os.RemoveAll(*dbPath)
	s, err := timetravel.Open(*dbPath, timetravel.Options{CheckpointInterval: *n, Incremental: *inc, Compression: cp, BlockSize: *block, CacheSize: *cacheMB << 20})
	if err != nil {
		return err
	}
	defer func() {
		s.Close()
		if !*keep {
			os.RemoveAll(*dbPath)
		}
	}()

	rng := rand.New(rand.NewSource(int64(*n)))
	const reservoir = 200000
	var hot, uniform []query
	var nChanges, nDistinct int
	seen := map[string]bool{}
	var tWrite, tCkpt, maxRound time.Duration
	var nCkpt int
	start := time.Now()
	for rd := range load(*data, *from, *to) {
		if rd.err != nil {
			return rd.err
		}
		hdr := rd.hdr
		if !*hdrs {
			hdr = nil
		}
		t0 := time.Now()
		if err := s.AddRound(rd.rnd, rd.changes, hdr, nil); err != nil {
			return err
		}
		d := time.Since(t0)
		tWrite += d
		maxRound = max(maxRound, d)
		if rd.rnd%*n == 0 {
			tCkpt += d
			nCkpt++
		}
		for _, ch := range rd.changes {
			nChanges++
			if len(hot) < reservoir {
				hot = append(hot, query{ch.Key, rd.rnd})
			} else if i := rng.Intn(nChanges); i < reservoir {
				hot[i] = query{ch.Key, rd.rnd}
			}
			if !seen[string(ch.Key)] {
				seen[string(ch.Key)] = true
				nDistinct++
				if len(uniform) < reservoir {
					uniform = append(uniform, query{ch.Key, rd.rnd})
				} else if i := rng.Intn(nDistinct); i < reservoir {
					uniform[i] = query{ch.Key, rd.rnd}
				}
			}
		}
	}
	wall := time.Since(start)
	if err := s.Sync(); err != nil {
		return err
	}
	rounds := *to - *from + 1
	res := map[string]any{
		"from": *from, "to": *to, "n": *n, "inc": *inc, "comp": *comp, "block": *block, "hdr": *hdrs,
		"rounds": rounds, "changes": nChanges, "distinct": nDistinct,
		"write_us_per_round":      float64(tWrite.Microseconds()) / float64(rounds),
		"ckpt_ms_mean":            float64(tCkpt.Microseconds()) / 1000 / float64(max(nCkpt, 1)),
		"max_round_ms":            float64(maxRound.Microseconds()) / 1000,
		"wall_s":                  wall.Seconds(),
		"size_before_compact_mib": float64(dirSize(*dbPath)) / (1 << 20),
	}

	t0 := time.Now()
	if err := s.DB().Compact(context.Background(), []byte{0}, []byte{0xff}, true); err != nil {
		return err
	}
	res["compact_s"] = time.Since(t0).Seconds()
	if err := s.DB().Flush(); err != nil {
		return err
	}
	disk := s.DB().Metrics().Total().TablesSize // live sstables only (no WAL/obsolete files)
	res["disk_mib"] = float64(disk) / (1 << 20)

	// logical bytes by keyspace/kind, overall and for the steady state
	logical, steady, steadyN := map[string]int64{}, map[string]int64{}, map[string]int64{}
	var total, steadyTotal int64
	it, err := s.DB().NewIter(nil)
	if err != nil {
		return err
	}
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		sz := int64(len(k) + len(it.Value()))
		total += sz
		cls := string(k[:1])
		if (k[0] == 'c' || k[0] == 'p') && len(k) > 9 {
			cls += string(k[1:2])
		}
		logical[cls] += sz
		if len(k) >= 9 && k[0] != 'm' && binary.BigEndian.Uint64(k[len(k)-8:]) >= *from+*warm {
			steady[cls] += sz
			steadyN[cls]++
			steadyTotal += sz
		}
	}
	it.Close()
	ratio := float64(disk) / float64(total)
	steadyRounds := rounds - *warm
	res["logical_mib"] = float64(total) / (1 << 20)
	res["compression_ratio"] = 1 / ratio
	res["steady_logical_bytes_per_round"] = float64(steadyTotal) / float64(steadyRounds)
	res["steady_disk_bytes_per_round"] = float64(steadyTotal) * ratio / float64(steadyRounds)
	res["steady_by_class"] = steady
	res["steady_records_by_class"] = steadyN
	res["logical_by_class"] = logical

	last := *to
	for _, class := range []struct {
		name string
		qs   []query
	}{{"hot", hot}, {"uniform", uniform}} {
		if *nq == 0 || len(class.qs) == 0 {
			continue
		}
		var lat latencies
		var depths []int
		var chain, found int
		for i := 0; i < *nq; i++ {
			q := class.qs[rng.Intn(len(class.qs))]
			if q.rnd < *from+*warm {
				q.rnd = *from + *warm
			}
			r := q.rnd + uint64(rng.Int63n(int64(last-q.rnd+1)))
			t0 := time.Now()
			_, ok, err := s.Get(q.obj, r)
			lat = append(lat, time.Since(t0))
			if err != nil {
				return err
			}
			if ok {
				found++
			}
			d, c := depth(s, q.obj, r)
			depths = append(depths, d)
			chain += c
		}
		sum := lat.summary()
		sum["found_frac"] = float64(found) / float64(*nq)
		sum["patches_in_window_mean"] = float64(chain) / float64(*nq)
		hist := map[string]float64{}
		for _, d := range depths {
			switch {
			case d < 0:
				hist["none"]++
			case d == 0:
				hist["0"]++
			case d <= 3:
				hist["1-3"]++
			case d <= 15:
				hist["4-15"]++
			case d <= 63:
				hist["16-63"]++
			default:
				hist["64+"]++
			}
		}
		for k := range hist {
			hist[k] /= float64(len(depths))
		}
		res["read_"+class.name] = sum
		res["ckpt_depth_"+class.name] = hist
	}
	emit(res)
	return nil
}

// depth reports how many sparse checkpoints back the base of obj@rnd is
// (-1: none) and how many patches sit in rnd's window.
func depth(s *timetravel.Store, obj []byte, rnd uint64) (int, int) {
	n := s.CheckpointInterval()
	c := rnd - rnd%n
	key := func(pfx byte, r uint64) []byte {
		return binary.BigEndian.AppendUint64(append([]byte{pfx}, obj...), r)
	}
	d := -1
	if it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: key('c', 0), UpperBound: key('c', c+1)}); err == nil {
		if it.Last() {
			d = int((c - binary.BigEndian.Uint64(it.Key()[len(it.Key())-8:])) / n)
		}
		it.Close()
	}
	chain := 0
	if it, err := s.DB().NewIter(&pebble.IterOptions{LowerBound: key('p', c+1), UpperBound: key('p', rnd+1)}); err == nil {
		for it.First(); it.Valid(); it.Next() {
			chain++
		}
		it.Close()
	}
	return d, chain
}
