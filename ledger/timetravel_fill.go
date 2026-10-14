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
	"fmt"
	"os"
	"path/filepath"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/store/trackerdb/sqlitedriver"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

// TimeTravelRounds supplies the rounds FillTimeTravelGap adds: each one's state
// delta (as GET /v2/deltas/{round} returns it) and block header.
type TimeTravelRounds interface {
	Delta(rnd basics.Round) (*ledgercore.StateDelta, error)
	Header(rnd basics.Round) (bookkeeping.BlockHeader, error)
}

// timeTravelFillCheck is how many of the stored rounds FillTimeTravelGap
// recomputes and compares before it adds any.
const timeTravelFillCheck = 10

// FillTimeTravelGap adds to the time travel store of a stopped node's data
// directory the rounds after its last one, up to the account DB round, which
// the ledger then refuses to open with: an algod without time travel run on the
// directory advances the account DB alone. The rounds come from src, with the
// expired stake computed from the online accounts the account DB still holds
// history for (its last MaxBalLookback rounds). It first recomputes the last
// timeTravelFillCheck stored rounds and adds nothing unless their digests,
// headers and expired stake match the store's. It returns the rounds added,
// none (from > to) if the store already reaches the account DB.
func FillTimeTravelGap(dirs DirsAndPrefix, cfg config.Local, src TimeTravelRounds, log logging.Logger, progress func(rnd basics.Round)) (from, to basics.Round, err error) {
	if cfg.StorageEngine == "pebbledb" {
		return 0, 0, fmt.Errorf("filling the time travel history supports the sqlite account DB only")
	}
	trackerPath := filepath.Join(dirs.TrackerGenesisDir, dirs.DBFilePrefix) + ".tracker.sqlite"
	if _, err = os.Stat(trackerPath); err != nil {
		return 0, 0, err
	}
	if _, err = os.Stat(TimeTravelPath(dirs)); err != nil {
		return 0, 0, fmt.Errorf("no time travel store: %w", err)
	}
	ht, err := openHistoryTracker(dirs, cfg, log)
	if err != nil {
		return 0, 0, err
	}
	defer ht.store.Close()
	last, ok := ht.store.Last()
	if !ok {
		return 0, 0, fmt.Errorf("time travel store %s is empty", ht.path)
	}

	trackerDBs, err := sqlitedriver.Open(trackerPath, false, log)
	if err != nil {
		return 0, 0, err
	}
	defer trackerDBs.Close()
	var dbRound basics.Round
	var rows []trackerdb.PersistedOnlineAccountData
	err = trackerDBs.Snapshot(func(ctx context.Context, tx trackerdb.SnapshotScope) error {
		ar, err0 := tx.MakeAccountsReader()
		if err0 != nil {
			return err0
		}
		if dbRound, err0 = ar.AccountsRound(); err0 != nil {
			return err0
		}
		rows, err0 = ar.OnlineAccountsAll(0)
		return err0
	})
	if err != nil {
		return 0, 0, fmt.Errorf("reading %s: %w", trackerPath, err)
	}
	from, to = basics.Round(last)+1, dbRound
	if from > to {
		return from, to, nil
	}

	start := basics.Round(last).SubSaturate(timeTravelFillCheck)
	hdr, err := src.Header(to)
	if err != nil {
		return from, to, err
	}
	// OnlineAccountsDelete keeps, of the rows before (dbRound+1)-MaxBalLookback,
	// only each address's latest: the online accounts are known from the round before
	if lookback := basics.Round(config.Consensus[hdr.CurrentProtocol].MaxBalLookback); start+lookback < to+1 {
		return from, to, fmt.Errorf("the account DB at round %d no longer holds the online accounts of round %d", to, start)
	}
	// the online accounts at start, as loadOnline loads them at the tracker DB round
	accts := map[basics.Address]onlineEntry{}
	for _, r := range rows {
		if r.UpdRound > start {
			continue
		}
		if r.AccountData.IsVotingEmpty() {
			delete(accts, r.Addr)
		} else {
			accts[r.Addr] = onlineEntry{microAlgos: r.AccountData.MicroAlgos.Raw, rewardsBase: r.AccountData.RewardsBase, voteLastValid: r.AccountData.VoteLastValid}
		}
	}
	ht.online = onlineSet{accts: accts}

	for rnd := start + 1; rnd <= to; rnd++ {
		delta, err := src.Delta(rnd)
		if err != nil {
			return from, to, fmt.Errorf("round %d: %w", rnd, err)
		}
		if hdr, err = src.Header(rnd); err != nil {
			return from, to, fmt.Errorf("round %d: %w", rnd, err)
		}
		if delta.Hdr != nil && delta.Hdr.Round != rnd {
			return from, to, fmt.Errorf("round %d: the delta is of round %d", rnd, delta.Hdr.Round)
		}
		changes := timetravel.Changes(delta)
		enc := protocol.Encode(&hdr)
		expired := ht.expiredStake(bookkeeping.Block{BlockHeader: hdr}, delta)
		if rnd < from {
			if err = checkStoredRound(ht.store, rnd, changes, enc, expired); err != nil {
				return from, to, err
			}
			continue
		}
		if err = ht.store.AddRound(uint64(rnd), changes, enc, expired); err != nil {
			return from, to, err
		}
		if progress != nil {
			progress(rnd)
		}
	}
	return from, to, ht.store.Sync()
}

// checkStoredRound fails unless round rnd is stored with these changes, header
// and expired stake.
func checkStoredRound(s *timetravel.Store, rnd basics.Round, changes []timetravel.Change, hdr []byte, expired *uint64) error {
	digest, ok, err := s.Digest(uint64(rnd))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("the time travel store lacks round %d", rnd)
	}
	if !bytes.Equal(digest, timetravel.ChangesDigest(changes)) {
		return fmt.Errorf("round %d: the changes from the delta differ from the stored ones", rnd)
	}
	stored, ok, err := s.Header(uint64(rnd))
	if err != nil {
		return err
	}
	if !ok || !bytes.Equal(stored, hdr) {
		return fmt.Errorf("round %d: the header differs from the stored one", rnd)
	}
	stake, ok, err := s.ExpiredStake(uint64(rnd))
	if err != nil {
		return err
	}
	if ok != (expired != nil) || (ok && stake != *expired) {
		return fmt.Errorf("round %d: the expired stake differs from the stored one", rnd)
	}
	return nil
}
