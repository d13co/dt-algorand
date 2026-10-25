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
	"bytes"
	"cmp"
	"container/heap"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/cockroachdb/pebble/v2"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/cmd/util/ttview"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// The store's record layout (ledger/timetravel/store.go): versioned records
// are keyspace, object key, big-endian round; their values start with a tag,
// 'F' (full value), 'D' (deleted) or 'X' (patch).
const (
	recDeleted = 'D'
	recPatch   = 'X'
)

// progressInterval is how often a long scan reports on stderr.
const progressInterval = 10 * time.Second

// splitRecord splits a versioned record key into its object key and round.
func splitRecord(k []byte) ([]byte, uint64, bool) {
	if len(k) < 1+1+8 {
		return nil, 0, false
	}
	return k[1 : len(k)-8], binary.BigEndian.Uint64(k[len(k)-8:]), true
}

// kindStats are the statistics of one kind of objects in one keyspace.
type kindStats struct {
	Keyspace   string  `json:"keyspace"`
	Kind       string  `json:"kind"`
	Name       string  `json:"name"`
	Records    uint64  `json:"records"`
	Objects    uint64  `json:"objects"`
	Tombstones uint64  `json:"tombstones"`
	Patches    uint64  `json:"patches,omitempty"`
	Noops      *uint64 `json:"noops,omitempty"` // N = 1 only
	KeyBytes   uint64  `json:"key-bytes"`
	ValueBytes uint64  `json:"value-bytes"`
	MostRecs   uint64  `json:"most-records"`
	MostObject string  `json:"most-records-object"`
}

type objCount struct {
	Object  string `json:"object"`
	Records uint64 `json:"records"`
}

// topHeap keeps the objects with the most records, the fewest on top.
type topHeap []objCount

