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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestProgressLine(t *testing.T) {
	partitiontest.PartitionTest(t)
	p := startProgress(nil, "downloaded", 10<<30, nil)
	p.Stop()
	p.done.Store(3 << 30) // 1 GiB of it from an earlier run
	p.moved.Store(2 << 30)
	require.Equal(t, "downloaded 3.0 of 10.0 GiB (30%), 20 MiB/s, about 6m left", p.line(p.start.Add(100*time.Second)))

	p = startProgress(nil, "hashed", 300<<20, func() string { return "1 MiB new" })
	p.Stop()
	p.add(100 << 20)
	require.Equal(t, "hashed 100 of 300 MiB (33%), 1 MiB new, 10 MiB/s, about 20s left", p.line(p.start.Add(10*time.Second)))
	require.Equal(t, "hashed 0 of 300 MiB (0%), 1 MiB new", (&progress{verb: "hashed", total: 300 << 20, extra: p.extra, start: p.start}).line(p.start))

	// done is shown within 0..total
	p.done.Store(400 << 20)
	require.Contains(t, p.line(p.start.Add(10*time.Second)), "hashed 300 of 300 MiB (100%)")
	// no ETA for a transfer that has barely started
	p = startProgress(nil, "downloaded", 65<<30, nil)
	p.Stop()
	p.add(40)
	require.Equal(t, "downloaded 0.0 of 65.0 GiB (0%), 0 KiB/s", p.line(time.Unix(0, p.firstMove.Load()).Add(10*time.Second)))

	require.Equal(t, "512 KiB", size(512<<10))
	require.Equal(t, "45s", roundDuration(44600*time.Millisecond))
	require.Equal(t, "3m", roundDuration(3*time.Minute+20*time.Second))
	require.Equal(t, "2h05m", roundDuration(2*time.Hour+5*time.Minute))
}

// TestPublishSaysWhatItDid checks the lines publish writes at its start and
// end.
func TestPublishSaysWhatItDid(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	_, _, err := prepare(n.producer, false, nil)
	require.NoError(t, err)
	var log bytes.Buffer
	_, err = publish(n.producer, n.out, publishOpts{signer: n.signer}, &log)
	require.NoError(t, err)
	lines := strings.Split(strings.TrimSpace(log.String()), "\n")
	require.Regexp(t, `^publishing \d+ files, \d+ KiB, of rounds 0\.\.30$`, lines[0])
	require.Regexp(t, `(?m)^hashed \d+ KiB in .+, \d+ KiB of it new$`, log.String())
	require.Regexp(t, `^commitment of rounds 0\.\.30: \d+ objects, \d+ versions, 31 headers, 0 expired stake records, in .+$`, lines[len(lines)-1])
}

// TestDownloadCountsOnce checks that the bytes of a download cut short count
// once, though the retry resumes after them.
func TestDownloadCountsOnce(t *testing.T) {
	partitiontest.PartitionTest(t)
	data := bytes.Repeat([]byte("ttsnap"), 100_000)
	sum := sha256.Sum256(data)
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Write(data[:len(data)/2])
			w.(http.Flusher).Flush()
			panic(http.ErrAbortHandler)
		}
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
	}))
	defer srv.Close()

	o := fetchOpts{base: srv.URL, stallLimit: time.Minute, client: srv.Client()}
	tg := target{File: File{Path: historyDir + "/000001.sst", Size: int64(len(data)), SHA256: hex.EncodeToString(sum[:])}, stage: filepath.Join(t.TempDir(), "000001.sst")}
	prog := startProgress(nil, "downloaded", tg.Size, nil)
	defer prog.Stop()
	require.Error(t, o.downloadFile(context.Background(), tg, prog))
	require.Zero(t, prog.done.Load(), "a failed attempt counts nothing done")
	require.NoError(t, o.downloadFile(context.Background(), tg, prog))
	require.Equal(t, tg.Size, prog.done.Load())
	require.Equal(t, tg.Size, prog.moved.Load(), "the retry resumed: half and half")

	// a complete file counts as done without a transfer
	prog = startProgress(nil, "downloaded", tg.Size, nil)
	defer prog.Stop()
	require.NoError(t, o.downloadFile(context.Background(), tg, prog))
	require.Equal(t, tg.Size, prog.done.Load())
	require.Zero(t, prog.moved.Load())

	// a server without range requests sends the whole file again: the half
	// that was here counts once
	noRange := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(data)
	}))
	defer noRange.Close()
	o = fetchOpts{base: noRange.URL, stallLimit: time.Minute, client: noRange.Client()}
	require.NoError(t, os.Truncate(tg.stage, tg.Size/2))
	prog = startProgress(nil, "downloaded", tg.Size, nil)
	defer prog.Stop()
	require.NoError(t, o.downloadFile(context.Background(), tg, prog))
	require.Equal(t, tg.Size, prog.done.Load())
	require.Equal(t, tg.Size, prog.moved.Load())
}

// TestProgressReports checks that the reports come every progressEvery and
// stop with Stop.
func TestProgressReports(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer func(d time.Duration) { progressEvery = d }(progressEvery)
	progressEvery = time.Millisecond
	var buf bytes.Buffer
	w := &syncWriter{w: &buf}
	p := startProgress(w, "hashed", 4<<20, nil)
	p.add(1 << 20)
	require.Eventually(t, func() bool {
		w.mu.Lock()
		defer w.mu.Unlock()
		return strings.Contains(buf.String(), "hashed 1 of 4 MiB (25%)")
	}, 10*time.Second, time.Millisecond)
	p.Stop()
	p.Stop() // twice is fine
	w.mu.Lock()
	n := buf.Len()
	w.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	require.Equal(t, n, buf.Len(), "no reports after Stop")
}
