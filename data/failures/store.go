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

// Package failures keeps failed simulations, and later failed transactions,
// for a while, so that they can be fetched and replayed against the state they
// ran on (GET /v2/failures). A record holds the request and the round, not the
// state: a time travel node rebuilds the state of any round. Non-standard fork
// feature.
package failures

import (
	"context"
	"database/sql"
	"encoding/base32"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

// Kind is what failed.
type Kind string

const (
	// KindSimulate is a simulate request with a failed group, or one the simulator refused.
	KindSimulate Kind = "simulate"
)

// GroupFailure is a failed group of a simulate request.
type GroupFailure struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	Index    int      `codec:"index"`
	Message  string   `codec:"message"`
	FailedAt []uint64 `codec:"failed-at"`
}

// Record is a kept failure.
type Record struct {
	_struct struct{} `codec:",omitempty,omitemptyarray"`

	ID   string `codec:"id"`   // set by Capture
	Time int64  `codec:"time"` // unix milliseconds, set by Capture
	Kind Kind   `codec:"kind"`
	// Round is the round whose state it ran on: it was evaluated as the block after Round.
	Round basics.Round `codec:"round"`
	// Error is a failure of the whole request (no group ran, or the simulator refused it).
	Error  string         `codec:"error"`
	Groups []GroupFailure `codec:"groups"`
	// Request is the request as received, in Format ("json" or "msgpack"). KindSimulate only.
	// It is stored apart from the rest, which listings read alone.
	Request []byte `codec:"-"`
	Format  string `codec:"format"`
	// Proto is the consensus version of the block it was evaluated in; Version the node's build.
	Proto   protocol.ConsensusVersion `codec:"proto"`
	Version string                    `codec:"version"`
}

// Config bounds what a Store keeps.
type Config struct {
	Retention time.Duration // records older than this go; 0: kept until MaxBytes
	MaxBytes  uint64        // the oldest records go once the records take more; 0: no limit
}

// recordOverhead is counted for each record besides its encoded bytes: its row's keys and index entries.
const recordOverhead = 96

// queueSize is how many captures can wait for the writer, and queueBytes how many bytes of
// requests they may hold; more are dropped.
const (
	queueSize  = 256
	queueBytes = 32 << 20
)

// pruneEvery is how often the writer drops expired records when nothing is captured.
const pruneEvery = time.Minute

// ErrNotFound is returned for an id the store doesn't hold (never captured, expired or dropped).
var ErrNotFound = errors.New("no such failure")

// Store keeps failures in a SQLite file. Capture never blocks: a background writer stores the
// records, and drops what it can't keep up with.
type Store struct {
	db  db.Accessor
	cfg Config
	log logging.Logger
	now func() time.Time

	queue     chan Record
	queued    atomic.Int64 // the bytes of the requests in queue
	flushes   chan chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	bytes   atomic.Int64 // what the stored records count, as size counts it
	dropped atomic.Uint64
}

// Open opens (or creates) the store in file path.
func Open(path string, cfg Config, log logging.Logger) (*Store, error) {
	return open(path, false, cfg, log)
}

func open(path string, inMemory bool, cfg Config, log logging.Logger) (*Store, error) {
	acc, err := db.MakeAccessor(path, false, inMemory)
	if err != nil {
		return nil, fmt.Errorf("opening the failure store %s: %w", path, err)
	}
	s := &Store{
		db: acc, cfg: cfg, log: log, now: time.Now,
		queue:   make(chan Record, queueSize),
		flushes: make(chan chan struct{}),
		done:    make(chan struct{}),
	}
	var total int64
	err = acc.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS failures (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			id TEXT NOT NULL UNIQUE,
			time INTEGER NOT NULL,
			kind TEXT NOT NULL,
			size INTEGER NOT NULL,
			record BLOB NOT NULL,
			request BLOB)`)
		if err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM failures`).Scan(&total)
	})
	if err != nil {
		acc.Close()
		return nil, fmt.Errorf("opening the failure store %s: %w", path, err)
	}
	s.bytes.Store(total)
	s.wg.Add(1)
	go s.writer()
	return s, nil
}

