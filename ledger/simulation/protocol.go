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

// Protocol override: simulate round R as if an upgrade to a later protocol took effect at R+1.
// Non-standard fork feature.

import (
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/protocol"
)

// ProtocolLatest names, in Request.Protocol, the protocol of the block after the node's latest one:
// the protocol of the latest block, or the one an upgrade switches to right after it.
const ProtocolLatest = "latest"

// ProtocolOverride reports how Request.Protocol applied to a simulated round.
type ProtocolOverride struct {
	// From is the protocol the round after the simulated one really had.
	From protocol.ConsensusVersion
	// To is the requested protocol, with aliases resolved.
	To protocol.ConsensusVersion
	// Applied is false when From is already To: the round is simulated as it was.
	Applied bool
	// Notes list where the simulation under To differs from what the chain recorded, or reads
	// data the chain never produced. They are only present when Applied.
	Notes []ProtocolNote
}

// ProtocolNote is one note of a ProtocolOverride.
type ProtocolNote struct {
	Code    string
	Message string
	// Hits are the transactions that read the data the note is about, up to maxNoteHits. Static
	// notes, about the protocol change as a whole, have none.
	Hits []NoteHit
}

// NoteHit is a transaction that hit a ProtocolNote: Path in the request's group Group. A logic
// signature's hit has the path of its top-level transaction.
type NoteHit struct {
	Group int
	Path  TxnPath
}

const maxNoteHits = 16

// Note codes, in the order notes are reported.
const (
	noteSemanticsChanged     = "semantics-changed"
	noteNoHeartbeatChallenge = "no-heartbeat-challenges"
	noteNoLoadHistory        = "no-load-history"
	noteBlockIncentiveFields = "block-incentive-fields"
	noteBlockHashFields      = "block-hash-fields"
	noteOnlineStake          = "online-stake-includes-expired"
	noteAccountIncentive     = "account-incentive-fields"
	noteAppVersion           = "app-version-starts-at-upgrade"
)

var noteOrder = []string{
	noteSemanticsChanged,
	noteNoHeartbeatChallenge,
	noteNoLoadHistory,
	noteBlockIncentiveFields,
	noteBlockHashFields,
	noteOnlineStake,
	noteAccountIncentive,
	noteAppVersion,
}

// hitNoteMessages are the messages of the notes that transactions hit.
var hitNoteMessages = map[string]string{
	noteBlockIncentiveFields: "block read the proposer, fees collected, bonus or proposer payout of a round whose protocol had no incentives; the chain never recorded them, so they are zero",
	noteBlockHashFields:      "block read a SHA-512 or SHA-256 header field of a round whose protocol did not compute it, so it is zero",
	noteOnlineStake:          "online_stake used a balance round whose protocol did not exclude expired participation keys, so it includes the stake of keys that expire before the simulated round, as it would for the first 320 rounds after a real upgrade",
	noteAccountIncentive:     "an account's incentive eligibility, last proposal or last heartbeat was read; accounts at this round predate incentives, so it is false or 0",
	noteAppVersion:           "AppVersion was read; app versions are only counted from the upgrade on, so it is 0 for apps updated before",
}

// shortProtocolNames are the vNN names Request.Protocol accepts for mainline protocols.
var shortProtocolNames = map[string]protocol.ConsensusVersion{
	"v24": protocol.ConsensusV24,
	"v25": protocol.ConsensusV25,
	"v26": protocol.ConsensusV26,
	"v27": protocol.ConsensusV27,
	"v28": protocol.ConsensusV28,
	"v29": protocol.ConsensusV29,
	"v30": protocol.ConsensusV30,
	"v31": protocol.ConsensusV31,
	"v32": protocol.ConsensusV32,
	"v33": protocol.ConsensusV33,
	"v34": protocol.ConsensusV34,
	"v35": protocol.ConsensusV35,
	"v36": protocol.ConsensusV36,
	"v37": protocol.ConsensusV37,
	"v38": protocol.ConsensusV38,
	"v39": protocol.ConsensusV39,
	"v40": protocol.ConsensusV40,
	"v41": protocol.ConsensusV41,
	"v42": protocol.ConsensusV42,
	"v43": protocol.ConsensusV43,
}

// extraUpgrades are upgrades a chain really made that the ApprovedUpgrades of config.Consensus
// leave out: FNet moved from v41 to fnet5 at round 22,981,868.
var extraUpgrades = map[protocol.ConsensusVersion][]protocol.ConsensusVersion{
	protocol.ConsensusV41: {protocol.ConsensusVFnet5},
}

