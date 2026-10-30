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
	"context"
	"errors"
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
	"sync/atomic"

	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/data/transactions/verify"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/eval"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
)

// Request packs simulation related txn-group(s), and configurations that are overlapping the ones in real transactions.
type Request struct {
	Round                 basics.Round
	TxnGroups             [][]transactions.SignedTxn
	AllowEmptySignatures  bool
	AllowMoreLogging      bool
	AllowUnnamedResources bool
	ExtraOpcodeBudget     int
	TraceConfig           ExecTraceConfig
	FixSigners            bool

	// ParallelGroups simulates every group alone against the start state, concurrently, instead
	// of as consecutive groups of one block. Non-standard fork feature.
	ParallelGroups bool

	// Protocol, when set, simulates the round as if an upgrade to this protocol took effect at
	// the round after Round: a consensus version, a vNN name for a mainline one, or ProtocolLatest
	// for the protocol of the block after the node's latest one. It must be a later protocol of the chain, or
	// vFuture (developer API only). Non-standard fork feature.
	Protocol string

	// ResourceLimitMultiplier scales the limits on unnamed resources (AllowUnnamedResources).
	// Values below 1 count as 1 (the consensus limits) and values above MaxResourceLimitMultiplier
	// are clamped to it. It is set by the node from its config (SimulateResourceLimitMultiplier),
	// never by API clients, and is not echoed in the result. Non-standard fork feature.
	ResourceLimitMultiplier uint64

	// MaxGroups is the most transaction groups the request may hold. Values below 1 count as 1
	// and values above MaxSimulateGroups are clamped to it. It is set by the node from its config
	// (MaxAPISimulateGroups), never by API clients. Non-standard fork feature.
	MaxGroups uint64

	// MaxExtraOpcodeBudget is the most ExtraOpcodeBudget the request may ask for, per group. 0 counts
	// as MaxExtraOpcodeBudget (the upstream limit) and values above MaxExtraOpcodeBudgetCeiling are
	// clamped to it. It is set by the node from its config (MaxAPISimulateExtraOpcodeBudget), never
	// by API clients. Non-standard fork feature.
	MaxExtraOpcodeBudget uint64

	// SkipValidityWindow evaluates txns outside their FirstValid..LastValid window, as sent: the
	// round-range part of the liveness check and the check for a txid confirmed earlier are skipped.
	// Leases, in-block duplicates, genesis and well-formedness are still checked, and headers after
	// the start round can't be read. The REST API allows it only with rounds. Non-standard fork
	// feature.
	SkipValidityWindow bool

	// RewriteValidityWindow moves every top-level txn's window to the simulated block, keeping its
	// width, and recomputes valid group IDs; the signatures over the body of the txns it changes are
	// dropped, so it needs AllowEmptySignatures (validity_window.go). Headers after the start round can't be read. The
	// REST API allows it only with rounds. Non-standard fork feature.
	RewriteValidityWindow bool

	// protocolGaps is set by Simulate when Protocol applies, for the tracers.
	protocolGaps *protocolGaps

	// ctx stops the simulation early once it is done (abort.go). Non-standard fork feature.
	ctx context.Context

	hooks *testHooks
}

// testHooks let tests inject failures. They are nil outside tests.
type testHooks struct {
	// parallelGroup runs in the worker before each parallel group. An error is a fatal error of
	// the request.
	parallelGroup func(g int) error
	// afterGroup runs at the end of the tracer's AfterTxnGroup for each top-level group, while the
	// evaluator is still in TransactionGroup.
	afterGroup func(g int, evalError error)
	// afterTxn runs at the end of the tracer's AfterTxn for each top-level txn, before the group
	// is committed.
	afterTxn func(g, i int)
	// beforeTxn runs at the end of the tracer's BeforeTxn for each top-level txn, while the txn is
	// being evaluated.
	beforeTxn func(g, i int)
}

// MaxSimulateGroups is the ceiling for Request.MaxGroups.
const MaxSimulateGroups = 256

// MaxSimulateRounds is the ceiling for the MaxAPISimulateRounds config value: the most entries the
// "rounds" of a REST simulate request may hold, duplicates included. Non-standard fork feature.
const MaxSimulateRounds = 256

// EffectiveMaxExtraOpcodeBudget returns the most extra opcode budget a request may ask for, per
// top-level group, for the value m of Request.MaxExtraOpcodeBudget: 0 counts as
// MaxExtraOpcodeBudget, and values above MaxExtraOpcodeBudgetCeiling count as that.
func EffectiveMaxExtraOpcodeBudget(m uint64) int {
	if m == 0 {
		return MaxExtraOpcodeBudget
	}
	return int(min(m, MaxExtraOpcodeBudgetCeiling))
}

// EffectiveMaxGroups returns the most transaction groups a request may hold for the value m of
// Request.MaxGroups: values below 1 count as 1, and values above MaxSimulateGroups count as that.
func EffectiveMaxGroups(m uint64) int {
	if m < 1 {
		return 1
	}
	if m > MaxSimulateGroups {
		return MaxSimulateGroups
	}
	return int(m)
}

