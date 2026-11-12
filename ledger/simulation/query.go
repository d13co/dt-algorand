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

// Read-only TEAL queries (POST /v2/query): one program, many calls, each run alone as an app call
// on the state a round left, without a block. Non-standard fork feature.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/eval"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

// MaxQueryRounds is the ceiling of QueryLimits.MaxRounds and QueryLimits.Parallelism.
const MaxQueryRounds = 256

// ProtocolRound names, in QueryRequest.Protocol, no override: each round runs under the protocol
// its next block really ran.
const ProtocolRound = "round"

// QueryLimits are a node's limits on query requests (QueryLimitsFromConfig).
type QueryLimits struct {
	// MaxCalls is the most calls a request may hold; QueryLimitsFromConfig keeps it at most
	// MaxTotalCalls. 0 disables queries.
	MaxCalls int
	// MaxCallBudget is the most opcode budget a call may ask for, and the budget of one that asks
	// for none.
	MaxCallBudget int
	// MaxCallLogBytes is the most bytes each program of a call may log. 0 keeps the protocol's
	// limits.
	MaxCallLogBytes int
	// MaxResponseBytes is the most log bytes a request may return. 0 means no limit.
	MaxResponseBytes uint64
	// MaxCallStateBytes is the most bytes of ledger state a call may read, as queryLedger counts
	// them: a call that reads more fails. 0 means no limit.
	MaxCallStateBytes uint64
	// MaxRounds is the most rounds a request may list.
	MaxRounds int
	// MaxTotalCalls is the most calls times listed rounds a request may run. 0 means no limit beyond
	// MaxCalls times MaxRounds.
	MaxTotalCalls int
	// Parallelism is the most workers that run the calls of a request at once.
	Parallelism int
	// MaxSeekRounds is the most rounds a call may move its reads to with tt_seek. 0 turns time
	// travel off.
	MaxSeekRounds int
}

// QueryLimitsFromConfig returns the query limits of a node configured with cfg, clamped as the
// config documents. The node and GET /v2/capabilities both take them from here.
func QueryLimitsFromConfig(cfg config.Local) QueryLimits {
	maxCalls := min(cfg.MaxAPIQueryCalls, math.MaxInt32)
	maxRounds := max(1, min(cfg.MaxAPIQueryRounds, MaxQueryRounds))
	maxTotalCalls := min(maxCalls*maxRounds, math.MaxInt32)
	if cfg.MaxAPIQueryTotalCalls > 0 {
		maxTotalCalls = min(maxTotalCalls, cfg.MaxAPIQueryTotalCalls)
	}
	// More calls than maxTotalCalls are over it even at one round, so max-calls never reports them.
	maxCalls = min(maxCalls, maxTotalCalls)
	return QueryLimits{
		MaxCalls:          int(maxCalls),
		MaxCallBudget:     int(max(1, min(cfg.MaxAPIQueryCallBudget, MaxExtraOpcodeBudgetCeiling))),
		MaxCallLogBytes:   int(min(cfg.MaxAPIQueryCallLogBytes, math.MaxInt32)),
		MaxResponseBytes:  cfg.MaxAPIQueryResponseBytes,
		MaxCallStateBytes: cfg.MaxAPIQueryCallStateBytes,
		MaxRounds:         int(maxRounds),
		MaxTotalCalls:     int(maxTotalCalls),
		Parallelism:       max(1, min(cfg.QueryParallelism, MaxQueryRounds)),
		MaxSeekRounds:     int(min(cfg.MaxAPIQuerySeekRounds, math.MaxInt32)),
	}
}

// CheckEnabled returns an InvalidRequestError if l disables queries: MaxCalls is 0.
func (l QueryLimits) CheckEnabled() error {
	if l.MaxCalls == 0 {
		return invalidQuery("query is disabled on this node (MaxAPIQueryCalls is 0)")
	}
	return nil
}

// CheckSize returns an InvalidRequestError if a request with the given numbers of calls and listed
// rounds is over l: too many rounds, calls times rounds, or calls, where a request that lists no
// rounds runs at one. Calls times rounds comes before calls, so that a request over a MaxCalls that
// MaxTotalCalls lowered is told about MaxAPIQueryTotalCalls.
func (l QueryLimits) CheckSize(calls, rounds int) error {
	if maxRounds := max(1, l.MaxRounds); rounds > maxRounds {
		return invalidQuery("too many rounds: got %d, the limit is %d", rounds, maxRounds)
	}
	// In int64: a 32-bit int could overflow.
	if total := int64(calls) * int64(max(1, rounds)); l.MaxTotalCalls > 0 && total > int64(l.MaxTotalCalls) {
		return invalidQuery("too many calls in total: %d calls at %d rounds is %d, the limit is %d (MaxAPIQueryTotalCalls)",
			calls, max(1, rounds), total, l.MaxTotalCalls)
	}
	if calls > l.MaxCalls {
		return invalidQuery("too many calls: got %d, the limit is %d", calls, l.MaxCalls)
	}
	return nil
}