// parseProtocol resolves a Request.Protocol name other than ProtocolLatest: a vNN name, or a key
// of config.Consensus.
func parseProtocol(name string) (protocol.ConsensusVersion, error) {
	if v, ok := shortProtocolNames[name]; ok {
		return v, nil
	}
	v := protocol.ConsensusVersion(name)
	if _, ok := config.Consensus[v]; !ok {
		return "", fmt.Errorf("unknown protocol %q", name)
	}
	return v, nil
}

// validateProtocol checks Request.Protocol. Only nodes with the developer API simulate under
// vFuture: it is the protocol of no production network.
func validateProtocol(name string, developerAPI bool) error {
	if name == "" || name == ProtocolLatest {
		return nil
	}
	v, err := parseProtocol(name)
	if err != nil {
		return InvalidRequestError{SimulatorError{err}}
	}
	if v == protocol.ConsensusFuture && !developerAPI {
		return InvalidRequestError{SimulatorError{
			errors.New("the local configuration of the node has `EnableDeveloperAPI` turned off, while requesting protocol future"),
		}}
	}
	return nil
}

// protocolReachable tells whether a chain on protocol from can upgrade, in one or more steps, to
// protocol to.
func protocolReachable(from, to protocol.ConsensusVersion) bool {
	seen := map[protocol.ConsensusVersion]bool{from: true}
	queue := []protocol.ConsensusVersion{from}
	visit := func(u protocol.ConsensusVersion) {
		if !seen[u] {
			seen[u] = true
			queue = append(queue, u)
		}
	}
	for len(queue) > 0 {
		v := queue[0]
		queue = queue[1:]
		for u := range config.Consensus[v].ApprovedUpgrades {
			visit(u)
		}
		for _, u := range extraUpgrades[v] {
			visit(u)
		}
	}
	return seen[to] && to != from
}

// decideProtocol tells whether a round whose next block really ran realNext can be simulated
// under target: target must be a later protocol of the chain, or vFuture. It is false, with no
// error, when target is realNext.
func decideProtocol(realNext, target protocol.ConsensusVersion) (bool, error) {
	if target == realNext {
		return false, nil
	}
	if target == protocol.ConsensusFuture || protocolReachable(realNext, target) {
		return true, nil
	}
	return false, InvalidRequestError{SimulatorError{
		fmt.Errorf("protocol %s is not a later protocol than %s, which the simulated round's next block ran", target, realNext),
	}}
}

// upgradeAt returns hdr with an upgrade to target pending at hdr.Round+1: the block after hdr
// switches to target, as after an approved upgrade, and votes for nothing.
func upgradeAt(hdr bookkeeping.BlockHeader, target protocol.ConsensusVersion) bookkeeping.BlockHeader {
	hdr.UpgradeState = bookkeeping.UpgradeState{
		CurrentProtocol:      hdr.CurrentProtocol,
		NextProtocol:         target,
		NextProtocolSwitchOn: hdr.Round + 1,
		// Before the switch round, so the next block neither approves the upgrade nor clears it
		// as failed.
		NextProtocolVoteBefore: hdr.Round,
	}
	return hdr
}

// overrideProtocol works out how the protocol name (Request.Protocol, QueryRequest.Protocol)
// applies to the round after prevHdr, whose header it is, with latest the node's latest round.
// ProtocolLatest is the protocol of the block after latest. When it applies, it sets l to switch to
// it, and returns the protocolGaps for the tracers.
func (l *simulatorLedger) overrideProtocol(name string, prevHdr bookkeeping.BlockHeader, latest basics.Round) (*ProtocolOverride, *protocolGaps, error) {
	var target protocol.ConsensusVersion
	if name == ProtocolLatest {
		// The protocol of the block after the tip, so that the round before an upgrade switches
		// already gets the new one.
		latestHdr, err := l.Ledger.BlockHdr(latest)
		if err != nil {
			return nil, nil, err
		}
		_, tipNext, err := bookkeeping.ProcessUpgradeParams(latestHdr)
		if err != nil {
			return nil, nil, err
		}
		target = tipNext.CurrentProtocol
	} else {
		var err error
		if target, err = parseProtocol(name); err != nil {
			return nil, nil, InvalidRequestError{SimulatorError{err}}
		}
	}

	_, next, err := bookkeeping.ProcessUpgradeParams(prevHdr)
	if err != nil {
		return nil, nil, err
	}
	override := &ProtocolOverride{From: next.CurrentProtocol, To: target}
	if override.Applied, err = decideProtocol(override.From, target); err != nil || !override.Applied {
		return override, nil, err
	}

	realNext, targetParams := config.Consensus[override.From], config.Consensus[target]
	l.upgradeTo = target
	gaps := makeProtocolGaps(*l, realNext, targetParams)
	override.Notes = staticNotes(realNext, targetParams)
	return override, gaps, nil
}