// WithNodeLimits returns r with the limits of a node configured with cfg: ResourceLimitMultiplier,
// MaxGroups and MaxExtraOpcodeBudget. The nodes, the REST handlers that check a request before
// passing it to the node, and GET /v2/transactions/simulate/limits all take the limits from it, so
// the limits the endpoint reports are the ones simulate enforces. Non-standard fork feature.
func (r Request) WithNodeLimits(cfg config.Local) Request {
	r.ResourceLimitMultiplier = cfg.SimulateResourceLimitMultiplier
	r.MaxGroups = cfg.MaxAPISimulateGroups
	r.MaxExtraOpcodeBudget = cfg.MaxAPISimulateExtraOpcodeBudget
	return r
}

// Validate checks the parts of a request that do not depend on the round it is simulated at. The
// errors are InvalidRequestErrors.
func (r Request) Validate(developerAPI bool) error {
	if r.FixSigners && !r.AllowEmptySignatures {
		return InvalidRequestError{
			SimulatorError{
				errors.New("FixSigners requires AllowEmptySignatures to be enabled"),
			},
		}
	}

	if r.SkipValidityWindow && r.RewriteValidityWindow {
		return InvalidRequestError{
			SimulatorError{
				errors.New("skip-validity-window and rewrite-validity-window are mutually exclusive"),
			},
		}
	}
	if r.RewriteValidityWindow && !r.AllowEmptySignatures {
		return InvalidRequestError{
			SimulatorError{
				errors.New("rewrite-validity-window requires allow-empty-signatures"),
			},
		}
	}

	if len(r.TxnGroups) == 0 {
		return InvalidRequestError{
			SimulatorError{
				errors.New("expected at least 1 transaction group, got 0"),
			},
		}
	}
	if maxGroups := EffectiveMaxGroups(r.MaxGroups); len(r.TxnGroups) > maxGroups {
		return InvalidRequestError{
			SimulatorError{
				fmt.Errorf("too many transaction groups: got %d, the limit is %d", len(r.TxnGroups), maxGroups),
			},
		}
	}

	// StateProof txns can't be simulated in any round, so a request with one is invalid as a whole.
	for _, txgroup := range r.TxnGroups {
		for _, stxn := range txgroup {
			if stxn.Txn.Type == protocol.StateProofTx {
				return InvalidRequestError{
					SimulatorError{
						errors.New("cannot simulate StateProof transactions"),
					},
				}
			}
		}
	}

	if r.ExtraOpcodeBudget < 0 {
		return InvalidRequestError{
			SimulatorError{
				fmt.Errorf("extra budget %d is negative", r.ExtraOpcodeBudget),
			},
		}
	}
	// The limit applies to each group: every group gets the extra budget, as a lone group does.
	if maxBudget := EffectiveMaxExtraOpcodeBudget(r.MaxExtraOpcodeBudget); r.ExtraOpcodeBudget > maxBudget {
		return InvalidRequestError{
			SimulatorError{
				fmt.Errorf(
					"extra budget %d > simulation extra budget limit %d",
					r.ExtraOpcodeBudget, maxBudget),
			},
		}
	}

	if err := validateProtocol(r.Protocol, developerAPI); err != nil {
		return err
	}

	if err := validateSimulateRequest(r, developerAPI); err != nil {
		return err
	}
	// An execution trace grows with the budget it records, so a traced request keeps upstream's
	// limit whatever the node allows. Checked once traces are known to be allowed. Non-standard
	// fork feature.
	if r.TraceConfig.Enable && r.ExtraOpcodeBudget > MaxExtraOpcodeBudget {
		return InvalidRequestError{
			SimulatorError{
				fmt.Errorf(
					"extra budget %d > %d, the limit for a request with an execution trace",
					r.ExtraOpcodeBudget, MaxExtraOpcodeBudget),
			},
		}
	}
	return nil
}

// simulatorLedger patches the ledger interface to use a constant latest round.
type simulatorLedger struct {
	*data.Ledger
	start basics.Round

	// skipValidityWindow and rewriteValidityWindow are the request's validity window modes. They
	// are set by Simulate, on its copy of the simulator.
	skipValidityWindow    bool
	rewriteValidityWindow bool

	// upgradeTo, when set, is the protocol that the round after start switches to (Request.Protocol).
	upgradeTo protocol.ConsensusVersion

	// pastStart is set when start is before the node's latest round, so the simulated block, the
	// one after start, is already on chain. The policies that apply only to new blocks are then
	// off, so a txn the chain accepted at the time simulates as accepted
	// (eval.EvaluatorOptions.SkipNewBlockPolicies). Set by Simulate, for each round of a request
	// with rounds, and by prepareRound.
	pastStart bool

	// counted is the ledger whose reads count in the request's Effort (effort.go). Set by Simulate.
	counted ledger.CountedLedger
}

