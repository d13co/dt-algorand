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

package failures

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// clock is a settable time for a Store.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func testStore(t *testing.T, cfg Config) (*Store, *clock) {
	t.Helper()
	s, err := open(filepath.Join(t.TempDir(), "failures.sqlite"), false, cfg, logging.TestingLog(t))
	require.NoError(t, err)
	t.Cleanup(s.Close)
	c := &clock{t: time.Unix(1_700_000_000, 0)}
	s.now = c.now
	return s, c
}

func simRecord(round uint64, body string) Record {
	return Record{
		Kind:    KindSimulate,
		Round:   basics.Round(1000 + round),
		Groups:  []GroupFailure{{Index: 1, Message: "logic eval error: assert failed", FailedAt: []uint64{0, 2}}},
		Request: []byte(body),
		Format:  "json",
		Proto:   protocol.ConsensusCurrentVersion,
		Version: "test",
	}
}

func TestCaptureGet(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, _ := testStore(t, Config{Retention: time.Hour})
	in := simRecord(1, `{"txn-groups":[]}`)
	id := s.Capture(in)
	require.Len(t, id, 24)
	s.Flush()

	out, err := s.Get(context.Background(), id)
	require.NoError(t, err)
	in.ID, in.Time = id, out.Time
	require.Equal(t, in, out)
	require.NotZero(t, out.Time)

	_, err = s.Get(context.Background(), "nope")
	require.ErrorIs(t, err, ErrNotFound)
	require.Positive(t, s.Bytes())
	require.Zero(t, s.Dropped())
}

func TestListPages(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, c := testStore(t, Config{})
	var ids []string
	for i := range 5 {
		ids = append(ids, s.Capture(simRecord(uint64(i), "body")))
		c.add(time.Second)
	}
	s.Flush()

	ctx := context.Background()
	var got []string
	var before uint64
	for {
		page, err := s.List(ctx, before, "", 2)
		require.NoError(t, err)
		for _, r := range page.Records {
			require.Nil(t, r.Request, "a listing leaves the request out")
			got = append(got, r.ID)
		}
		if page.Next == 0 {
			break
		}
		before = page.Next
	}
	require.Equal(t, []string{ids[4], ids[3], ids[2], ids[1], ids[0]}, got)

	page, err := s.List(ctx, 0, "other", 10)
	require.NoError(t, err)
	require.Empty(t, page.Records)
}

func TestRetention(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, c := testStore(t, Config{Retention: time.Hour})
	old := s.Capture(simRecord(1, "old"))
	s.Flush()
	c.add(50 * time.Minute)
	recent := s.Capture(simRecord(2, "recent"))
	s.Flush()
	c.add(20 * time.Minute) // old is 70 minutes old, recent 20

	ctx := context.Background()
	// expired but not pruned yet: hidden all the same
	_, err := s.Get(ctx, old)
	require.ErrorIs(t, err, ErrNotFound)
	page, err := s.List(ctx, 0, "", 10)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Equal(t, recent, page.Records[0].ID)
	page, err = s.List(ctx, 0, "", 1)
	require.NoError(t, err)
	require.Len(t, page.Records, 1)
	require.Zero(t, page.Next, "the next record is expired: no next page")

	before := s.Bytes()
	s.prune()
	require.Less(t, s.Bytes(), before)
	_, err = s.Get(ctx, recent)
	require.NoError(t, err)
}

func TestMaxBytes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	one := int64(len(protocol.EncodeJSON(func() *Record {
		r := simRecord(0, "0123456789")
		r.ID, r.Time = newID(), time.Now().UnixMilli()
		return &r
	}()))+len("0123456789")) + recordOverhead
	// room for three records, give or take a few bytes of the time's encoding
	s, _ := testStore(t, Config{MaxBytes: uint64(3*one + 8)})
	var ids []string
	for i := range 5 {
		ids = append(ids, s.Capture(simRecord(uint64(i), "0123456789")))
		s.Flush()
	}
	require.LessOrEqual(t, s.Bytes(), 3*one+8)

	ctx := context.Background()
	for i, id := range ids {
		_, err := s.Get(ctx, id)
		if i < 2 {
			require.ErrorIs(t, err, ErrNotFound, "record %d", i)
		} else {
			require.NoError(t, err, "record %d", i)
		}
	}

	// a record larger than the whole store is dropped
	s.Capture(simRecord(9, string(make([]byte, 4*one))))
	s.Flush()
	require.EqualValues(t, 1, s.Dropped())
}

func TestReopen(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	path := filepath.Join(t.TempDir(), "failures.sqlite")
	s, err := Open(path, Config{}, logging.TestingLog(t))
	require.NoError(t, err)
	id := s.Capture(simRecord(1, "kept"))
	s.Close() // stores what was captured
	bytes := s.Bytes()

	s, err = Open(path, Config{}, logging.TestingLog(t))
	require.NoError(t, err)
	defer s.Close()
	require.Equal(t, bytes, s.Bytes())
	r, err := s.Get(context.Background(), id)
	require.NoError(t, err)
	require.Equal(t, []byte("kept"), r.Request)
}

func TestCaptureNeverBlocks(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// no writer: the queue fills
	s := &Store{queue: make(chan Record, queueSize), now: time.Now}
	kept := 0
	for range queueSize + 10 {
		if s.Capture(simRecord(1, "x")) != "" {
			kept++
		}
	}
	require.Equal(t, queueSize, kept)
	require.EqualValues(t, 10, s.Dropped())

	// the queue's requests are bounded in bytes too
	s = &Store{queue: make(chan Record, queueSize), now: time.Now}
	big := string(make([]byte, 1<<20))
	kept = 0
	for range 40 {
		if s.Capture(simRecord(1, big)) != "" {
			kept++
		}
	}
	require.Equal(t, queueBytes>>20, kept)
	require.EqualValues(t, 40-kept, s.Dropped())
}

func TestClose(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	s, err := Open(filepath.Join(t.TempDir(), "failures.sqlite"), Config{}, logging.TestingLog(t))
	require.NoError(t, err)
	s.Close()
	require.Empty(t, s.Capture(simRecord(1, "late")), "dropped once closed")
	require.EqualValues(t, 1, s.Dropped())
	s.Close() // again: nothing
}