func (h topHeap) Len() int           { return len(h) }
func (h topHeap) Less(i, j int) bool { return h[i].Records < h[j].Records }
func (h topHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *topHeap) Push(x any)        { *h = append(*h, x.(objCount)) }
func (h *topHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

func cmdStats(e *env, args []string) error {
	fs := e.newFlags("stats")
	kindFlag := fs.String("kind", "", "only this kind: a h l A P k T C")
	top := fs.Int("top", 10, "also list the objects with the most records")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	if len(*kindFlag) > 1 {
		return usagef("-kind is one letter: a h l A P k T C")
	}
	n1 := e.store.CheckpointInterval() == 1
	var all []*kindStats
	tops := &topHeap{}
	scanned, started := uint64(0), time.Now()
	printed := started
	for _, pfx := range []byte{'c', 'p'} {
		lo := append([]byte{pfx}, *kindFlag...)
		it, err := e.store.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
		if err != nil {
			return err
		}
		var cur *kindStats
		var obj, prev []byte
		var objRecs uint64
		endObject := func() {
			if obj == nil {
				return
			}
			if objRecs > cur.MostRecs {
				cur.MostRecs, cur.MostObject = objRecs, objectOf(obj).String()
			}
			if *top > 0 {
				if tops.Len() < *top {
					heap.Push(tops, objCount{objectOf(obj).String(), objRecs})
				} else if objRecs > (*tops)[0].Records {
					(*tops)[0] = objCount{objectOf(obj).String(), objRecs}
					heap.Fix(tops, 0)
				}
			}
		}
		for valid := it.First(); valid; valid = it.Next() {
			k, v := it.Key(), it.Value()
			o, _, ok := splitRecord(k)
			if !ok {
				continue
			}
			if cur == nil || cur.Kind != string(o[0]) {
				endObject()
				obj = nil
				cur = &kindStats{Keyspace: string(pfx), Kind: string(o[0]), Name: kindName(o[0])}
				if n1 {
					cur.Noops = new(uint64)
				}
				all = append(all, cur)
			}
			if !bytes.Equal(o, obj) {
				endObject()
				obj = append(obj[:0], o...)
				prev = append(prev[:0], recDeleted) // absent before its first record
				objRecs = 0
				cur.Objects++
			}
			cur.Records++
			objRecs++
			cur.KeyBytes += uint64(len(k))
			cur.ValueBytes += uint64(len(v))
			switch {
			case len(v) > 0 && v[0] == recDeleted:
				cur.Tombstones++
			case len(v) > 0 && v[0] == recPatch:
				cur.Patches++
			}
			if n1 {
				if bytes.Equal(v, prev) {
					*cur.Noops++
				}
				prev = append(prev[:0], v...)
			}
			if scanned++; scanned%65536 == 0 && time.Since(printed) >= progressInterval {
				printed = time.Now()
				fmt.Fprintf(e.errOut, "scanned %d records (%s) in %v\n", scanned, cur.Name, time.Since(started).Round(time.Second))
			}
		}
		if cur != nil {
			endObject()
		}
		if err := it.Close(); err != nil {
			return err
		}
	}
	topList := slices.Clone(*tops)
	slices.SortFunc(topList, func(a, b objCount) int {
		return cmp.Or(cmp.Compare(b.Records, a.Records), strings.Compare(a.Object, b.Object))
	})
	if e.json {
		return e.emit(struct {
			Kinds []*kindStats `json:"kinds"`
			Top   []objCount   `json:"top"`
		}{all, topList})
	}
	rows := make([][]string, 0, len(all))
	for _, s := range all {
		noops := "-"
		if s.Noops != nil {
			noops = strconv.FormatUint(*s.Noops, 10)
		}
		rows = append(rows, []string{s.Keyspace, s.Kind, s.Name, strconv.FormatUint(s.Records, 10), strconv.FormatUint(s.Objects, 10),
			strconv.FormatUint(s.Tombstones, 10), noops, sizeString(s.KeyBytes), sizeString(s.ValueBytes),
			strconv.FormatUint(s.MostRecs, 10)})
	}
	if err := ttview.WriteTable(e.out, []string{"keyspace", "kind", "contents", "records", "objects", "tombstones", "no-ops", "keys", "values", "most per object"}, rows); err != nil {
		return err
	}
	if len(topList) == 0 {
		return nil
	}
	fmt.Fprintln(e.out)
	rows = rows[:0]
	for _, t := range topList {
		rows = append(rows, []string{strconv.FormatUint(t.Records, 10), t.Object})
	}
	return ttview.WriteTable(e.out, []string{"records", "object"}, rows)
}

// prefixEnd returns the smallest key greater than every key starting with p.
func prefixEnd(p []byte) []byte {
	end := append([]byte(nil), p...)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] != 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

type rawOut struct {
	Key      string  `json:"key"`
	Keyspace string  `json:"keyspace"`
	Object   string  `json:"object,omitempty"`
	Round    *uint64 `json:"round,omitempty"`
	Tag      string  `json:"tag,omitempty"`
	Value    []byte  `json:"value"`
	// Intervals are the rounds at which the holding of an asset holders index
	// entry was created and deleted, alternating.
	Intervals []uint64 `json:"intervals,omitempty"`
}

func cmdRaw(e *env, args []string) error {
	fs := e.newFlags("raw")
	prefixFlag := fs.String("prefix", "", "an object, a keyspace (a b c p r H E m) or hex:PREFIX")
	after := fs.String("after", "", "start after this record key (hex, as printed)")
	n := fs.Uint64("n", 100, "at most this many records (0: all)")
	if _, err := parse(fs, args, 0, 0); err != nil {
		return err
	}
	// the key ranges to list: an object's records are in two keyspaces
	var ranges [][]byte
	switch p := *prefixFlag; {
	case p == "":
		ranges = [][]byte{nil}
	case len(p) == 1 && strings.Contains("abcprHEm", p):
		ranges = [][]byte{[]byte(p)}
	case strings.HasPrefix(p, "hex:"):
		b, err := hex.DecodeString(p[4:])
		if err != nil {
			return usagef("bad -prefix %q: %v", p, err)
		}
		ranges = [][]byte{b}
	default:
		o, err := parseSpec(p)
		if err != nil {
			return usageError{err}
		}
		ranges = [][]byte{append([]byte{'c'}, o.key...), append([]byte{'p'}, o.key...)}
	}
	var afterKey []byte
	if *after != "" {
		var err error
		if afterKey, err = hex.DecodeString(*after); err != nil {
			return usagef("bad -after %q: %v", *after, err)
		}
	}
	t := &table{w: e.out}
	count := uint64(0)
	var last []byte
	truncated := false
	for _, lo := range ranges {
		opts := &pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)}
		it, err := e.store.DB().NewIter(opts)
		if err != nil {
			return err
		}
		valid := it.First()
		if afterKey != nil {
			valid = it.SeekGE(append(slices.Clone(afterKey), 0))
		}
		for ; valid; valid = it.Next() {
			if *n > 0 && count == *n {
				truncated = true
				break
			}
			count++
			k, v := it.Key(), it.Value()
			last = append(last[:0], k...)
			out := rawRecord(k, v)
			if e.json {
				err = e.emit(out)
			} else {
				rnd := ""
				if out.Round != nil {
					rnd = strconv.FormatUint(*out.Round, 10)
				}
				val := hexPrefix(out.Value, 16)
				if out.Intervals != nil {
					val = "rounds " + strings.Trim(fmt.Sprint(out.Intervals), "[]")
				}
				err = t.add([]string{"key", "keyspace", "object", "round", "tag", "bytes", "value"},
					[]string{out.Key, out.Keyspace, out.Object, rnd, out.Tag, strconv.Itoa(len(out.Value)), val})
			}
			if err != nil {
				it.Close()
				return err
			}
		}
		if err := it.Close(); err != nil {
			return err
		}
		if truncated {
			break
		}
	}
	if err := t.flush(); err != nil {
		return err
	}
	if truncated {
		e.more(hex.EncodeToString(last))
	}
	return nil
}