// QueryCall is one call of a QueryRequest: the args and refs of its app call. The refs are only
// needed by programs before v9, which can't use resources the txn does not name.
type QueryCall struct {
	Args          [][]byte
	Accounts      []basics.Address
	ForeignApps   []basics.AppIndex
	ForeignAssets []basics.AssetIndex
	Boxes         []transactions.BoxRef
}

// QueryRequest runs its calls at each of its rounds. Each call is an app call alone in its group,
// in the round after the queried one, on the state the queried round left: an app create with
// Program, or a call of the deployed app AppID.
type QueryRequest struct {
	// Program is the approval program of the app each call creates. ClearProgram defaults to
	// "pushint 1" at Program's version. Exactly one of Program and AppID is set.
	Program      []byte
	ClearProgram []byte
	GlobalSchema basics.StateSchema
	LocalSchema  basics.StateSchema
	// AppID is the deployed app each call calls, with OnCompletion NoOp.
	AppID basics.AppIndex
	// Sender sends every call. The zero address is the fee sink of the queried round.
	Sender basics.Address
	// Rounds are the rounds queried, 0 being the latest. None is the latest.
	Rounds []basics.Round
	// Protocol runs the round after each queried one under another protocol, as
	// Request.Protocol does. "" is ProtocolLatest; ProtocolRound is no override.
	Protocol string
	// Budget is the opcode budget of each call. 0 is Limits.MaxCallBudget.
	Budget int
	Calls  []QueryCall

	// Limits are the node's limits, set by the node from its config, never by API clients.
	Limits QueryLimits

	ctx    context.Context
	slots  querySlots
	stream func(QueryEvent)
}

// querySlots let a request's extra workers take free slots of the node's limiter (WithSlots).
type querySlots struct {
	tryAcquire func() bool
	release    func()
}

// WithContext returns r run under ctx: once ctx is done, the request stops and Query returns
// context.Cause(ctx).
func (r QueryRequest) WithContext(ctx context.Context) QueryRequest {
	r.ctx = ctx
	return r
}

// Context returns the context set by WithContext, or context.Background.
func (r QueryRequest) Context() context.Context {
	if r.ctx == nil {
		return context.Background()
	}
	return r.ctx
}

// WithSlots returns r whose workers but the first take a slot with tryAcquire before each piece of
// work and give it back with release. tryAcquire must not wait: a worker that gets no slot stops and
// leaves the rest of the work to the others. The first runs in the slot its caller holds.
func (r QueryRequest) WithSlots(tryAcquire func() bool, release func()) QueryRequest {
	r.slots = querySlots{tryAcquire: tryAcquire, release: release}
	return r
}

// QueryResult is the result of a QueryRequest.
type QueryResult struct {
	// LastRound is the node's latest round when the request started.
	LastRound basics.Round
	// Rounds has one result for each round of the request, in order.
	Rounds []QueryRoundResult
}

// QueryRoundResult are the calls run at one round.
type QueryRoundResult struct {
	Round basics.Round
	// Protocol is how the request's protocol applied to the round: set when the request named one,
	// or when the default, ProtocolLatest, changed the round's.
	Protocol *ProtocolOverride
	// Error is set when the round could not be queried; there are no calls then.
	Error string
	// Calls has one result for each call of the request, in order.
	Calls []QueryCallResult
}

// QueryCallResult is the result of one call at one round.
type QueryCallResult struct {
	// Logs are the logs of the called app, also when the call failed part way.
	Logs []string
	// Cost is the opcode budget the call used, the apps it called included.
	Cost int
	// Error is the call's error, if it failed.
	Error string
}

// ErrQueryResponseTooLarge is the cause of a query stopped because its logs outgrew
// QueryLimits.MaxResponseBytes. The error Query returns wraps it.
var ErrQueryResponseTooLarge = errors.New("query response too large")

func invalidQuery(format string, args ...any) error {
	return InvalidRequestError{SimulatorError{fmt.Errorf(format, args...)}}
}

