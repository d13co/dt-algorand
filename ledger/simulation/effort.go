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

package simulation

// The simulator's ledger reads counted in the request's Effort (EnableEffortHeader). Non-standard
// fork feature, kept in its own file to keep upstream merges of simulator.go clean.

import (
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// lookups returns the ledger whose reads count in the request's Effort.
func (l simulatorLedger) lookups() ledger.CountedLedger {
	if l.counted.Ledger == nil {
		return l.Ledger.Ledger.Counted(nil)
	}
	return l.counted
}

// LookupWithoutRewards is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, error) {
	return l.lookups().LookupWithoutRewards(rnd, addr)
}

// LookupAccount is used by fix-signers.
func (l simulatorLedger) LookupAccount(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, basics.MicroAlgos, error) {
	return l.lookups().LookupAccount(rnd, addr)
}

// LookupAgreement is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) LookupAgreement(rnd basics.Round, addr basics.Address) (basics.OnlineAccountData, error) {
	return l.lookups().LookupAgreement(rnd, addr)
}

// GetKnockOfflineCandidates is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) GetKnockOfflineCandidates(rnd basics.Round, proto config.ConsensusParams) (map[basics.Address]basics.OnlineAccountData, error) {
	return l.lookups().GetKnockOfflineCandidates(rnd, proto)
}

// LookupAsset is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	return l.lookups().LookupAsset(rnd, addr, aidx)
}

// LookupApplication is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	return l.lookups().LookupApplication(rnd, addr, aidx)
}

// LookupKv is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) LookupKv(rnd basics.Round, key string) ([]byte, error) {
	return l.lookups().LookupKv(rnd, key)
}

// GetCreatorForRound is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	return l.lookups().GetCreatorForRound(rnd, cidx, ctype)
}

// OnlineCirculation is part of the eval.LedgerForCowBase interface.
func (l simulatorLedger) OnlineCirculation(rnd basics.Round, voteRnd basics.Round) (basics.MicroAlgos, error) {
	return l.lookups().OnlineCirculation(rnd, voteRnd)
}

// Totals is used by LatestTotals.
func (l simulatorLedger) Totals(rnd basics.Round) (ledgercore.AccountTotals, error) {
	return l.lookups().Totals(rnd)
}
