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
	"maps"
	"slices"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/ledger/ledgercore"
)

// queryLedger is the ledger of one worker's eval.QueryState, and the cache its calls share. The
// state starts each call with an empty cache of its own, so it asks queryLedger once for each
// object the call reads. queryLedger answers from its cache when it can, and counts every object it
// returns, cached or not, in bytes: the state the running call read. So whether a call is over
// MaxCallStateBytes depends only on what it reads, never on what the calls before it on the worker
// left cached.
//
// The cache drops the objects of the calls run longest ago once it holds more than its limit, so
// what every call reads, such as the called app, stays cached.
//
// It also keeps the first lookup error, which is the node's failure to read the round, not the
// call's. Errors are not cached.
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
	// bytes is the state the running call read (startCall resets it).
	bytes int
	err   error
	cache queryCache
}

// newQueryLedger returns the ledger of a worker's calls at l's round.
func newQueryLedger(l simulatorLedger) *queryLedger {
	return &queryLedger{simulatorLedger: l, cache: queryCache{limit: maxQueryCacheBytes}}
}

// startCall readies the ledger for the next call: it has read nothing yet.
func (l *queryLedger) startCall() {
	l.bytes = 0
	l.cache.gen++
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

// maxQueryCacheBytes is the most memory, as queryLedger counts it, a worker keeps cached for later
// calls; a running call's objects are kept even past it. Every worker holds a slot of
// SimulateParallelism, so the node's total stays bounded. Within a call, MaxCallStateBytes bounds
// it. A variable for tests.
var maxQueryCacheBytes = 32 << 20

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

// queryLookup answers a lookup of method at rnd from the cache under key, or else with read, which
// returns the object and its bytes; the cache keeps what read returns. Either way the object
// counts in the running call's bytes.
func queryLookup[V any](l *queryLedger, method string, rnd basics.Round, key queryCacheKey, read func() (V, int, error)) (V, error) {
	key.rnd = rnd
	var v V
	var n int
	var err error
	e, cached := l.cache.get(key)
	if cached {
		v, n = e.value.(V), e.bytes
	} else {
		v, n, err = read()
	}
	if err = l.note(method, rnd, n, err); err != nil {
		return v, err
	}
	if !cached {
		l.cache.put(key, v, n)
	}
	return v, nil
}

// queryAccount is the answer of LookupWithoutRewards.
type queryAccount struct {
	data         ledgercore.AccountData
	validThrough basics.Round
}

func (l *queryLedger) LookupWithoutRewards(rnd basics.Round, addr basics.Address) (ledgercore.AccountData, basics.Round, error) {
	a, err := queryLookup(l, "LookupWithoutRewards", rnd, queryCacheKey{kind: queryCacheAccount, addr: addr}, func() (queryAccount, int, error) {
		ad, validThrough, err := l.simulatorLedger.LookupWithoutRewards(rnd, addr)
		return queryAccount{ad, validThrough}, queryObjectBytes, err
	})
	return a.data, a.validThrough, err
}

func (l *queryLedger) LookupAgreement(rnd basics.Round, addr basics.Address) (basics.OnlineAccountData, error) {
	return queryLookup(l, "LookupAgreement", rnd, queryCacheKey{kind: queryCacheAgreement, addr: addr}, func() (basics.OnlineAccountData, int, error) {
		ad, err := l.simulatorLedger.LookupAgreement(rnd, addr)
		return ad, queryObjectBytes, err
	})
}

func (l *queryLedger) OnlineCirculation(rnd basics.Round, voteRnd basics.Round) (basics.MicroAlgos, error) {
	return queryLookup(l, "OnlineCirculation", rnd, queryCacheKey{kind: queryCacheOnlineCirculation, index: uint64(voteRnd)}, func() (basics.MicroAlgos, int, error) {
		total, err := l.simulatorLedger.OnlineCirculation(rnd, voteRnd)
		return total, 0, err
	})
}

// GetKnockOfflineCandidates is not cached: only the end of a block asks for it, which queries never
// reach.
func (l *queryLedger) GetKnockOfflineCandidates(rnd basics.Round, proto config.ConsensusParams) (map[basics.Address]basics.OnlineAccountData, error) {
	candidates, err := l.simulatorLedger.GetKnockOfflineCandidates(rnd, proto)
	return candidates, l.note("GetKnockOfflineCandidates", rnd, queryObjectBytes*len(candidates), err)
}

// The resource lookups return copies of the cached params, holdings and local states, so that no
// call can change what a later call reads through them, as the state's own cache did. Their maps
// (global and local state) are shared, as they were.

func (l *queryLedger) LookupAsset(rnd basics.Round, addr basics.Address, aidx basics.AssetIndex) (ledgercore.AssetResource, error) {
	r, err := queryLookup(l, "LookupAsset", rnd, queryCacheKey{kind: queryCacheAsset, addr: addr, index: uint64(aidx)}, func() (ledgercore.AssetResource, int, error) {
		r, err := l.simulatorLedger.LookupAsset(rnd, addr, aidx)
		n := 2 * queryObjectBytes // the params and the holding, each cached found or not
		if p := r.AssetParams; p != nil {
			n += len(p.URL) + len(p.AssetName) + len(p.UnitName)
		}
		return r, n, err
	})
	if p := r.AssetParams; p != nil {
		c := *p
		r.AssetParams = &c
	}
	if h := r.AssetHolding; h != nil {
		c := *h
		r.AssetHolding = &c
	}
	return r, err
}

func (l *queryLedger) LookupApplication(rnd basics.Round, addr basics.Address, aidx basics.AppIndex) (ledgercore.AppResource, error) {
	r, err := queryLookup(l, "LookupApplication", rnd, queryCacheKey{kind: queryCacheApp, addr: addr, index: uint64(aidx)}, func() (ledgercore.AppResource, int, error) {
		r, err := l.simulatorLedger.LookupApplication(rnd, addr, aidx)
		n := 2 * queryObjectBytes // the params and the local state, each cached found or not
		if p := r.AppParams; p != nil {
			n += len(p.ApprovalProgram) + len(p.ClearStateProgram) + tkvBytes(p.GlobalState)
		}
		if s := r.AppLocalState; s != nil {
			n += tkvBytes(s.KeyValue)
		}
		return r, n, err
	})
	if p := r.AppParams; p != nil {
		c := *p
		r.AppParams = &c
	}
	if s := r.AppLocalState; s != nil {
		c := *s
		r.AppLocalState = &c
	}
	return r, err
}

func (l *queryLedger) LookupKv(rnd basics.Round, key string) ([]byte, error) {
	return queryLookup(l, "LookupKv", rnd, queryCacheKey{kind: queryCacheKv, name: key}, func() ([]byte, int, error) {
		v, err := l.simulatorLedger.LookupKv(rnd, key)
		return v, queryObjectBytes + len(key) + len(v), err
	})
}

// queryCreator is the answer of GetCreatorForRound.
type queryCreator struct {
	addr basics.Address
	ok   bool
}

func (l *queryLedger) GetCreatorForRound(rnd basics.Round, cidx basics.CreatableIndex, ctype basics.CreatableType) (basics.Address, bool, error) {
	c, err := queryLookup(l, "GetCreatorForRound", rnd, queryCacheKey{kind: queryCacheCreator, index: uint64(cidx), ctype: ctype}, func() (queryCreator, int, error) {
		addr, ok, err := l.simulatorLedger.GetCreatorForRound(rnd, cidx, ctype)
		return queryCreator{addr, ok}, queryObjectBytes, err
	})
	return c.addr, c.ok, err
}

// queryCacheKind is the lookup a cache entry answers.
type queryCacheKind uint8

const (
	queryCacheAccount queryCacheKind = iota
	queryCacheAgreement
	queryCacheOnlineCirculation
	queryCacheAsset
	queryCacheApp
	queryCacheKv
	queryCacheCreator
)

// queryCacheKey is a lookup and its arguments.
type queryCacheKey struct {
	kind  queryCacheKind
	ctype basics.CreatableType
	rnd   basics.Round
	addr  basics.Address
	// index is the asset, app or creatable, or the vote round of OnlineCirculation.
	index uint64
	// name is the key of LookupKv.
	name string
}

// queryCacheEntry is a cached answer, of bytes bytes, last asked for by call gen.
type queryCacheEntry struct {
	value any
	bytes int
	gen   uint64
}

// queryCache is the cache of a queryLedger. Each entry is marked with the last call that asked for
// it. Once the cache holds more than limit bytes, the entries of the calls longest ago go first,
// down to 3/4 of limit; those of the running call stay.
type queryCache struct {
	entries map[queryCacheKey]*queryCacheEntry
	bytes   int
	limit   int
	// gen numbers the running call. full is set to it when the running call's own entries keep the
	// cache over its limit, so that its later lookups don't look for entries to drop again.
	gen  uint64
	full uint64
}

// get returns the entry of key, if cached, and marks it as asked for by the running call.
func (c *queryCache) get(key queryCacheKey) (*queryCacheEntry, bool) {
	e, ok := c.entries[key]
	if ok {
		e.gen = c.gen
	}
	return e, ok
}

// put caches v, of n bytes, under key, and drops the entries of earlier calls if the cache is then
// over its limit.
func (c *queryCache) put(key queryCacheKey, v any, n int) {
	if c.entries == nil {
		c.entries = make(map[queryCacheKey]*queryCacheEntry)
	}
	c.entries[key] = &queryCacheEntry{value: v, bytes: n, gen: c.gen}
	c.bytes += n
	if c.bytes > c.limit && c.full != c.gen {
		c.evict()
	}
}

// evict drops the entries of the calls longest ago until the cache holds at most 3/4 of its limit,
// or only the running call's entries are left.
func (c *queryCache) evict() {
	excess := c.bytes - c.limit/4*3
	byGen := make(map[uint64]int)
	for _, e := range c.entries {
		if e.gen != c.gen {
			byGen[e.gen] += e.bytes
		}
	}
	gens := slices.Sorted(maps.Keys(byGen))
	drop := 0 // the entries of calls up to gens[drop-1] go
	for drop < len(gens) && excess > 0 {
		excess -= byGen[gens[drop]]
		drop++
	}
	if drop > 0 {
		last := gens[drop-1]
		for k, e := range c.entries {
			if e.gen <= last {
				delete(c.entries, k)
				c.bytes -= e.bytes
			}
		}
	}
	if excess > 0 {
		c.full = c.gen
	}
}
