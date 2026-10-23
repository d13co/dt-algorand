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
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/algorand/go-deadlock"
)

// progressEvery is how often a long task reports how far it is.
var progressEvery = 10 * time.Second

// progress reports a task of total bytes to w every progressEvery until
// stopped: how much is done, how fast it goes, and how long is left.
type progress struct {
	w     io.Writer
	verb  string // what the done bytes went through, e.g. "downloaded"
	total int64
	// done is the bytes of the task finished, including what an earlier run
	// left; moved is the bytes this run handled, which give the rate
	done, moved atomic.Int64
	extra       func() string // appended to each line, if set
	start       time.Time
	firstMove   atomic.Int64 // when this run first handled a byte, in Unix ns
	stop        chan struct{}
	stopOnce    sync.Once
	wg          sync.WaitGroup
}

// startProgress starts reporting to w; a nil w reports nothing.
func startProgress(w io.Writer, verb string, total int64, extra func() string) *progress {
	p := &progress{w: w, verb: verb, total: total, extra: extra, start: time.Now(), stop: make(chan struct{})}
	if w == nil {
		return p
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		tick := time.NewTicker(progressEvery)
		defer tick.Stop()
		for {
			select {
			case <-p.stop:
				return
			case <-tick.C:
				fmt.Fprintln(p.w, p.line(time.Now()))
			}
		}
	}()
	return p
}

// add counts n bytes done and handled by this run.
func (p *progress) add(n int64) {
	p.firstMove.CompareAndSwap(0, time.Now().UnixNano())
	p.done.Add(n)
	p.moved.Add(n)
}

// Stop stops the reports.
func (p *progress) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
	p.wg.Wait()
}

// line says how far the task is at now, e.g. "downloaded 12.3 of 65.0 GiB
// (19%), 105 MiB/s, about 9m left".
func (p *progress) line(now time.Time) string {
	done, moved := min(max(p.done.Load(), 0), p.total), p.moved.Load()
	// the rate counts from the first byte handled, not from checking what
	// an earlier run left
	since := p.start
	if first := p.firstMove.Load(); first != 0 {
		since = time.Unix(0, first)
	}
	elapsed := now.Sub(since)
	s := fmt.Sprintf("%s %s of %s", p.verb, sizeIn(done, p.total), size(p.total))
	if p.total > 0 {
		s += fmt.Sprintf(" (%d%%)", done*100/p.total)
	}
	if p.extra != nil {
		s += ", " + p.extra()
	}
	if elapsed > 0 && moved > 0 {
		rate := float64(moved) / elapsed.Seconds()
		s += fmt.Sprintf(", %s/s", size(int64(rate)))
		// a transfer that has barely started has no meaningful ETA
		if secs := float64(p.total-done) / rate; secs > 0 && secs < 100*3600 {
			s += fmt.Sprintf(", about %s left", roundDuration(time.Duration(secs*float64(time.Second))))
		}
	}
	return s
}

// size prints n bytes in KiB, MiB or GiB.
func size(n int64) string {
	return sizeIn(n, n) + " " + unit(n)
}

// sizeIn prints n bytes in the unit size picks for total, without it.
func sizeIn(n, total int64) string {
	switch {
	case total >= 1<<30:
		return fmt.Sprintf("%.1f", float64(n)/(1<<30))
	case total >= 1<<20:
		return fmt.Sprintf("%d", n>>20)
	default:
		return fmt.Sprintf("%d", n>>10)
	}
}

func unit(n int64) string {
	switch {
	case n >= 1<<30:
		return "GiB"
	case n >= 1<<20:
		return "MiB"
	default:
		return "KiB"
	}
}

// roundDuration rounds d to what a reader needs: seconds under a minute,
// minutes under an hour, else hours and minutes.
func roundDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return d.Round(time.Second).String()
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Round(time.Minute)/time.Minute))
	default:
		d = d.Round(time.Minute)
		return fmt.Sprintf("%dh%02dm", int(d/time.Hour), int(d%time.Hour/time.Minute))
	}
}

// syncWriter serializes the writes of several goroutines to w.
type syncWriter struct {
	mu deadlock.Mutex
	w  io.Writer
}

func (s *syncWriter) Write(b []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(b)
}

// countingWriter counts what is written through it into p.
type countingWriter struct {
	w io.Writer
	p *progress
}

func (c countingWriter) Write(b []byte) (int, error) {
	n, err := c.w.Write(b)
	c.p.add(int64(n))
	return n, err
}
