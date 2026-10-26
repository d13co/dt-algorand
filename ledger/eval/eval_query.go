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

package eval

// App calls run on the state after a round without a block, for POST /v2/query. Non-standard fork
// feature, kept in its own file to keep upstream merges of eval.go clean.

import (
	"errors"
	"fmt"
	"math"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/config/bounds"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger/apply"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/protocol"
)

// QueryState runs app calls on the state a round left, each as if it were the first transaction of
// the next block, alone in its group. There are no signatures, fees, minimum balances, duplicate or
// lease checks, and no block. A call may read any app's boxes, as if every app had set
// ForeignBoxReads. A call may write state, but its writes are dropped when it ends, so
// every call sees the state the round left. The state's lookups are cached: later calls reuse the
// reads of earlier ones.
//
// A QueryState is not safe for concurrent use.
type QueryState struct {
	cow      *roundCowState
	proto    config.ConsensusParams
	specials transactions.SpecialAddresses
}

// NewQueryState returns a QueryState on the state of l after the round of prevHdr, whose account
// totals are prevTotals. nextHdr is the header of the block after it, whose round, protocol,
// rewards state and special addresses the calls see. As at the start of a block, the rewards of
// the round are withdrawn from the rewards pool.
func NewQueryState(l LedgerForCowBase, prevHdr, nextHdr bookkeeping.BlockHeader, prevTotals ledgercore.AccountTotals) (*QueryState, error) {
	if nextHdr.Round != prevHdr.Round+1 {
		return nil, fmt.Errorf("query header round %d does not follow round %d", nextHdr.Round, prevHdr.Round)
	}
	proto, ok := config.Consensus[nextHdr.CurrentProtocol]
	if !ok {
		return nil, protocol.Error(nextHdr.CurrentProtocol)
	}
	// The real TxnCounter, so the apps a call creates get the ids the next block would give them.
	base := makeRoundCowBase(l, prevHdr.Round, prevHdr.TxnCounter,
		prevHdr.StateProofTracking[protocol.StateProofBasic].StateProofNextRound, proto)
	cow := makeRoundCowState(base, nextHdr, proto, prevHdr.TimeStamp, prevTotals, 1)
	// Like simulate, which assembles a block: the checks that apply only to new blocks apply.
	cow.generate = true

	// Withdraw the round's rewards from the pool, as StartEvaluator does (without its 2019 testnet
	// workaround), so the pool's balance is the one a call in the next block sees.
	var ot basics.OverflowTracker
	rewardsPerUnit := ot.Sub(nextHdr.RewardsLevel, prevHdr.RewardsLevel)
	if withdrawn := ot.Mul(prevTotals.RewardUnits(), rewardsPerUnit); withdrawn > 0 && !ot.Overflowed {
		pool, err := cow.Get(nextHdr.RewardsPool, true)
		if err != nil {
			return nil, err
		}
		pool.MicroAlgos = ot.SubA(pool.MicroAlgos, basics.MicroAlgos{Raw: withdrawn})
		if err := cow.Put(nextHdr.RewardsPool, pool); err != nil {
			return nil, err
		}
	}
	if ot.Overflowed {
		return nil, fmt.Errorf("overflowed withdrawing the rewards of round %d", nextHdr.Round)
	}
	return &QueryState{
		cow:   cow,
		proto: proto,
		specials: transactions.SpecialAddresses{
			FeeSink:     nextHdr.FeeSink,
			RewardsPool: nextHdr.RewardsPool,
		},
	}, nil
}

// QueryCallOptions are the settings of one QueryState.Call.
type QueryCallOptions struct {
	// Budget is the opcode budget of the call, shared by its app and the apps it calls. 0 leaves the
	// protocol's. Before cost pooling (EnableAppCostPooling) each program has the protocol's
	// budget, as in simulate, whose extra budget only adds to the pool.
	Budget int
	// MaxLogBytes is the most bytes each program of the call may log, in up to bounds.MaxLogCalls
	// logs, as simulate's allow-more-logging allows. 0 leaves the protocol's limits.
	MaxLogBytes int
	// UnnamedResources, when set, allows the resources a txn does not name. The box bytes a call
	// may read and write are still limited by the IO budget its box refs pay for: a Tracer may raise
	// it from BeforeProgram (cx.SetIOBudget).
	UnnamedResources logic.UnnamedResourcePolicy
	// Tracer, when set, sees the call's programs and inner transactions. There is no
	// BeforeTxnGroup or BeforeTxn for the call itself.
	Tracer logic.EvalTracer
}

// ErrQueryNotAppCall is the error of a Call of a txn that is not an app call.
var ErrQueryNotAppCall = errors.New("query: not an app call")

// Call runs stxn, an app call, on the state. It returns the logs of the app it calls, also when the
// call fails part way, and the call's error. The calls of inner transactions are paid from an
// unlimited fee credit, so inner fees left to their default are 0.
func (q *QueryState) Call(stxn transactions.SignedTxn, opts QueryCallOptions) (logs []string, err error) {
	txn := &stxn.Txn
	if txn.Type != protocol.ApplicationCallTx {
		return nil, ErrQueryNotAppCall
	}
	if wfErr := txn.WellFormed(q.specials, q.proto); wfErr != nil {
		return nil, wfErr
	}

	txgroup := []transactions.SignedTxnWithAD{{SignedTxn: stxn}}
	ep := logic.NewAppEvalParams(txgroup, &q.proto, &q.specials)
	if opts.Budget > 0 && ep.PooledApplicationBudget != nil {
		*ep.PooledApplicationBudget = opts.Budget
	}
	if opts.MaxLogBytes > 0 {
		ep.MaxLogSize = opts.MaxLogBytes
		ep.MaxLogCalls = bounds.MaxLogCalls
	}
	ep.UnnamedResources = opts.UnnamedResources
	// The call, and the apps it calls, may read any app's boxes (app_box_get and the like).
	ep.AllowForeignBoxReads = true
	if ep.FeeCredit != nil {
		*ep.FeeCredit = basics.MicroAlgos{Raw: math.MaxUint64}
	}
	ep.Tracer = opts.Tracer

	// The call's writes go to a child that is never committed.
	child := q.cow.child(1)
	defer child.recycle()
	var ad transactions.ApplyData
	err = apply.ApplicationCall(txn.ApplicationCallTxnFields, txn.Header, child, &ad, 0, ep, q.cow.Counter())
	// The program logs into its EvalParams' txn, which ApplicationCall does not clear on failure.
	return ep.TxnGroup[0].EvalDelta.Logs, err
}

// queryCachedObjectBytes is the estimated memory of a cached object, besides a box's value.
const queryCachedObjectBytes = 256

// CachedBytes estimates the memory of the ledger lookups the state keeps cached: it grows with
// every distinct object the calls read. Box values count their length.
func (q *QueryState) CachedBytes() int {
	b, ok := q.cow.lookupParent.(*roundCowBase)
	if !ok {
		return 0
	}
	n := queryCachedObjectBytes * (len(b.accounts) + len(b.onlineAccounts) + len(b.appParams) +
		len(b.assetParams) + len(b.appLocalStates) + len(b.assets) + len(b.creators) + len(b.kvStore))
	for _, v := range b.kvStore {
		n += len(v)
	}
	return n
}