// semanticParams are the ConsensusParams fields that change how transactions are checked or
// evaluated. A difference in one of them makes the semantics-changed note. Every other field is in
// ignoredParams; TestProtocolParamsClassified keeps the two lists complete.
var semanticParams = []string{
	"MaxTxnBytesPerBlock", "MaxTxnNoteBytes", "MaxAbsoluteTxnNoteBytes", "MaxTxnLife",
	"SupportGenesisHash", "RequireGenesisHash", "MinBalance", "MinTxnFee",
	"EnableAppCostPooling", "EnableLogicSigCostPooling", "RewardUnit", "RewardsRateRefreshInterval",
	"ForceNonParticipatingFeeSink", "SupportBecomeNonParticipatingTransactions", "PendingResidueRewards",
	"Asset", "MaxAssetsPerAccount", "MaxAssetNameBytes", "MaxAssetUnitNameBytes", "MaxAssetURLBytes",
	"SupportTxGroups", "MaxTxGroupSize", "SupportTransactionLeases", "FixTransactionLeases",
	"LogicSigVersion", "LogicSigMaxSize", "MaxAbsoluteLogicSigProgramSize", "LogicSigMaxCost",
	"LogicSigMsig", "LogicSigLMsig", "MaxAssetDecimals", "SupportRekeying", "EnforceAuthAddrSenderDiff",
	"Application", "MaxAppArgs", "MaxAppTotalArgLen", "MaxAbsoluteTotalArgLen", "MaxAppProgramLen",
	"MaxAppTotalProgramLen", "MaxExtraAppProgramPages", "MaxAbsoluteExtraProgramPages",
	"MaxAppTxnAccounts", "MaxAppTxnForeignApps", "MaxAppTxnForeignAssets", "MaxAppTotalTxnReferences",
	"MaxAppProgramCost", "MaxAppKeyLen", "MaxAppBytesValueLen", "MaxAppSumKeyValueLens",
	"MaxInnerTransactions", "EnableInnerTransactionPooling", "IsolateClearState", "MinInnerApplVersion",
	"MaxAppsCreated", "MaxAppsOptedIn", "AppFlatParamsMinBalance", "AppFlatOptInMinBalance",
	"SchemaMinBalancePerEntry", "SchemaUintMinBalance", "SchemaBytesMinBalance", "MaxBoxSize",
	"BoxFlatMinBalance", "BoxByteMinBalance", "MaxAppBoxReferences", "MaxAppAccess", "BytesPerBoxReference",
	"MaxLocalSchemaEntries", "MaxGlobalSchemaEntries", "MaximumMinimumBalance", "EnableAssetCloseAmount",
	"InitialRewardsRateCalculation", "NoEmptyLocalDeltas", "EnableKeyregCoherencyCheck",
	"EnableProperExtraPageAccounting", "EnableAppVersioning", "RewardsCalculationFix",
	"EnableStateProofKeyregCheck", "MaxKeyregValidPeriod", "UnifyInnerTxIDs", "UnfundedSenders",
	"EnablePrecheckECDSACurve", "EnableBareBudgetError", "AppForbidLowResources", "EnableBoxRefNameError",
	"Payouts", "Heartbeat", "AppSizeUpdates", "AllowZeroLocalAppRef",
	"PerByteTxnSurcharge", "EnablePQSchemeFalcon1024", "CloseRequiresBaseMinBalance",
}

