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
	"errors"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ExitOnError)
	dataDir := fs.String("d", "", "data directory of a stopped node (its genesis.json and config.json say where the history is)")
	deltas := fs.String("deltas", "", "algod API URL serving /v2/deltas for the sampled rounds (an archival node)")
	sample := fs.Int("sample", 0, "rounds to compare with -deltas")
	seed := fs.Int64("seed", 0, "seed of the sampled rounds; 0: random")
	clear := fs.Bool("clear-diverged", false, "if the checks pass, remove the mark algod leaves on a history it found to differ from the chain")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap verify -d <data dir> [-deltas <algod URL> -sample N]")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if *dataDir == "" || fs.NArg() != 0 || (*sample > 0) != (*deltas != "") {
		fs.Usage()
		os.Exit(2)
	}
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(*dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfigFromDisk(*dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := cfg.TimeTravelGenesisDir(*dataDir, genesis.ID())
	if err != nil {
		return err
	}
	st, err := timetravel.Open(filepath.Join(dir, historyDir), timetravel.Options{ReadOnly: !*clear})
	if err != nil {
		return err
	}
	defer st.Close()
	if reason, found, derr := st.Diverged(); derr != nil {
		return derr
	} else if found {
		fmt.Printf("algod marked this history as differing from the chain: %s\n", reason)
	}
	last, err := verifyHeaders(st, genesis.Hash(), os.Stderr)
	if err != nil {
		return err
	}
	fmt.Printf("the headers of rounds 0..%d chain from the genesis block\n", last)
	if *sample > 0 {
		if *seed == 0 {
			*seed = time.Now().UnixNano()
		}
		if err := verifySample(st, *deltas, *sample, *seed, &http.Client{Timeout: time.Minute}, os.Stderr); err != nil {
			return err
		}
		fmt.Printf("%d sampled rounds (seed %d) match %s\n", *sample, *seed, *deltas)
	}
	if *clear {
		if err := st.ClearDiverged(); err != nil {
			return err
		}
		fmt.Println("cleared the diverged mark: algod verifies again only the rounds it replays")
	}
	return nil
}

// verifyHeaders checks that the store holds a header for every round from 0
// to its last, of the network with this genesis hash, each one hash-chained
// to the one before. With the network's own block after the last round
// chaining onto it, which the node checks as it catches up, every header is
// the chain's.
func verifyHeaders(st *timetravel.Store, genesisHash crypto.Digest, progress io.Writer) (uint64, error) {
	last, ok := st.Last()
	if !ok {
		return 0, errors.New("the history is empty")
	}
	it, err := st.DB().NewIter(&pebble.IterOptions{LowerBound: []byte{'H'}, UpperBound: []byte{'H' + 1}})
	if err != nil {
		return 0, err
	}
	defer it.Close()
	var prev bookkeeping.BlockHash
	next := uint64(0)
	start := time.Now()
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if len(k) != 9 {
			return 0, fmt.Errorf("bad header key %x", k)
		}
		r := binary.BigEndian.Uint64(k[1:])
		if r != next {
			return 0, fmt.Errorf("the history has no header for round %d", next)
		}
		v := it.Value()
		if len(v) < timetravel.DigestLen {
			return 0, fmt.Errorf("round %d: short header record", r)
		}
		var hdr bookkeeping.BlockHeader
		if err := protocol.Decode(v[timetravel.DigestLen:], &hdr); err != nil {
			return 0, fmt.Errorf("round %d: %w", r, err)
		}
		switch {
		case uint64(hdr.Round) != r:
			return 0, fmt.Errorf("round %d: the header is of round %d", r, hdr.Round)
		case hdr.GenesisHash != genesisHash:
			return 0, fmt.Errorf("round %d: the header is of another network", r)
		case r > 0 && hdr.Branch != prev:
			return 0, fmt.Errorf("round %d: the header does not chain onto round %d's", r, r-1)
		}
		prev = hdr.Hash()
		next++
		if progress != nil && next%1_000_000 == 0 {
			rate := float64(next) / time.Since(start).Seconds()
			fmt.Fprintf(progress, "checked headers up to round %d of %d (%d%%), %.0f rounds/s, about %s left\n",
				r, last, min(r, last)*100/max(last, 1), rate, roundDuration(time.Duration(float64(last-min(r, last))/rate*float64(time.Second))))
		}
	}
	if err := it.Error(); err != nil {
		return 0, err
	}
	if next != last+1 {
		return 0, fmt.Errorf("the history has headers up to round %d, its last round is %d", next-1, last)
	}
	return last, nil
}

// verifySample compares n random rounds of the store with their state deltas
// as the algod at api serves them: the round's digest and the stored value of
// every object the delta changes. It reports its progress to progress, if
// not nil.
func verifySample(st *timetravel.Store, api string, n int, seed int64, client *http.Client, progress io.Writer) error {
	last, _ := st.Last()
	if last == 0 {
		return errors.New("the history has no rounds to sample")
	}
	rng := rand.New(rand.NewSource(seed))
	start, reported := time.Now(), time.Now()
	for i := 0; i < n; i++ {
		if progress != nil && i > 0 && time.Since(reported) >= progressEvery {
			reported = time.Now()
			fmt.Fprintf(progress, "compared %d of %d sampled rounds with %s, about %s left\n",
				i, n, api, roundDuration(time.Since(start)/time.Duration(i)*time.Duration(n-i)))
		}
		r := 1 + uint64(rng.Int63n(int64(last)))
		sd, err := fetchDelta(client, api, r)
		if err != nil {
			return err
		}
		if err := st.VerifyChanges(r, timetravel.Changes(sd)); err != nil {
			return err
		}
	}
	return nil
}

func fetchDelta(client *http.Client, api string, rnd uint64) (*ledgercore.StateDelta, error) {
	u := fmt.Sprintf("%s/v2/deltas/%d?format=msgpack", strings.TrimSuffix(api, "/"), rnd)
	var err error
	for try := range 3 {
		if try > 0 {
			time.Sleep(time.Second)
		}
		var body []byte
		if body, err = getSmall(context.Background(), client, u, 256<<20); err != nil {
			continue
		}
		var sd ledgercore.StateDelta
		if err = protocol.DecodeReflect(body, &sd); err != nil {
			return nil, fmt.Errorf("GET %s: %w", u, err)
		}
		return &sd, nil
	}
	return nil, err
}
