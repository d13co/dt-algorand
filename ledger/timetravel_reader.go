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
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/timetravel"
)

// ErrRoundOutOfRange is returned by a HistoryReader for a round its store
// does not hold.
var ErrRoundOutOfRange = errors.New("round outside the time travel history")

// HistoryReader answers lookups at past rounds from a time travel store
// alone, as a time travel ledger answers them from its history: for tools that
// open the store of a stopped node (timetravel.Options.ReadOnly). Unlike the
// ledger's, its lookups fail for a round outside [First, Last].
type HistoryReader struct {
	h     *histView
	first basics.Round
}

// NewHistoryReader reads s, which must hold at least one round and must not
// change while the reader is in use. The consensus protocol of the last
// round must be known (config.Consensus), since accounts are read with their
// pending rewards.
func NewHistoryReader(s *timetravel.Store) (*HistoryReader, error) {
	last, ok := s.Last()
	if !ok {
		return nil, errors.New("the time travel store is empty")
	}
	first, ok, err := s.First()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("the time travel store has no headers")
	}
	r := &HistoryReader{h: &histView{store: s, last: basics.Round(last)}, first: basics.Round(first)}
	hdr, err := r.BlockHdr(r.Last())
	if err != nil {
		return nil, err
	}
	if _, ok := config.Consensus[hdr.CurrentProtocol]; !ok {
		return nil, fmt.Errorf("round %d has an unknown consensus protocol %s: load the network's consensus.json", last, hdr.CurrentProtocol)
	}
	return r, nil
}

// First returns the first round of the history.
func (r *HistoryReader) First() basics.Round { return r.first }

// Last returns the last round of the history.
func (r *HistoryReader) Last() basics.Round { return r.h.last }

func (r *HistoryReader) check(rnd basics.Round) error {
	if rnd < r.first || rnd > r.h.last {
		return fmt.Errorf("%w: round %d, history %d..%d", ErrRoundOutOfRange, rnd, r.first, r.h.last)
	}
	return nil
}

func (r *HistoryReader) checkRange(vr VersionRange) error {
	if vr.Min < r.first || vr.Max > r.h.last {
		return fmt.Errorf("%w: rounds %d..%d, history %d..%d", ErrRoundOutOfRange, vr.Min, vr.Max, r.first, r.h.last)
	}
	if vr.Min > vr.Max {
		return fmt.Errorf("empty range of rounds %d..%d", vr.Min, vr.Max)
	}
	return nil
}

// BlockHdr returns the header of round rnd.
func (r *HistoryReader) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	if err := r.check(rnd); err != nil {
		return bookkeeping.BlockHeader{}, err
	}
	return histHeader(r.h, rnd)
}

// Totals returns the account totals at round rnd.
func (r *HistoryReader) Totals(rnd basics.Round) (ledgercore.AccountTotals, error) {
	if err := r.check(rnd); err != nil {
		return ledgercore.AccountTotals{}, err
	}
	return histTotals(r.h, rnd)
}

// LookupAccountAt returns an account's base record at round rnd with its
// pending rewards applied, and its balance before them. An account that does
// not exist at rnd is empty.
func (r *HistoryReader) LookupAccountAt(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.MicroAlgos, error) {
	if err := r.check(rnd); err != nil {
		return ledgercore.AccountData{}, basics.MicroAlgos{}, err
	}
	return histAccountWithRewards(r.h, rnd, addr)
}

// LookupLatestAt is Ledger.LookupLatestAt: the account at round rnd with all
// its resources and pending rewards applied.
func (r *HistoryReader) LookupLatestAt(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	if err := r.check(rnd); err != nil {
		return basics.AccountData{}, 0, basics.MicroAlgos{}, err
	}
	return histLatest(r.h, rnd, addr)
}

// LookupAssetsAt is Ledger.LookupAssetsAt.
func (r *HistoryReader) LookupAssetsAt(rnd basics.Round, addr basics.Address, assetIDGT basics.AssetIndex, limit uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	if err := r.check(rnd); err != nil {
		return nil, 0, err
	}
	return histAssets(r.h, rnd, addr, assetIDGT, limit)
}