// defaultClearProgram returns "pushint 1" at the version of program, which must be v3 or later.
func defaultClearProgram(program []byte) ([]byte, error) {
	version, n := binary.Uvarint(program)
	if n <= 0 {
		return nil, errors.New("program has no version")
	}
	// pushint came with v3.
	if version < 3 {
		return nil, fmt.Errorf("program version %d is before v3: send a clear-program", version)
	}
	return append(binary.AppendUvarint(nil, version), 0x81, 0x01), nil
}

// Validate checks the parts of r that do not depend on the rounds it runs at. The errors are
// InvalidRequestErrors.
func (r QueryRequest) Validate(developerAPI bool) error {
	if err := r.Limits.CheckEnabled(); err != nil {
		return err
	}
	if (len(r.Program) == 0) == (r.AppID == 0) {
		return invalidQuery("exactly one of program and app-id is required")
	}
	if r.AppID != 0 && (len(r.ClearProgram) > 0 || r.GlobalSchema != (basics.StateSchema{}) || r.LocalSchema != (basics.StateSchema{})) {
		return invalidQuery("clear-program, global-schema and local-schema need program, not app-id")
	}
	if len(r.Program) > 0 && len(r.ClearProgram) == 0 {
		if _, err := defaultClearProgram(r.Program); err != nil {
			return invalidQuery("%v", err)
		}
	}
	// The largest app any protocol allows; a round whose protocol allows less fails the calls.
	programLen := len(r.Program) + len(r.ClearProgram)
	if len(r.Program) > 0 && len(r.ClearProgram) == 0 {
		clear, _ := defaultClearProgram(r.Program)
		programLen += len(clear)
	}
	if maxLen := maxQueryProgramLen(); programLen > maxLen {
		return invalidQuery("program and clear-program are %d bytes, more than the %d an app may have", programLen, maxLen)
	}
	if len(r.Calls) == 0 {
		return invalidQuery("expected at least 1 call, got 0")
	}
	if err := r.Limits.CheckSize(len(r.Calls), len(r.Rounds)); err != nil {
		return err
	}
	if r.Budget < 0 {
		return invalidQuery("budget %d is negative", r.Budget)
	}
	if r.Budget > r.Limits.MaxCallBudget {
		return invalidQuery("budget %d is over the limit of %d", r.Budget, r.Limits.MaxCallBudget)
	}
	if r.Protocol != ProtocolRound {
		if err := validateProtocol(r.Protocol, developerAPI); err != nil {
			return err
		}
	}
	return nil
}

// queryRound is what the calls at one distinct round need.
type queryRound struct {
	round    basics.Round
	ledger   simulatorLedger
	prevHdr  bookkeeping.BlockHeader
	nextHdr  bookkeeping.BlockHeader
	override *ProtocolOverride
	gaps     *protocolGaps
	totals   ledgercore.AccountTotals
	txns     []transactions.SignedTxn
	err      error
	// failed is set when the node fails to read the round while the calls run: it is the round's
	// error then. charged is what its calls added to the response size. Guarded by queryRun.mu.
	failed  error
	charged uint64
	// void is set with failed, for the calls of the round that still run to see without the lock:
	// they stop at their next opcode.
	void  atomic.Bool
	calls []QueryCallResult
	// uses is how many entries of the request's rounds are this round: its results are in the
	// response that many times.
	uses uint64
	// hits are the notes the calls hit, one set per piece of work. A note keeps its lowest hits,
	// whatever order they are merged in. pieces is how many pieces of work of the round have not
	// ended. Both guarded by queryRun.mu.
	hits   []*ProtocolOverride
	pieces int
}

// prepareRound reads what the calls at round rnd need. Its error is the round's.
func (s Simulator) prepareRound(req *QueryRequest, rnd, latest basics.Round) *queryRound {
	override := req.Protocol
	switch override {
	case ProtocolRound:
		override = ""
	case "":
		override = ProtocolLatest
	}
	qr := s.prepareState(override, rnd, latest)
	if qr.err != nil {
		return qr
	}
	proto := config.Consensus[qr.nextHdr.CurrentProtocol]
	qr.txns = make([]transactions.SignedTxn, len(req.Calls))
	for i := range req.Calls {
		qr.txns[i] = req.callTxn(&req.Calls[i], qr.nextHdr, proto)
	}
	qr.calls = make([]QueryCallResult, len(req.Calls))
	return qr
}

