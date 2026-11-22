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

package v2

// Per-transaction app state changes of a round (timetravel tag). Non-standard
// fork feature, kept in its own file to keep upstream merges of handlers.go clean.

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/labstack/echo/v4"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/util"
)

// errStateChangesTooLarge is the message of an answer over MaxAPIStateChangesResponseBytes.
const errStateChangesTooLarge = "the state changes take more than MaxAPIStateChangesResponseBytes (%d bytes): narrow them with the app, txid or txids parameters, or include less"

// errFailedParsingIncludeOption is the message of a bad include parameter.
const errFailedParsingIncludeOption = "include takes accounts, assets, apps, budget, reads and trace, comma-separated"

// errTraceNeedsDeveloperAPI is the message of include=trace on a node without EnableDeveloperAPI.
const errTraceNeedsDeveloperAPI = "include=trace needs node config EnableDeveloperAPI, as simulate's execution traces do"

// errTraceNeedsOneTxn is the message of include=trace without exactly one transaction.
const errTraceNeedsOneTxn = "include=trace needs exactly one transaction, in txid or txids"

// stateChangesIncludes are the values of the include parameter, as listed in
// GET /v2/capabilities.
var stateChangesIncludes = map[string]ledger.StateChangesInclude{
	string(model.GetTxnStateChangesForRoundParamsIncludeAccounts): ledger.IncludeAccounts,
	string(model.GetTxnStateChangesForRoundParamsIncludeAssets):   ledger.IncludeAssets,
	string(model.GetTxnStateChangesForRoundParamsIncludeApps):     ledger.IncludeApps,
	string(model.GetTxnStateChangesForRoundParamsIncludeBudget):   ledger.IncludeBudget,
	string(model.GetTxnStateChangesForRoundParamsIncludeReads):    ledger.IncludeReads,
	string(model.GetTxnStateChangesForRoundParamsIncludeTrace):    ledger.IncludeTrace,
}

// stateChangesIncludeGroups are the includes that return groups.
const stateChangesIncludeGroups = ledger.IncludeAccounts | ledger.IncludeAssets | ledger.IncludeApps

// stateChangesTraceConfig is what include=trace records: everything simulate can.
var stateChangesTraceConfig = simulation.ExecTraceConfig{Enable: true, Stack: true, Scratch: true, State: true, StateAppID: true}

// StateChangesIncludeValues lists the values of the include parameter this node takes, in a fixed
// order: trace only with EnableDeveloperAPI, as simulate's execution traces.
func StateChangesIncludeValues(developerAPI bool) []string {
	values := []string{
		string(model.GetTxnStateChangesForRoundParamsIncludeAccounts),
		string(model.GetTxnStateChangesForRoundParamsIncludeAssets),
		string(model.GetTxnStateChangesForRoundParamsIncludeApps),
		string(model.GetTxnStateChangesForRoundParamsIncludeBudget),
		string(model.GetTxnStateChangesForRoundParamsIncludeReads),
	}
	if developerAPI {
		values = append(values, string(model.GetTxnStateChangesForRoundParamsIncludeTrace))
	}
	return values
}

