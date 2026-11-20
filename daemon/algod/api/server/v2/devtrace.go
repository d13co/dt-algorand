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

// The conversions of what ledger.DevTracer records (budget, reads, and a
// group's execution traces with the disassembly of its programs), shared by the
// answers that report them. Non-standard fork feature.

import (
	"slices"
	"strings"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/util"
)

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
func convertGroupTrace(g *ledger.GroupTrace, traces []*simulation.TransactionTrace) (*[]model.TxnExecTrace, *[]model.ProgramDisassembly) {
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
		out = append(out, model.TxnExecTrace{Txid: g.TxIDs[i].String(), ExecTrace: *convertTxnTrace(t)})
	}
	programs := make([]model.ProgramDisassembly, 0, len(order))
	for _, h := range order {
		program, ok := g.Programs[h]
		if !ok {
			continue
		}
		programs = append(programs, model.ProgramDisassembly{Hash: slices.Clone(h[:]), Lines: disassemblyLines(program)})
	}
	return &out, &programs
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
