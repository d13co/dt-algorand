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
	"database/sql"
	"database/sql/driver"
	"encoding/binary"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/duckdb/duckdb-go/v2"
	_ "github.com/mattn/go-sqlite3"

	"github.com/algorand/go-algorand/ledger/timetravel"
)

type version struct {
	obj []byte
	rnd uint64
	val []byte // record as stored: tag byte + payload
}

// engines loads the versions of a store (built by ingest -keep) into
// SQLite and DuckDB, in round order like a syncing node would, and runs the
// same latest-version-at-or-before-R lookups against every engine.
func engines(args []string) error {
	fs := flag.NewFlagSet("engines", flag.ExitOnError)
	src := fs.String("store", "", "timetravel store")
	out := fs.String("out", "", "scratch directory")
	nq := fs.Int("q", 5000, "queries")
	batch := fs.Uint64("batch", 100, "rounds per write transaction")
	fs.Parse(args)

	s, err := timetravel.Open(*src, timetravel.Options{})
	if err != nil {
		return err
	}
	last, _ := s.Last()
	var vs []version
	it, err := s.DB().NewIter(nil)
	if err != nil {
		return err
	}
	for it.First(); it.Valid(); it.Next() {
		k := it.Key()
		if k[0] != 'c' {
			continue
		}
		vs = append(vs, version{append([]byte(nil), k[1:len(k)-8]...), binary.BigEndian.Uint64(k[len(k)-8:]), append([]byte(nil), it.Value()...)})
	}
	it.Close()
	sort.SliceStable(vs, func(i, j int) bool { return vs[i].rnd < vs[j].rnd })
	rng := rand.New(rand.NewSource(7))
	qs := make([]query, *nq)
	for i := range qs {
		v := vs[rng.Intn(len(vs))]
		qs[i] = query{v.obj, v.rnd + uint64(rng.Int63n(int64(last-v.rnd+1)))}
	}
	// reference answers from pebble
	want := make([][]byte, len(qs))
	var plat latencies
	for i, q := range qs {
		t0 := time.Now()
		v, ok, err := s.Get(q.obj, q.rnd)
		plat = append(plat, time.Since(t0))
		if err != nil {
			return err
		}
		if ok {
			want[i] = v
		}
	}
	emit(map[string]any{"engine": "pebble", "versions": len(vs), "disk_mib": float64(s.DB().Metrics().Total().TablesSize) / (1 << 20), "read": plat.summary()})
	s.Close()

	os.MkdirAll(*out, 0o755)
	const q = "SELECT val FROM v WHERE obj = ? AND rnd <= ? ORDER BY rnd DESC LIMIT 1"
	runQueries := func(db *sql.DB) (map[string]float64, error) {
		stmt, err := db.Prepare(q)
		if err != nil {
			return nil, err
		}
		defer stmt.Close()
		var lat latencies
		for i, qq := range qs {
			t0 := time.Now()
			var rec []byte
			err := stmt.QueryRow(qq.obj, qq.rnd).Scan(&rec)
			lat = append(lat, time.Since(t0))
			if err != nil && err != sql.ErrNoRows {
				return nil, err
			}
			var got []byte
			if len(rec) > 0 && rec[0] == 'F' {
				got = rec[1:]
			}
			if string(got) != string(want[i]) {
				return nil, fmt.Errorf("query %d mismatch", i)
			}
		}
		return lat.summary(), nil
	}

	// SQLite: clustered (WITHOUT ROWID) primary key, batched transactions
	{
		path := filepath.Join(*out, "v.sqlite")
		os.Remove(path)
		db, err := sql.Open("sqlite3", path+"?_journal_mode=WAL&_synchronous=NORMAL")
		if err != nil {
			return err
		}
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("CREATE TABLE v(obj BLOB, rnd INTEGER, val BLOB, PRIMARY KEY(obj, rnd)) WITHOUT ROWID"); err != nil {
			return err
		}
		t0 := time.Now()
		for i := 0; i < len(vs); {
			tx, err := db.Begin()
			if err != nil {
				return err
			}
			stmt, err := tx.Prepare("INSERT INTO v VALUES (?, ?, ?)")
			if err != nil {
				return err
			}
			end := vs[i].rnd + *batch
			for ; i < len(vs) && vs[i].rnd < end; i++ {
				if _, err := stmt.Exec(vs[i].obj, vs[i].rnd, vs[i].val); err != nil {
					return err
				}
			}
			stmt.Close()
			if err := tx.Commit(); err != nil {
				return err
			}
		}
		ingest := time.Since(t0)
		db.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
		read, err := runQueries(db)
		if err != nil {
			return err
		}
		db.Close()
		emit(map[string]any{"engine": "sqlite", "ingest_s": ingest.Seconds(), "disk_mib": float64(dirSize(path)) / (1 << 20), "read": read})
	}

	// DuckDB: appender in round order; then point lookups with no index,
	// with an ART index on obj, and on a copy sorted by (obj, rnd).
	{
		path := filepath.Join(*out, "v.duckdb")
		os.Remove(path)
		os.Remove(path + ".wal")
		conn, err := duckdb.NewConnector(path, nil)
		if err != nil {
			return err
		}
		db := sql.OpenDB(conn)
		db.SetMaxOpenConns(1)
		if _, err := db.Exec("CREATE TABLE v(obj BLOB, rnd UBIGINT, val BLOB)"); err != nil {
			return err
		}
		t0 := time.Now()
		c, err := conn.Connect(context.Background())
		if err != nil {
			return err
		}
		for i := 0; i < len(vs); {
			app, err := duckdb.NewAppenderFromConn(c, "", "v")
			if err != nil {
				return err
			}
			end := vs[i].rnd + *batch
			for ; i < len(vs) && vs[i].rnd < end; i++ {
				if err := app.AppendRow(vs[i].obj, vs[i].rnd, vs[i].val); err != nil {
					return err
				}
			}
			if err := app.Close(); err != nil {
				return err
			}
		}
		c.(driver.Conn).Close()
		ingest := time.Since(t0)
		if _, err := db.Exec("CHECKPOINT"); err != nil {
			return err
		}
		size := dirSize(path)
		res := map[string]any{"engine": "duckdb", "ingest_s": ingest.Seconds(), "disk_mib": float64(size) / (1 << 20)}
		if res["read_noindex"], err = runQueries(db); err != nil {
			return err
		}
		t0 = time.Now()
		if _, err := db.Exec("CREATE INDEX v_obj ON v(obj)"); err != nil {
			return err
		}
		res["index_build_s"] = time.Since(t0).Seconds()
		if res["read_art"], err = runQueries(db); err != nil {
			return err
		}
		// best case for the ART index: equality lookup, round filter client-side
		{
			stmt, err := db.Prepare("SELECT rnd, val FROM v WHERE obj = ?")
			if err != nil {
				return err
			}
			var lat latencies
			for i, qq := range qs {
				t0 := time.Now()
				rows, err := stmt.Query(qq.obj)
				if err != nil {
					return err
				}
				var best uint64
				var rec []byte
				for rows.Next() {
					var r uint64
					var v []byte
					if err := rows.Scan(&r, &v); err != nil {
						return err
					}
					if r <= qq.rnd && (rec == nil || r > best) {
						best, rec = r, v
					}
				}
				rows.Close()
				lat = append(lat, time.Since(t0))
				var got []byte
				if len(rec) > 0 && rec[0] == 'F' {
					got = rec[1:]
				}
				if string(got) != string(want[i]) {
					return fmt.Errorf("art_eq query %d mismatch", i)
				}
			}
			stmt.Close()
			res["read_art_eq"] = lat.summary()
		}
		db.Exec("DROP INDEX v_obj")
		if _, err := db.Exec("CREATE TABLE s AS SELECT * FROM v ORDER BY obj, rnd; DROP TABLE v; ALTER TABLE s RENAME TO v; CHECKPOINT"); err != nil {
			return err
		}
		res["disk_sorted_mib"] = float64(dirSize(path)) / (1 << 20)
		if res["read_sorted"], err = runQueries(db); err != nil {
			return err
		}
		db.Close()
		conn.Close()
		emit(res)
	}
	return nil
}
