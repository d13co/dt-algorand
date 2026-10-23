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

// Lookups counted for one request (EnableEffortHeader). Non-standard fork feature, kept in its own
// file to keep upstream merges of ledger.go clean.

import (
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// CountedLedger is the ledger with its object lookups counted in an Effort: each lookup counts
// once, and as answered by the time travel history when the trackers no longer held its round.
// Scans of the history count the entries they read. With a nil Effort it is the plain ledger.
type CountedLedger struct {
	*Ledger
	e *ledgercore.Effort
}

// Counted returns the ledger with its lookups counted in e.
func (l *Ledger) Counted(e *ledgercore.Effort) CountedLedger {
	return CountedLedger{Ledger: l, e: e}
}

// Effort returns the Effort the lookups count in, or nil.
func (c CountedLedger) Effort() *ledgercore.Effort { return c.e }

// LookupAccount is Ledger.LookupAccount, counted.
func (c CountedLedger) LookupAccount(round basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, basics.MicroAlgos, error) {
	c.e.Lookup(false)
	return c.lookupAccount(round, addr, c.e)
}

// LookupWithoutRewards is Ledger.LookupWithoutRewards, counted.
func (c CountedLedger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, error) {
	c.e.Lookup(false)
	return c.lookupWithoutRewards(rnd, addr, c.e)
}

// LookupAgreement is Ledger.LookupAgreement, counted.
func (c CountedLedger) LookupAgreement(rnd basics.Round, addr basics.Address) (basics.OnlineAccountData, error) {
	c.e.Lookup(false)
	return c.lookupAgreement(rnd, addr, c.e)
}

// GetKnockOfflineCandidates is Ledger.GetKnockOfflineCandidates, counted as one lookup.
func (c CountedLedger) GetKnockOfflineCandidates(rnd basics.Round, proto config.ConsensusParams) (map[basics.Address]basics.OnlineAccountData, error) {
	c.e.Lookup(false)
	return c.Ledger.GetKnockOfflineCandidates(rnd, proto)
}

// LookupApplication is Ledger.LookupApplication, counted.
func (c CountedLedger) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	c.e.Lookup(false)
	r, err := c.lookupResource(rnd, addr, basics.CreatableIndex(aidx), basics.AppCreatable, c.e)
	return ledgercore.AppResource{AppParams: r.AppParams, AppLocalState: r.AppLocalState}, err
}

// LookupAsset is Ledger.LookupAsset, counted.
func (c CountedLedger) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	c.e.Lookup(false)
	r, err := c.lookupResource(rnd, addr, basics.CreatableIndex(aidx), basics.AssetCreatable, c.e)
	return ledgercore.AssetResource{AssetParams: r.AssetParams, AssetHolding: r.AssetHolding}, err
}

// LookupKv is Ledger.LookupKv, counted.
func (c CountedLedger) LookupKv(rnd basics.Round, key string) ([]byte, error) {
	c.e.Lookup(false)
	return c.lookupKv(rnd, key, c.e)
}

// LookupKeysByPrefix is Ledger.LookupKeysByPrefix, counted as one lookup; a history scan also
// counts its entries.
func (c CountedLedger) LookupKeysByPrefix(round basics.Round, keyPrefix string, maxKeyNum uint64) ([]string, error) {
	c.e.Lookup(false)
	return c.lookupKeysByPrefix(round, keyPrefix, maxKeyNum, c.e)
}

// LookupKvPairsByPrefix is Ledger.LookupKvPairsByPrefix, counted as one lookup; a history scan
// also counts its entries.
func (c CountedLedger) LookupKvPairsByPrefix(round basics.Round, keyPrefix string, cursor string, limit uint64, maxBytes uint64, includeValues bool) ([]ledgercore.KvPairResult, basics.Round, bool, error) {
	c.e.Lookup(false)
	return c.lookupKvPairsByPrefix(round, keyPrefix, cursor, limit, maxBytes, includeValues, c.e)
}

// GetCreatorForRound is Ledger.GetCreatorForRound, counted.
func (c CountedLedger) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	c.e.Lookup(false)
	return c.getCreatorForRound(rnd, cidx, ctype, c.e)
}

// Totals is Ledger.Totals, counted.
func (c CountedLedger) Totals(rnd basics.Round) (ledgercore.AccountTotals, error) {
	c.e.Lookup(false)
	return c.totals(rnd, c.e)
}

// OnlineCirculation is Ledger.OnlineCirculation, counted.
func (c CountedLedger) OnlineCirculation(rnd basics.Round, voteRnd basics.Round) (basics.MicroAlgos, error) {
	c.e.Lookup(false)
	return c.onlineCirculation(rnd, voteRnd, c.e)
}

// BlockHdr is Ledger.BlockHdr, counted.
func (c CountedLedger) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	c.e.Lookup(false)
	return c.blockHdr(rnd, c.e)
}

// LookupLatestAt is Ledger.LookupLatestAt, counted as one lookup, answered by the history; its
// scans of the account's resources count their entries.
func (c CountedLedger) LookupLatestAt(rnd basics.Round, addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	c.e.Lookup(false)
	return c.lookupLatestAt(rnd, addr, c.e)
}

// LookupAssetsAt is Ledger.LookupAssetsAt, counted as LookupLatestAt is.
func (c CountedLedger) LookupAssetsAt(rnd basics.Round, addr basics.Address, assetIDGT basics.AssetIndex, limit uint64) ([]ledgercore.AssetResourceWithIDs, basics.Round, error) {
	c.e.Lookup(false)
	return c.lookupAssetsAt(rnd, addr, assetIDGT, limit, c.e)
}

// LookupApplicationsAt is Ledger.LookupApplicationsAt, counted as LookupLatestAt is.
func (c CountedLedger) LookupApplicationsAt(rnd basics.Round, addr basics.Address, appIDGT basics.AppIndex, limit uint64, includeParams bool) ([]ledgercore.AppResourceWithIDs, basics.Round, error) {
	c.e.Lookup(false)
	return c.lookupApplicationsAt(rnd, addr, appIDGT, limit, includeParams, c.e)
}
