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
	"errors"
	"fmt"
	"slices"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// AssetHolder is an account that holds an asset at a round, and its holding.
type AssetHolder struct {
	Address basics.Address
	Holding basics.AssetHolding
}

// AssetHoldersPage is a page of an asset's holders, in address order.
type AssetHoldersPage struct {
	Holders []AssetHolder
	// More reports that more holders may remain after Next, the last address
	// the page examined, which may not be a holder's: the next page starts
	// after it. A page can be short, or empty, while more holders remain.
	More bool
	Next basics.Address
}

// ErrNoHoldersIndex is returned by AssetHoldersAt while the time travel
// store's asset holders index is missing or stale: ttimport -holders-index
// builds it.
var ErrNoHoldersIndex = timetravel.ErrNoHoldersIndex

// ErrAssetNotFound is returned by AssetHoldersAt for an asset that does not
// exist at the round.
var ErrAssetNotFound = errors.New("asset does not exist")

// holdersExamineCap bounds the index entries a page of holders examines, so
// that a page of an asset with few holders left at the round ends in bounded
// time. holdersBatch is the number of index entries read at once. Variables
// for tests.
var (
	holdersExamineCap = 100_000
	holdersBatch      = 1024
)

// InitHoldersIndex deletes the time travel store's asset holders index when on
// is false (TimeTravelHoldersIndex is off), and otherwise warns if it is not
// usable. Only the node calls it, after loading the ledger and before its
// services start: tools that load a ledger (ttimport) leave the index alone,
// and keep a complete one up to date. A no-op without EnableTimeTravel.
func (l *Ledger) InitHoldersIndex(on bool) error {
	if l.history == nil {
		return nil
	}
	s := l.history.store
	if !on {
		dropped, err := s.DropHoldersIndex()
		if err != nil {
			return fmt.Errorf("deleting the asset holders index: %w", err)
		}
		if dropped {
			l.log.Infof("deleted the asset holders index: TimeTravelHoldersIndex is off")
		}
		return nil
	}
	if n := s.CheckpointInterval(); n != 1 {
		l.log.Warnf("TimeTravelHoldersIndex needs a time travel store with N = 1, not %d: GET /v2/assets/{asset-id}/holders answers 400", n)
		return nil
	}
	if st := s.HoldersIndexState(); st != timetravel.HoldersComplete {
		l.log.Warnf("the asset holders index is %s: GET /v2/assets/{asset-id}/holders answers 503 until it is built: stop the node and run ttimport -holders-index -d on its data directory", st)
	}
	return nil
}

// HoldersIndexState returns the state of the time travel store's asset holders
// index and the store's checkpoint interval N, which the index needs to be 1.
func (l *Ledger) HoldersIndexState() (timetravel.HoldersState, uint64) {
	if l.history == nil {
		return timetravel.HoldersNone, 0
	}
	return l.history.store.HoldersIndexState(), l.history.store.CheckpointInterval()
}

// AssetHoldersAt lists the accounts that hold asset aidx at round rnd, zero
// balances included, in address order, from the one following the address
// after (nil: from the first), up to limit (> 0). A page also ends once it
// has examined holdersExamineCap index entries (accounts that ever held the
// asset), so it can be short or empty while more holders remain: More is the
// only reliable signal. A full page looks ahead for one more holder, so the
// last page has no More even when it is full, unless the look-ahead reaches
// the cap. Pages at a fixed round never change.
func (l *Ledger) AssetHoldersAt(rnd basics.Round, aidx basics.AssetIndex, after *basics.Address, limit uint64) (AssetHoldersPage, error) {
	h := l.historyNow(rnd)
	if h == nil {
		// the history may have been found untrusted since the caller checked
		if err := l.HistoryUntrusted(); err != nil {
			return AssetHoldersPage{}, err
		}
		return AssetHoldersPage{}, errHistoryUnavailable
	}
	return histAssetHolders(h, rnd, aidx, after, limit)
}

// pendingHolding is the last change, in a round in memory, of a holding.
type pendingHolding struct {
	addr    basics.Address
	val     []byte
	deleted bool
}

// holdersEntry is an asset holders index entry.
type holdersEntry struct {
	addr      basics.Address
	intervals []byte
}