// prepareState reads the state round rnd left and the header of the block after it, run under
// the protocol override (as QueryRequest.Protocol names it), or the protocol it really ran when
// override is "". Its error is the round's.
func (s Simulator) prepareState(override string, rnd, latest basics.Round) *queryRound {
	qr := &queryRound{round: rnd, ledger: s.ledger}
	qr.ledger.start = rnd
	qr.ledger.pastStart = rnd < latest
	prevHdr, err := qr.ledger.BlockHdr(rnd)
	if err != nil {
		qr.err = err
		return qr
	}
	if override != "" {
		qr.override, qr.gaps, err = qr.ledger.overrideProtocol(override, prevHdr, latest)
		if err != nil {
			qr.err = err
			return qr
		}
		if qr.override.Applied {
			prevHdr = upgradeAt(prevHdr, qr.override.To)
		}
	}
	// MakeBlock panics on a protocol the node does not know.
	if _, next, upgradeErr := bookkeeping.ProcessUpgradeParams(prevHdr); upgradeErr != nil {
		qr.err = upgradeErr
		return qr
	} else if _, ok := config.Consensus[next.CurrentProtocol]; !ok {
		qr.err = protocol.Error(next.CurrentProtocol)
		return qr
	}
	nextHdr := bookkeeping.MakeBlock(prevHdr).BlockHeader

	// The rewards state of the next block, as StartEvaluator works it out: balances include the
	// rewards up to its rewards level.
	proto, prevProto := config.Consensus[nextHdr.CurrentProtocol], config.Consensus[prevHdr.CurrentProtocol]
	totals, err := qr.ledger.Totals(rnd)
	if err != nil {
		qr.err = err
		return qr
	}
	pool, _, err := qr.ledger.LookupWithoutRewards(rnd, prevHdr.RewardsPool)
	if err != nil {
		qr.err = err
		return qr
	}
	pool = pool.WithUpdatedRewards(prevProto.RewardUnit, prevHdr.RewardsLevel)
	nextHdr.RewardsState = prevHdr.NextRewardsState(nextHdr.Round, proto, pool.MicroAlgos, totals.RewardUnits(), logging.Base())

	qr.prevHdr, qr.nextHdr, qr.totals = prevHdr, nextHdr, totals
	return qr
}

// callTxn returns the app call of call in the block of nextHdr.
func (r *QueryRequest) callTxn(call *QueryCall, nextHdr bookkeeping.BlockHeader, proto config.ConsensusParams) transactions.SignedTxn {
	sender := r.Sender
	if sender.IsZero() {
		sender = nextHdr.FeeSink
	}
	txn := transactions.Transaction{
		Type: protocol.ApplicationCallTx,
		Header: transactions.Header{
			Sender:      sender,
			Fee:         basics.MicroAlgos{Raw: proto.MinTxnFee},
			FirstValid:  nextHdr.Round,
			LastValid:   nextHdr.Round,
			GenesisID:   nextHdr.GenesisID,
			GenesisHash: nextHdr.GenesisHash,
		},
		ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
			ApplicationID:   r.AppID,
			OnCompletion:    transactions.NoOpOC,
			ApplicationArgs: call.Args,
			Accounts:        call.Accounts,
			ForeignApps:     call.ForeignApps,
			ForeignAssets:   call.ForeignAssets,
			Boxes:           call.Boxes,
		},
	}
	if r.AppID == 0 {
		clear := r.ClearProgram
		if len(clear) == 0 {
			clear, _ = defaultClearProgram(r.Program) // checked by Validate
		}
		txn.ApprovalProgram = r.Program
		txn.ClearStateProgram = clear
		txn.GlobalStateSchema = r.GlobalSchema
		txn.LocalStateSchema = r.LocalSchema
		txn.ExtraProgramPages = extraProgramPages(len(r.Program), len(clear), proto)
	}
	return transactions.SignedTxn{Txn: txn}
}

// maxQueryProgramLen is the most bytes of programs an app may have under any known protocol. Extra
// pages past MaxExtraAppProgramPages cost fees, which queries do not charge, so the bound uses
// MaxAbsoluteExtraProgramPages.
func maxQueryProgramLen() int {
	maxLen := 0
	for _, proto := range config.Consensus {
		maxLen = max(maxLen, proto.MaxAppTotalProgramLen*(1+proto.MaxAbsoluteExtraProgramPages))
	}
	return maxLen
}

// extraProgramPages returns the fewest extra pages an app with programs of these lengths needs.
func extraProgramPages(approvalLen, clearLen int, proto config.ConsensusParams) uint32 {
	pagesFor := func(n, pageLen int) int {
		if pageLen <= 0 {
			return 1
		}
		return (n + pageLen - 1) / pageLen
	}
	pages := max(1,
		pagesFor(approvalLen, proto.MaxAppProgramLen),
		pagesFor(clearLen, proto.MaxAppProgramLen),
		pagesFor(approvalLen+clearLen, proto.MaxAppTotalProgramLen))
	return uint32(pages - 1)
}