// Close stops the writer, after it stored what was captured, and closes the file. Later
// captures are dropped; later calls do nothing.
func (s *Store) Close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.wg.Wait()
		s.db.Close()
	})
}

// newID returns a random id: unguessable, so that knowing one doesn't reveal the others.
func newID() string {
	var b [15]byte
	crypto.RandBytes(b[:])
	return strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b[:]))
}

// Capture keeps r, and returns the id it is fetched by. It never blocks: when the writer is
// behind, or the store is closed, r is dropped and the id is "". r.Request must not change
// after the call.
func (s *Store) Capture(r Record) string {
	select {
	case <-s.done:
		s.dropped.Add(1)
		return ""
	default:
	}
	n := int64(len(r.Request))
	if s.queued.Add(n) > queueBytes {
		s.queued.Add(-n)
		s.dropped.Add(1)
		return ""
	}
	r.ID = newID()
	r.Time = s.now().UnixMilli()
	select {
	case s.queue <- r:
		return r.ID
	default:
		s.queued.Add(-n)
		s.dropped.Add(1)
		return ""
	}
}

// Dropped is how many captures were dropped: the writer was behind, a record was larger than
// MaxBytes, or it couldn't be stored.
func (s *Store) Dropped() uint64 {
	return s.dropped.Load()
}

// Bytes is what the stored records take, as the MaxBytes limit counts it.
func (s *Store) Bytes() int64 {
	return s.bytes.Load()
}

// Flush returns once every record captured before it is stored or dropped.
func (s *Store) Flush() {
	ch := make(chan struct{})
	select {
	case s.flushes <- ch:
		<-ch
	case <-s.done:
	}
}

func (s *Store) writer() {
	defer s.wg.Done()
	ticker := time.NewTicker(pruneEvery)
	defer ticker.Stop()
	for {
		select {
		case r := <-s.queue:
			s.storeQueued(r)
		case ch := <-s.flushes:
			s.storeQueued()
			close(ch)
		case <-ticker.C:
			s.prune()
		case <-s.done:
			s.storeQueued()
			return
		}
	}
}

// storeQueued stores the records given and those waiting in the queue, in one transaction, then
// prunes.
func (s *Store) storeQueued(first ...Record) {
	batch := first
	for len(batch) < queueSize {
		select {
		case r := <-s.queue:
			batch = append(batch, r)
			continue
		default:
		}
		break
	}
	if len(batch) == 0 {
		return
	}
	type row struct {
		r    *Record
		enc  []byte
		size int64
	}
	rows := make([]row, 0, len(batch))
	for i := range batch {
		s.queued.Add(-int64(len(batch[i].Request)))
		enc := protocol.EncodeJSON(&batch[i]) // without the request
		size := int64(len(enc)+len(batch[i].Request)) + recordOverhead
		if s.cfg.MaxBytes > 0 && uint64(size) > s.cfg.MaxBytes {
			s.dropped.Add(1)
			continue
		}
		rows = append(rows, row{r: &batch[i], enc: enc, size: size})
	}
	var added int64
	err := s.db.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		added = 0
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO failures (id, time, kind, size, record, request) VALUES (?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, w := range rows {
			if _, err := stmt.ExecContext(ctx, w.r.ID, w.r.Time, string(w.r.Kind), w.size, w.enc, w.r.Request); err != nil {
				return err
			}
			added += w.size
		}
		return nil
	})
	if err != nil {
		s.dropped.Add(uint64(len(rows)))
		s.log.Warnf("failure store: storing %d records: %v", len(rows), err)
		return
	}
	s.bytes.Add(added)
	s.prune()
}

