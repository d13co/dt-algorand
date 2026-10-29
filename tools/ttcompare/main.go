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

// ttcompare spot-checks that two time travel stores, built by two nodes of
// the same network, hold the same history. It compares sampled rounds
// (header, change digest, expired stake) and sampled objects (value at a
// round, versions over a range of rounds) within the rounds both stores
// have. Records that repeat their object's previous state (no-op writes,
// which one store may have skipped or removed and the other kept) are not
// versions, so stores that differ only by them compare equal.
//
// The stores are opened read-only but locked: stop their algods, or compare
// copies (e.g. btrfs snapshots or reflink copies).
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

func main() {
	rounds := flag.Int("rounds", 1000, "rounds to sample: header, change digest and expired stake")
	samples := flag.Int("objects", 200, "random starting keys to sample objects from")
	window := flag.Int("window", 20, "consecutive objects compared from each starting key")
	span := flag.Uint64("span", 100_000, "rounds of each object's history whose versions are compared")
	maxVersions := flag.Int("maxversions", 5000, "versions compared per object at most")
	seed := flag.Uint64("seed", 0, "random seed; 0 picks one")
	maxDiffs := flag.Int("maxdiffs", 50, "differences printed at most")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <data dir or time travel store> <data dir or time travel store>\n", os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 2 {
		flag.Usage()
		os.Exit(2)
	}
	if *seed == 0 {
		*seed = uint64(time.Now().UnixNano())
	}
	c := &comparer{
		rng:         rand.New(rand.NewPCG(*seed, *seed)),
		span:        *span,
		maxVersions: *maxVersions,
		maxDiffs:    *maxDiffs,
		seenObjs:    map[string]bool{},
	}
	fmt.Printf("seed %d\n", *seed)
	err := c.run(flag.Arg(0), flag.Arg(1), *rounds, *samples, *window)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if c.diffs > 0 {
		os.Exit(1)
	}
}

type comparer struct {
	a, b   *timetravel.Store
	lo, hi uint64
	maxID  uint64 // largest creatable ID at hi

	rng         *rand.Rand
	span        uint64
	maxVersions int
	maxDiffs    int

	seenObjs                   map[string]bool
	roundsChecked, objsChecked int
	versionsChecked            int
	diffs                      int
}

func (c *comparer) diff(format string, args ...any) {
	c.diffs++
	if c.diffs <= c.maxDiffs {
		fmt.Printf("DIFF "+format+"\n", args...)
	} else if c.diffs == c.maxDiffs+1 {
		fmt.Println("(further differences not printed)")
	}
}

func (c *comparer) run(pathA, pathB string, rounds, samples, window int) error {
	var err error
	if c.a, err = openStore(pathA); err != nil {
		return err
	}
	defer c.a.Close()
	if c.b, err = openStore(pathB); err != nil {
		return err
	}
	defer c.b.Close()
	if err = c.commonRange(); err != nil {
		return err
	}

	start := time.Now()
	if err = c.compareTotals(); err != nil {
		return err
	}
	// one round sample at each end, where the stores' histories meet their gaps
	for _, r := range []uint64{c.lo, c.hi} {
		if err = c.compareRound(r); err != nil {
			return err
		}
	}
	seenRounds := map[uint64]bool{c.lo: true, c.hi: true}
	for i := 0; i < rounds && uint64(len(seenRounds)) <= c.hi-c.lo; i++ {
		r := c.randRound()
		if seenRounds[r] {
			i--
			continue
		}
		seenRounds[r] = true
		if err = c.compareRound(r); err != nil {
			return err
		}
	}
	for i := 0; i < samples; i++ {
		if err = c.compareWindow(window); err != nil {
			return err
		}
	}
	fmt.Printf("compared %d rounds, %d objects (%d versions) in %v: %d differences\n",
		c.roundsChecked, c.objsChecked, c.versionsChecked, time.Since(start).Round(time.Millisecond), c.diffs)
	return nil
}

// openStore opens, read-only, the time travel store at path or that of the
// data directory at path.
func openStore(path string) (*timetravel.Store, error) {
	if _, err := os.Stat(filepath.Join(path, config.GenesisJSONFile)); err == nil {
		genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(path, config.GenesisJSONFile))
		if err != nil {
			return nil, err
		}
		cfg, err := config.LoadConfigFromDisk(path)
		if err != nil && !os.IsNotExist(err) {
			return nil, err
		}
		dirs, err := cfg.EnsureAndResolveGenesisDirs(path, genesis.ID(), logging.Base())
		if err != nil {
			return nil, err
		}
		path = ledger.TimeTravelPath(ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs})
	}
	if _, err := os.Stat(path); err != nil {
		return nil, fmt.Errorf("no time travel store: %w", err)
	}
	s, err := timetravel.Open(path, timetravel.Options{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w (is its algod running?)", path, err)
	}
	return s, nil
}