// queryWork is a piece of work: the calls lo up to hi of round round.
type queryWork struct {
	round  int
	lo, hi int
}

// splitQueryWork splits the calls of each round into pieces for the workers: each round into as few
// pieces as keep them busy, so the calls of a piece share their lookups.
func splitQueryWork(rounds, calls, workers int) []queryWork {
	perRound := min(calls, max(1, (workers+rounds-1)/rounds))
	work := make([]queryWork, 0, rounds*perRound)
	for j := range rounds {
		for p := range perRound {
			work = append(work, queryWork{round: j, lo: p * calls / perRound, hi: (p + 1) * calls / perRound})
		}
	}
	return work
}

// Query runs the request's calls at each of its rounds.
func (s Simulator) Query(req QueryRequest) (QueryResult, error) {
	if err := req.Validate(s.developerAPI); err != nil {
		return QueryResult{}, err
	}
	if req.Budget == 0 {
		req.Budget = req.Limits.MaxCallBudget
	}

	// Access the underlying data.Ledger to get the real latest round.
	latest := s.ledger.Ledger.Latest()
	rounds := req.Rounds
	if len(rounds) == 0 {
		rounds = []basics.Round{0}
	}
	// Each distinct round runs once; index maps every entry of rounds to its distinct round.
	var distinct []basics.Round
	index := make([]int, len(rounds))
	seen := make(map[basics.Round]int, len(rounds))
	for i, rnd := range rounds {
		if rnd == 0 {
			rnd = latest
		}
		if rnd > latest {
			return QueryResult{}, invalidQuery("round %d is after the latest round %d", rnd, latest)
		}
		j, ok := seen[rnd]
		if !ok {
			j = len(distinct)
			seen[rnd] = j
			distinct = append(distinct, rnd)
		}
		index[i] = j
	}

	ctx, cancel := context.WithCancelCause(req.Context())
	defer cancel(nil)
	effort := ledgercore.EffortFromContext(ctx)
	s.ledger.counted = s.ledger.Ledger.Ledger.Counted(effort)

	plans := make([]*queryRound, len(distinct))
	for j, rnd := range distinct {
		if ctx.Err() != nil {
			return QueryResult{}, context.Cause(ctx)
		}
		// An error here, a protocol the round can't run under included, is the round's.
		plans[j] = s.prepareRound(&req, rnd, latest)
	}
	for _, j := range index {
		plans[j].uses++
	}

	run := &queryRun{
		req:     &req,
		sim:     s,
		latest:  latest,
		ctx:     ctx,
		cancel:  cancel,
		effort:  effort,
		plans:   plans,
		maxSize: req.Limits.MaxResponseBytes,
	}
	work := splitQueryWork(len(distinct), len(req.Calls), max(1, req.Limits.Parallelism))
	for _, piece := range work {
		plans[piece.round].pieces++
	}
	if req.stream != nil {
		resolved := make([]basics.Round, len(rounds))
		for i, j := range index {
			resolved[i] = distinct[j]
		}
		req.stream(QueryEvent{Kind: QueryEventStart, LastRound: latest, Rounds: resolved})
	}
	run.runAll(work)
	if err := context.Cause(ctx); err != nil {
		return QueryResult{}, err
	}

	result := QueryResult{LastRound: latest, Rounds: make([]QueryRoundResult, len(rounds))}
	for i, j := range index {
		rr := plans[j].result(&req)
		if rr.Error == "" {
			rr.Calls = plans[j].calls
		}
		result.Rounds[i] = rr
	}
	return result, nil
}

// result is qr's round result, without its calls. qr's pieces must have ended.
func (qr *queryRound) result(req *QueryRequest) QueryRoundResult {
	rr := QueryRoundResult{Round: qr.round}
	if qr.override != nil && (qr.override.Applied || req.Protocol != "") {
		rr.Protocol = qr.override
	}
	switch {
	case qr.err != nil:
		rr.Error = qr.err.Error()
	case qr.failed != nil:
		rr.Error = fmt.Sprintf("reading round %d failed: %v", qr.round, qr.failed)
	}
	return rr
}

