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

import (
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// queryLedger is the ledger of one eval.QueryState. The state caches every object its lookups
// return, found or not, and asks the ledger only for objects it has not cached, so the bytes of
// what the lookups return are the memory the cache holds: queryLedger counts them. It also keeps
// the first lookup error, which is the node's failure to read the round, not the call's.
//
// Errors of the online account lookups are not kept: they read the balance round, 320 rounds
// back, where a history may lack what online_stake or voter_params_get need for good (the online
// stake before it was recorded, or a round before a snapshot's first). They fail the call.
//
// Block headers are neither counted nor kept: the state doesn't cache them, and a header the
// round can't see (the block opcode at a later round) fails the call that asked for it.
//
// A queryLedger is used by one worker at a time, as its QueryState is.
type queryLedger struct {
	simulatorLedger
	bytes int
	err   error
}

// queryObjectBytes is what a cached object costs besides its variable-length parts: the map entry,
// its key and the fixed-size fields of its value.
const queryObjectBytes = 256

// queryKeyValueBytes is what each entry of a TEAL key-value store costs besides its key and bytes.
const queryKeyValueBytes = 64

func tkvBytes(kv basics.TealKeyValue) int {
	n := 0
	for k, v := range kv {
		n += queryKeyValueBytes + len(k) + len(v.Bytes)
	}
	return n
}

// lookupFault, set only by tests, makes the lookups of a method at a round fail, as when the node
// can't read the round: it returns their error.
var lookupFault func(method string, rnd basics.Round) error

// onlineLookup reports whether method reads online accounts, at the balance round.
func onlineLookup(method string) bool {
	switch method {
	case "LookupAgreement", "OnlineCirculation", "GetKnockOfflineCandidates":
		return true
	}
	return false
}

// note counts an object of n bytes that method returned, or keeps err. It returns err.
func (l *queryLedger) note(method string, rnd basics.Round, n int, err error) error {
	if lookupFault != nil && err == nil {
		err = lookupFault(method, rnd)
	}
	if err != nil {
		if l.err == nil && !onlineLookup(method) {
			l.err = err
		}
		return err
	}
	l.bytes += n
	return nil
}

func (l *queryLedger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, error) {
	ad, validThrough, err := l.simulatorLedger.LookupWithoutRewards(rnd, addr)
	return ad, validThrough, l.note("LookupWithoutRewards", rnd, queryObjectBytes, err)
}

func (l *queryLedger) LookupAgreement(rnd basics.Round, addr basics.Address) (basics.OnlineAccountData, error) {
	ad, err := l.simulatorLedger.LookupAgreement(rnd, addr)
	return ad, l.note("LookupAgreement", rnd, queryObjectBytes, err)
}

func (l *queryLedger) OnlineCirculation(rnd basics.Round, voteRnd basics.Round) (basics.MicroAlgos, error) {
	total, err := l.simulatorLedger.OnlineCirculation(rnd, voteRnd)
	return total, l.note("OnlineCirculation", rnd, 0, err)
}

func (l *queryLedger) GetKnockOfflineCandidates(rnd basics.Round, proto config.ConsensusParams) (map[basics.Address]basics.OnlineAccountData, error) {
	candidates, err := l.simulatorLedger.GetKnockOfflineCandidates(rnd, proto)
	return candidates, l.note("GetKnockOfflineCandidates", rnd, queryObjectBytes*len(candidates), err)
}

func (l *queryLedger) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	r, err := l.simulatorLedger.LookupAsset(rnd, addr, aidx)
	n := 2 * queryObjectBytes // the params and the holding, each cached found or not
	if p := r.AssetParams; p != nil {
		n += len(p.URL) + len(p.AssetName) + len(p.UnitName)
	}
	return r, l.note("LookupAsset", rnd, n, err)
}

func (l *queryLedger) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	r, err := l.simulatorLedger.LookupApplication(rnd, addr, aidx)
	n := 2 * queryObjectBytes // the params and the local state, each cached found or not
	if p := r.AppParams; p != nil {
		n += len(p.ApprovalProgram) + len(p.ClearStateProgram) + tkvBytes(p.GlobalState)
	}
	if s := r.AppLocalState; s != nil {
		n += tkvBytes(s.KeyValue)
	}
	return r, l.note("LookupApplication", rnd, n, err)
}

func (l *queryLedger) LookupKv(rnd basics.Round, key string) ([]byte, error) {
	v, err := l.simulatorLedger.LookupKv(rnd, key)
	return v, l.note("LookupKv", rnd, queryObjectBytes+len(key)+len(v), err)
}

func (l *queryLedger) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	addr, ok, err := l.simulatorLedger.GetCreatorForRound(rnd, cidx, ctype)
	return addr, ok, l.note("GetCreatorForRound", rnd, queryObjectBytes, err)
}
