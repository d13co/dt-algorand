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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/blockdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// A time travel node can catch up from a catchpoint when its history store
// was installed beforehand (a snapshot of another node's store, see
// tools/ttsnap): the catchpoint brings the state at its balances round B,
// verified against its label as usual, and the history must reach B. Before
// the catchup swaps the tables, CompleteCatchup checks the history against
// what the catchpoint verified: its header of round B (hash-chained to the
// labelled block) and its totals. The ledger then replays the rounds after B,
// and newBlock checks each one the store already holds against its own
// evaluation (verifyReplayed). A history that differs is not served.

// ErrHistoryUntrusted is returned for lookups of a history that differs from
// the chain. The store has to be replaced.
var ErrHistoryUntrusted = errors.New("the time travel history differs from the chain and is not served")

// ErrHistoryTooShort is returned for a catchpoint whose balances round the
// installed history does not reach.
var ErrHistoryTooShort = errors.New("the time travel history does not reach the catchpoint")

// checkStart checks that the store starts with this network's genesis block.
func (ht *historyTracker) checkStart() error {
	if ht.genesisHdr == nil {
		return nil
	}
	h0, ok, err := ht.store.Header(0)
	if err != nil {
		return err
	}
	if !ok {
		first, found, ferr := ht.store.First()
		if ferr != nil {
			return ferr
		}
		if !found {
			return fmt.Errorf("time travel store %s holds no headers", ht.path)
		}
		return fmt.Errorf("time travel store %s starts at round %d, not at genesis", ht.path, first)
	}
	if !bytes.Equal(h0, ht.genesisHdr) {
		return fmt.Errorf("time travel store %s holds another network's genesis block", ht.path)
	}
	return nil
}

// checkHeader compares the store's header of round rnd with the block DB's,
// if the block DB still has it. The ledger's BlockHdr would fall back to the
// history itself, so this reads the block DB directly.
func (ht *historyTracker) checkHeader(l ledgerForTracker, rnd basics.Round) error {
	var hdr bookkeeping.BlockHeader
	rdb := l.blockDB().Rdb
	err := rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		hdr, err = blockdb.BlockGetHdr(tx, rnd)
		return err
	})
	var noEntry ledgercore.ErrNoEntry
	if errors.As(err, &noEntry) {
		return nil
	}
	if err != nil {
		return err
	}
	return ht.sameHeader(rnd, hdr)
}

func (ht *historyTracker) sameHeader(rnd basics.Round, hdr bookkeeping.BlockHeader) error {
	stored, ok, err := ht.store.Header(uint64(rnd))
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(stored, protocol.Encode(&hdr)) {
		return fmt.Errorf("time travel store %s has another header for round %d than the blocks: it is from another chain, or corrupt", ht.path, rnd)
	}
	return nil
}

// loadDiverged marks the history untrusted if the store says it diverged.
func (ht *historyTracker) loadDiverged() error {
	reason, found, err := ht.store.Diverged()
	if err != nil || !found {
		return err
	}
	ht.mu.Lock()
	ht.untrusted = fmt.Errorf("%w: %s", ErrHistoryUntrusted, reason)
	ht.mu.Unlock()
	ht.log.Errorf("time travel store %s is marked as differing from the chain (%s): the history is not served until the store is replaced", ht.path, reason)
	return nil
}

// verifyReplayed checks a round the store already holds, which the ledger
// replays after a restart or a catchpoint catchup, against its evaluation. A
// difference marks the history untrusted, durably, rather than failing the
// round: the trackers keep committing, and only the history is not served.
func (ht *historyTracker) verifyReplayed(r historyRound) {
	err := ht.store.Verify(uint64(r.rnd), r.changes, r.hdr, r.expired)
	if err == nil || errors.Is(err, timetravel.ErrClosed) {
		return
	}
	if !errors.Is(err, timetravel.ErrDiverged) {
		// a read error: the round is unverified, so the history is not
		// served, but only until a restart verifies it again
		ht.mu.Lock()
		if ht.untrusted == nil {
			ht.untrusted = fmt.Errorf("%w: round %d could not be verified: %v", ErrHistoryUntrusted, r.rnd, err)
		}
		ht.mu.Unlock()
		ht.log.Errorf("time travel: could not verify stored round %d, not serving the history until a restart: %v", r.rnd, err)
		return
	}
	ht.mu.Lock()
	first := ht.untrusted == nil
	if first {
		ht.untrusted = fmt.Errorf("%w: %v", ErrHistoryUntrusted, err)
	}
	ht.mu.Unlock()
	if !first {
		return
	}
	ht.log.Errorf("time travel store %s differs from the chain (%v): the history is not served until the store is replaced", ht.path, err)
	if merr := ht.store.MarkDiverged(err.Error()); merr != nil {
		ht.log.Errorf("time travel: could not mark store %s as diverged: %v", ht.path, merr)
	}
}