// ignoredParams are the ConsensusParams fields of agreement, upgrades, block format, state proofs
// and storage, which do not change how a transaction is evaluated, or that have notes of their own.
// ExcludeExpiredCirculation does not change online_stake in the simulated round, which follows the
// balance round's protocol (the online-stake note), and Bonus only changes the proposer payout.
var ignoredParams = []string{
	"ExcludeExpiredCirculation", "Bonus",
	"UpgradeVoteRounds", "UpgradeThreshold", "DefaultUpgradeWaitRounds", "MinUpgradeWaitRounds",
	"MaxUpgradeWaitRounds", "MaxVersionStringLen", "ApprovedUpgrades", "DefaultKeyDilution",
	"SeedLookback", "SeedRefreshInterval", "MaxBalLookback", "NumProposers",
	"SoftCommitteeSize", "SoftCommitteeThreshold", "CertCommitteeSize", "CertCommitteeThreshold",
	"NextCommitteeSize", "NextCommitteeThreshold", "LateCommitteeSize", "LateCommitteeThreshold",
	"RedoCommitteeSize", "RedoCommitteeThreshold", "DownCommitteeSize", "DownCommitteeThreshold",
	"AgreementFilterTimeout", "AgreementFilterTimeoutPeriod0", "AgreementDeadlineTimeoutPeriod0",
	"FastRecoveryLambda", "PaysetCommit", "MaxTimestampIncrement", "SupportSignedTxnInBlock",
	"ApplyData", "RewardsInApplyData", "CredentialDomainSeparationEnabled", "TxnCounter",
	"StateProofInterval", "StateProofTopVoters", "StateProofVotersLookback", "StateProofWeightThreshold",
	"StateProofStrengthTarget", "StateProofMaxRecoveryIntervals", "StateProofExcludeTotalWeightWithRewards",
	"StateProofBlockHashInLightHeader", "MaxProposedExpiredOnlineAccounts", "EnableLedgerDataUpdateRound",
	"EnableSHA256TxnCommitmentHeader", "CatchpointLookback", "DeeperBlockHeaderHistory",
	"StateProofUseTrackerVerification", "EnableCatchpointsWithSPContexts",
	"EnableCatchpointsWithOnlineAccounts", "DynamicFilterTimeout", "EnableSha512BlockHash",
	"LoadTracking", "EnableSelectF128",
}

// paramChanges lists the semanticParams that differ between from and to, as "Name: from → to".
func paramChanges(from, to config.ConsensusParams) []string {
	fv, tv := reflect.ValueOf(from), reflect.ValueOf(to)
	var changes []string
	for _, name := range semanticParams {
		a, b := fv.FieldByName(name).Interface(), tv.FieldByName(name).Interface()
		if !reflect.DeepEqual(a, b) {
			changes = append(changes, fmt.Sprintf("%s: %+v → %+v", name, a, b))
		}
	}
	return changes
}

// staticNotes are the notes about the change from realNext to target as a whole.
func staticNotes(realNext, target config.ConsensusParams) []ProtocolNote {
	var notes []ProtocolNote
	if changes := paramChanges(realNext, target); len(changes) > 0 {
		notes = append(notes, ProtocolNote{
			Code:    noteSemanticsChanged,
			Message: "consensus parameters that affect transactions, old programs included, differ from the real protocol: " + strings.Join(changes, "; "),
		})
	}
	if target.Heartbeat && realNext.Payouts != target.Payouts {
		notes = append(notes, ProtocolNote{
			Code:    noteNoHeartbeatChallenge,
			Message: "the challenge rounds before the simulated one ran other payout rules, so there is no challenge: only heartbeats that pay the full fee are valid, and no account is absent",
		})
	}
	if target.LoadTracking && !realNext.LoadTracking {
		notes = append(notes, ProtocolNote{
			Code:    noteNoLoadHistory,
			Message: "earlier blocks recorded no load, so the congestion tax is computed from a load of 0",
		})
	}
	return notes
}

// protocolGaps is what a tracer needs to tell when an opcode reads data the chain never produced,
// for a simulation under a protocol override. It is only read, and shared by the tracers of a
// request.
type protocolGaps struct {
	// realNext are the parameters of the protocol the simulated round's next block really ran.
	realNext config.ConsensusParams
	// onlineStake is set when online_stake includes the stake of expired keys: the balance round
	// ran a protocol that did not exclude them, and the target excludes them.
	onlineStake bool
	// headerParams returns the parameters of the protocol a round really ran.
	headerParams func(basics.Round) (config.ConsensusParams, bool)
}

// makeProtocolGaps returns the protocolGaps of simulating the round after start under target,
// with l the simulation's ledger.
func makeProtocolGaps(l simulatorLedger, realNext, target config.ConsensusParams) *protocolGaps {
	gaps := &protocolGaps{
		realNext: realNext,
		headerParams: func(rnd basics.Round) (config.ConsensusParams, bool) {
			hdr, err := l.BlockHdr(rnd)
			if err != nil {
				return config.ConsensusParams{}, false
			}
			params, ok := config.Consensus[hdr.CurrentProtocol]
			return params, ok
		},
	}
	// A real next block that already excluded expired keys is no gap of the override.
	if target.ExcludeExpiredCirculation && !realNext.ExcludeExpiredCirculation {
		// The balance round of online_stake, as roundCowBase.balanceRound works it out.
		current := l.start + 1
		if phdr, err := l.BlockHdr(agreement.ParamsRound(current)); err == nil {
			balanceRound := agreement.BalanceRound(current, config.Consensus[phdr.CurrentProtocol])
			// Round 0 counts all online stake under every protocol.
			if params, ok := gaps.headerParams(balanceRound); ok && balanceRound != 0 {
				gaps.onlineStake = !params.ExcludeExpiredCirculation
			}
		}
	}
	return gaps
}