// BlockHdr is part of the ledger.Ledger interface.
//
// Headers after the start round are refused, as on chain, where the ledger does not yet have the
// header of the round being evaluated or any later one. Without this, a ledger that has them (a
// past start round on a time-travel node) would show them to heartbeats (the seed of FirstValid,
// which may be start+1) and, with a validity window mode, where a txn's window may lie after the
// start round, to programs (block, txn FirstValidTime). The evaluator itself reads only headers up
// to the start round.
//
// With upgradeTo set, the header of the start round carries an upgrade to it, pending at the next
// round, so the simulated block runs it. The evaluator checks the simulated header against this
// one, so every reader gets the same.
func (l simulatorLedger) BlockHdr(rnd basics.Round) (bookkeeping.BlockHeader, error) {
	if rnd > l.start {
		return bookkeeping.BlockHeader{}, fmt.Errorf("round %d is after the simulation start round %d", rnd, l.start)
	}
	hdr, err := l.lookups().BlockHdr(rnd)
	if err != nil || l.upgradeTo == "" || rnd != l.start {
		return hdr, err
	}
	return upgradeAt(hdr, l.upgradeTo), nil
}

// Latest is part of the ledger.Ledger interface.
// We override this to use the set latest to prevent racing with the network
func (l simulatorLedger) Latest() basics.Round {
	return l.start
}

// LatestTotals is part of the ledger.Ledger interface.
func (l simulatorLedger) LatestTotals() (basics.Round, ledgercore.AccountTotals, error) {
	totals, err := l.Totals(l.start)
	return l.start, totals, err
}

// CheckDup is part of the ledger.Ledger interface. We override this so that
// a simulation at a past round does not count the transactions and leases of
// the rounds after it.
//
// With skipValidityWindow, a txn may be evaluated long after its LastValid, so only its lease is
// checked: a txid confirmed earlier is not a duplicate.
func (l simulatorLedger) CheckDup(currentProto config.ConsensusParams, current, firstValid, lastValid basics.Round, txid transactions.Txid, txl ledgercore.Txlease) error {
	if l.skipValidityWindow {
		return l.Ledger.CheckLeaseAt(currentProto, current, firstValid, lastValid, txid, txl)
	}
	return l.Ledger.CheckDupAt(currentProto, current, firstValid, lastValid, txid, txl)
}

// LookupLatest would implicitly use the latest round in the _underlying_
// Ledger, it would give wrong results if that ledger has moved forward. But it
// should never be called, as the REST API is the only code using this function,
// and the REST API should never have access to a simulatorLedger.
func (l simulatorLedger) LookupLatest(addr basics.Address) (basics.AccountData, basics.Round, basics.MicroAlgos, error) {
	err := errors.New("unexpected call to LookupLatest")
	return basics.AccountData{}, 0, basics.MicroAlgos{}, err
}

// StartEvaluator is part of the ledger.Ledger interface. We override this so that
// the eval.LedgerForEvaluator value passed into eval.StartEvaluator is a simulatorLedger,
// not a data.Ledger. This ensures our overridden LookupLatest method will be used.
func (l simulatorLedger) StartEvaluator(hdr bookkeeping.BlockHeader, paysetHint, maxTxnBytesPerBlock int, tracer logic.EvalTracer) (*eval.BlockEvaluator, error) {
	if tracer == nil {
		return nil, errors.New("tracer is nil")
	}
	return eval.StartEvaluator(&l, hdr,
		eval.EvaluatorOptions{
			PaysetHint:           paysetHint,
			Generate:             true,
			Validate:             true,
			MaxTxnBytesPerBlock:  maxTxnBytesPerBlock,
			Tracer:               tracer,
			SkipValidityWindow:   l.skipValidityWindow,
			SkipNewBlockPolicies: l.pastStart,
		})
}

// SimulatorError is the base error type for all simulator errors.
type SimulatorError struct {
	err error
}

func (s SimulatorError) Error() string {
	return s.err.Error()
}

func (s SimulatorError) Unwrap() error {
	return s.err
}

// InvalidRequestError occurs when an invalid transaction group was submitted to the simulator.
type InvalidRequestError struct {
	SimulatorError
}

// EvalFailureError represents an error that occurred during evaluation.
type EvalFailureError struct {
	SimulatorError
}

// Simulator is a transaction group simulator for the block evaluator.
type Simulator struct {
	ledger       simulatorLedger
	developerAPI bool
}

// MakeSimulator creates a new simulator from a ledger.
func MakeSimulator(ledger *data.Ledger, developerAPI bool) *Simulator {
	return &Simulator{
		ledger:       simulatorLedger{Ledger: ledger}, // start round to be specified in Simulate method
		developerAPI: developerAPI,
	}
}