// pieceDone records that a piece of qr's work ended. The last one merges the protocol override
// notes the pieces hit, and reports the round to a stream.
func (run *queryRun) pieceDone(qr *queryRound) {
	run.mu.Lock()
	qr.pieces--
	last := qr.pieces == 0
	run.mu.Unlock()
	if !last {
		return
	}
	if qr.override != nil {
		// The calls ran in pieces, each with its own hits: merge them.
		for _, hits := range qr.hits {
			for _, note := range hits.Notes {
				for _, hit := range note.Hits {
					qr.override.addHit(note.Code, hit)
				}
			}
		}
		qr.override.sortNotes()
	}
	if run.req.stream != nil && run.ctx.Err() == nil {
		rr := qr.result(run.req)
		run.req.stream(QueryEvent{Kind: QueryEventRound, Round: qr.round, Protocol: rr.Protocol, Error: rr.Error})
	}
}

// queryRun is one Query's work, shared by its workers.
type queryRun struct {
	req *QueryRequest
	// sim and latest are the simulator and the latest round the request started with, which the
	// states of the rounds calls seek to are built from.
	sim    Simulator
	latest basics.Round
	ctx    context.Context
	cancel context.CancelCauseFunc
	effort *ledgercore.Effort
	plans  []*queryRound
	// size counts the log bytes of the calls run, up to maxSize (0: no limit), less those of the
	// rounds that failed. Guarded by mu.
	size    uint64
	maxSize uint64
	// mu guards size, and the hits, failed and charged of the plans.
	mu deadlock.Mutex
}

// runAll runs work in a pool of workers. The first runs in the slot the request's caller holds;
// the others take a free slot before each piece and stop when none is free, so they never queue
// ahead of new requests for the node's slots.
func (run *queryRun) runAll(work []queryWork) {
	workers := min(max(1, run.req.Limits.Parallelism), len(work))
	ch := make(chan queryWork)
	slots := run.req.slots
	var wg sync.WaitGroup
	for w := range workers {
		wg.Add(1)
		if w == 0 || slots.tryAcquire == nil {
			go func() {
				defer wg.Done()
				for piece := range ch {
					run.runSafely(piece)
				}
			}()
			continue
		}
		go func() {
			defer wg.Done()
			for slots.tryAcquire() {
				piece, ok := <-ch
				if ok {
					run.runSafely(piece)
				}
				slots.release()
				if !ok {
					return
				}
			}
		}()
	}
	for _, piece := range work {
		ch <- piece
	}
	close(ch)
	// Every worker has stopped writing to the plans once Wait returns.
	wg.Wait()
}

// runSafely runs piece and reports that it ended. A panic, which callQuery doesn't catch outside
// the calls (a stream's sink, the request's bookkeeping), stops the request with it as the error:
// on a worker's goroutine it would crash the node.
func (run *queryRun) runSafely(piece queryWork) {
	defer func() {
		if r := recover(); r != nil {
			logging.Base().Errorf("recovered from panic in a query worker: %v\n%s", r, debug.Stack())
			run.cancel(fmt.Errorf("panic in a query worker: %v", r))
		}
	}()
	run.runPiece(piece)
	run.pieceDone(run.plans[piece.round])
}

// runPiece runs the calls of piece, on one QueryState, so they share its ledger's cache.
func (run *queryRun) runPiece(piece queryWork) {
	qr := run.plans[piece.round]
	if qr.err != nil || run.ctx.Err() != nil || run.failed(qr) {
		return
	}
	tracer := &queryTracer{
		run:        run,
		round:      qr,
		abort:      abortState{ctx: run.ctx, done: run.ctx.Done(), effort: run.effort},
		gaps:       qr.gaps,
		stateLimit: int(min(run.req.Limits.MaxCallStateBytes, math.MaxInt)),
	}
	if qr.gaps != nil {
		tracer.hits = &ProtocolOverride{}
		defer func() {
			run.mu.Lock()
			qr.hits = append(qr.hits, tracer.hits)
			run.mu.Unlock()
		}()
	}

	ql := newQueryLedger(qr.ledger)
	qs, err := eval.NewQueryState(ql, qr.prevHdr, qr.nextHdr, qr.totals, !qr.ledger.pastStart)
	if err != nil {
		run.fail(qr, err)
		return
	}
	tracer.ledger = ql
	opts := eval.QueryCallOptions{
		Budget:           run.req.Budget,
		MaxLogBytes:      run.req.Limits.MaxCallLogBytes,
		UnnamedResources: allowAllPolicy{},
		Tracer:           tracer,
	}
	seeker := run.newQuerySeeker(qr, ql)
	opts.TimeTravel = seeker
	defer seeker.endCall()
	for i := piece.lo; i < piece.hi; i++ {
		if run.ctx.Err() != nil || run.failed(qr) {
			return
		}
		ql.startCall()
		tracer.startCall(i)
		seeker.startCall()
		logs, err := callQuery(qs, qr.txns[i], opts)
		if tracer.abort.aborted != nil {
			// The request is stopping; the call did not finish.
			return
		}
		if ql.err != nil {
			// The node failed to read the round: no answer of this round can be trusted.
			run.fail(qr, ql.err)
			return
		}
		res := QueryCallResult{Logs: logs, Cost: tracer.cost}
		switch {
		case tracer.overState():
			res.Error = fmt.Sprintf("the call read more than %d bytes of ledger state (MaxAPIQueryCallStateBytes)", tracer.stateLimit)
		case err != nil:
			res.Error = err.Error()
		}
		if !run.charge(qr, &res) {
			return
		}
		if run.req.stream != nil {
			// The logs were reported as they came.
			res.Logs = nil
			run.req.stream(QueryEvent{Kind: QueryEventCall, Round: qr.round, Call: i, Logs: tracer.logs, Cost: res.Cost, Error: res.Error})
		}
		qr.calls[i] = res
	}
}

