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

package ledger

import (
	"context"
	"encoding/binary"
	"fmt"
	"math/rand"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// ttFixture is a time travel ledger whose rounds each change an account, an
// asset holding, an app's state and a box, with each round's state recorded
// while the round was the latest, from the trackers.
type ttFixture struct {
	t     *testing.T
	dl    DoubleLedger
	l     *Ledger
	addrs []basics.Address
	asa   basics.AssetIndex
	app   basics.AppIndex
	first basics.Round // the first recorded round

	mu     sync.Mutex
	last   basics.Round // the last recorded round
	snaps  map[basics.Round]ttSnapshot
	lists  map[basics.Round]ttListings
	deltas map[basics.Round]ledgercore.StateDelta
}

// ttCommitLookback is MaxAcctLookback: the trackers hold only the last rounds,
// so that most reads come from the history.
const ttCommitLookback = 4

func newTTFixture(t *testing.T) *ttFixture {
	genBalances, addrs, _ := ledgertesting.NewTestGenesis()
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	cfg.MaxAcctLookback = ttCommitLookback
	// on disk, as nodes run: the commit's transaction stays open while the
	// tests read
	dl := NewDoubleLedger(t, genBalances, protocol.ConsensusFuture, cfg, simpleLedgerOnDisk())
	t.Cleanup(dl.Close)
	f := &ttFixture{t: t, dl: dl, l: dl.validator, addrs: addrs,
		snaps: map[basics.Round]ttSnapshot{}, lists: map[basics.Round]ttListings{}, deltas: map[basics.Round]ledgercore.StateDelta{}}
	f.asa = dl.txn(&txntest.Txn{Type: "acfg", Sender: addrs[0], AssetParams: basics.AssetParams{Total: 1e12, UnitName: "x"}}).ApplyData.ConfigAsset
	dl.txn(&txntest.Txn{Type: "axfer", Sender: addrs[1], AssetReceiver: addrs[1], XferAsset: f.asa})
	f.app = dl.createApp(addrs[2], counterApp, basics.StateSchema{NumUint: 1})
	dl.fullBlock(&txntest.Txn{Type: "pay", Sender: addrs[2], Receiver: f.app.Address(), Amount: 10_000_000})
	f.first = f.l.Latest() + 1
	// rounds in the store already
	for range 20 {
		f.addRound()
	}
	commitRoundLookback(ttCommitLookback, f.l)
	return f
}

// addRound adds a round and records its state.
func (f *ttFixture) addRound() {
	t, dl, l := f.t, &f.dl, f.l
	r := l.Latest() + 1
	dl.beginBlock()
	dl.txns(
		&txntest.Txn{Type: "pay", Sender: f.addrs[3], Receiver: f.addrs[4], Amount: uint64(r)},
		&txntest.Txn{Type: "axfer", Sender: f.addrs[0], AssetReceiver: f.addrs[1], XferAsset: f.asa, AssetAmount: uint64(r)},
		&txntest.Txn{Type: "appl", Sender: f.addrs[3], ApplicationID: f.app, ApplicationArgs: [][]byte{binary.BigEndian.AppendUint64(nil, uint64(r))},
			Boxes: []transactions.BoxRef{{Name: []byte("b")}}},
	)
	dl.endBlock()
	snap := ttSnap(t, l, r, f.addrs, f.asa, f.app)
	list := ttList(t, l, f.addrs)
	sd, err := l.GetStateDeltaForRound(r)
	require.NoError(t, err)
	f.mu.Lock()
	f.snaps[r], f.lists[r], f.deltas[r] = snap, list, sd
	f.last = r
	f.mu.Unlock()
}

// check reads round r as the trackers and the history serve it, and compares
// it with what the round recorded.
func (f *ttFixture) check(t require.TestingT, r basics.Round, msg string) {
	f.mu.Lock()
	snap, list := f.snaps[r], f.lists[r]
	f.mu.Unlock()
	require.Equal(t, snap, ttSnap(t, f.l, r, f.addrs, f.asa, f.app), "%s: round %d", msg, r)
	require.Equal(t, list, ttListAt(t, f.l, r, f.addrs), "%s: round %d listings", msg, r)
}

// checkAll checks every recorded round, and the deltas of the rounds from
// deltasFrom on: those still in the trackers and those just out of them, which
// are re-evaluated from the history.
func (f *ttFixture) checkAll(msg string, deltasFrom basics.Round) {
	t := f.t
	f.mu.Lock()
	last := f.last
	f.mu.Unlock()
	require.Equal(t, last, f.l.Latest())
	for r := f.first; r <= last; r++ {
		f.check(t, r, msg)
	}
	for r := max(deltasFrom, f.first); r <= last; r++ {
		sd, err := f.l.GetStateDeltaForRound(r)
		require.NoError(t, err, "%s: round %d", msg, r)
		require.Equal(t, deltaJSON(t, f.deltas[r]), deltaJSON(t, sd), "%s: round %d", msg, r)
	}
}

// forceCommit asks for a commit of the rounds out of the trackers' window.
func (f *ttFixture) forceCommit() {
	tr := &f.l.trackers
	tr.mu.Lock()
	tr.lastFlushTime = time.Time{}
	tr.mu.Unlock()
	tr.scheduleCommit(f.l.Latest(), ttCommitLookback)
}

// Steps a commitPauser pauses commits at.
const (
	pauseNone int32 = iota
	pausePrepare
	pauseCommit
	pausePostCommit
)

// commitPauser is a tracker that pauses a commit at one step until the test
// releases it, and that can hold commits off.
type commitPauser struct {
	emptyTracker
	hold    atomic.Bool  // produce no commits
	at      atomic.Int32 // the step the next commit pauses at
	upTo    atomic.Uint64
	paused  chan *deferredCommitContext
	release chan struct{}
	done    chan struct{} // closed when the test ends, failed or not
}

// newCommitPauser returns a pauser that lets its commit go when the test
// ends: created after the ledger, its cleanup runs before the ledger's Close,
// which waits for the commit.
func newCommitPauser(t *testing.T) *commitPauser {
	p := &commitPauser{paused: make(chan *deferredCommitContext), release: make(chan struct{}), done: make(chan struct{})}
	t.Cleanup(func() { close(p.done) })
	return p
}

func (p *commitPauser) pause(step int32, dcc *deferredCommitContext) {
	if p.at.CompareAndSwap(step, pauseNone) {
		select {
		case p.paused <- dcc:
		case <-p.done:
			return
		}
		select {
		case <-p.release:
		case <-p.done:
		}
	}
}

// committedUpTo records the round the block queue last wrote, and keeps no
// blocks for the tracker.
func (p *commitPauser) committedUpTo(rnd basics.Round) (basics.Round, basics.Round) {
	p.upTo.Store(uint64(rnd))
	return rnd, 0
}

func (p *commitPauser) produceCommittingTask(_ basics.Round, _ basics.Round, dcr *deferredCommitRange) *deferredCommitRange {
	if p.hold.Load() {
		return nil
	}
	return dcr
}

func (p *commitPauser) prepareCommit(dcc *deferredCommitContext) error {
	p.pause(pausePrepare, dcc)
	return nil
}

func (p *commitPauser) commitRound(_ context.Context, _ trackerdb.TransactionScope, dcc *deferredCommitContext) error {
	p.pause(pauseCommit, dcc)
	return nil
}

func (p *commitPauser) postCommit(_ context.Context, dcc *deferredCommitContext) {
	p.pause(pausePostCommit, dcc)
}

// waitSyncer waits until the block queue wrote every block and told the
// trackers. Its notifyCommit holds trackerMu while it waits for the tracker
// registry's lock, which postCommit holds: reads would wait on a paused
// postCommit.
func (f *ttFixture) waitSyncer(p *commitPauser) {
	f.l.WaitForCommit(f.l.Latest())
	require.Eventually(f.t, func() bool { return basics.Round(p.upTo.Load()) >= f.l.Latest() }, time.Minute, time.Millisecond)
	f.l.trackerMu.Lock() // notifyCommit holds it until it is done
	f.l.trackerMu.Unlock()
}

// pendingRange returns the first and last rounds the history tracker holds in
// memory.
func pendingRange(ht *historyTracker) (basics.Round, basics.Round) {
	ht.mu.Lock()
	defer ht.mu.Unlock()
	if len(ht.pending) == 0 {
		return 0, 0
	}
	return ht.pending[0].rnd, ht.pending[len(ht.pending)-1].rnd
}

// TestTimeTravelReadsDuringCommit pauses a commit at each step and reads every
// round, those the commit moves from the trackers to the history among them,
// before, while and after it is paused. At each pause, the test first checks
// that the ledger is in the state the step stands for. A round added during
// the commit is read too.
func TestTimeTravelReadsDuringCommit(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := newTTFixture(t)
	l, ht, tr := f.l, f.l.history, &f.l.trackers

	// late runs after the history tracker; beforeHist after every other
	// tracker, so that its postCommit pauses with the trackers' window moved
	// and the history's in-memory rounds not yet trimmed
	late, beforeHist := newCommitPauser(t), newCommitPauser(t)
	late.hold.Store(true)
	tr.waitAccountsWriting()
	tr.mu.Lock()
	n := len(tr.trackers)
	require.Equal(t, ledgerTracker(ht), tr.trackers[n-1], "the history tracker is the last one")
	tr.trackers = append(tr.trackers[:n-1:n-1], beforeHist, ht, late)
	tr.mu.Unlock()

	storeLast := func() basics.Round {
		last, ok := ht.store.Last()
		require.True(t, ok)
		return basics.Round(last)
	}

	for _, st := range []struct {
		name   string
		p      *commitPauser
		step   int32
		addRnd bool // add a round while paused
		// window checks the ledger is in the state the step stands for
		window func(dcc *deferredCommitContext)
	}{
		{"prepareCommit", late, pausePrepare, true, func(dcc *deferredCommitContext) {
			require.NotNil(t, ht.write, "the history write is started")
		}},
		{"commitRound", late, pauseCommit, true, func(dcc *deferredCommitContext) {
			require.GreaterOrEqual(t, storeLast(), dcc.newBase(), "the store has the rounds")
			_, _, err := l.accts.LookupWithoutRewards(dcc.oldBase, f.addrs[4])
			require.NoError(t, err, "the trackers still have the old base")
			lo, _ := pendingRange(ht)
			require.LessOrEqual(t, lo, dcc.oldBase+1, "the rounds are still in memory")
		}},
		// adding a round here would stall reads: see waitSyncer
		{"postCommit", beforeHist, pausePostCommit, false, func(dcc *deferredCommitContext) {
			require.GreaterOrEqual(t, storeLast(), dcc.newBase(), "the store has the rounds")
			_, _, err := l.accts.LookupWithoutRewards(dcc.oldBase, f.addrs[4])
			require.True(t, tooOld(err), "the trackers moved past the old base: %v", err)
			lo, _ := pendingRange(ht)
			require.LessOrEqual(t, lo, dcc.oldBase+1, "the rounds are still in memory")
		}},
	} {
		for range 2 * ttCommitLookback {
			f.addRound()
		}
		f.checkAll(st.name+" before", l.Latest()-2*ttCommitLookback)
		f.waitSyncer(late)

		st.p.at.Store(st.step)
		late.hold.Store(false)
		f.forceCommit()
		var dcc *deferredCommitContext
		select {
		case dcc = <-st.p.paused:
		case <-time.After(time.Minute):
			require.FailNow(t, "no commit paused", st.name)
		}
		late.hold.Store(true) // the rounds added while paused stay in memory
		require.Greater(t, dcc.newBase(), dcc.oldBase)
		st.window(dcc)

		f.checkAll(st.name+" paused", dcc.oldBase-1)
		if st.addRnd {
			f.addRound()
			f.checkAll(st.name+" paused, with a new round", dcc.oldBase-1)
		}

		st.p.release <- struct{}{}
		tr.waitAccountsWriting()
		require.Equal(t, dcc.newBase(), tr.getDbRound())
		require.GreaterOrEqual(t, storeLast(), dcc.newBase())
		lo, hi := pendingRange(ht)
		require.Equal(t, dcc.newBase()+1, lo, "postCommit trimmed the stored rounds")
		require.Equal(t, l.Latest(), hi)
		f.checkAll(st.name+" after", dcc.oldBase-1)
	}
}

// goT is the require.TestingT of a reader goroutine, which may not call
// t.FailNow: a failure ends the goroutine and the test reports it.
type goT struct{ errs []string }

func (g *goT) Errorf(format string, args ...any) {
	g.errs = append(g.errs, fmt.Sprintf(format, args...))
}
func (g *goT) FailNow() { runtime.Goexit() }

// TestTimeTravelReadsWithConcurrentCommits reads rounds from goroutines while
// rounds are added and committed as fast as the trackers go: each read must
// return what the round recorded, wherever the round is at the time. Half
// the reads are of the rounds around the trackers' DB round, those commits
// move. Run it with -race.
func TestTimeTravelReadsWithConcurrentCommits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	f := newTTFixture(t)
	l := f.l
	stop := make(chan struct{})
	var wg sync.WaitGroup

	var commits int // dbRound changes the committer saw
	wg.Add(1)
	go func() {
		defer wg.Done()
		prev := l.LatestTrackerCommitted()
		for {
			select {
			case <-stop:
				return
			default:
			}
			f.forceCommit()
			time.Sleep(time.Millisecond)
			if r := l.LatestTrackerCommitted(); r != prev {
				commits, prev = commits+1, r
			}
		}
	}()

	const readers = 4
	readerTs := make([]*goT, readers)
	reads := make([]int, readers)
	for i := range readers {
		rt := &goT{}
		readerTs[i] = rt
		wg.Add(1)
		go func() {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(i)))
			for {
				select {
				case <-stop:
					return
				default:
				}
				f.mu.Lock()
				last := f.last
				f.mu.Unlock()
				lo := f.first
				if rng.Intn(2) == 0 {
					lo = max(lo, l.LatestTrackerCommitted().SubSaturate(2))
				}
				r := lo + basics.Round(rng.Int63n(int64(last-lo+1)))
				f.check(rt, r, fmt.Sprintf("reader %d", i))
				reads[i]++
			}
		}()
	}

	const rounds = 200
	for range rounds {
		f.addRound()
	}
	close(stop)
	wg.Wait()
	for i, rt := range readerTs {
		require.Empty(t, rt.errs, "reader %d", i)
		require.NotZero(t, reads[i], "reader %d", i)
	}
	require.Greater(t, commits, 20)
	f.checkAll("after", l.LatestTrackerCommitted()-ttCommitLookback)
}
