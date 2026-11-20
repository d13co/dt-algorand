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
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/failures"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// failNode is a simNode that keeps failures.
type failNode struct {
	*simNode
	store *failures.Store
}

func (n *failNode) FailureStore() *failures.Store {
	return n.store
}

func getJSON(t *testing.T, h http.Handler, path string, out any) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	if out != nil && rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), out), rec.Body.String())
	}
	return rec
}

// TestSimulateFailureCapture checks that a simulate request with a failed group is kept as received,
// with the round it ran on, that its answer names it, and that the kept request replays to the same
// failure at that round, after the chain moved on.
func TestSimulateFailureCapture(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender, receiver := env.Accounts[0], env.Accounts[1]
	// rejects every call while its global "open" is unset; a later block sets it
	app := env.CreateApp(sender.Addr, simulationtesting.AppParams{
		ApprovalProgram: `#pragma version 8
txn ApplicationID
bz ok
txn NumAppArgs
bnz set
byte "open"
app_global_get
assert
b ok
set:
byte "open"
int 1
app_global_put
ok:
int 1`,
		ClearStateProgram: "#pragma version 8\nint 1",
		GlobalStateSchema: basics.StateSchema{NumUint: 1},
	})

	pay := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: receiver.Addr, Amount: 1000}).Txn().Sign(sender.Sk)
	call := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app}).Txn().Sign(sender.Sk)
	failing := v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{
		{Txns: []transactions.SignedTxn{pay}},
		{Txns: []transactions.SignedTxn{call}},
	}}

	store, err := failures.Open(filepath.Join(t.TempDir(), "failures.sqlite"), failures.Config{Retention: time.Hour}, logging.TestingLog(t))
	require.NoError(t, err)
	defer store.Close()
	n := &failNode{simNode: newSimNode(t, env, func(c *config.Local) { c.EnableFailureCapture = true }), store: store}
	router := server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000)

	var ids []string
	for _, format := range []string{"json", "msgpack"} {
		body := encodeSimulateRequest(t, failing, format)
		rec := postSimulate(t, router, body, format)
		response := decodeSimulateResponse(t, rec, format)
		require.Nil(t, response.TxnGroups[0].FailureMessage, format)
		require.NotNil(t, response.TxnGroups[1].FailureMessage, format)
		id := rec.Header().Get(v2.FailureIDHeader)
		require.NotEmpty(t, id, format)
		ids = append(ids, id)
		store.Flush()

		var f model.Failure
		got := getJSON(t, router, "/v2/failures/"+id, &f)
		require.Equal(t, http.StatusOK, got.Code, got.Body.String())
		require.Equal(t, id, f.Id)
		require.Equal(t, "simulate", f.Kind)
		require.Equal(t, response.LastRound, f.Round, format)
		require.NotNil(t, f.Groups)
		require.Len(t, *f.Groups, 1)
		require.EqualValues(t, 1, (*f.Groups)[0].Index)
		require.Equal(t, *response.TxnGroups[1].FailureMessage, (*f.Groups)[0].Message)
		require.NotNil(t, f.Request)
		require.Equal(t, body, *f.Request, format)
		require.Equal(t, format, *f.RequestFormat)
		require.NotNil(t, f.ConsensusVersion)
		require.NotNil(t, f.NodeVersion)
	}

	// the chain moves on: the app now accepts the same call
	env.Txn(env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.ApplicationCallTx, Sender: sender.Addr, ApplicationID: app,
		ApplicationArgs: [][]byte{[]byte("set")}}).SignedTxn())
	require.Nil(t, simulateOK(t, router, failing, "json").TxnGroups[1].FailureMessage, "passes at the latest round")

	// the kept request, at its round, fails as it did
	var f model.Failure
	getJSON(t, router, "/v2/failures/"+ids[0], &f)
	var replay v2.PreEncodedSimulateRequest
	require.NoError(t, protocol.DecodeJSON(*f.Request, &replay))
	replay.Round = f.Round
	rec := postSimulate(t, router, encodeSimulateRequest(t, replay, "json"), "json")
	again := decodeSimulateResponse(t, rec, "json")
	require.Equal(t, f.Round, again.LastRound)
	// a replay that fails is a failure too
	ids = append(ids, rec.Header().Get(v2.FailureIDHeader))
	require.NotEmpty(t, ids[2])
	store.Flush()
	require.NotNil(t, again.TxnGroups[1].FailureMessage)
	require.Equal(t, (*f.Groups)[0].Message, *again.TxnGroups[1].FailureMessage)

	// a request that passes is not kept
	rec = postSimulate(t, router, encodeSimulateRequest(t, failing, "json"), "json")
	require.Equal(t, http.StatusOK, rec.Code)
	require.Empty(t, rec.Header().Get(v2.FailureIDHeader))

	// the listing, newest first, without requests, in pages
	var page model.FailuresResponse
	got := getJSON(t, router, "/v2/failures?limit=2", &page)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.Len(t, page.Failures, 2)
	require.Equal(t, ids[2], page.Failures[0].Id)
	require.Equal(t, ids[1], page.Failures[1].Id)
	require.Nil(t, page.Failures[0].Request)
	require.NotNil(t, page.NextToken)
	var last model.FailuresResponse
	got = getJSON(t, router, "/v2/failures?limit=2&next="+*page.NextToken, &last)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.Len(t, last.Failures, 1)
	require.Equal(t, ids[0], last.Failures[0].Id)
	require.Nil(t, last.NextToken)
	var other model.FailuresResponse
	getJSON(t, router, "/v2/failures?kind=other", &other)
	require.Empty(t, other.Failures)

	require.Equal(t, http.StatusNotFound, getJSON(t, router, "/v2/failures/nope", nil).Code)
	require.Equal(t, http.StatusBadRequest, getJSON(t, router, "/v2/failures?next=x", nil).Code)

	var caps model.CapabilitiesResponse
	getJSON(t, router, "/v2/capabilities", &caps)
	require.NotNil(t, caps.Failures)
	require.EqualValues(t, 86400, caps.Failures.RetentionSeconds)
}