// covers checks that the store reaches round b from genesis.
func (ht *historyTracker) covers(b basics.Round) error {
	last, ok := ht.store.Last()
	if !ok {
		return fmt.Errorf("%w: the time travel store %s is empty; install a history snapshot first (tools/ttsnap)", ErrHistoryTooShort, ht.path)
	}
	if basics.Round(last) < b {
		return fmt.Errorf("%w: the history ends at round %d and the catchpoint needs round %d; install a newer history snapshot or use an older catchpoint", ErrHistoryTooShort, last, b)
	}
	return ht.checkStart()
}

// HistoryUntrusted returns an ErrHistoryUntrusted error if the time travel
// history was found to differ from the chain, nil otherwise.
func (l *Ledger) HistoryUntrusted() error {
	if l.history == nil {
		return nil
	}
	l.history.mu.Lock()
	defer l.history.mu.Unlock()
	return l.history.untrusted
}

// CheckHistoryForCatchpoint is a quick check, before a catchpoint catchup
// starts, that the time travel history reaches the catchpoint's balances
// round. It takes the lookback of the latest block's protocol; the catchup
// checks the exact balances round, and more, before it completes.
func (l *Ledger) CheckHistoryForCatchpoint(catchpointRound basics.Round) error {
	if l.history == nil {
		return nil
	}
	if err := l.HistoryUntrusted(); err != nil {
		return err
	}
	hdr, err := l.BlockHdr(l.Latest())
	if err != nil {
		return err
	}
	return l.history.covers(catchpointRound.SubSaturate(catchpointLookback(hdr.CurrentProtocol)))
}

func catchpointLookback(proto protocol.ConsensusVersion) basics.Round {
	if lb := config.Consensus[proto].CatchpointLookback; lb != 0 {
		return basics.Round(lb)
	}
	return basics.Round(config.Consensus[proto].MaxBalLookback)
}

// checkCatchpoint checks the history against a catchpoint about to be
// applied: it must reach the balances round b from genesis, hold the
// catchpoint's header of b, which the catchup authenticated through the hash
// chain from the labelled block, and the catchpoint's totals at b.
func (ht *historyTracker) checkCatchpoint(b basics.Round, hdr bookkeeping.BlockHeader, totals ledgercore.AccountTotals) error {
	if err := ht.loadDiverged(); err != nil {
		return err
	}
	ht.mu.Lock()
	untrusted := ht.untrusted
	ht.mu.Unlock()
	if untrusted != nil {
		return untrusted
	}
	if err := ht.covers(b); err != nil {
		return err
	}
	if err := ht.sameHeader(b, hdr); err != nil {
		return err
	}
	stored, ok, err := ht.store.Get(timetravel.TotalsKey, uint64(b))
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(stored, protocol.Encode(&totals)) {
		return fmt.Errorf("time travel store %s has other totals at round %d than the catchpoint", ht.path, b)
	}
	return nil
}

// checkCatchpointHistory runs checkCatchpoint with the staged catchpoint.
func (c *catchpointCatchupAccessorImpl) checkCatchpointHistory(ctx context.Context) error {
	ht := c.ledger.history
	if ht == nil {
		return nil
	}
	b, err := c.catchpointStore.ReadCatchpointStateUint64(ctx, trackerdb.CatchpointStateCatchupBalancesRound)
	if err != nil {
		return err
	}
	var totals ledgercore.AccountTotals
	err = c.ledger.trackerDB().Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
		ar, rerr := tx.MakeAccountsReader()
		if rerr != nil {
			return rerr
		}
		totals, rerr = ar.AccountsTotals(ctx, true)
		return rerr
	})
	if err != nil {
		return err
	}
	var hdr bookkeeping.BlockHeader
	rdb := c.ledger.blockDB().Rdb
	err = rdb.Atomic(func(ctx context.Context, tx *sql.Tx) (err error) {
		hdr, err = blockdb.BlockGetStagingHdr(tx, basics.Round(b))
		return err
	})
	if err != nil {
		return fmt.Errorf("reading the catchpoint's header of round %d: %w", b, err)
	}
	return ht.checkCatchpoint(basics.Round(b), hdr, totals)
}