func (c *comparer) commonRange() error {
	var firsts, lasts [2]uint64
	for i, s := range []*timetravel.Store{c.a, c.b} {
		first, ok, err := s.First()
		if err != nil {
			return err
		}
		last, lok := s.Last()
		if !ok || !lok {
			return fmt.Errorf("store %c is empty", 'A'+i)
		}
		firsts[i], lasts[i] = first, last
		fmt.Printf("store %c: rounds %d..%d\n", 'A'+i, first, last)
	}
	c.lo, c.hi = max(firsts[0], firsts[1]), min(lasts[0], lasts[1])
	if c.lo > c.hi {
		return fmt.Errorf("the stores have no rounds in common")
	}
	fmt.Printf("comparing rounds %d..%d\n", c.lo, c.hi)

	enc, ok, err := c.a.Header(c.hi)
	if err != nil || !ok {
		return fmt.Errorf("store A has no header for round %d: %v", c.hi, err)
	}
	var hdr bookkeeping.BlockHeader
	if err = protocol.Decode(enc, &hdr); err != nil {
		return err
	}
	c.maxID = hdr.TxnCounter
	return nil
}

func (c *comparer) randRound() uint64 { return c.lo + c.rng.Uint64N(c.hi-c.lo+1) }

func (c *comparer) compareRound(r uint64) error {
	c.roundsChecked++
	ha, oka, err := c.a.Header(r)
	if err != nil {
		return err
	}
	hb, okb, err := c.b.Header(r)
	if err != nil {
		return err
	}
	if oka != okb || !bytes.Equal(ha, hb) {
		c.diff("round %d: headers differ (A: %d bytes, present %v; B: %d bytes, present %v)", r, len(ha), oka, len(hb), okb)
	}
	da, oka, err := c.a.Digest(r)
	if err != nil {
		return err
	}
	db, okb, err := c.b.Digest(r)
	if err != nil {
		return err
	}
	if oka != okb || !bytes.Equal(da, db) {
		c.diff("round %d: change digests differ (A: %x, B: %x)", r, da, db)
	}
	ea, oka, err := c.a.ExpiredStake(r)
	if err != nil {
		return err
	}
	eb, okb, err := c.b.ExpiredStake(r)
	if err != nil {
		return err
	}
	if oka != okb || ea != eb {
		c.diff("round %d: expired stake differs (A: %d present %v, B: %d present %v)", r, ea, oka, eb, okb)
	}
	return nil
}

// compareTotals compares the totals, a single object that no window samples
// and that changes in every round.
func (c *comparer) compareTotals() error {
	return c.compareObject(timetravel.TotalsKey)
}

// sampledKinds are the object kinds compareWindow starts from, with the
// random key after which it lists objects.
var sampledKinds = []byte{
	timetravel.KindAccount, timetravel.KindHolding, timetravel.KindLocal,
	timetravel.KindAssetParams, timetravel.KindAppParams, timetravel.KindKv, timetravel.KindCreated,
}

func (c *comparer) randomAfter(kind byte) []byte {
	switch kind {
	case timetravel.KindAssetParams, timetravel.KindAppParams:
		return binary.BigEndian.AppendUint64([]byte{kind}, c.rng.Uint64N(c.maxID+1))
	case timetravel.KindKv: // boxes: "bx:" app ID name
		app := binary.BigEndian.AppendUint64([]byte("bx:"), c.rng.Uint64N(c.maxID+1))
		return timetravel.KvPrefix(string(app))
	default: // address first
		var addr basics.Address
		for i := range addr {
			addr[i] = byte(c.rng.Uint32())
		}
		return append([]byte{kind}, addr[:]...)
	}
}

// compareWindow compares the window objects of a random kind after a random
// key, as listed by either store.
func (c *comparer) compareWindow(window int) error {
	kind := sampledKinds[c.rng.IntN(len(sampledKinds))]
	after := c.randomAfter(kind)
	la, endA, err := listObjects(c.a, []byte{kind}, after, window)
	if err != nil {
		return err
	}
	lb, endB, err := listObjects(c.b, []byte{kind}, after, window)
	if err != nil {
		return err
	}
	// Each list is complete up to its last object (or to the end of the
	// kind, if shorter than window): compare the union up to the smaller
	// bound. Objects listed by one store only are fine if they have no
	// versions, such as the tombstones of objects created and deleted in one
	// round that dedup drops.
	bound := endA
	if bound == nil || (endB != nil && bytes.Compare(endB, bound) < 0) {
		bound = endB
	}
	for _, obj := range mergeUnique(la, lb) {
		if bound != nil && bytes.Compare(obj, bound) > 0 {
			break
		}
		if c.seenObjs[string(obj)] {
			continue
		}
		c.seenObjs[string(obj)] = true
		if err = c.compareObject(obj); err != nil {
			return err
		}
	}
	return nil
}

// listObjects returns up to n objects of s with prefix after after, and the
// last of them if there may be more.
func listObjects(s *timetravel.Store, prefix, after []byte, n int) (objs [][]byte, end []byte, err error) {
	err = s.ObjectsAfter(prefix, after, func(obj []byte) bool {
		objs = append(objs, obj)
		return len(objs) < n
	})
	if len(objs) == n {
		end = objs[n-1]
	}
	return objs, end, err
}