// histAssetHolders merges the asset holders index, which reaches the store's
// last round h.last, with the holdings that the rounds in memory changed.
//
// The store may add rounds while the page is read, and their entries may be
// seen: they only append boundaries after h.last, which HeldAt at min(rnd,
// h.last) ignores, and h.pending, never modified in place, holds the rounds
// they are of. So the page needs no snapshot, and reads the index in batches.
func histAssetHolders(h *histView, rnd basics.Round, aidx basics.AssetIndex, after *basics.Address, limit uint64) (AssetHoldersPage, error) {
	var page AssetHoldersPage
	if limit == 0 {
		return page, errors.New("the limit of a page of holders must be positive")
	}
	if _, ok, err := histParams(h, rnd, timetravel.AssetParamsKey(aidx), nil); err != nil {
		return page, err
	} else if !ok {
		return page, fmt.Errorf("%w: asset %d at round %d", ErrAssetNotFound, aidx, rnd)
	}
	storeRnd := min(uint64(rnd), uint64(h.last))
	isAfter := func(a basics.Address) bool { return after == nil || bytes.Compare(a[:], after[:]) > 0 }

	// the holdings of aidx that the rounds in memory up to rnd changed, each
	// with its latest change
	var pend []pendingHolding
	seen := map[basics.Address]bool{}
	for i := len(h.pending) - 1; i >= 0 && h.pending[i].rnd > h.last; i-- {
		p := &h.pending[i]
		if p.rnd > rnd {
			continue
		}
		for _, ch := range p.changes {
			addr, asset, ok := timetravel.HoldingOf(ch.Key)
			if !ok || asset != uint64(aidx) || seen[addr] || !isAfter(addr) {
				continue
			}
			seen[addr] = true
			last := p.changes[p.idx[string(ch.Key)]] // the key's last change in the round
			pend = append(pend, pendingHolding{addr: addr, val: last.Value, deleted: last.Deleted})
		}
	}
	slices.SortFunc(pend, func(a, b pendingHolding) int { return bytes.Compare(a.addr[:], b.addr[:]) })

	// the index, in batches: the callback must not read the store
	var batch []holdersEntry
	var cursor []byte
	if after != nil {
		cursor = after[:]
	}
	storeDone := false
	peek := func() (*holdersEntry, error) {
		if len(batch) > 0 || storeDone {
			if len(batch) == 0 {
				return nil, nil
			}
			return &batch[0], nil
		}
		err := h.store.AssetHolders(uint64(aidx), cursor, func(addr, intervals []byte) bool {
			batch = append(batch, holdersEntry{addr: basics.Address(addr), intervals: intervals})
			return len(batch) < holdersBatch
		})
		if err != nil {
			return nil, err
		}
		if len(batch) < holdersBatch {
			storeDone = true
		}
		if len(batch) == 0 {
			return nil, nil
		}
		last := batch[len(batch)-1].addr
		cursor = last[:]
		return &batch[0], nil
	}

	// A full page goes on examining the accounts that do not hold the asset at
	// rnd, which cost no read, until it finds one that does: the last page has
	// no More even when it is full, and the next page starts at that holder.
	examined := 0
	for {
		se, err := peek()
		if err != nil {
			return AssetHoldersPage{}, err
		}
		if se == nil && len(pend) == 0 {
			return page, nil
		}
		if examined == holdersExamineCap {
			page.More = true
			return page, nil
		}
		// which comes first: the index (-1), the rounds in memory (1), or both (0),
		// in which case a round in memory changed it last
		c := 1
		if se != nil && len(pend) > 0 {
			c = bytes.Compare(se.addr[:], pend[0].addr[:])
		} else if se != nil {
			c = -1
		}
		var addr basics.Address
		var held bool
		if c >= 0 {
			addr, held = pend[0].addr, !pend[0].deleted
		} else {
			addr, held = se.addr, timetravel.HeldAt(se.intervals, storeRnd)
		}
		if held && uint64(len(page.Holders)) == limit {
			page.More = true
			return page, nil
		}
		examined++
		page.Next = addr
		var p pendingHolding
		if c <= 0 {
			batch = batch[1:]
		}
		if c >= 0 {
			p, pend = pend[0], pend[1:]
		}
		if !held {
			continue
		}
		var hold basics.AssetHolding
		if c >= 0 {
			if err := protocol.Decode(p.val, &hold); err != nil {
				return AssetHoldersPage{}, err
			}
		} else {
			// no round in memory up to rnd changed it, or it would be in pend
			v, ok, err := h.store.Get(timetravel.HoldingKey(addr, aidx), storeRnd)
			if err == nil && ok {
				err = protocol.Decode(v, &hold)
			}
			if err != nil {
				return AssetHoldersPage{}, err
			}
			if !ok {
				return AssetHoldersPage{}, fmt.Errorf("the asset holders index lists %s as holding asset %d at round %d, but it has no holding", addr, aidx, rnd)
			}
		}
		page.Holders = append(page.Holders, AssetHolder{Address: addr, Holding: hold})
	}
}