// TestFailuresOff checks that a node that keeps no failures refuses the endpoints and names none.
func TestFailuresOff(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	router := newSimRouter(t, newSimNode(t, env))
	require.Equal(t, http.StatusBadRequest, getJSON(t, router, "/v2/failures", nil).Code)
	require.Equal(t, http.StatusBadRequest, getJSON(t, router, "/v2/failures/x", nil).Code)

	sender := env.Accounts[0]
	bad := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: sender.Addr, Amount: 1 << 62}).Txn().Sign(sender.Sk)
	rec := postSimulate(t, router, encodeSimulateRequest(t, v2.PreEncodedSimulateRequest{
		TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{bad}}}}, "json"), "json")
	require.NotNil(t, decodeSimulateResponse(t, rec, "json").TxnGroups[0].FailureMessage)
	require.Empty(t, rec.Header().Get(v2.FailureIDHeader))

	var caps model.CapabilitiesResponse
	getJSON(t, router, "/v2/capabilities", &caps)
	require.Nil(t, caps.Failures)
}

// TestSimulateRefusalCapture checks that a request the simulator refused (a bad
// signature, 400) is kept with its error and round, and one refused for the
// node's limits, before any simulation, is not.
func TestSimulateRefusalCapture(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender, receiver := env.Accounts[0], env.Accounts[1]
	store, err := failures.Open(filepath.Join(t.TempDir(), "failures.sqlite"), failures.Config{}, logging.TestingLog(t))
	require.NoError(t, err)
	defer store.Close()
	n := &failNode{simNode: newSimNode(t, env, func(c *config.Local) { c.EnableFailureCapture = true }), store: store}
	router := server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000)

	stxn := env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: receiver.Addr}).Txn().Sign(sender.Sk)
	stxn.Sig[0]++
	request := v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{stxn}}}}
	body := encodeSimulateRequest(t, request, "json")
	rec := postSimulate(t, router, body, "json")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	id := rec.Header().Get(v2.FailureIDHeader)
	require.NotEmpty(t, id)
	store.Flush()
	var f model.Failure
	got := getJSON(t, router, "/v2/failures/"+id, &f)
	require.Equal(t, http.StatusOK, got.Code, got.Body.String())
	require.NotNil(t, f.Error)
	require.Nil(t, f.Groups)
	require.Equal(t, env.Ledger.Latest(), f.Round)
	require.Equal(t, body, *f.Request)

	// over the node's extra budget limit: refused before it takes a slot
	request.TxnGroups[0].Txns[0] = env.TxnInfo.NewTxn(txntest.Txn{Type: protocol.PaymentTx, Sender: sender.Addr, Receiver: receiver.Addr}).Txn().Sign(sender.Sk)
	request.ExtraOpcodeBudget = 1 << 40
	rec = postSimulate(t, router, encodeSimulateRequest(t, request, "json"), "json")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Empty(t, rec.Header().Get(v2.FailureIDHeader))
}

// TestFailuresUnavailable checks a node configured to keep failures that has
// no store (a follower, or one whose store didn't open): 503, and capabilities
// leave failures out.
func TestFailuresUnavailable(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	router := newSimRouter(t, newSimNode(t, env, func(c *config.Local) { c.EnableFailureCapture = true }))
	require.Equal(t, http.StatusServiceUnavailable, getJSON(t, router, "/v2/failures", nil).Code)
	require.Equal(t, http.StatusServiceUnavailable, getJSON(t, router, "/v2/failures/x", nil).Code)
	var caps model.CapabilitiesResponse
	getJSON(t, router, "/v2/capabilities", &caps)
	require.Nil(t, caps.Failures)
}