// rawRecord decodes a record of any keyspace.
func rawRecord(k, v []byte) rawOut {
	out := rawOut{Key: hex.EncodeToString(k), Keyspace: string(k[:1]), Value: v}
	switch k[0] {
	case 'c', 'p':
		if o, rnd, ok := splitRecord(k); ok {
			out.Object, out.Round = objectOf(o).String(), &rnd
		}
		if len(v) > 0 {
			out.Tag, out.Value = string(v[:1]), v[1:]
		}
	case 'r', 'H', 'E':
		if len(k) == 9 {
			rnd := binary.BigEndian.Uint64(k[1:])
			out.Round = &rnd
		}
	case 'a': // asset, addr -> the holding's intervals
		if len(k) == 1+8+32 {
			var addr basics.Address
			copy(addr[:], k[9:])
			out.Object = objectOf(timetravel.HoldingKey(addr, basics.AssetIndex(binary.BigEndian.Uint64(k[1:])))).String()
		}
		if iv, err := timetravel.HolderIntervals(v); err == nil {
			out.Intervals = iv
		}
	case 'b': // app, round, name
		if len(k) >= 1+8+8 {
			app, rnd := binary.BigEndian.Uint64(k[1:]), binary.BigEndian.Uint64(k[9:])
			o := object{kind: timetravel.KindKv, kv: apps.MakeBoxKey(app, string(k[17:]))}
			o.splitBox()
			out.Object, out.Round = o.String(), &rnd
		}
		if len(v) > 0 {
			out.Tag, out.Value = string(v[:1]), v[1:]
		}
	}
	return out
}

func hexPrefix(b []byte, n int) string {
	if len(b) <= n {
		return hex.EncodeToString(b)
	}
	return hex.EncodeToString(b[:n]) + "…"
}

type everOut struct {
	Object  string `json:"object"`
	First   uint64 `json:"first-round"`
	Last    uint64 `json:"last-round"`
	Records uint64 `json:"records"`
	Live    bool   `json:"live"`
}

// lsEver lists every object under prefix that has a record, in either
// versioned keyspace, with the rounds of its first and last records, its
// record count, and whether it exists at the last round.
func lsEver(e *env, prefix, after []byte, n uint64) error {
	its := make([]*pebble.Iterator, 0, 2)
	defer func() {
		for _, it := range its {
			it.Close()
		}
	}()
	for _, pfx := range []byte{'c', 'p'} {
		lo := append([]byte{pfx}, prefix...)
		it, err := e.store.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: prefixEnd(lo)})
		if err != nil {
			return err
		}
		if after != nil {
			it.SeekGE(prefixEnd(append([]byte{pfx}, after...)))
		} else {
			it.First()
		}
		its = append(its, it)
	}
	// the object an iterator is on, nil when done
	objOf := func(it *pebble.Iterator) []byte {
		if !it.Valid() {
			return nil
		}
		o, _, _ := splitRecord(it.Key())
		return o
	}
	t := &table{w: e.out}
	count := uint64(0)
	var lastObj object
	for {
		// the smallest object either keyspace is on
		var obj []byte
		for _, it := range its {
			if o := objOf(it); o != nil && (obj == nil || bytes.Compare(o, obj) < 0) {
				obj = slices.Clone(o)
			}
		}
		if obj == nil {
			break
		}
		if n > 0 && count == n {
			defer e.more(lastObj.String())
			break
		}
		out := everOut{Object: objectOf(obj).String(), First: ^uint64(0)}
		for _, it := range its {
			for o := objOf(it); o != nil && bytes.Equal(o, obj); o = objOf(it) {
				_, rnd, _ := splitRecord(it.Key())
				out.First, out.Last = min(out.First, rnd), max(out.Last, rnd)
				out.Records++
				it.Next()
			}
		}
		_, live, err := e.store.Get(obj, uint64(e.last))
		if err != nil {
			return err
		}
		out.Live = live
		count++
		lastObj = objectOf(obj)
		if e.json {
			err = e.emit(out)
		} else {
			state := "deleted"
			if live {
				state = "live"
			}
			err = t.add([]string{"object", "first", "last", "records", "state"},
				[]string{out.Object, strconv.FormatUint(out.First, 10), strconv.FormatUint(out.Last, 10), strconv.FormatUint(out.Records, 10), state})
		}
		if err != nil {
			return err
		}
	}
	for _, it := range its {
		if err := it.Error(); err != nil {
			return err
		}
	}
	if count == 0 && !e.json {
		fmt.Fprintln(e.out, "no records")
	}
	return t.flush()
}