// fail records err, a failure of the node to read qr's round, as the round's error. The response
// leaves out the round's calls, so their bytes no longer count toward its limit.
func (run *queryRun) fail(qr *queryRound, err error) {
	run.mu.Lock()
	defer run.mu.Unlock()
	if qr.failed == nil {
		qr.failed = err
		qr.void.Store(true)
		if roundFailed != nil {
			roundFailed()
		}
		if run.req.stream == nil {
			run.size -= qr.charged
			qr.charged = 0
		}
	}
}

// roundFailed, set only by tests, is called when a round fails, once its calls can see it.
var roundFailed func()

// failed reports whether qr's round failed.
func (run *queryRun) failed(qr *queryRound) bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return qr.failed != nil
}

// queryLogOverhead is what the response bytes of a call count for each of its logs, besides its
// bytes: the memory of its string header, then of the []byte header encoding makes.
const queryLogOverhead = 48

// charge counts the bytes of res, a result of qr, in the response, and stops the request once they
// are over its limit: its logs, an overhead per log, and its error, qr.uses times. A stream counts
// its logs as they come (streamLogs), and its results once. It returns false when the piece must
// stop: the request is over its limit, or qr failed, whose results the response leaves out.
func (run *queryRun) charge(qr *queryRound, res *QueryCallResult) bool {
	n := uint64(len(res.Error))
	if run.req.stream != nil {
		return run.chargeBytes(qr, n)
	}
	for _, l := range res.Logs {
		n += uint64(len(l)) + queryLogOverhead
	}
	return run.chargeBytes(qr, n*qr.uses)
}

// chargeBytes counts n bytes of qr's results in the response, as charge does.
func (run *queryRun) chargeBytes(qr *queryRound, n uint64) bool {
	run.mu.Lock()
	if qr.failed != nil {
		run.mu.Unlock()
		return false
	}
	qr.charged += n
	run.size += n
	over := run.maxSize > 0 && run.size > run.maxSize
	run.mu.Unlock()
	if over {
		run.cancel(fmt.Errorf("%w: the logs are over %d bytes (MaxAPIQueryResponseBytes); query fewer calls or rounds",
			ErrQueryResponseTooLarge, run.maxSize))
		return false
	}
	return true
}

