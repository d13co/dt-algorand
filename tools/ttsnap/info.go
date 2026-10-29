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
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

// Info is what a genesis directory's ledger holds.
type Info struct {
	History HistoryInfo `json:"history"`
	// TrackerRound and Blocks describe the tracker and block DBs, for a full
	// snapshot only.
	TrackerRound *basics.Round `json:"tracker-round,omitempty"`
	Blocks       *RoundRange   `json:"blocks,omitempty"`
}

// HistoryInfo describes a time travel store.
type HistoryInfo struct {
	First        basics.Round `json:"first"`
	Last         basics.Round `json:"last"`
	LastHash     string       `json:"last-hash"`
	LastTime     int64        `json:"last-timestamp"`
	BoxIndexFrom basics.Round `json:"box-index-from"`
	// PebbleFormat is the store's Pebble format major version: a reader
	// needs a Pebble that knows it.
	PebbleFormat uint64 `json:"pebble-format"`
}

// RoundRange is an inclusive range of rounds.
type RoundRange struct {
	First basics.Round `json:"first"`
	Last  basics.Round `json:"last"`
}

// network is the genesis ID and hash of a store, from its round 0 header.
type network struct {
	id   string
	hash string
}

// readHistory describes the time travel store in dir.
func readHistory(dir string) (HistoryInfo, network, error) {
	var hi HistoryInfo
	var nw network
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return hi, nw, fmt.Errorf("no time travel store at %s", dir)
	}
	st, err := timetravel.Open(dir, timetravel.Options{ReadOnly: true})
	if err != nil {
		return hi, nw, err
	}
	defer st.Close()
	if reason, found, derr := st.Diverged(); derr != nil || found {
		if derr == nil {
			derr = fmt.Errorf("the store is marked as differing from the chain: %s", reason)
		}
		return hi, nw, derr
	}
	last, ok := st.Last()
	if !ok {
		return hi, nw, fmt.Errorf("the time travel store at %s is empty", dir)
	}
	first, _, err := st.First()
	if err != nil {
		return hi, nw, err
	}
	hi.First, hi.Last = basics.Round(first), basics.Round(last)
	hdr0, err := storeHeader(st, first)
	if err != nil {
		return hi, nw, err
	}
	nw = network{id: hdr0.GenesisID, hash: hdr0.GenesisHash.String()}
	hdr, err := storeHeader(st, last)
	if err != nil {
		return hi, nw, err
	}
	hi.LastHash = hdr.Hash().String()
	hi.LastTime = hdr.TimeStamp
	bif, ok := st.BoxIndexFrom()
	if !ok {
		return hi, nw, errors.New("the store's box index is being rebuilt")
	}
	hi.BoxIndexFrom = basics.Round(bif)
	hi.PebbleFormat = uint64(st.DB().FormatMajorVersion())
	return hi, nw, nil
}

func storeHeader(st *timetravel.Store, rnd uint64) (bookkeeping.BlockHeader, error) {
	var hdr bookkeeping.BlockHeader
	b, ok, err := st.Header(rnd)
	if err != nil {
		return hdr, err
	}
	if !ok {
		return hdr, fmt.Errorf("the store has no header for round %d", rnd)
	}
	return hdr, protocol.Decode(b, &hdr)
}

// readInfo describes the ledger in a genesis directory: its history, and for
// a full snapshot its tracker and block DBs.
func readInfo(dir string, full bool) (Info, network, error) {
	var info Info
	hi, nw, err := readHistory(filepath.Join(dir, historyDir))
	if err != nil {
		return info, nw, err
	}
	info.History = hi
	if !full {
		return info, nw, nil
	}
	rnd, err := sqliteQuery(filepath.Join(dir, trackerFile), 1, "SELECT rnd FROM acctrounds WHERE id = 'acctbase'")
	if err != nil {
		return info, nw, fmt.Errorf("the tracker DB's round: %w", err)
	}
	info.TrackerRound = &rnd[0]
	rr, err := sqliteQuery(filepath.Join(dir, blocksFile), 2, "SELECT MIN(rnd), MAX(rnd) FROM blocks")
	if err != nil {
		return info, nw, fmt.Errorf("the block DB's rounds: %w", err)
	}
	info.Blocks = &RoundRange{First: rr[0], Last: rr[1]}
	return info, nw, nil
}

// check refuses what a snapshot must not be: a history that does not start at
// genesis or whose box index is incomplete, and for a full snapshot DBs that
// are out of order (blocks >= tracker <= history, as on a live node).
func (i Info) check() error {
	if i.History.First != 0 {
		return fmt.Errorf("the history starts at round %d, not at genesis", i.History.First)
	}
	if i.History.BoxIndexFrom != 0 {
		return fmt.Errorf("the box index is complete only from round %d: run ttimport -index-boxes first", i.History.BoxIndexFrom)
	}
	if i.TrackerRound == nil {
		return nil
	}
	t := *i.TrackerRound
	if t > i.History.Last {
		return fmt.Errorf("the tracker DB is at round %d, past the history's last round %d: the snapshot is not of one instant (is the history on another filesystem?)", t, i.History.Last)
	}
	if i.Blocks.Last < t || i.Blocks.First > t {
		return fmt.Errorf("the block DB holds rounds %d..%d, which do not include the tracker DB's round %d", i.Blocks.First, i.Blocks.Last, t)
	}
	return nil
}

