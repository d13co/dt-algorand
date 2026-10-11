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

// ttlab benchmarks time-travel storage designs against real mainnet state
// deltas (as served by GET /v2/deltas/{round}?format=msgpack, one file per
// round named <round>.msgp).
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ttlab stats|ingest|engines|catchpoint|demo [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "stats":
		err = stats(os.Args[2:])
	case "ingest":
		err = ingest(os.Args[2:])
	case "engines":
		err = engines(os.Args[2:])
	case "catchpoint":
		err = catchpoint(os.Args[2:])
	case "demo":
		err = demo(os.Args[2:])
	default:
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

type roundData struct {
	rnd     uint64
	changes []timetravel.Change
	hdr     []byte
	err     error
}

func decodeDelta(path string) (*ledgercore.StateDelta, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var sd ledgercore.StateDelta
	if err := protocol.DecodeReflect(b, &sd); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &sd, nil
}

// load decodes the deltas of rounds [from, to] in parallel, delivering them in order.
func load(dir string, from, to uint64) <-chan roundData {
	out := make(chan roundData, 64)
	queue := make(chan chan roundData, 512)
	sem := make(chan struct{}, runtime.NumCPU())
	go func() {
		for r := from; r <= to; r++ {
			ch := make(chan roundData, 1)
			queue <- ch
			sem <- struct{}{}
			go func(r uint64) {
				defer func() { <-sem }()
				sd, err := decodeDelta(filepath.Join(dir, fmt.Sprintf("%d.msgp", r)))
				if err != nil {
					ch <- roundData{rnd: r, err: err}
					return
				}
				rd := roundData{rnd: r, changes: timetravel.Changes(sd)}
				if sd.Hdr != nil {
					rd.hdr = protocol.Encode(sd.Hdr)
				}
				ch <- rd
			}(r)
		}
		close(queue)
	}()
	go func() {
		for ch := range queue {
			out <- <-ch
		}
		close(out)
	}()
	return out
}

func emit(v any) {
	b, _ := json.Marshal(v)
	fmt.Println(string(b))
}

func dirSize(dir string) int64 {
	var n int64
	filepath.Walk(dir, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			n += info.Size()
		}
		return nil
	})
	return n
}

type latencies []time.Duration

func (l latencies) summary() map[string]float64 {
	if len(l) == 0 {
		return nil
	}
	sort.Slice(l, func(i, j int) bool { return l[i] < l[j] })
	var sum time.Duration
	for _, d := range l {
		sum += d
	}
	us := func(d time.Duration) float64 { return float64(d.Nanoseconds()) / 1000 }
	return map[string]float64{
		"mean_us": us(sum / time.Duration(len(l))),
		"p50_us":  us(l[len(l)/2]),
		"p90_us":  us(l[len(l)*9/10]),
		"p99_us":  us(l[len(l)*99/100]),
		"max_us":  us(l[len(l)-1]),
	}
}