// callQuery runs one call. A panic is the call's error: it would otherwise crash the node, since
// it happens off the handler's goroutine.
func callQuery(qs *eval.QueryState, stxn transactions.SignedTxn, opts eval.QueryCallOptions) (logs []string, err error) {
	defer func() {
		if r := recover(); r != nil {
			logging.Base().Errorf("recovered from panic in a query call: %v\n%s", r, debug.Stack())
			logs, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return qs.Call(stxn, opts)
}

// allowAllPolicy allows every resource a query call does not name: the opcode budget bounds what a
// call can read.
type allowAllPolicy struct{}

func (allowAllPolicy) AvailableAccount(basics.Address) bool                    { return true }
func (allowAllPolicy) AvailableAsset(basics.AssetIndex) bool                   { return true }
func (allowAllPolicy) AvailableApp(basics.AppIndex) bool                       { return true }
func (allowAllPolicy) AllowsHolding(basics.Address, basics.AssetIndex) bool    { return true }
func (allowAllPolicy) AllowsLocal(basics.Address, basics.AppIndex) bool        { return true }
func (allowAllPolicy) AvailableBox(basics.AppIndex, string, bool, uint64) bool { return true }
func (allowAllPolicy) IOSurplus(int64) bool                                    { return true }

// queryTracer stops a query's calls when the request ends, counts their cost, and records the
// protocol override notes they hit. It runs the calls of one worker, one at a time.
type queryTracer struct {
	// cursorEvalTracer tracks the path of the running txn: the call is [0], as the first txn of
	// its group, and its inner txns follow from it.
	cursorEvalTracer
	run   *queryRun
	round *queryRound
	abort abortState
	// call is the index of the running call, cost the budget it used so far, and logs how many logs
	// of it were streamed.
	call int
	cost int
	logs int
	// ledger is that of the call's state, whose bytes stateLimit bounds (0: no limit). stateOver is
	// set once the running call read more than stateLimit: every later opcode of the call stops its
	// program.
	ledger     *queryLedger
	stateLimit int
	stateOver  bool
	// gaps, when set, are those of a protocol override, and hits the notes the calls hit.
	gaps       *protocolGaps
	hits       *ProtocolOverride
	pendingGap string
}

// startCall readies the tracer for call i.
func (t *queryTracer) startCall(i int) {
	t.call, t.cost, t.logs, t.pendingGap, t.stateOver = i, 0, 0, "", false
	// As BeforeTxnGroup and BeforeTxn of the call's group would, which Call does not run.
	t.relativeCursor = append(t.relativeCursor[:0], 0)
	t.previousInnerTxns = append(t.previousInnerTxns[:0], 0)
}

// BeforeProgram sets the IO budget of the call's group to what a full group of app calls could
// have, with every reference a box reference: simulate's limit with allow-unnamed-resources. The
// unnamed-resource policy lets programs read boxes beyond it; it bounds the box bytes they write,
// which a call holds in memory until it ends. The program bytes of the app a call creates beyond
// the first page count as written too, so they are added. Inner groups get their caller's budget.
func (t *queryTracer) BeforeProgram(cx *logic.EvalContext) {
	if cx.GetCaller() != nil {
		return
	}
	proto := cx.Proto
	budget := basics.MulSaturate(uint64(proto.MaxTxGroupSize*proto.MaxAppTotalTxnReferences), proto.BytesPerBoxReference)
	txn := &cx.TxnGroup[0].Txn
	if txn.ApplicationID == 0 {
		extra := transactions.LargeProgramExtraBytes(*proto, len(txn.ApprovalProgram)+len(txn.ClearStateProgram))
		budget = basics.AddSaturate(budget, uint64(extra))
	}
	cx.SetIOBudget(budget)
}

// BeforeOpcode stops the program once the request ends, or once its round failed in another piece
// of work, whose results the response leaves out. It notes when the opcode hits a gap of the
// protocol override.
func (t *queryTracer) BeforeOpcode(cx *logic.EvalContext) {
	t.abort.checkAbort(cx)
	if t.round.void.Load() {
		panic(logic.EvalAborted{Cause: errQueryRoundFailed})
	}
	if t.overState() {
		panic(logic.EvalAborted{Cause: errQueryCallState})
	}
	if t.gaps != nil {
		t.pendingGap = t.gaps.opcodeGap(cx)
	}
}

// AfterOpcode records the gap the opcode hit, if it ran, and streams the log it made.
func (t *queryTracer) AfterOpcode(cx *logic.EvalContext, evalError error) {
	if t.run.req.stream != nil {
		t.streamLogs(cx)
	}
	if t.pendingGap != "" {
		if evalError == nil {
			t.hits.addHit(t.pendingGap, NoteHit{Group: t.call, Path: t.absolutePath()})
		}
		t.pendingGap = ""
	}
}

// AfterProgram counts the program's cost: in the call's cost, and in the request's Effort.
func (t *queryTracer) AfterProgram(cx *logic.EvalContext, pass bool, evalError error) {
	t.cost += cx.Cost()
	t.abort.effort.AddOpcodes(uint64(cx.Cost()))
}

// errQueryRoundFailed stops a call whose round failed while it ran. Its result is dropped.
var errQueryRoundFailed = errors.New("the node failed to read the call's round")

// errQueryCallState stops a call that read more ledger state than it may.
var errQueryCallState = errors.New("the call read more ledger state than MaxAPIQueryCallStateBytes allows")

// overState reports whether the running call read more ledger state than it may. Its lookups
// outside programs count too, so it is asked after the call as well.
func (t *queryTracer) overState() bool {
	if !t.stateOver && t.stateLimit > 0 && t.ledger.bytes > t.stateLimit {
		t.stateOver = true
	}
	return t.stateOver
}

// DetailedEvalErrors makes the errors carry the disassembly of where the program failed.
func (t *queryTracer) DetailedEvalErrors() bool { return true }
