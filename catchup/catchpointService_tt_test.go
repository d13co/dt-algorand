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

package catchup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/components/mocks"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// historyAccessorMock's history step runs until its context ends, or until
// release is closed.
type historyAccessorMock struct {
	mocks.MockCatchpointCatchupAccessor
	started   chan struct{}
	release   chan struct{}
	completed bool
	reset     bool
	// completeInterrupted makes CompleteCatchup fail as its own history
	// step does when ctx ends
	completeInterrupted bool
	// onRelease runs as PrepareCatchpointHistory returns after release
	onRelease func()
	// releaseErr is what PrepareCatchpointHistory returns after release
	releaseErr error
}

func (m *historyAccessorMock) PrepareCatchpointHistory(ctx context.Context) error {
	close(m.started)
	select {
	case <-ctx.Done():
		return fmt.Errorf("%w: %w", ledger.ErrHistoryTruncationInterrupted, ctx.Err())
	case <-m.release:
		if m.onRelease != nil {
			m.onRelease()
		}
		return m.releaseErr
	}
}

func (m *historyAccessorMock) CompleteCatchup(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		if m.completeInterrupted {
			return fmt.Errorf("%w: %w", ledger.ErrHistoryTruncationInterrupted, err)
		}
		return err
	}
	m.completed = true
	return nil
}

func (m *historyAccessorMock) ResetStagingBalances(context.Context, bool) error {
	m.reset = true
	return nil
}

type catchupModeNode struct{}

func (catchupModeNode) SetCatchpointCatchupMode(bool) <-chan context.Context {
	ch := make(chan context.Context)
	close(ch) // the node is stopping
	return ch
}

// TestProcessStageSwitchHistory checks that a shutdown interrupts the time
// travel history's truncation and keeps the stage, to resume, while an abort
// waits for it and then aborts, unless a shutdown follows, and that a
// truncation that fails unfinished keeps the node in catchup mode.
func TestProcessStageSwitchHistory(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	setup := func(t *testing.T) (*CatchpointCatchupService, *historyAccessorMock, chan error) {
		ctx, cancel := context.WithCancel(context.Background())
		abortCtx, abortCancel := context.WithCancel(context.Background())
		stopCtx, stopCancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		t.Cleanup(abortCancel)
		t.Cleanup(stopCancel)
		acc := &historyAccessorMock{started: make(chan struct{}), release: make(chan struct{})}
		cs := &CatchpointCatchupService{
			ctx: ctx, cancelCtxFunc: cancel, abortCtx: abortCtx, abortCtxFunc: abortCancel,
			stopCtx: stopCtx, stopCtxFunc: stopCancel,
			ledgerAccessor: acc, node: catchupModeNode{}, log: logging.TestingLog(t),
			stage: ledger.CatchpointCatchupStateSwitch,
		}
		done := make(chan error, 1)
		go func() { done <- cs.processStageSwitch() }()
		<-acc.started
		return cs, acc, done
	}

	t.Run("stop", func(t *testing.T) {
		cs, acc, done := setup(t)
		cs.cancelCtxFunc() // as Stop does, before it marks abortCtx
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("a shutdown waits for the history")
		}
		require.False(t, acc.completed)
		require.False(t, acc.reset, "the staged catchpoint is kept, to resume")
		require.EqualValues(t, ledger.CatchpointCatchupStateSwitch, cs.stage)
	})

	t.Run("stop after the history", func(t *testing.T) {
		// a shutdown that CompleteCatchup's own history step sees resumes too
		cs, acc, done := setup(t)
		acc.completeInterrupted = true
		acc.onRelease = cs.cancelCtxFunc // a Stop between the two steps
		close(acc.release)
		require.NoError(t, <-done)
		require.False(t, acc.completed)
		require.False(t, acc.reset)
	})

	t.Run("abort", func(t *testing.T) {
		cs, acc, done := setup(t)
		cs.Abort()
		select {
		case <-done:
			t.Fatal("an abort cut the truncation short")
		case <-time.After(100 * time.Millisecond):
		}
		close(acc.release)
		require.Error(t, <-done)
		require.False(t, acc.completed, "the abort holds once the history is done")
		require.True(t, acc.reset)
	})

	t.Run("abort then stop", func(t *testing.T) {
		// a shutdown during the truncation an abort waits for interrupts it
		cs, acc, done := setup(t)
		cs.Abort()
		select {
		case <-done:
			t.Fatal("an abort cut the truncation short")
		case <-time.After(100 * time.Millisecond):
		}
		cs.Stop() // processStageSwitch runs outside run here: nothing to wait for
		select {
		case err := <-done:
			require.Error(t, err)
		case <-time.After(10 * time.Second):
			t.Fatal("a shutdown after an abort waits for the history")
		}
		require.False(t, acc.completed)
		require.True(t, acc.reset, "the abort goes on once the truncation stops")
	})

	t.Run("unfinished", func(t *testing.T) {
		// a truncation that fails and leaves the store refusing rounds neither
		// aborts nor completes: the next start resumes
		for _, abort := range []bool{false, true} {
			cs, acc, done := setup(t)
			acc.releaseErr = fmt.Errorf("%w: disk error", ledger.ErrHistoryTruncationUnfinished)
			if abort {
				cs.Abort()
			}
			close(acc.release)
			require.ErrorIs(t, <-done, ledger.ErrHistoryTruncationUnfinished)
			require.False(t, acc.completed)
			require.False(t, acc.reset, "abort=%v: the staged catchpoint is kept, to resume", abort)
			require.Error(t, cs.ctx.Err(), "run returns")
			require.EqualValues(t, ledger.CatchpointCatchupStateSwitch, cs.stage)
		}
	})
}