// isPlaceholderPQSig reports whether txn carries a placeholder PQSig:
// a non-blank PQ envelope with empty signature bytes and no other signature
// category set. There are two placeholders forms:
// - Scheme-only placeholder: Scheme set, PublicKey empty, Signature empty, for fee surcharge calculation
// - Full placeholder: Scheme, Salt, and PublicKey set, Signature empty, for authorizer address derivation
func isPlaceholderPQSig(txn transactions.SignedTxn) bool {
	return txn.Sig.Blank() && txn.Msig.Blank() && txn.Lsig.Blank() &&
		!txn.PQsig.Blank() && len(txn.PQsig.Signature) == 0
}

func validatePlaceholderPQSig(proto config.ConsensusParams, pqSig transactions.PQSig, authorizer basics.Address, fixSigners bool) error {
	if len(pqSig.PublicKey) == 0 || fixSigners {
		return pqSig.ValidateScheme(proto)
	}
	return pqSig.ValidateEnvelope(proto, authorizer)
}

// isPlaceholderDelegatedPQSig reports whether txn carries a placeholder PQSig
// nested in its LogicSig: a program with a non-blank nested PQ envelope with
// empty signature bytes and no other signature category set on either the
// transaction or the LogicSig.
func isPlaceholderDelegatedPQSig(txn transactions.SignedTxn) bool {
	return txn.Sig.Blank() && txn.Msig.Blank() && txn.PQsig.Blank() &&
		txn.Lsig.HasProgram() && txn.Lsig.Sig.Blank() &&
		txn.Lsig.Msig.Blank() && txn.Lsig.LMsig.Blank() &&
		!txn.Lsig.PQsig.Blank() && len(txn.Lsig.PQsig.Signature) == 0
}

func txnNeedsSyntheticSignature(txn transactions.SignedTxn) bool {
	return !txn.HasSignature() || isPlaceholderPQSig(txn) || isPlaceholderDelegatedPQSig(txn)
}

// A randomly generated private key. The actual value does not matter, as long as this is a valid
// private key.
var proxySigner = crypto.PrivateKey{
	128, 128, 92, 23, 212, 119, 175, 51, 157, 2, 165,
	215, 137, 37, 82, 42, 52, 227, 54, 41, 243, 67,
	141, 76, 208, 17, 199, 17, 140, 46, 113, 0, 159,
	50, 105, 52, 77, 104, 118, 200, 104, 220, 105, 21,
	147, 162, 191, 236, 115, 201, 197, 128, 8, 91, 224,
	78, 104, 209, 2, 185, 110, 28, 42, 97,
}

// check verifies that the transaction is well-formed and has valid or missing signatures.
// An invalid transaction group error is returned if the transaction is not well-formed or there are invalid signatures.
// To make things easier, we support submitting unsigned transactions and will respond whether signatures are missing.
func (s Simulator) check(hdr bookkeeping.BlockHeader, txgroup []transactions.SignedTxnWithAD, tracer logic.EvalTracer, overrides ResultEvalOverrides) error {
	proxySignerSecrets, err := crypto.SecretKeyToSignatureSecrets(proxySigner)
	if err != nil {
		return err
	}

	// If signaturesOptional is enabled, find and prep any transactions that are missing signatures.
	// We will modify a copy of these transactions to pass signature verification. The modifications
	// will not affect the input txgroup slice.
	//
	// Note: currently we only support missing transaction signatures, but it should be possible to
	// support unsigned delegated LogicSigs as well. A single-signature unsigned delegated LogicSig
	// is indistinguishable from an escrow LogicSig, so we would need to decide on another way of
	// denoting that a LogicSig's delegation signature is omitted, e.g. by setting all the bits of
	// the signature.
	proto := config.Consensus[hdr.CurrentProtocol]
	txnsToVerify := make([]transactions.SignedTxn, len(txgroup))
	for i, stxnad := range txgroup {
		stxn := stxnad.SignedTxn
		// StateProof txns were rejected by Request.Validate.
		if overrides.AllowEmptySignatures && !stxn.HasSignature() {
			// Replace the signed txn with one signed by the proxySigner. At evaluation this would
			// raise an error, since the proxySigner's public key likely does not have authority
			// over the sender's account. However, this will pass validation, since the signature
			// itself is valid.
			txnsToVerify[i] = stxn.Txn.Sign(proxySignerSecrets)
		} else if overrides.AllowEmptySignatures && isPlaceholderPQSig(stxn) {
			placeholderErr := validatePlaceholderPQSig(proto, stxn.PQsig, stxn.Authorizer(), overrides.FixSigners)
			if placeholderErr == nil {
				txnsToVerify[i] = stxn.Txn.Sign(proxySignerSecrets)
			} else {
				txnsToVerify[i] = stxn
			}
		} else if overrides.AllowEmptySignatures && isPlaceholderDelegatedPQSig(stxn) {
			if validatePlaceholderPQSig(proto, stxn.Lsig.PQsig, stxn.Authorizer(), overrides.FixSigners) == nil {
				// A delegated LogicSig carries a program that must still be
				// evaluated and traced, so unlike the no-signature cases we
				// cannot swap in a proxy-signed bare transaction. Keep the
				// program but drop the placeholder PQ proof and authorize via
				// the program hash, so the verification pass treats it as a
				// contract-only (escrow) account and runs the program without a
				// real Falcon signature.
				escrow := stxn
				escrow.Lsig.PQsig = transactions.PQSig{}
				escrow.AuthAddr = basics.Address(logic.HashProgram(escrow.Lsig.Logic))
				txnsToVerify[i] = escrow
			} else {
				txnsToVerify[i] = stxn
			}
		} else {
			txnsToVerify[i] = stxn
		}
	}

	// Verify the signed transactions are well-formed and have valid signatures
	_, err = verify.TxnGroupWithTracer(txnsToVerify, &hdr, nil, s.ledger, tracer)
	if err != nil {
		err = InvalidRequestError{SimulatorError{err}}
	}
	return err
}