// prune drops the records older than Retention, then the oldest ones while the records take more
// than MaxBytes.
func (s *Store) prune() {
	var freed int64
	err := s.db.Atomic(func(ctx context.Context, tx *sql.Tx) error {
		freed = 0
		if s.cfg.Retention > 0 {
			cutoff := s.now().Add(-s.cfg.Retention).UnixMilli()
			if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(size), 0) FROM failures WHERE time < ?`, cutoff).Scan(&freed); err != nil {
				return err
			}
			if freed > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM failures WHERE time < ?`, cutoff); err != nil {
					return err
				}
			}
		}
		over := s.bytes.Load() - freed - int64(s.cfg.MaxBytes)
		if s.cfg.MaxBytes > 0 && over > 0 {
			// the oldest rows that free enough
			rows, err := tx.QueryContext(ctx, `SELECT seq, size FROM failures ORDER BY seq`)
			if err != nil {
				return err
			}
			var last, sum int64
			for sum < over && rows.Next() {
				var size int64
				if err := rows.Scan(&last, &size); err != nil {
					rows.Close()
					return err
				}
				sum += size
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return err
			}
			rows.Close()
			if sum > 0 {
				if _, err := tx.ExecContext(ctx, `DELETE FROM failures WHERE seq <= ?`, last); err != nil {
					return err
				}
				freed += sum
			}
		}
		return nil
	})
	if err != nil {
		s.log.Warnf("failure store: pruning: %v", err)
		return
	}
	s.bytes.Add(-freed)
}

// Get returns the record with id id, or ErrNotFound.
func (s *Store) Get(ctx context.Context, id string) (Record, error) {
	var enc, request []byte
	err := s.db.AtomicContext(ctx, func(ctx context.Context, tx *sql.Tx) error {
		return tx.QueryRowContext(ctx, `SELECT record, request FROM failures WHERE id = ? AND time >= ?`, id, s.cutoff()).Scan(&enc, &request)
	}, nil)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, ErrNotFound
	}
	if err != nil {
		return Record{}, err
	}
	var r Record
	if err := protocol.DecodeJSON(enc, &r); err != nil {
		return Record{}, fmt.Errorf("failure %s: %w", id, err)
	}
	r.Request = request
	return r, nil
}

// cutoff is the time of the oldest record kept: older ones are expired, though maybe not pruned
// yet.
func (s *Store) cutoff() int64 {
	if s.cfg.Retention <= 0 {
		return math.MinInt64
	}
	return s.now().Add(-s.cfg.Retention).UnixMilli()
}

// Page is a page of List.
type Page struct {
	Records []Record // newest first, without Request
	Next    uint64   // pass as before for the next page; 0: no more
}

// List returns up to limit records, newest first, captured before the one at before (0: the
// newest), of kind kind ("" for all). The records come without their Request.
func (s *Store) List(ctx context.Context, before uint64, kind Kind, limit int) (Page, error) {
	if limit <= 0 {
		return Page{}, nil
	}
	if before == 0 {
		before = 1<<63 - 1
	}
	var page Page
	err := s.db.AtomicContext(ctx, func(ctx context.Context, tx *sql.Tx) error {
		page = Page{}
		// the request column is left unread
		q := `SELECT seq, record FROM failures WHERE seq < ? AND time >= ?`
		args := []any{int64(before), s.cutoff()}
		if kind != "" {
			q += ` AND kind = ?`
			args = append(args, string(kind))
		}
		q += ` ORDER BY seq DESC LIMIT ?`
		args = append(args, limit+1)
		rows, err := tx.QueryContext(ctx, q, args...)
		if err != nil {
			return err
		}
		defer rows.Close()
		var lastSeq uint64
		for rows.Next() {
			var seq int64
			var enc []byte
			if err := rows.Scan(&seq, &enc); err != nil {
				return err
			}
			if len(page.Records) == limit {
				page.Next = lastSeq
				break
			}
			var r Record
			if err := protocol.DecodeJSON(enc, &r); err != nil {
				return fmt.Errorf("failure at %d: %w", seq, err)
			}
			lastSeq = uint64(seq)
			page.Records = append(page.Records, r)
		}
		return rows.Err()
	}, nil)
	return page, err
}
