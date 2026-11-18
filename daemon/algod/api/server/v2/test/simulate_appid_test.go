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

package test

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulateStateChangeAppID checks that each state change of an execution trace names the app
// whose state it changed: the running app for its own global, the owner for a foreign app_box_put.
func TestSimulateStateChangeAppID(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	creator := env.Accounts[0]

	// Throwaway apps first, so that no app id equals a position in the foreign apps array.
	var spare basics.AppIndex
	for range 3 {
		spare = env.CreateApp(creator.Addr, simulationtesting.AppParams{
			ApprovalProgram: "#pragma version 13\nint 1", ClearStateProgram: "#pragma version 13\nint 1"})
	}

	// Both apps share a creator, so the writer may write the owner's boxes once the owner opts into
	// family box access.
	owner := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 13
txn ApplicationID
bz end
byte "b"
byte "IIII"
box_put
int 1
app_params_set AppFamilyBoxAccess
end:
int 1`,
		ClearStateProgram: "#pragma version 13\nint 1",
	})
	writer := env.CreateApp(creator.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 13
txn ApplicationID
bz end
byte "g"
byte "v"
app_global_put
txn Applications 2
byte "b"
byte "WWWW"
app_box_put
end:
int 1`,
		ClearStateProgram: "#pragma version 13\nint 1",
		GlobalStateSchema: basics.StateSchema{NumByteSlice: 1},
	})
	env.TransferAlgos(env.Accounts[1].Addr, owner.Address(), 1_000_000)
	env.Txn(env.TxnInfo.NewTxn(txntest.Txn{
		Type:          protocol.ApplicationCallTx,
		Sender:        creator.Addr,
		ApplicationID: owner,
		Boxes:         []transactions.BoxRef{{Name: []byte("b")}},
	}).SignedTxn())

	call := env.TxnInfo.NewTxn(txntest.Txn{
		Type:          protocol.ApplicationCallTx,
		Sender:        creator.Addr,
		ApplicationID: writer,
		ForeignApps:   []basics.AppIndex{spare, owner},
		Boxes:         []transactions.BoxRef{{Index: 2, Name: []byte("b")}},
	}).Txn().Sign(creator.Sk)
	require.Greater(t, uint64(owner), uint64(3), "owner's id is not its foreign position (2)")
	router := newSimRouter(t, newSimNode(t, env, func(c *config.Local) { c.EnableDeveloperAPI = true }))
	request := v2.PreEncodedSimulateRequest{
		TxnGroups:       []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{call}}},
		ExecTraceConfig: simulation.ExecTraceConfig{Enable: true, State: true},
	}

	for _, format := range []string{"json", "msgpack"} {
		rec := postSimulate(t, router, encodeSimulateRequest(t, request, format), format)
		response := decodeSimulateResponse(t, rec, format)
		group := response.TxnGroups[0]
		require.Nil(t, group.FailureMessage, format)
		trace := group.Txns[0].TransactionTrace
		require.NotNil(t, trace, format)
		require.NotNil(t, trace.ApprovalProgramTrace, format)
		var changes []model.ApplicationStateOperation
		for _, unit := range *trace.ApprovalProgramTrace {
			if unit.StateChanges != nil {
				changes = append(changes, *unit.StateChanges...)
			}
		}
		require.Len(t, changes, 2, format)
		require.Equal(t, "g", changes[0].AppStateType, format)
		require.Equal(t, &writer, changes[0].AppId, "%s: own global", format)
		require.Equal(t, "b", changes[1].AppStateType, format)
		require.Equal(t, []byte("b"), changes[1].Key, format)
		require.Equal(t, &owner, changes[1].AppId, "%s: the foreign box's owner, not the running app", format)

		// the wire field is app-id, in either format
		var raw any
		if format == "json" {
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw), format)
		} else {
			require.NoError(t, protocol.DecodeReflect(rec.Body.Bytes(), &raw), format)
		}
		require.Equal(t, []string{fmt.Sprint(writer), fmt.Sprint(owner)}, stateChangeAppIDs(raw), format)
	}
}

// stateChangeAppIDs returns the app-id field of each state change in a decoded response, in order.
func stateChangeAppIDs(v any) []string {
	var out []string
	switch x := v.(type) {
	case map[string]any:
		if _, ok := x["app-state-type"]; ok {
			return []string{fmt.Sprint(x["app-id"])}
		}
		for _, k := range slices.Sorted(maps.Keys(x)) {
			out = append(out, stateChangeAppIDs(x[k])...)
		}
	case map[any]any:
		m := make(map[string]any, len(x))
		for k, e := range x {
			m[fmt.Sprint(k)] = e
		}
		return stateChangeAppIDs(m)
	case []any:
		for _, e := range x {
			out = append(out, stateChangeAppIDs(e)...)
		}
	}
	return out
}