// GetTxnStateChangesForRound returns the app state changes of each transaction of a round, from its
// block re-evaluated.
// (GET /v2/deltas/{round}/txn/state-changes)
func (v2 *Handlers) GetTxnStateChangesForRound(ctx echo.Context, round basics.Round, params model.GetTxnStateChangesForRoundParams) error {
	handle, contentType, err := getCodecHandle((*string)(params.Format))
	if err != nil {
		return badRequest(ctx, err, errFailedParsingFormatOption, v2.Log)
	}
	maxBytes := v2.Node.Config().MaxAPIStateChangesResponseBytes
	f := ledger.StateChangesFilter{MaxBytes: maxBytes}
	if params.App != nil {
		f.App = *params.App
	}
	var txids []string
	if params.Txid != nil {
		txids = append(txids, *params.Txid)
	}
	if params.Txids != nil {
		if len(*params.Txids) == 0 { // not every transaction
			return badRequest(ctx, errors.New("empty txids"), errNoValidTxnSpecified, v2.Log)
		}
		txids = append(txids, *params.Txids...)
	}
	if params.Include != nil {
		if len(*params.Include) == 0 {
			return badRequest(ctx, errors.New("empty include"), errFailedParsingIncludeOption, v2.Log)
		}
		for _, inc := range *params.Include {
			bit, ok := stateChangesIncludes[string(inc)]
			if !ok {
				return badRequest(ctx, fmt.Errorf("unknown include %q", inc), fmt.Sprintf("%s: %q", errFailedParsingIncludeOption, inc), v2.Log)
			}
			f.Include |= bit
		}
	}
	for _, s := range txids {
		var txid transactions.Txid
		if parseErr := txid.FromString(s); parseErr != nil {
			return badRequest(ctx, parseErr, fmt.Sprintf("%s: %q", errNoValidTxnSpecified, s), v2.Log)
		}
		f.TxIDs = append(f.TxIDs, txid)
	}
	var trace *simulation.GroupExecTracer
	if f.Include&ledger.IncludeTrace != 0 {
		if !v2.Node.Config().EnableDeveloperAPI {
			return badRequest(ctx, errors.New("trace without EnableDeveloperAPI"), errTraceNeedsDeveloperAPI, v2.Log)
		}
		if len(util.MakeSet(f.TxIDs...)) != 1 {
			return badRequest(ctx, ledger.ErrTraceNeedsOneTxn, errTraceNeedsOneTxn, v2.Log)
		}
		f.Trace = func(group []transactions.SignedTxnWithAD) logic.EvalTracer {
			trace = simulation.NewGroupExecTracer(round, group, stateChangesTraceConfig)
			return trace
		}
	}
	sc, answered, err := v2.stateChangesForRound(ctx, round, f)
	if answered {
		return err
	}
	switch {
	case err == nil:
	case errors.Is(err, ledger.ErrStateChangesTooLarge):
		return badRequest(ctx, err, fmt.Sprintf(errStateChangesTooLarge, maxBytes), v2.Log)
	case errors.Is(err, ledger.ErrTraceNeedsOneTxn):
		return badRequest(ctx, err, errTraceNeedsOneTxn, v2.Log)
	case errors.Is(err, ledger.ErrDeltaReevaluation):
		return internalError(ctx, err, fmt.Sprintf(errFailedRetrievingStateDelta, err), v2.Log)
	default:
		return notFound(ctx, err, fmt.Sprintf(errFailedRetrievingStateDelta, err), v2.Log)
	}
	out := convertStateChanges(sc, f.Include&stateChangesIncludeGroups != 0)
	if f.Include&ledger.IncludeBudget != 0 {
		out.Budget = sliceOrNil(util.Map(sc.Budget, convertProgramBudget))
	}
	if f.Include&ledger.IncludeReads != 0 {
		out.Reads = sliceOrNil(util.Map(sc.Reads, convertTxnStateReads))
	}
	if trace != nil {
		if sc.Trace == nil {
			err = errors.New("the trace is missing")
			return internalError(ctx, err, fmt.Sprintf(errFailedRetrievingStateDelta, err), v2.Log)
		}
		out.Traces, out.Programs, err = convertGroupTrace(sc.Trace, trace.Traces(), maxBytes)
		if err != nil {
			return badRequest(ctx, err, fmt.Sprintf(errStateChangesTooLarge, maxBytes), v2.Log)
		}
	}
	data, err := encode(handle, out)
	if err != nil {
		return internalError(ctx, err, errFailedToEncodeResponse, v2.Log)
	}
	if maxBytes > 0 && uint64(len(data)) > maxBytes {
		err = fmt.Errorf("%w: %d bytes", ledger.ErrStateChangesTooLarge, len(data))
		return badRequest(ctx, err, fmt.Sprintf(errStateChangesTooLarge, maxBytes), v2.Log)
	}
	return writeEncoded(ctx, contentType, data)
}

// convertAccountState converts an account's own fields to the REST model.
func convertAccountState(ad *ledgercore.AccountData, proto *config.ConsensusParams) *model.AccountState {
	if ad == nil {
		return nil
	}
	var part *model.AccountParticipation
	if !ad.VoteID.IsEmpty() {
		part = &model.AccountParticipation{
			VoteParticipationKey:      slices.Clone(ad.VoteID[:]),
			SelectionParticipationKey: slices.Clone(ad.SelectionID[:]),
			VoteFirstValid:            ad.VoteFirstValid,
			VoteLastValid:             ad.VoteLastValid,
			VoteKeyDilution:           ad.VoteKeyDilution,
		}
		if !ad.StateProofID.IsEmpty() {
			spk := slices.Clone(ad.StateProofID[:])
			part.StateProofKey = &spk
		}
	}
	var minBalance uint64
	if proto != nil {
		minBalance = ad.MinBalance(proto).Raw
	}
	return &model.AccountState{
		Amount:             ad.MicroAlgos.Raw,
		MinBalance:         minBalance,
		Status:             ad.Status.String(),
		AuthAddr:           addrOrNil(ad.AuthAddr),
		IncentiveEligible:  omitEmpty(ad.IncentiveEligible),
		Participation:      part,
		TotalAppsOptedIn:   ad.TotalAppLocalStates,
		TotalAssetsOptedIn: ad.TotalAssets,
		TotalCreatedApps:   ad.TotalAppParams,
		TotalCreatedAssets: ad.TotalAssetParams,
		AppsTotalSchema: &model.ApplicationStateSchema{
			NumUint:      ad.TotalAppSchema.NumUint,
			NumByteSlice: ad.TotalAppSchema.NumByteSlice,
		},
		AppsTotalExtraPages: omitEmpty(uint64(ad.TotalExtraAppPages)),
		TotalBoxes:          omitEmpty(ad.TotalBoxes),
		TotalBoxBytes:       omitEmpty(ad.TotalBoxBytes),
		RewardBase:          omitEmpty(ad.RewardsBase),
		Rewards:             omitEmpty(ad.RewardedMicroAlgos.Raw),
		LastProposed:        omitEmpty(ad.LastProposed),
		LastHeartbeat:       omitEmpty(ad.LastHeartbeat),
	}
}

