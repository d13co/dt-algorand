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

	// MaxGroups is the most transaction groups the request may hold. Values below 1 count as 1
	// and values above MaxSimulateGroups are clamped to it. It is set by the node from its config
	// (MaxAPISimulateGroups), never by API clients. Non-standard fork feature.
	MaxGroups uint64

	// MaxExtraOpcodeBudget is the most ExtraOpcodeBudget the request may ask for, per group. 0 counts
	// as MaxExtraOpcodeBudget (the upstream limit) and values above MaxExtraOpcodeBudgetCeiling are
	// clamped to it. It is set by the node from its config (MaxAPISimulateExtraOpcodeBudget), never
	// by API clients. Non-standard fork feature.
	MaxExtraOpcodeBudget uint64

	// ctx stops the simulation early once it is done (abort.go). Non-standard fork feature.
	ctx context.Context

	hooks *testHooks
}

// testHooks let tests inject failures. They are nil outside tests.
type testHooks struct {
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

// WithNodeLimits returns r with the limits of a node configured with cfg: MaxGroups and
// MaxExtraOpcodeBudget. The nodes, the REST handlers that check a request before
// passing it to the node, and GET /v2/capabilities all take the limits from it, so the limits the
// endpoint reports are the ones simulate enforces. Non-standard fork feature.
func (r Request) WithNodeLimits(cfg config.Local) Request {
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

	// upgradeTo, when set, is the protocol that the round after start switches to
	// (QueryRequest.Protocol).
	upgradeTo protocol.ConsensusVersion

	// pastStart is set when start is before the node's latest round, so the simulated block, the
	// one after start, is already on chain. The policies that apply only to new blocks are then
	// off, so a txn the chain accepted at the time simulates as accepted
	// (eval.EvaluatorOptions.SkipNewBlockPolicies). Set by Simulate and by prepareRound.
	pastStart bool

	// counted is the ledger whose reads count in the request's Effort (effort.go). Set by Simulate.
	counted ledger.CountedLedger
}

// BlockHdr is part of the ledger.Ledger interface.
//
// Headers after the start round are refused, as on chain, where the ledger does not yet have the
// header of the round being evaluated or any later one. Without this, a ledger that has them (a
// past start round on a time-travel node) would show them to heartbeats (the seed of FirstValid,
// which may be start+1). The evaluator itself reads only headers up to the start round.
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
func (l simulatorLedger) CheckDup(currentProto config.ConsensusParams, current, firstValid, lastValid basics.Round, txid transactions.Txid, txl ledgercore.Txlease) error {
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

// isPlaceholderSaltedKeySig reports whether txn carries a placeholder SaltedKeySig:
// a non-blank SaltedKeySig envelope with empty signature bytes and no other signature
// category set. There are two placeholders forms:
// - Scheme-only placeholder: Scheme set, PublicKey empty, Signature empty, for fee surcharge calculation
// - Full placeholder: Scheme, Salt, and PublicKey set, Signature empty, for authorizer address derivation
// An ls-scheme SaltedKeySig is never a placeholder. Empty Signature
// bytes there mean a program that takes no arguments, which is a complete
// authorization: the program must be evaluated and traced, not swapped for a
// proxy signature. Nothing is missing from it, either, since the address
// commits to the program and the program is what the fee is charged for.
func isPlaceholderSaltedKeySig(txn transactions.SignedTxn) bool {
	return txn.Sig.Blank() && txn.Msig.Blank() && txn.Lsig.Blank() &&
		!txn.SKsig.Blank() && len(txn.SKsig.Signature) == 0 &&
		!txn.SKsig.IsLogicSig()
}

func validatePlaceholderSaltedKeySig(proto config.ConsensusParams, sksig transactions.SaltedKeySig, authorizer basics.Address, fixSigners bool) error {
	if len(sksig.PublicKey) == 0 || fixSigners {
		return sksig.ValidateScheme(proto)
	}
	return sksig.ValidateEnvelope(proto, authorizer)
}

// isPlaceholderDelegatedSaltedKeySig reports whether txn carries a placeholder SaltedKeySig
// nested in its LogicSig: a program with a non-blank nested salted-key signature envelope with
// empty signature bytes and no other signature category set on either the
// transaction or the LogicSig.
// An ls-scheme nested SaltedKeySig is never a placeholder. A salted LogicSig account
// authorizes only by evaluating its own program, so it cannot delegate to
// another program, and consensus rejects any such delegation. Treating it as a
// placeholder would let simulation succeed for a transaction that can never be
// valid.
func isPlaceholderDelegatedSaltedKeySig(txn transactions.SignedTxn) bool {
	return txn.Sig.Blank() && txn.Msig.Blank() && txn.SKsig.Blank() &&
		txn.Lsig.HasProgram() && txn.Lsig.Sig.Blank() &&
		txn.Lsig.Msig.Blank() && txn.Lsig.LMsig.Blank() &&
		!txn.Lsig.SKsig.Blank() && len(txn.Lsig.SKsig.Signature) == 0 &&
		!txn.Lsig.SKsig.IsLogicSig()
}

func txnNeedsSyntheticSignature(txn transactions.SignedTxn) bool {
	return !txn.HasSignature() || isPlaceholderSaltedKeySig(txn) || isPlaceholderDelegatedSaltedKeySig(txn)
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
		} else if overrides.AllowEmptySignatures && isPlaceholderSaltedKeySig(stxn) {
			placeholderErr := validatePlaceholderSaltedKeySig(proto, stxn.SKsig, stxn.Authorizer(), overrides.FixSigners)
			if placeholderErr == nil {
				txnsToVerify[i] = stxn.Txn.Sign(proxySignerSecrets)
			} else {
				txnsToVerify[i] = stxn
			}
		} else if overrides.AllowEmptySignatures && isPlaceholderDelegatedSaltedKeySig(stxn) {
			if validatePlaceholderSaltedKeySig(proto, stxn.Lsig.SKsig, stxn.Authorizer(), overrides.FixSigners) == nil {
				// A delegated LogicSig carries a program that must still be
				// evaluated and traced, so unlike the no-signature cases we
				// cannot swap in a proxy-signed bare transaction. Keep the
				// program but drop the placeholder SaltedKeySig proof and authorize via
				// the program hash, so the verification pass treats it as a
				// contract-only (escrow) account and runs the program without a
				// real authorization signature.
				escrow := stxn
				escrow.Lsig.SKsig = transactions.SaltedKeySig{}
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

func validateFixedPlaceholderSaltedKeySigEnvelopes(proto config.ConsensusParams, txgroup []transactions.SignedTxnWithAD) (int, error) {
	for i, stxnad := range txgroup {
		stxn := stxnad.SignedTxn
		if isPlaceholderSaltedKeySig(stxn) && len(stxn.SKsig.PublicKey) != 0 {
			if err := validatePlaceholderSaltedKeySig(proto, stxn.SKsig, stxn.Authorizer(), false); err != nil {
				return i, fmt.Errorf("salted-key signature validation failed: %w", err)
			}
		}
		if isPlaceholderDelegatedSaltedKeySig(stxn) && len(stxn.Lsig.SKsig.PublicKey) != 0 {
			if err := validatePlaceholderSaltedKeySig(proto, stxn.Lsig.SKsig, stxn.Authorizer(), false); err != nil {
				return i, fmt.Errorf("salted-key signature delegation validation failed: %w", err)
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

// runGroups simulates the request's groups (tracer.groups), in order, with one tracer and one
// evaluator, so each group sees the effects of the ones before it. A failure that belongs
// to a group is written into that group's result and the next group still runs; a failed group's
// changes are rolled back by the evaluator. Any other error aborts the request.
//
// runGroups stops early, marks the remaining groups as not simulated and returns stopped when a
// group fails a post-evaluation check (it has already been committed) or leaves the evaluator state
// corrupted (a panic while it was being committed). The block must then be discarded. committed is
// the number of groups the evaluator committed.
//
// A request with one group keeps upstream's behaviour: a verification failure that can't be blamed
// on one transaction is an InvalidRequestError for the whole request.
func (s Simulator) runGroups(hdr bookkeeping.BlockHeader, tracer *evalTracer, evaluator *eval.BlockEvaluator) (committed int, stopped bool, err error) {
	overrides := tracer.result.EvalOverrides
	singleGroupRequest := len(tracer.groups) == 1
	// The groups run in the order of tracer.groups, so the ones after index g are the ones left.
	notSimulated := func(g int, reason string) {
		for later := g + 1; later < len(tracer.groups); later++ {
			tracer.result.TxnGroups[later].FailureMessage = "not simulated: " + reason
		}
	}
	for g := range tracer.groups {
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
				notSimulated(g, fmt.Sprintf("group index %d left the evaluator state corrupted", g))
				return committed, true, nil
			}
			continue
		}
		committed++
		tracer.commitGroupAuthAddrs()

		// Full SaltedKeySig placeholders need the final AuthAddr after FixSigners has run through evaluation.
		if overrides.AllowEmptySignatures && overrides.FixSigners {
			if failedAt, message := checkFixedPlaceholderSaltedKeySigEnvelopes(hdr, group); message != "" {
				if err := tracer.stopped(); err != nil {
					return committed, false, err
				}
				groupResult.FailureMessage = message
				groupResult.FailedAt = failedAt
				// The group is already part of the shared state, so later groups would see the
				// effects of a group reported as failed.
				notSimulated(g, fmt.Sprintf("group index %d failed a post-evaluation check", g))
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

// checkFixedPlaceholderSaltedKeySigEnvelopes runs validateFixedPlaceholderSaltedKeySigEnvelopes under the block's
// protocol, and returns the failing txn and the failure message, if any.
func checkFixedPlaceholderSaltedKeySigEnvelopes(hdr bookkeeping.BlockHeader, group []transactions.SignedTxnWithAD) (TxnPath, string) {
	proto, ok := config.Consensus[hdr.CurrentProtocol]
	if !ok {
		return nil, ""
	}
	idx, err := validateFixedPlaceholderSaltedKeySigEnvelopes(proto, group)
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
	totalTxns := 0
	for i, txgroup := range request.TxnGroups {
		groups[i] = transactions.WrapSignedTxnsWithAD(txgroup)
		totalTxns += len(txgroup)
	}

	tracer := makeEvalTracer(s.ledger.start, groups, request)

	// s.ledger has 'StartEvaluator' because *data.Ledger is embedded in the simulatorLedger
	// and data.Ledger embeds *ledger.Ledger
	evaluator, err := s.ledger.StartEvaluator(hdr, totalTxns, 0, tracer)
	if err != nil {
		return nil, err
	}

	committed, stopped, err := s.runGroups(hdr, tracer, evaluator)
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
	// s is a copy, so this holds for this call only.
	s.ledger.counted = s.ledger.Ledger.Ledger.Counted(ledgercore.EffortFromContext(simulateRequest.Context()))

	prevBlockHdr, err := s.ledger.BlockHdr(s.ledger.start)
	if err != nil {
		return Result{}, err
	}
	nextBlock := bookkeeping.MakeBlock(prevBlockHdr)

	result, err := s.simulateSequential(nextBlock.BlockHeader, simulateRequest)
	if err != nil {
		return Result{}, err
	}

	populateFeeUsage(result, config.Consensus[nextBlock.CurrentProtocol])

	return *result, nil
}