func validateFixedPlaceholderPQEnvelopes(proto config.ConsensusParams, txgroup []transactions.SignedTxnWithAD) (int, error) {
	for i, stxnad := range txgroup {
		stxn := stxnad.SignedTxn
		if isPlaceholderPQSig(stxn) && len(stxn.PQsig.PublicKey) != 0 {
			if err := validatePlaceholderPQSig(proto, stxn.PQsig, stxn.Authorizer(), false); err != nil {
				return i, fmt.Errorf("pq signature validation failed: %w", err)
			}
		}
		if isPlaceholderDelegatedPQSig(stxn) && len(stxn.Lsig.PQsig.PublicKey) != 0 {
			if err := validatePlaceholderPQSig(proto, stxn.Lsig.PQsig, stxn.Authorizer(), false); err != nil {
				return i, fmt.Errorf("pq delegated logic signature validation failed: %w", err)
			}
		}
	}
	return -1, nil
}

// fixSigners sets the AuthAddr of the txns in txgroup that need a synthetic signature, up to and
// including the first app call; AfterProgram fixes the ones after it. authAddrs holds the signers
// set by earlier groups of the request, which the ledger at the start round does not know yet.
func (s Simulator) fixSigners(txgroup []transactions.SignedTxnWithAD, authAddrs map[basics.Address]basics.Address) error {
	// Map of rekeys for senders in the group
	staticRekeys := make(map[basics.Address]basics.Address)

	for i := range txgroup {
		stxn := &txgroup[i].SignedTxn
		sender := stxn.Txn.Sender

		if authAddr, ok := staticRekeys[sender]; ok && txnNeedsSyntheticSignature(*stxn) {
			// If there is a static rekey for the sender set the auth addr to that address
			stxn.AuthAddr = authAddr
			if stxn.AuthAddr == sender {
				stxn.AuthAddr = basics.Address{}
			}
		} else if txnNeedsSyntheticSignature(*stxn) {
			// Otherwise use the auth addr an earlier group left, or the one in the ledger. A
			// recorded zero (un-rekey) is used as is.
			authAddr, ok := authAddrs[sender]
			if !ok {
				data, _, _, err := s.ledger.LookupAccount(s.ledger.start, sender)
				if err != nil {
					return err
				}
				authAddr = data.AuthAddr
			}

			stxn.AuthAddr = authAddr
			if stxn.AuthAddr == sender {
				stxn.AuthAddr = basics.Address{}
			}
		}

		// Stop processing transactions after the first application because auth addr correction will be done in AfterProgram
		if stxn.Txn.Type == protocol.ApplicationCallTx {
			break
		}

		if stxn.Txn.RekeyTo != (basics.Address{}) {
			staticRekeys[sender] = stxn.Txn.RekeyTo
		}
	}
	return nil
}