func convertHolding(h *basics.AssetHolding, asset basics.AssetIndex) *model.AssetHolding {
	if h == nil {
		return nil
	}
	m := AssetHolding(*h, asset)
	return &m
}

func convertAssetParams(p *basics.AssetParams, creator basics.Address, asset basics.AssetIndex) *model.AssetParams {
	if p == nil {
		return nil
	}
	return AssetParamsToAsset(creator.String(), asset, p).Params
}

func convertAppParams(p *basics.AppParams, creator basics.Address, app basics.AppIndex) *model.ApplicationParams {
	if p == nil {
		return nil
	}
	m := AppParamsToApplication(creator.String(), app, p).Params
	m.GlobalState = nil // global state changes are per transaction
	return m
}

// convertLocalState converts a local state's schema to the REST model, with its app id: a
// zero schema alone would be an empty object, which the codec leaves out.
func convertLocalState(s *basics.StateSchema, app basics.AppIndex) *model.ApplicationLocalState {
	if s == nil {
		return nil
	}
	return &model.ApplicationLocalState{Id: app, Schema: model.ApplicationStateSchema{NumUint: s.NumUint, NumByteSlice: s.NumByteSlice}}
}

// convertGroupStateChanges converts the groups of state changes to the REST model.
func convertGroupStateChanges(sc ledger.StateChanges) []model.GroupStateChanges {
	var proto *config.ConsensusParams
	if p, ok := config.Consensus[sc.Proto]; ok {
		proto = &p
	}
	out := make([]model.GroupStateChanges, len(sc.Groups))
	for i := range sc.Groups {
		g := &sc.Groups[i]
		m := model.GroupStateChanges{
			First: uint64(g.First),
			Count: uint64(g.Count),
			Txids: util.Map(g.TxIDs, func(id transactions.Txid) string { return id.String() }),
		}
		if !g.GroupID.IsZero() {
			id := slices.Clone(g.GroupID[:])
			m.GroupId = &id
		}
		m.Accounts = sliceOrNil(util.Map(g.Accounts, func(c ledger.AccountChange) model.AccountStateChange {
			return model.AccountStateChange{Address: c.Addr.String(),
				Before: convertAccountState(c.Before, proto), After: convertAccountState(c.After, proto)}
		}))
		m.Holdings = sliceOrNil(util.Map(g.Holdings, func(c ledger.HoldingChange) model.AssetHoldingChange {
			return model.AssetHoldingChange{Address: c.Addr.String(), AssetId: c.Asset,
				Before: convertHolding(c.Before, c.Asset), After: convertHolding(c.After, c.Asset)}
		}))
		m.Assets = sliceOrNil(util.Map(g.Assets, func(c ledger.AssetParamsChange) model.AssetParamsChange {
			return model.AssetParamsChange{AssetId: c.Asset,
				Before: convertAssetParams(c.Before, c.Creator, c.Asset), After: convertAssetParams(c.After, c.Creator, c.Asset)}
		}))
		m.Apps = sliceOrNil(util.Map(g.Apps, func(c ledger.AppParamsChange) model.AppParamsChange {
			return model.AppParamsChange{AppId: c.App,
				Before: convertAppParams(c.Before, c.Creator, c.App), After: convertAppParams(c.After, c.Creator, c.App)}
		}))
		m.LocalStates = sliceOrNil(util.Map(g.LocalStates, func(c ledger.LocalStateChange) model.LocalStateChange {
			return model.LocalStateChange{Address: c.Addr.String(), AppId: c.App,
				Before: convertLocalState(c.Before, c.App), After: convertLocalState(c.After, c.App)}
		}))
		out[i] = m
	}
	return out
}

var programKinds = map[ledger.ProgramKind]model.ProgramBudgetKind{
	ledger.ProgramApproval:   model.ProgramBudgetKindApproval,
	ledger.ProgramClearState: model.ProgramBudgetKindClear,
	ledger.ProgramLogicSig:   model.ProgramBudgetKindLogicSig,
}

