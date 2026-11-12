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

// Time travel inside a POST /v2/query call: a program moves its reads to a past round with tt_seek
// (logic/timetravel.go). Non-standard fork feature.

import (
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/eval"
)

// querySeekStateBytes is what each round a call seeks to counts toward the state it read
// (MaxCallStateBytes): the round's headers and totals, and its state's own maps.
const querySeekStateBytes = 4096

// maxKeptSeekStates is the most states of past rounds a worker keeps for its next calls. Calls that
// search seldom seek to the same rounds; the worker's ledger keeps what they read anyway.
const maxKeptSeekStates = 16

// querySeeker is the logic.TimeTraveller of the calls of a worker: it gives them the states of the
// rounds they seek to. Every state reads through the worker's queryLedger, so what a call reads at
// any round counts toward its state limit, and is cached for later calls.
type querySeeker struct {
	run  *queryRun
	home *queryRound
	ql   *queryLedger
	// states are the states of the rounds the worker's calls sought to.
	states map[basics.Round]*eval.QueryState
	// visits are the states of the rounds the running call sought to.
	visits map[basics.Round]*eval.QueryState
	// forks drop the ledgers the running call's seeks got that no program has released yet, by the
	// number of the seek that got them.
	forks map[uint64]func()
	seeks uint64
}

// newQuerySeeker returns the seeker of a worker's calls at home, whose ledger is ql. Calls get one
// also when the node turns time travel off, so that tt_seek fails with an error that says so, rather
// than as an opcode the AVM lacks.
func (run *queryRun) newQuerySeeker(home *queryRound, ql *queryLedger) *querySeeker {
	return &querySeeker{
		run:    run,
		home:   home,
		ql:     ql,
		states: make(map[basics.Round]*eval.QueryState),
		visits: make(map[basics.Round]*eval.QueryState),
		forks:  make(map[uint64]func()),
	}
}

// startCall readies the seeker for the next call, which has sought nowhere yet.
func (s *querySeeker) startCall() {
	s.endCall()
}

// endCall drops the ledgers of the call's seeks that its programs did not release, and so its
// writes at the rounds it sought to, and empties the caches of the states it visited, so that kept
// states hold none of its reads. It keeps the states for the next calls only while they are few.
func (s *querySeeker) endCall() {
	for _, drop := range s.forks {
		drop()
	}
	clear(s.forks)
	for _, qs := range s.visits {
		qs.ResetCache()
	}
	clear(s.visits)
	if len(s.states) > maxKeptSeekStates {
		clear(s.states)
	}
}

// Seek returns a new ledger on the state rnd left, for a program of the running call that seeks to
// rnd, and the release that drops it; it calls firstReach first when the call reaches rnd for the
// first time. Each seek gets its own ledger, so the writes of one program at rnd never reach
// another's, nor the same program's after its next seek.
func (s *querySeeker) Seek(rnd basics.Round, firstReach func() error) (logic.TimeTravelLedger, func(), error) {
	limit := s.run.req.Limits.MaxSeekRounds
	if limit <= 0 {
		return nil, nil, errors.New("time travel is off on this node (MaxAPIQuerySeekRounds is 0)")
	}
	if rnd > s.home.round {
		return nil, nil, fmt.Errorf("round %d is after the query round %d", rnd, s.home.round)
	}
	if qs, ok := s.visits[rnd]; ok {
		l, release := s.fork(qs)
		return l, release, nil
	}
	if len(s.visits) >= limit {
		return nil, nil, fmt.Errorf("the call seeks to more than %d rounds (MaxAPIQuerySeekRounds)", limit)
	}
	if err := firstReach(); err != nil {
		return nil, nil, err
	}
	qs, ok := s.states[rnd]
	if !ok {
		var err error
		qs, err = s.build(rnd)
		if err != nil {
			// Not kept: the next call that seeks to rnd tries again, as a failure may not last.
			return nil, nil, err
		}
		s.states[rnd] = qs
	}
	qs.ResetCache()
	s.visits[rnd] = qs
	s.ql.bytes += querySeekStateBytes
	l, release := s.fork(qs)
	return l, release, nil
}

// fork returns a new ledger on qs, and its release, which drops it at once; endCall drops it if
// nothing released it.
func (s *querySeeker) fork(qs *eval.QueryState) (logic.TimeTravelLedger, func()) {
	l, drop := qs.Fork()
	n := s.seeks
	s.seeks++
	s.forks[n] = drop
	return l, func() {
		if drop, ok := s.forks[n]; ok {
			delete(s.forks, n)
			drop()
		}
	}
}

// build makes the state of rnd, run under the protocol of the home round's calls, so that a program
// keeps its rules wherever it seeks. Its error is the seek's, not the round's: a call may seek to a
// round the node has no state of.
func (s *querySeeker) build(rnd basics.Round) (*eval.QueryState, error) {
	if lookupFault != nil {
		if err := lookupFault("seek", rnd); err != nil {
			return nil, fmt.Errorf("seeking to round %d: %w", rnd, err)
		}
	}
	qr := s.run.sim.prepareState(string(s.home.nextHdr.CurrentProtocol), rnd, s.run.latest)
	if qr.err != nil {
		return nil, fmt.Errorf("seeking to round %d: %w", rnd, qr.err)
	}
	// The lookups of building the state count toward no call's state, so that whether a call is over
	// its limit doesn't depend on whether an earlier call built the states it seeks to. Each visit
	// counts querySeekStateBytes instead.
	bytes, err := s.ql.bytes, s.ql.err
	qs, buildErr := eval.NewQueryState(querySeekLedger{queryLedger: s.ql, hdrs: qr.ledger}, qr.prevHdr, qr.nextHdr, qr.totals, !qr.ledger.pastStart)
	s.ql.bytes, s.ql.err = bytes, err
	if buildErr != nil {
		return nil, fmt.Errorf("seeking to round %d: %w", rnd, buildErr)
	}
	return qs, nil
}

// querySeekLedger is the ledger of the state of a round calls seek to: the worker's queryLedger,
// with the headers that round may read.
type querySeekLedger struct {
	*queryLedger
	hdrs simulatorLedger
}

// BlockHdr refuses the headers after the round, as simulatorLedger.BlockHdr does, and carries the
// protocol of the home round's calls.
func (l querySeekLedger) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	return l.hdrs.BlockHdr(rnd)
}