// runGroups simulates the groups at idxs (indexes into tracer.groups), in order, with one tracer
// and one evaluator, so each group sees the effects of the ones before it. A failure that belongs
// to a group is written into that group's result and the next group still runs; a failed group's
// changes are rolled back by the evaluator. Any other error aborts the request.
//
// runGroups stops early, marks the remaining groups as not simulated and returns stopped when a
// group fails a post-evaluation check (it has already been committed) or leaves the evaluator state
// corrupted (a panic while it was being committed). The block must then be discarded. committed is
// the number of groups the evaluator committed.
//
// singleGroupRequest keeps the behaviour of a request with one group: a verification failure that
// can't be blamed on one transaction is an InvalidRequestError for the whole request.
func (s Simulator) runGroups(hdr bookkeeping.BlockHeader, tracer *evalTracer, evaluator *eval.BlockEvaluator, idxs []int, singleGroupRequest bool) (committed int, stopped bool, err error) {
	overrides := tracer.result.EvalOverrides
	notSimulated := func(n int, reason string) {
		for _, later := range idxs[n+1:] {
			tracer.result.TxnGroups[later].FailureMessage = "not simulated: " + reason
		}
	}
	for n, g := range idxs {
		if err := tracer.abortError(); err != nil {
			return committed, false, err
		}
		tracer.startGroup(g)
		group := tracer.groups[g]
		groupResult := tracer.currentGroup()

		if overrides.FixSigners {
			if err := s.fixSigners(group, tracer.authAddrs); err != nil {
				return committed, false, err
			}
		}

		// check that the transaction is well-formed and mark whether signatures are missing
		if err := s.check(hdr, group, tracer, overrides); err != nil {
			if stopErr := tracer.stopped(); stopErr != nil {
				return committed, false, stopErr
			}
			var verifyError *verify.TxGroupError
			if errors.As(err, &verifyError) && verifyError.GroupIndex >= 0 {
				groupResult.FailureMessage = verifyError.Error()
				groupResult.FailedAt = TxnPath{verifyError.GroupIndex}
				continue
			}
			// This group failed verification, but the problem can't be blamed on a single
			// transaction (e.g. a bad signature found by the batch verifier). It is an invalid
			// request when it is the only group, and this group's failure among several.
			var invalidRequest InvalidRequestError
			if singleGroupRequest || !errors.As(err, &invalidRequest) {
				return committed, false, err
			}
			groupResult.FailureMessage = err.Error()
			continue
		}

		if err := evaluator.TransactionGroup(group...); err != nil {
			if stopErr := tracer.stopped(); stopErr != nil {
				return committed, false, stopErr
			}
			groupResult.FailureMessage = err.Error()
			if errors.Is(err, ledgercore.ErrNoSpace) {
				// The group does not fit in what is left of the block. A smaller group after it may.
				groupResult.FailedAt = nil
			} else {
				groupResult.FailedAt = tracer.failedAt
			}
			if evaluatorCorrupted(evaluator, err) {
				// The evaluator state may hold part of this group, so no later group can run on it.
				notSimulated(n, fmt.Sprintf("group index %d left the evaluator state corrupted", g))
				return committed, true, nil
			}
			continue
		}
		committed++
		tracer.commitGroupAuthAddrs()

		// Full PQ placeholders need the final AuthAddr after FixSigners has run through evaluation.
		if overrides.AllowEmptySignatures && overrides.FixSigners {
			if failedAt, message := checkFixedPlaceholderPQEnvelopes(hdr, group); message != "" {
				if err := tracer.stopped(); err != nil {
					return committed, false, err
				}
				groupResult.FailureMessage = message
				groupResult.FailedAt = failedAt
				// The group is already part of the shared state, so later groups would see the
				// effects of a group reported as failed.
				notSimulated(n, fmt.Sprintf("group index %d failed a post-evaluation check", g))
				return committed, true, nil
			}
		}
	}
	// The last group may have been stopped part way without failing: a ClearState program stopped
	// part way doesn't fail its group. One that finished keeps its result, even when the context
	// ended since; an earlier group's result is only kept when the request could finish.
	if err := tracer.stopped(); err != nil {
		return committed, false, err
	}
	return committed, false, nil
}

// checkFixedPlaceholderPQEnvelopes runs validateFixedPlaceholderPQEnvelopes under the block's
// protocol, and returns the failing txn and the failure message, if any.
func checkFixedPlaceholderPQEnvelopes(hdr bookkeeping.BlockHeader, group []transactions.SignedTxnWithAD) (TxnPath, string) {
	proto, ok := config.Consensus[hdr.CurrentProtocol]
	if !ok {
		return nil, ""
	}
	idx, err := validateFixedPlaceholderPQEnvelopes(proto, group)
	if err != nil {
		return TxnPath{idx}, err.Error()
	}
	return nil, ""
}

// evaluatorCorrupted tells whether err, returned by evaluator.TransactionGroup, left the evaluator
// unusable: a panic recovered while the group was being committed.
func evaluatorCorrupted(evaluator *eval.BlockEvaluator, err error) bool {
	if errors.Is(err, ledgercore.ErrEvaluatorCorruptedState) {
		return true
	}
	var panicErr ledgercore.EvalPanicError
	if !errors.As(err, &panicErr) {
		return false
	}
	// A panic before the commit leaves the evaluator usable. An empty group changes nothing and
	// only reports whether the evaluator is corrupted.
	return errors.Is(evaluator.TransactionGroup(), ledgercore.ErrEvaluatorCorruptedState)
}

