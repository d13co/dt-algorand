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
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-sqlite3"

	"github.com/algorand/go-algorand/data/basics"
)

const compactBatch = 10_000

func compactCmd(args []string) error {
	fs, dataDir, stateDir := newFlags("compact", `With algod stopped, writes a copy of the block DB in which the blocks more than -retention rounds behind the tip
have no certificate, in round order, then with -replace swaps it in. This returns the space that stripping in place
(CertificateRetentionRounds) leaves free inside the block DB. It needs free space for the copy.

The cutoff must not be above the round recorded by anchor unless -unverified is given.`)
	out := fs.String("out", "", "file to write (default: the block DB's path + .compact, on the same filesystem)")
	retention := fs.Uint64("retention", 0, "rounds behind the tip whose certificates are kept (default: CertificateRetentionRounds of config.json)")
	unverified := fs.Bool("unverified", false, "strip without a verified, anchored chain: the archive keeps whatever trust the snapshot had")
	replace := fs.Bool("replace", false, "swap the copy in: the block DB becomes <name>.old (delete it once algod runs fine)")
	_ = fs.Parse(args)

	n, err := openNode(*dataDir, *stateDir)
	if err != nil {
		return err
	}
	if err = checkRunAs(n.dataDir, os.Geteuid()); err != nil {
		return err
	}
	if err = n.requireStopped(); err != nil {
		return err
	}
	if *retention == 0 {
		*retention = n.cfg.CertificateRetentionRounds
	}
	if *retention == 0 {
		return errors.New("set -retention, or CertificateRetentionRounds in config.json")
	}
	cfg := n.cfg
	cfg.CertificateRetentionRounds = *retention
	if err = cfg.ValidateCertificateRetention(); err != nil {
		return err
	}
	if *out == "" {
		*out = n.blockDB() + ".compact"
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return compact(ctx, n, *out, basics.Round(*retention), *unverified, *replace)
}

// stripCutoff returns the highest round whose certificate compact discards, checked against the anchor.
func stripCutoff(n *node, tip, retention basics.Round, unverified bool) (basics.Round, error) {
	cutoff := tip.SubSaturate(retention)
	if unverified {
		return cutoff, nil
	}
	// a genesis replay checked every certificate it walked
	st, err := n.readChainState()
	if err != nil {
		return 0, err
	}
	if st != nil && st.Full && cutoff <= st.Round {
		return cutoff, nil
	}
	a, err := n.readAnchor()
	if err != nil {
		return 0, err
	}
	if a == nil {
		return 0, fmt.Errorf("no %s: verify the chain and anchor it first (verify-chain, reset-trie, anchor) or replay it (verify-chain -full), or pass -unverified", n.anchor())
	}
	if cutoff > a.Round {
		return 0, fmt.Errorf("stripping up to round %d would go past the anchored round %d: anchor a later round, raise -retention, or pass -unverified", cutoff, a.Round)
	}
	return cutoff, nil
}

func compact(ctx context.Context, n *node, out string, retention basics.Round, unverified, replace bool) (err error) {
	if _, err = os.Stat(out); err == nil {
		return fmt.Errorf("%s exists", out)
	}
	written := false
	defer func() {
		// after the connection closes: an interrupted or failed copy is of no use
		if !written {
			os.Remove(out)
		}
	}()
	src := n.blockDB()
	if replace {
		// the swap renames: across filesystems the second rename would fail after the first, leaving no block DB
		absOut, err1 := filepath.Abs(out)
		if err1 != nil {
			return err1
		}
		if filepath.Dir(absOut) != filepath.Dir(src) {
			return fmt.Errorf("with -replace, -out must be in the block DB's directory %s", filepath.Dir(src))
		}
	}
	conn, err := sql.Open("sqlite3", "file:"+out+"?_journal_mode=OFF&_synchronous=OFF")
	if err != nil {
		return err
	}
	conn.SetMaxOpenConns(1) // the attached DB is per connection
	defer conn.Close()
	if _, err = conn.ExecContext(ctx, "ATTACH DATABASE ? AS src", src); err != nil {
		return fmt.Errorf("attaching %s: %w", src, err)
	}

	var tip basics.Round
	if err = conn.QueryRowContext(ctx, "SELECT MAX(rnd) FROM src.blocks").Scan(&tip); err != nil {
		return fmt.Errorf("%s: %w", src, err)
	}
	cutoff, err := stripCutoff(n, tip, retention, unverified)
	if err != nil {
		return err
	}

	// copy the schema, then every table but blocks as is (e.g. a catchpoint catchup's staging blocks)
	tables, err := srcTables(ctx, conn)
	if err != nil {
		return err
	}
	var blockCols []string
	for _, t := range tables {
		if _, err = conn.ExecContext(ctx, t.sql); err != nil {
			return fmt.Errorf("creating %s: %w", t.name, err)
		}
		if t.name == "blocks" {
			blockCols, err = columns(ctx, conn, "blocks")
			if err != nil {
				return err
			}
			continue
		}
		if _, err = conn.ExecContext(ctx, fmt.Sprintf("INSERT INTO main.%q SELECT * FROM src.%q", t.name, t.name)); err != nil {
			return fmt.Errorf("copying %s: %w", t.name, err)
		}
	}
	if blockCols == nil {
		return fmt.Errorf("%s has no blocks table", src)
	}
	sel := make([]string, len(blockCols))
	for i, c := range blockCols {
		sel[i] = fmt.Sprintf("%q", c)
		if c == "certdata" {
			// like algod, keep genesis's (empty) certificate
			sel[i] = "CASE WHEN rnd >= 1 AND rnd <= :cutoff THEN NULL ELSE certdata END"
		}
	}
	insert := fmt.Sprintf("INSERT INTO main.blocks (%s) SELECT %s FROM src.blocks WHERE rnd >= :lo AND rnd <= :hi ORDER BY rnd",
		quoteJoin(blockCols), strings.Join(sel, ", "))

	var earliest basics.Round
	if err = conn.QueryRowContext(ctx, "SELECT MIN(rnd) FROM src.blocks").Scan(&earliest); err != nil {
		return err
	}
	fmt.Printf("copying rounds %d..%d of %s into %s, without the certificates of rounds up to %d\n", earliest, tip, src, out, cutoff)
	p := &progress{start: time.Now(), from: earliest, to: tip, interval: progressInterval}
	var copied int64
	for lo := earliest; lo <= tip; lo += compactBatch {
		hi := min(tip, lo+compactBatch-1)
		res, err1 := conn.ExecContext(ctx, insert, sql.Named("cutoff", cutoff), sql.Named("lo", lo), sql.Named("hi", hi))
		if err1 == nil {
			var n int64
			n, err1 = res.RowsAffected()
			copied += n
		}
		if err = err1; err != nil {
			var serr sqlite3.Error
			if errors.As(err, &serr) && serr.Code == sqlite3.ErrFull {
				err = fmt.Errorf("%w: not enough free space for %s", err, out)
			}
			return err
		}
		p.report(hi, false)
	}

	// the copy must span the same rounds (MIN and MAX read the ends of the rowid b-tree; COUNT would read every
	// leaf page of both files)
	var outMin, outMax basics.Round
	if err = conn.QueryRowContext(ctx, "SELECT MIN(rnd), MAX(rnd) FROM main.blocks").Scan(&outMin, &outMax); err != nil {
		return err
	}
	if outMin != earliest || outMax != tip {
		return fmt.Errorf("the copy holds rounds %d..%d, the source %d..%d", outMin, outMax, earliest, tip)
	}
	if want := int64(tip-earliest) + 1; copied != want {
		fmt.Printf("note: copied %d rows for rounds %d..%d: the source has gaps\n", copied, earliest, tip)
	}
	if _, err = conn.ExecContext(ctx, "DETACH DATABASE src"); err != nil {
		return err
	}
	if err = conn.Close(); err != nil {
		return err
	}
	if err = syncFile(out); err != nil {
		return err
	}
	written = true
	p.report(tip, true)
	fmt.Printf("wrote %s\n", out)
	if !replace {
		fmt.Printf("to use it, stop algod and run this again with -replace, or move it over %s yourself after removing %s-wal and %s-shm\n", src, src, src)
		return nil
	}
	return swapIn(src, out)
}

// swapIn renames the block DB to .old, with its WAL and shared memory files, and the copy to the block DB.
func swapIn(src, out string) error {
	old := src + ".old"
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if _, err := os.Stat(old + suffix); err == nil {
			return fmt.Errorf("%s exists: remove it first", old+suffix)
		}
	}
	if err := os.Rename(src, old); err != nil {
		return err
	}
	// SQLite would replay a leftover WAL into the new file: it must go with the old one
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Rename(src+suffix, old+suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Rename(out, src); err != nil {
		return fmt.Errorf("%w (the old block DB is at %s)", err, old)
	}
	fmt.Printf("%s is the compacted copy; the old one is %s: delete it once algod runs fine\n", src, old)
	return nil
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

type tableDef struct{ name, sql string }

func srcTables(ctx context.Context, conn *sql.DB) ([]tableDef, error) {
	rows, err := conn.QueryContext(ctx, "SELECT name, sql FROM src.sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []tableDef
	for rows.Next() {
		var t tableDef
		if err = rows.Scan(&t.name, &t.sql); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func columns(ctx context.Context, conn *sql.DB, table string) ([]string, error) {
	rows, err := conn.QueryContext(ctx, fmt.Sprintf("SELECT name FROM pragma_table_info(%s, 'src')", quoteString(table)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var cols []string
	for rows.Next() {
		var c string
		if err = rows.Scan(&c); err != nil {
			return nil, err
		}
		cols = append(cols, c)
	}
	return cols, rows.Err()
}

func quoteJoin(cols []string) string {
	q := make([]string, len(cols))
	for i, c := range cols {
		q[i] = fmt.Sprintf("%q", c)
	}
	return strings.Join(q, ", ")
}

func quoteString(s string) string { return "'" + strings.ReplaceAll(s, "'", "''") + "'" }