func txidOrNil(txid transactions.Txid) *string {
	if txid == (transactions.Txid{}) {
		return nil
	}
	s := txid.String()
	return &s
}

func convertProgramBudget(b ledger.ProgramBudget) model.ProgramBudget {
	return model.ProgramBudget{
		Path:         b.Path,
		Txid:         txidOrNil(b.TxID),
		Kind:         programKinds[b.Kind],
		AppId:        omitEmpty(b.App),
		Cost:         uint64(b.Cost),
		BudgetBefore: b.BudgetBefore,
		BudgetAfter:  b.BudgetAfter,
		Failed:       omitEmpty(b.Failed),
	}
}

func convertTxnStateReads(r ledger.TxnStateReads) model.TxnStateReads {
	reads := util.Map(r.Reads, func(sr ledger.StateRead) model.AppStateRead {
		m := model.AppStateRead{
			AppId:        sr.App,
			AppStateType: model.AppStateReadAppStateType(convertApplicationState(sr.Type)),
			Key:          []byte(sr.Key),
			Exists:       sr.Exists,
		}
		if sr.Type == logic.LocalState {
			m.Account = addrOrNil(sr.Account)
		}
		if sr.Exists {
			if sr.Type == logic.BoxState {
				n := uint64(sr.Length)
				m.Length = &n
			} else {
				m.Value = convertStateChangeValue(&sr.Value)
			}
		}
		return m
	})
	return model.TxnStateReads{Path: r.Path, Txid: txidOrNil(r.TxID), Reads: reads}
}

// convertGroupTrace converts the traced group's execution traces, and the
// disassembly of the programs they ran, in the order the traces first ran them.
// It fails with ledger.ErrStateChangesTooLarge once the disassembly's text alone
// passes maxBytes (0: no limit), which the answer could not fit.
func convertGroupTrace(g *ledger.GroupTrace, traces []*simulation.TransactionTrace, maxBytes uint64) (*[]model.TxnExecTrace, *[]model.ProgramDisassembly, error) {
	out := make([]model.TxnExecTrace, 0, len(traces))
	var order []crypto.Digest
	seen := make(map[crypto.Digest]bool)
	var visit func(t *simulation.TransactionTrace)
	visit = func(t *simulation.TransactionTrace) {
		for _, h := range []crypto.Digest{t.LogicSigHash, t.ApprovalProgramHash, t.ClearStateProgramHash} {
			if !h.IsZero() && !seen[h] {
				seen[h] = true
				order = append(order, h)
			}
		}
		for i := range t.InnerTraces {
			visit(&t.InnerTraces[i])
		}
	}
	for i, t := range traces {
		if i >= len(g.TxIDs) {
			break
		}
		if t == nil {
			t = &simulation.TransactionTrace{}
		}
		visit(t)
		out = append(out, model.TxnExecTrace{Txid: g.TxIDs[i].String(), ExecTrace: *convertTxnTrace(t, true)})
	}
	programs := make([]model.ProgramDisassembly, 0, len(order))
	var text uint64
	for _, h := range order {
		program, ok := g.Programs[h]
		if !ok {
			continue
		}
		lines := disassemblyLines(program)
		for _, l := range lines {
			text += uint64(len(l.Text))
		}
		if maxBytes > 0 && text > maxBytes {
			return nil, nil, fmt.Errorf("%w: the disassembly passes %d bytes", ledger.ErrStateChangesTooLarge, maxBytes)
		}
		programs = append(programs, model.ProgramDisassembly{Hash: slices.Clone(h[:]), Lines: lines})
	}
	return &out, &programs, nil
}

// disassemblyLines splits a program's disassembly into lines, each with the pc
// of the opcode it holds.
func disassemblyLines(program []byte) []model.DisassemblyLine {
	text, offsets, err := logic.DisassembleWithOffsets(program)
	if err != nil && text == "" {
		return []model.DisassemblyLine{{Text: "// " + err.Error()}}
	}
	pcAt := make(map[int]uint64, len(offsets))
	for _, o := range offsets {
		pcAt[o.Offset] = uint64(o.PC)
	}
	lines := make([]model.DisassemblyLine, 0, len(offsets)+2)
	start := 0
	for start < len(text) {
		end := strings.IndexByte(text[start:], '\n')
		if end < 0 {
			end = len(text) - start
		}
		line := model.DisassemblyLine{Text: text[start : start+end]}
		if pc, ok := pcAt[start]; ok {
			line.Pc = &pc
		}
		lines = append(lines, line)
		start += end + 1
	}
	return lines
}