// finalizeGroup fills in the parts of group g's result that are known only after evaluation.
func (tracer *evalTracer) finalizeGroup(g int) {
	groupResult := &tracer.result.TxnGroups[g]

	if groupResult.UnnamedResourcesAccessed != nil {
		// Remove private fields for easier test comparison
		groupResult.UnnamedResourcesAccessed.removePrivateFields()
		if !groupResult.UnnamedResourcesAccessed.HasResources() {
			groupResult.UnnamedResourcesAccessed = nil
		}
		for i := range groupResult.Txns {
			txnResult := &groupResult.Txns[i]
			txnResult.UnnamedResourcesAccessed.removePrivateFields()
			if !txnResult.UnnamedResourcesAccessed.HasResources() {
				// Clean up any unused local resource assignments
				txnResult.UnnamedResourcesAccessed = nil
			}
		}
	}

	// Update total cost by aggregating individual txn costs
	totalCost := 0
	for _, txn := range groupResult.Txns {
		totalCost += txn.AppBudgetConsumed
	}
	groupResult.AppBudgetConsumed = totalCost

	// Set the FixedSigner for each transaction that had a signer change during evaluation
	for i := range groupResult.Txns {
		sender := groupResult.Txns[i].Txn.Txn.Sender
		inputSigner := groupResult.Txns[i].Txn.AuthAddr
		if inputSigner.IsZero() {
			// A zero AuthAddr indicates the sender is the signer
			inputSigner = sender
		}

		actualSigner := tracer.groups[g][i].SignedTxn.AuthAddr
		if actualSigner.IsZero() {
			// A zero AuthAddr indicates the sender is the signer
			actualSigner = sender
		}

		if inputSigner != actualSigner {
			groupResult.Txns[i].FixedSigner = actualSigner
		}
	}
}

// simulateSequential simulates the request's groups in order, as consecutive groups of one block.
func (s Simulator) simulateSequential(hdr bookkeeping.BlockHeader, request Request) (*Result, error) {
	groups := make([][]transactions.SignedTxnWithAD, len(request.TxnGroups))
	idxs := make([]int, len(request.TxnGroups))
	totalTxns := 0
	for i, txgroup := range request.TxnGroups {
		groups[i] = transactions.WrapSignedTxnsWithAD(txgroup)
		idxs[i] = i
		totalTxns += len(txgroup)
	}

	tracer := makeEvalTracer(s.ledger.start, groups, request)

	// s.ledger has 'StartEvaluator' because *data.Ledger is embedded in the simulatorLedger
	// and data.Ledger embeds *ledger.Ledger
	evaluator, err := s.ledger.StartEvaluator(hdr, totalTxns, 0, tracer)
	if err != nil {
		return nil, err
	}

	committed, stopped, err := s.runGroups(hdr, tracer, evaluator, idxs, len(groups) == 1)
	if err != nil {
		return nil, err
	}
	for g := range groups {
		tracer.finalizeGroup(g)
	}

	if committed > 0 && !stopped {
		// Finally, process any pending end-of-block state changes.
		ub, err := evaluator.GenerateBlock(nil)
		if err != nil {
			return nil, err
		}
		// Since we skip agreement, this block is imperfect w/ respect to seed/proposer/payouts
		vb := ledgercore.MakeValidatedBlock(ub.UnfinishedBlock(), ub.UnfinishedDeltas())
		tracer.result.Block = &vb
	}
	return tracer.result, nil
}

// parallelGroupSlotCount is the most evaluators that parallel groups run at once, across all
// simulate requests of the node. GOMAXPROCS follows the CPU quota of the process.
var parallelGroupSlotCount = runtime.GOMAXPROCS(0)

// parallelGroupSlots limits the evaluators that parallel groups run at once.
var parallelGroupSlots = semaphore.NewWeighted(int64(parallelGroupSlotCount))

// simulateOneGroup simulates group g of the request alone against the start state, with its own
// tracer and evaluator. Its sub-request is a copy of the whole request but for TxnGroups, so every
// other setting applies. No block is generated.
func (s Simulator) simulateOneGroup(hdr bookkeeping.BlockHeader, request Request, g int) (*Result, error) {
	sub := request
	sub.TxnGroups = request.TxnGroups[g : g+1]
	group := transactions.WrapSignedTxnsWithAD(sub.TxnGroups[0])

	tracer := makeEvalTracer(s.ledger.start, [][]transactions.SignedTxnWithAD{group}, sub)
	evaluator, err := s.ledger.StartEvaluator(hdr, len(group), 0, tracer)
	if err != nil {
		return nil, err
	}
	if _, _, err := s.runGroups(hdr, tracer, evaluator, []int{0}, len(request.TxnGroups) == 1); err != nil {
		return nil, err
	}
	tracer.finalizeGroup(0)
	return tracer.result, nil
}

// simulateParallelGroup runs simulateOneGroup in a parallel group slot. A panic is recovered and
// returned as an error: it would otherwise crash the node, since it happens off the handler's
// goroutine.
func (s Simulator) simulateParallelGroup(hdr bookkeeping.BlockHeader, request Request, g int) (result *Result, err error) {
	if parallelGroupSlots.Acquire(request.Context(), 1) != nil {
		return nil, context.Cause(request.Context())
	}
	defer func() {
		if r := recover(); r != nil {
			logging.Base().Errorf("recovered from panic simulating transaction group %d: %v\n%s", g, r, debug.Stack())
			result, err = nil, fmt.Errorf("simulating transaction group %d: panic: %v", g, r)
		}
		parallelGroupSlots.Release(1)
	}()

	if request.hooks != nil && request.hooks.parallelGroup != nil {
		if err := request.hooks.parallelGroup(g); err != nil {
			return nil, err
		}
	}
	return s.simulateOneGroup(hdr, request, g)
}