// LookupApplicationsAt is Ledger.LookupApplicationsAt.
func (r *HistoryReader) LookupApplicationsAt(rnd basics.Round, addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	if err := r.check(rnd); err != nil {
		return nil, 0, err
	}
	return histApplications(r.h, rnd, addr, appIDGT, limit, includeParams)
}

// LookupKvPairsAt lists the kv pairs (boxes) with keyPrefix after cursor at
// round rnd, up to limit pairs or maxBytes (0: unlimited); more reports a
// truncation.
func (r *HistoryReader) LookupKvPairsAt(rnd basics.Round, keyPrefix, cursor string, limit, maxBytes uint64, includeValues bool) ([]ledgercore.KvPairResult, bool, error) {
	if err := r.check(rnd); err != nil {
		return nil, false, err
	}
	return histKvPairs(r.h, rnd, keyPrefix, cursor, limit, maxBytes, includeValues)
}

// AssetHoldersAt is Ledger.AssetHoldersAt. It needs the store's asset holders
// index (ErrNoHoldersIndex), whose state a read-only store takes from the
// store as it is.
func (r *HistoryReader) AssetHoldersAt(rnd basics.Round, aidx basics.AssetIndex, after *basics.Address, limit uint64) (AssetHoldersPage, error) {
	if err := r.check(rnd); err != nil {
		return AssetHoldersPage{}, err
	}
	return histAssetHolders(r.h, rnd, aidx, after, limit)
}

// AccountVersions is Ledger.AccountVersions.
func (r *HistoryReader) AccountVersions(addr basics.Address, ex AccountVersionsExclude, vr VersionRange) (VersionPage, error) {
	if err := r.checkRange(vr); err != nil {
		return VersionPage{}, err
	}
	return histAccountVersions(r.h, addr, ex, vr)
}

// AccountAssetVersions is Ledger.AccountAssetVersions.
func (r *HistoryReader) AccountAssetVersions(addr basics.Address, aidx basics.AssetIndex, vr VersionRange) (VersionPage, error) {
	if err := r.checkRange(vr); err != nil {
		return VersionPage{}, err
	}
	return histAccountAssetVersions(r.h, addr, aidx, vr)
}

// AccountAppVersions is Ledger.AccountAppVersions.
func (r *HistoryReader) AccountAppVersions(addr basics.Address, aidx basics.AppIndex, vr VersionRange) (VersionPage, error) {
	if err := r.checkRange(vr); err != nil {
		return VersionPage{}, err
	}
	return histAccountAppVersions(r.h, addr, aidx, vr)
}

// AssetVersions is Ledger.AssetVersions.
func (r *HistoryReader) AssetVersions(aidx basics.AssetIndex, vr VersionRange) (VersionPage, error) {
	return r.objectVersions(timetravel.AssetParamsKey(aidx), vr)
}

// AppVersions is Ledger.AppVersions.
func (r *HistoryReader) AppVersions(aidx basics.AppIndex, vr VersionRange) (VersionPage, error) {
	return r.objectVersions(timetravel.AppParamsKey(aidx), vr)
}

// KvVersions is Ledger.KvVersions.
func (r *HistoryReader) KvVersions(key string, vr VersionRange) (VersionPage, error) {
	return r.objectVersions(timetravel.KvKey(key), vr)
}

// AppBoxVersions is Ledger.AppBoxVersions.
func (r *HistoryReader) AppBoxVersions(app basics.AppIndex, vr VersionRange) (VersionPage, error) {
	if err := r.checkRange(vr); err != nil {
		return VersionPage{}, err
	}
	return histAppBoxVersions(r.h, app, vr)
}

func (r *HistoryReader) objectVersions(obj []byte, vr VersionRange) (VersionPage, error) {
	if err := r.checkRange(vr); err != nil {
		return VersionPage{}, err
	}
	return versions(r.h, [][]byte{obj}, vr)
}