// opcodeGap returns the code of the note the opcode cx is about to run hits, or "".
func (g *protocolGaps) opcodeGap(cx *logic.EvalContext) string {
	spec := cx.GetOpSpec()
	field := func() (byte, bool) {
		program, pc := cx.GetProgram(), cx.PC()
		if pc+1 >= len(program) {
			return 0, false
		}
		return program[pc+1], true
	}
	switch spec.Name {
	case "block":
		f, ok := field()
		if !ok || len(cx.Stack) == 0 {
			return ""
		}
		// The round queried is on top of the stack. Its protocol decides what it recorded; a
		// field the round after the simulated one really had is no gap of the override.
		queried := func(has func(config.ConsensusParams) bool) bool {
			if has(g.realNext) {
				return false
			}
			params, ok := g.headerParams(basics.Round(cx.Stack[len(cx.Stack)-1].Uint))
			return ok && !has(params)
		}
		switch logic.BlockField(f) {
		case logic.BlkProposer, logic.BlkFeesCollected, logic.BlkBonus, logic.BlkProposerPayout:
			if queried(func(p config.ConsensusParams) bool { return p.Payouts.Enabled }) {
				return noteBlockIncentiveFields
			}
		case logic.BlkBranch512, logic.BlkSha512TxnCommitment:
			if queried(func(p config.ConsensusParams) bool { return p.EnableSha512BlockHash }) {
				return noteBlockHashFields
			}
		case logic.BlkSha256TxnCommitment:
			if queried(func(p config.ConsensusParams) bool { return p.EnableSHA256TxnCommitmentHeader }) {
				return noteBlockHashFields
			}
		}
	case "acct_params_get":
		f, ok := field()
		if !ok || g.realNext.Payouts.Enabled {
			return ""
		}
		switch logic.AcctParamsField(f) {
		case logic.AcctIncentiveEligible, logic.AcctLastProposed, logic.AcctLastHeartbeat:
			return noteAccountIncentive
		}
	case "voter_params_get":
		f, ok := field()
		if ok && !g.realNext.Payouts.Enabled && logic.VoterParamsField(f) == logic.VoterIncentiveEligible {
			return noteAccountIncentive
		}
	case "app_params_get":
		f, ok := field()
		if ok && !g.realNext.EnableAppVersioning && logic.AppParamsField(f) == logic.AppVersion {
			return noteAppVersion
		}
	case "online_stake":
		if g.onlineStake {
			return noteOnlineStake
		}
	}
	return ""
}

// compareHits orders hits by group, then path.
func compareHits(a, b NoteHit) int {
	if a.Group != b.Group {
		return a.Group - b.Group
	}
	return slices.Compare(a.Path, b.Path)
}

// addHit records that the transaction at path of group hit the note code. A note keeps the
// maxNoteHits lowest hits in group and path order, whatever order they ran in (logic signatures
// run before the apps of their group).
func (po *ProtocolOverride) addHit(code string, hit NoteHit) {
	i := slices.IndexFunc(po.Notes, func(n ProtocolNote) bool { return n.Code == code })
	if i < 0 {
		po.Notes = append(po.Notes, ProtocolNote{Code: code, Message: hitNoteMessages[code]})
		i = len(po.Notes) - 1
	}
	note := &po.Notes[i]
	if slices.ContainsFunc(note.Hits, func(h NoteHit) bool { return compareHits(h, hit) == 0 }) {
		return
	}
	if len(note.Hits) < maxNoteHits {
		note.Hits = append(note.Hits, hit)
		return
	}
	highest := 0
	for j := range note.Hits {
		if compareHits(note.Hits[j], note.Hits[highest]) > 0 {
			highest = j
		}
	}
	if compareHits(hit, note.Hits[highest]) < 0 {
		note.Hits[highest] = hit
	}
}

// sortNotes puts the notes in noteOrder, and the hits of each in group and path order.
func (po *ProtocolOverride) sortNotes() {
	slices.SortStableFunc(po.Notes, func(a, b ProtocolNote) int {
		return slices.Index(noteOrder, a.Code) - slices.Index(noteOrder, b.Code)
	})
	for i := range po.Notes {
		slices.SortFunc(po.Notes[i].Hits, compareHits)
	}
}