// sqliteQuery runs a query returning one row of cols rounds on a SQLite DB.
func sqliteQuery(path string, cols int, query string) ([]basics.Round, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	acc, err := db.MakeAccessor(path, true, false)
	if err != nil {
		return nil, err
	}
	defer acc.Close()
	row := acc.Handle.QueryRow(query)
	out := make([]basics.Round, cols)
	ptrs := make([]any, cols)
	for i := range out {
		ptrs[i] = &out[i]
	}
	if err := row.Scan(ptrs...); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("%s: no rows", path)
		}
		return nil, err
	}
	return out, nil
}

// checkpointWAL folds a SQLite DB's WAL into the DB file and empties it, so
// that the file alone holds the DB.
func checkpointWAL(path string) error {
	acc, err := db.MakeAccessor(path, false, false)
	if err != nil {
		return err
	}
	defer acc.Close()
	var busy, logFrames, checkpointed int
	if err := acc.Handle.QueryRow("PRAGMA wal_checkpoint(TRUNCATE)").Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if busy != 0 {
		return fmt.Errorf("%s: the WAL checkpoint was blocked by another connection", path)
	}
	return nil
}

// walEmpty reports whether a SQLite DB has no WAL content outside its file.
func walEmpty(path string) (bool, error) {
	fi, err := os.Stat(path + "-wal")
	if os.IsNotExist(err) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return fi.Size() == 0, nil
}

func printJSON(v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	fmt.Println(string(b))
	return nil
}

func cmdInfo(args []string) error {
	fs := flag.NewFlagSet("info", flag.ExitOnError)
	full := fs.Bool("full", false, "also describe the tracker and block DBs")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap info [-full] <genesis dir>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	info, nw, err := readInfo(fs.Arg(0), *full)
	if err != nil {
		return err
	}
	return printJSON(struct {
		GenesisID   string `json:"genesis-id"`
		GenesisHash string `json:"genesis-hash"`
		Info
		Problem string `json:"problem,omitempty"`
	}{nw.id, nw.hash, info, errString(info.check())})
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// cmdPrepare makes a writable copy of a node's genesis directory (a
// filesystem snapshot taken while algod ran) self-contained: it opens the
// history store, which replays the store's WAL, flushes it to tables and
// deletes obsolete files, and folds the SQLite WALs into their DBs.
func cmdPrepare(args []string) error {
	fs := flag.NewFlagSet("prepare", flag.ExitOnError)
	full := fs.Bool("full", false, "also prepare the tracker and block DBs, for a full snapshot")
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: ttsnap prepare [-full] <genesis dir of a writable snapshot, never a running node's>")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args) // flag.ExitOnError
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}
	info, nw, err := prepare(fs.Arg(0), *full, os.Stderr)
	if err != nil {
		return err
	}
	return printJSON(struct {
		GenesisID   string `json:"genesis-id"`
		GenesisHash string `json:"genesis-hash"`
		Info
	}{nw.id, nw.hash, info})
}

// prepare is cmdPrepare without the flags. It says what it is doing to log,
// if not nil.
func prepare(dir string, full bool, log io.Writer) (Info, network, error) {
	say := func(format string, args ...any) {
		if log != nil {
			fmt.Fprintf(log, format+"\n", args...)
		}
	}
	start := time.Now()
	hdir := filepath.Join(dir, historyDir)
	// check it is a store before opening it writable, which would create one
	if _, _, err := readHistory(hdir); err != nil {
		return Info{}, network{}, err
	}
	say("opening %s, which replays its WAL", hdir)
	st, err := timetravel.Open(hdir, timetravel.Options{})
	if err != nil {
		return Info{}, network{}, err
	}
	say("flushing it into tables")
	err = st.DB().Flush()
	if cerr := st.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return Info{}, network{}, fmt.Errorf("flushing %s: %w", hdir, err)
	}
	for _, name := range []string{trackerFile, blocksFile} {
		p := filepath.Join(dir, name)
		if _, err = os.Stat(p); os.IsNotExist(err) && !full {
			continue
		}
		say("folding the WAL of %s into it", p)
		if err = checkpointWAL(p); err != nil {
			return Info{}, network{}, err
		}
	}
	say("reading what %s holds", dir)
	info, nw, err := readInfo(dir, full)
	if err != nil {
		return Info{}, network{}, err
	}
	if err := info.check(); err != nil {
		return Info{}, network{}, err
	}
	say("prepared in %s", roundDuration(time.Since(start)))
	return info, nw, nil
}