func mergeUnique(a, b [][]byte) [][]byte {
	out := make([][]byte, 0, len(a)+len(b))
	for len(a) > 0 || len(b) > 0 {
		switch {
		case len(b) == 0 || (len(a) > 0 && bytes.Compare(a[0], b[0]) < 0):
			out, a = append(out, a[0]), a[1:]
		case len(a) == 0 || bytes.Compare(b[0], a[0]) < 0:
			out, b = append(out, b[0]), b[1:]
		default:
			out, a, b = append(out, a[0]), a[1:], b[1:]
		}
	}
	return out
}

type version struct {
	rnd uint64
	val []byte
	ok  bool
}

// compareObject compares obj's value at a random round and its versions over
// span rounds around it.
func (c *comparer) compareObject(obj []byte) error {
	c.objsChecked++
	r := c.randRound()
	va, oka, err := c.a.Get(obj, r)
	if err != nil {
		return err
	}
	vb, okb, err := c.b.Get(obj, r)
	if err != nil {
		return err
	}
	if oka != okb || !bytes.Equal(va, vb) {
		c.diff("%s at round %d: A %s, B %s", describe(obj), r, showValue(va, oka), showValue(vb, okb))
	}

	lo := max(c.lo, r-min(r, c.span/2))
	hi := min(c.hi, lo+c.span)
	ca, err := c.versions(c.a, obj, lo, hi)
	if err != nil {
		return err
	}
	cb, err := c.versions(c.b, obj, lo, hi)
	if err != nil {
		return err
	}
	c.versionsChecked += max(len(ca), len(cb))
	for i := 0; i < max(len(ca), len(cb)); i++ {
		switch {
		case i >= len(ca):
			c.diff("%s: B has an extra version at round %d (%s) in rounds %d..%d", describe(obj), cb[i].rnd, showValue(cb[i].val, cb[i].ok), lo, hi)
		case i >= len(cb):
			c.diff("%s: A has an extra version at round %d (%s) in rounds %d..%d", describe(obj), ca[i].rnd, showValue(ca[i].val, ca[i].ok), lo, hi)
		case ca[i].rnd != cb[i].rnd || ca[i].ok != cb[i].ok || !bytes.Equal(ca[i].val, cb[i].val):
			c.diff("%s: version %d of rounds %d..%d: A round %d %s, B round %d %s", describe(obj), i, lo, hi,
				ca[i].rnd, showValue(ca[i].val, ca[i].ok), cb[i].rnd, showValue(cb[i].val, cb[i].ok))
		default:
			continue
		}
		return nil // later versions are offset: one difference per object
	}
	return nil
}

func (c *comparer) versions(s *timetravel.Store, obj []byte, lo, hi uint64) ([]version, error) {
	var vs []version
	err := s.Versions(obj, lo, hi, false, func(rnd uint64, val []byte, ok bool) bool {
		vs = append(vs, version{rnd, val, ok})
		return len(vs) < c.maxVersions
	})
	return vs, err
}

func showValue(v []byte, ok bool) string {
	if !ok {
		return "absent"
	}
	if len(v) > 48 {
		return fmt.Sprintf("%d bytes %s…", len(v), hex.EncodeToString(v[:48]))
	}
	return fmt.Sprintf("%d bytes %s", len(v), hex.EncodeToString(v))
}

// describe renders an object key for humans.
func describe(obj []byte) string {
	addr := func(b []byte) string {
		var a basics.Address
		copy(a[:], b)
		return a.String()
	}
	id := func(b []byte) uint64 { return binary.BigEndian.Uint64(b) }
	switch k := obj[0]; {
	case k == timetravel.KindTotals:
		return "totals"
	case k == timetravel.KindAccount && len(obj) == 33:
		return "account " + addr(obj[1:])
	case k == timetravel.KindHolding && len(obj) == 41:
		return fmt.Sprintf("holding %s asset %d", addr(obj[1:33]), id(obj[33:]))
	case k == timetravel.KindLocal && len(obj) == 41:
		return fmt.Sprintf("local state %s app %d", addr(obj[1:33]), id(obj[33:]))
	case k == timetravel.KindAssetParams && len(obj) == 9:
		return fmt.Sprintf("asset params %d", id(obj[1:]))
	case k == timetravel.KindAppParams && len(obj) == 9:
		return fmt.Sprintf("app params %d", id(obj[1:]))
	case k == timetravel.KindCreated && len(obj) == 42:
		return fmt.Sprintf("creatable %s type %d id %d", addr(obj[1:33]), obj[33], id(obj[34:]))
	case k == timetravel.KindKv:
		key := timetravel.KvKeyString(obj)
		if len(key) >= 11 && key[:3] == "bx:" {
			return fmt.Sprintf("box app %d name %q", id([]byte(key[3:11])), key[11:])
		}
		return fmt.Sprintf("kv %q", key)
	}
	return "object " + hex.EncodeToString(obj)
}