// simulateParallel simulates every group of the request alone against the start state, in a pool
// of workers.
func (s Simulator) simulateParallel(hdr bookkeeping.BlockHeader, request Request) (*Result, error) {
	n := len(request.TxnGroups)
	results := make([]*Result, n)
	errs := make([]error, n)

	// failed is set by the first fatal error, after which the workers skip the groups left: the
	// request fails anyway.
	var failed atomic.Bool
	work := make(chan int)
	var wg sync.WaitGroup
	for range min(parallelGroupSlotCount, n) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for g := range work {
				if failed.Load() {
					continue
				}
				results[g], errs[g] = s.simulateParallelGroup(hdr, request, g)
				if errs[g] != nil {
					failed.Store(true)
				}
			}
		}()
	}
	for g := range n {
		work <- g
	}
	close(work)
	// Every worker has stopped writing to results and errs once Wait returns.
	wg.Wait()

	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}

	result := makeSimulationResult(s.ledger.start, request)
	for g, groupResult := range results {
		result.TxnGroups[g] = groupResult.TxnGroups[0]
		// Merge in ascending group index, so that the result does not depend on the order in
		// which the groups finished.
		result.InitialStates.merge(groupResult.InitialStates)
		if po := groupResult.ProtocolOverride; po != nil {
			for _, note := range po.Notes {
				for _, hit := range note.Hits {
					// Each group ran as group 0 of its own sub-request.
					result.ProtocolOverride.addHit(note.Code, NoteHit{Group: g, Path: hit.Path})
				}
			}
		}
	}
	return &result, nil
}

// Simulate simulates the request's transaction groups using the simulator. Will error if the request is invalid.
func (s Simulator) Simulate(simulateRequest Request) (Result, error) {
	if err := simulateRequest.Validate(s.developerAPI); err != nil {
		return Result{}, err
	}

	// Access underlying data.Ledger to get the real latest round
	latest := s.ledger.Ledger.Latest()
	if simulateRequest.Round != 0 {
		s.ledger.start = simulateRequest.Round
	} else {
		s.ledger.start = latest
	}
	s.ledger.pastStart = s.ledger.start < latest
	// s is a copy, so these hold for this call only.
	s.ledger.skipValidityWindow = simulateRequest.SkipValidityWindow
	s.ledger.rewriteValidityWindow = simulateRequest.RewriteValidityWindow
	s.ledger.counted = s.ledger.Ledger.Ledger.Counted(ledgercore.EffortFromContext(simulateRequest.Context()))

	prevBlockHdr, err := s.ledger.BlockHdr(s.ledger.start)
	if err != nil {
		return Result{}, err
	}
	var override *ProtocolOverride
	if simulateRequest.Protocol != "" {
		override, err = s.overrideProtocol(&simulateRequest, prevBlockHdr, latest)
		if err != nil {
			return Result{}, err
		}
		if override.Applied {
			prevBlockHdr = upgradeAt(prevBlockHdr, override.To)
		}
	}
	nextBlock := bookkeeping.MakeBlock(prevBlockHdr)

	if simulateRequest.RewriteValidityWindow {
		// simulateRequest is a copy: the caller's groups, which other rounds may share, are kept.
		simulateRequest.TxnGroups = rewriteValidityWindow(simulateRequest.TxnGroups, nextBlock.Round(), config.Consensus[nextBlock.CurrentProtocol])
	}

	var result *Result
	if simulateRequest.ParallelGroups {
		result, err = s.simulateParallel(nextBlock.BlockHeader, simulateRequest)
	} else {
		result, err = s.simulateSequential(nextBlock.BlockHeader, simulateRequest)
	}
	if err != nil {
		return Result{}, err
	}

	populateFeeUsage(result, config.Consensus[nextBlock.CurrentProtocol])

	if override != nil {
		// The tracers added the notes that transactions hit.
		if hits := result.ProtocolOverride; hits != nil {
			override.Notes = append(override.Notes, hits.Notes...)
		}
		override.sortNotes()
		result.ProtocolOverride = override
	}

	return *result, nil
}

// overrideProtocol works out how request.Protocol applies to the round after prevHdr, whose
// header it is, with latest the node's latest round. When it applies, it sets the ledger to switch
// to it, and request.protocolGaps for the tracers.
func (s *Simulator) overrideProtocol(request *Request, prevHdr bookkeeping.BlockHeader, latest basics.Round) (*ProtocolOverride, error) {
	override, gaps, err := s.ledger.overrideProtocol(request.Protocol, prevHdr, latest)
	request.protocolGaps = gaps
	return override, err
}
