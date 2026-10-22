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
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
	"golang.org/x/sync/semaphore"

	"github.com/algorand/go-codec/codec"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/daemon/algod/api/server"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// roundsNode wraps mockNode to count simulations, track how many run at once, and inject a
// panic, an error or an invalid request at chosen rounds. Set the injections before simulating.
type roundsNode struct {
	*mockNode
	calls       atomic.Int64
	inFlight    atomic.Int64
	maxInFlight atomic.Int64
	// delay makes every simulation last at least this long, so that they overlap.
	delay time.Duration
	// slowAt makes the simulations of these rounds (0 for a request without rounds) last until
	// their request's context is done.
	slowAt    map[basics.Round]bool
	panicAt   basics.Round
	errAt     map[basics.Round]error
	invalidAt basics.Round
}

func (n *roundsNode) Simulate(request simulation.Request) (simulation.Result, error) {
	n.calls.Add(1)
	now := n.inFlight.Add(1)
	defer n.inFlight.Add(-1)
	for {
		high := n.maxInFlight.Load()
		if now <= high || n.maxInFlight.CompareAndSwap(high, now) {
			break
		}
	}
	// A simulation stops when its request's context is done, as the simulator does.
	ctx := request.Context()
	if n.slowAt[request.Round] {
		<-ctx.Done()
	}
	select {
	case <-ctx.Done():
		return simulation.Result{}, context.Cause(ctx)
	case <-time.After(n.delay):
	}

	if n.panicAt != 0 && request.Round == n.panicAt {
		panic(fmt.Sprintf("injected panic at round %d", request.Round))
	}
	if err := n.errAt[request.Round]; err != nil {
		return simulation.Result{}, err
	}
	if n.invalidAt != 0 && request.Round == n.invalidAt {
		// a genuine InvalidRequestError from the simulator
		request.ExtraOpcodeBudget = simulation.MaxExtraOpcodeBudget + 1
	}
	return n.mockNode.Simulate(request)
}

// newRoundsNode makes a mock node on env's ledger whose status reports the ledger's latest round.
func newRoundsNode(t *testing.T, env simulationtesting.Environment, configure ...func(*config.Local)) *roundsNode {
	t.Helper()
	cfg := config.GetDefaultLocal()
	for _, f := range configure {
		f(&cfg)
	}
	status := cannedStatusReportGolden
	status.LastRound = env.Ledger.Latest()
	return &roundsNode{mockNode: makeMockNodeWithConfig(env.Ledger, t.Name(), nil, status, false, cfg)}
}

// newRoundsRouter serves node's REST API, as algod does.
func newRoundsRouter(t *testing.T, n *roundsNode) *echo.Echo {
	t.Helper()
	return server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000)
}

func encodeSimulateRequest(t *testing.T, request v2.PreEncodedSimulateRequest, format string) []byte {
	t.Helper()
	if format == "json" {
		return []byte(protocol.EncodeJSON(&request))
	}
	return protocol.EncodeReflect(&request)
}

// postSimulate posts body to the simulate endpoint of h, asking for a response in format.
func postSimulate(t *testing.T, h http.Handler, body []byte, format string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v2/transactions/simulate?format="+format, bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decodeSimulateResponse(t *testing.T, rec *httptest.ResponseRecorder, format string) v2.PreEncodedSimulateResponse {
	t.Helper()
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	handle := codec.Handle(protocol.JSONStrictHandle)
	if format == "msgpack" {
		handle = protocol.CodecHandle
	}
	var response v2.PreEncodedSimulateResponse
	require.NoError(t, codec.NewDecoderBytes(rec.Body.Bytes(), handle).Decode(&response))
	return response
}

func simulateOK(t *testing.T, h http.Handler, request v2.PreEncodedSimulateRequest, format string) v2.PreEncodedSimulateResponse {
	t.Helper()
	return decodeSimulateResponse(t, postSimulate(t, h, encodeSimulateRequest(t, request, format), format), format)
}

// requireRoundResultsMatch checks that response has one round result per round, each equal to a
// single-round simulation of request at that round.
func requireRoundResultsMatch(t *testing.T, h http.Handler, request v2.PreEncodedSimulateRequest, response v2.PreEncodedSimulateResponse, rounds ...basics.Round) {
	t.Helper()
	require.Zero(t, response.LastRound)
	require.Empty(t, response.TxnGroups)
	require.NotNil(t, response.RoundResults)
	require.Len(t, *response.RoundResults, len(rounds))
	for i, rnd := range rounds {
		roundResult := (*response.RoundResults)[i]
		require.Nil(t, roundResult.Error, "entry %d", i)
		require.Equal(t, rnd, roundResult.LastRound, "entry %d", i)

		single := request
		single.Rounds = nil
		single.Round = rnd
		expected := simulateOK(t, h, single, "msgpack")
		require.Nil(t, expected.RoundResults)
		require.Equal(t, expected.TxnGroups, roundResult.TxnGroups, "entry %d", i)
		require.Equal(t, expected.InitialStates, roundResult.InitialStates, "entry %d", i)
		require.Equal(t, expected.ProtocolOverride, roundResult.ProtocolOverride, "entry %d", i)
		require.Equal(t, expected.EvalOverrides, response.EvalOverrides)
		require.Equal(t, expected.Version, response.Version)
	}
}

// roundLoggerApp logs the round of the simulated block.
const roundLoggerApp = `#pragma version 8
global Round
itob
log
int 1`

// roundLoggerCall returns a signed app create that logs the round, valid at every round of the test.
func roundLoggerCall(env simulationtesting.Environment, sender simulationtesting.Account, note byte) transactions.SignedTxn {
	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   roundLoggerApp,
		ClearStateProgram: "#pragma version 8\nint 1",
		Note:              []byte{note},
	})
	txn.FirstValid = 0
	txn.LastValid = 1000
	return txn.Txn().Sign(sender.Sk)
}

func oneGroupRequest(stxns ...transactions.SignedTxn) v2.PreEncodedSimulateRequest {
	return v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: stxns}}}
}

func TestSimulateRounds(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	n := newRoundsNode(t, env)
	router := newRoundsRouter(t, n)
	request := oneGroupRequest(roundLoggerCall(env, sender, 1))

	for _, format := range []string{"json", "msgpack"} {
		t.Run(format, func(t *testing.T) {
			// request order is kept
			request := request
			request.Rounds = []basics.Round{latest, latest - 2}
			body := encodeSimulateRequest(t, request, format)
			rec := postSimulate(t, router, body, format)
			if format == "json" {
				// the top-level txn-groups is an empty array, not null
				var raw map[string]json.RawMessage
				require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
				require.Equal(t, "[]", string(raw["txn-groups"]))
			}
			response := decodeSimulateResponse(t, rec, format)
			requireRoundResultsMatch(t, router, request, response, latest, latest-2)
			for i, rnd := range []basics.Round{latest, latest - 2} {
				requireRoundLogged(t, (*response.RoundResults)[i].TxnGroups[0], rnd)
			}
		})
	}

	t.Run("0 is the latest round, and a round is simulated once", func(t *testing.T) {
		request := request
		request.Rounds = []basics.Round{latest - 1, latest - 1, 0, latest}
		before := n.calls.Load()
		response := simulateOK(t, router, request, "msgpack")
		require.Equal(t, int64(2), n.calls.Load()-before)
		requireRoundResultsMatch(t, router, request, response, latest-1, latest-1, latest, latest)
		require.Equal(t, (*response.RoundResults)[2], (*response.RoundResults)[3])
	})

	t.Run("explicit round 0", func(t *testing.T) {
		request := request
		request.Rounds = []basics.Round{latest}
		body := encodeSimulateRequest(t, request, "json")
		require.True(t, bytes.HasPrefix(body, []byte("{")))
		body = append([]byte(`{"round":0,`), body[1:]...)
		response := decodeSimulateResponse(t, postSimulate(t, router, body, "json"), "json")
		requireRoundResultsMatch(t, router, request, response, latest)
	})

	t.Run("empty rounds is a single-round simulate", func(t *testing.T) {
		body := encodeSimulateRequest(t, request, "json")
		body = append([]byte(`{"rounds":[],`), body[1:]...)
		rec := postSimulate(t, router, body, "json")
		require.NotContains(t, rec.Body.String(), "round-results")
		response := decodeSimulateResponse(t, rec, "json")
		require.Nil(t, response.RoundResults)
		require.Equal(t, latest, response.LastRound)
		require.Len(t, response.TxnGroups, 1)
	})
}

// requireRoundLogged checks that the round logger app in group logged the round after rnd.
func requireRoundLogged(t *testing.T, group v2.PreEncodedSimulateTxnGroupResult, rnd basics.Round) {
	t.Helper()
	require.Nil(t, group.FailureMessage)
	logs := group.Txns[0].Txn.Logs
	require.NotNil(t, logs)
	require.Equal(t, [][]byte{binary.BigEndian.AppendUint64(nil, uint64(rnd+1))}, *logs)
}

// TestSimulateRoundsGroups checks rounds with several groups, sequential and parallel: a group that
// fails at one round (its sender is funded only later) does not affect the other round.
func TestSimulateRoundsGroups(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	a, b := env.Accounts[0], env.Accounts[1]
	fresh, freshSk := newRoundsTestAccount(7)
	env.TransferAlgos(a.Addr, b.Addr, 0)
	before := env.Ledger.Latest()
	env.TransferAlgos(a.Addr, fresh, 1_000_000)
	funded := env.Ledger.Latest()

	pay := func(sender basics.Address, receiver basics.Address, amount uint64) txntest.Txn {
		txn := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender,
			Receiver: receiver,
			Amount:   amount,
		})
		txn.FirstValid = 0
		txn.LastValid = 1000
		return txn
	}
	spend := pay(fresh, a.Addr, 1000)
	other := pay(a.Addr, b.Addr, 1)
	request := v2.PreEncodedSimulateRequest{
		TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{
			{Txns: []transactions.SignedTxn{spend.Txn().Sign(freshSk)}},
			{Txns: []transactions.SignedTxn{other.Txn().Sign(a.Sk)}},
		},
		Rounds: []basics.Round{before, funded},
	}

	n := newRoundsNode(t, env)
	router := newRoundsRouter(t, n)
	for _, parallel := range []bool{false, true} {
		t.Run(fmt.Sprintf("parallel=%t", parallel), func(t *testing.T) {
			request := request
			request.ParallelGroups = parallel
			response := simulateOK(t, router, request, "msgpack")
			requireRoundResultsMatch(t, router, request, response, before, funded)
			if parallel {
				require.NotNil(t, response.EvalOverrides)
				require.Equal(t, &parallel, response.EvalOverrides.ParallelGroups)
			} else {
				require.Nil(t, response.EvalOverrides)
			}

			results := *response.RoundResults
			require.Len(t, results[0].TxnGroups, 2)
			require.NotNil(t, results[0].TxnGroups[0].FailureMessage)
			require.Contains(t, *results[0].TxnGroups[0].FailureMessage, "overspend")
			require.Nil(t, results[0].TxnGroups[1].FailureMessage)
			require.Len(t, results[1].TxnGroups, 2)
			for g, group := range results[1].TxnGroups {
				require.Nil(t, group.FailureMessage, "group %d", g)
			}
		})
	}
}

// windowRoundLoggerApp logs txn FirstValid and the round of the simulated block.
const windowRoundLoggerApp = `#pragma version 8
txn FirstValid
itob
log
global Round
itob
log
int 1`

// TestSimulateRoundsValidityWindow simulates one txn at rounds more than MaxTxnLife apart, which
// only the validity window modes allow, and checks the requests they make invalid.
func TestSimulateRoundsValidityWindow(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t, func(cfg *config.Local) {
		cfg.Archival = false
		cfg.EnableTimeTravel = true
	})
	defer env.Close()
	sender := env.Accounts[0]
	proto := env.TxnInfo.CurrentProtocolParams()
	for range proto.MaxTxnLife + 200 {
		hdr := env.TxnInfo.LatestHeader
		blk := bookkeeping.MakeBlock(hdr)
		blk.TxnCounter = hdr.TxnCounter
		require.NoError(t, env.Ledger.AddBlock(blk, agreement.Certificate{}))
		env.TxnInfo.LatestHeader = blk.BlockHeader
	}
	latest := env.Ledger.Latest()
	old := latest - basics.Round(proto.MaxTxnLife) - 100
	rounds := []basics.Round{latest, old}

	txn := env.TxnInfo.NewTxn(txntest.Txn{
		Type:              protocol.ApplicationCallTx,
		Sender:            sender.Addr,
		ApprovalProgram:   windowRoundLoggerApp,
		ClearStateProgram: "#pragma version 8\nint 1",
	})
	txn.FirstValid, txn.LastValid = latest-5, latest+5
	request := oneGroupRequest(txn.Txn().Sign(sender.Sk))
	request.Rounds = rounds

	n := newRoundsNode(t, env)
	router := newRoundsRouter(t, n)
	// logged returns the first valid and the block round that round result i logged.
	logged := func(t *testing.T, response v2.PreEncodedSimulateResponse, i int) (basics.Round, basics.Round) {
		t.Helper()
		roundResult := (*response.RoundResults)[i]
		require.Nil(t, roundResult.Error)
		group := roundResult.TxnGroups[0]
		require.Nil(t, group.FailureMessage, "entry %d", i)
		logs := *group.Txns[0].Txn.Logs
		require.Len(t, logs, 2)
		return basics.Round(binary.BigEndian.Uint64(logs[0])), basics.Round(binary.BigEndian.Uint64(logs[1]))
	}

	t.Run("no mode", func(t *testing.T) {
		response := simulateOK(t, router, request, "msgpack")
		results := *response.RoundResults
		require.Nil(t, results[0].TxnGroups[0].FailureMessage)
		require.NotNil(t, results[1].TxnGroups[0].FailureMessage)
		require.Contains(t, *results[1].TxnGroups[0].FailureMessage, "txn dead")
		require.Nil(t, response.EvalOverrides)
	})

	t.Run("skip", func(t *testing.T) {
		request := request
		request.SkipValidityWindow = true
		response := simulateOK(t, router, request, "json")
		for i, rnd := range rounds {
			fv, blockRound := logged(t, response, i)
			require.Equal(t, latest-5, fv)
			require.Equal(t, rnd+1, blockRound)
		}
		require.NotNil(t, response.EvalOverrides)
		require.Equal(t, &request.SkipValidityWindow, response.EvalOverrides.SkipValidityWindow)
		require.Nil(t, response.EvalOverrides.RewriteValidityWindow)
	})

	t.Run("rewrite", func(t *testing.T) {
		request := request
		request.RewriteValidityWindow = true
		request.AllowEmptySignatures = true
		response := simulateOK(t, router, request, "msgpack")
		for i, rnd := range rounds {
			fv, blockRound := logged(t, response, i)
			require.Equal(t, rnd+1, fv)
			require.Equal(t, rnd+1, blockRound)
			echoed := (*response.RoundResults)[i].TxnGroups[0].Txns[0].Txn.Txn
			require.Equal(t, rnd+1, echoed.Txn.FirstValid)
			require.Equal(t, rnd+11, echoed.Txn.LastValid)
			require.True(t, echoed.Sig.Blank())
		}
		require.NotNil(t, response.EvalOverrides)
		require.Equal(t, &request.RewriteValidityWindow, response.EvalOverrides.RewriteValidityWindow)
		require.Nil(t, response.EvalOverrides.SkipValidityWindow)
	})

	for _, tc := range []struct {
		name    string
		request v2.PreEncodedSimulateRequest
		err     string
	}{
		{"skip without rounds", v2.PreEncodedSimulateRequest{SkipValidityWindow: true}, "skip-validity-window and rewrite-validity-window require rounds"},
		{"rewrite without rounds", v2.PreEncodedSimulateRequest{RewriteValidityWindow: true, AllowEmptySignatures: true}, "skip-validity-window and rewrite-validity-window require rounds"},
		{"both modes", v2.PreEncodedSimulateRequest{Rounds: rounds, SkipValidityWindow: true, RewriteValidityWindow: true, AllowEmptySignatures: true}, "skip-validity-window and rewrite-validity-window are mutually exclusive"},
		{"rewrite without empty signatures", v2.PreEncodedSimulateRequest{Rounds: rounds, RewriteValidityWindow: true}, "rewrite-validity-window requires allow-empty-signatures"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := newRoundsNode(t, env)
			r := tc.request
			r.TxnGroups = request.TxnGroups
			rec := postSimulate(t, newRoundsRouter(t, n), encodeSimulateRequest(t, r, "json"), "json")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.err)
			require.Zero(t, n.calls.Load())
		})
	}
}

func newRoundsTestAccount(seed byte) (basics.Address, *crypto.SignatureSecrets) {
	var s crypto.Seed
	s[0] = 0x5e
	s[1] = seed
	secrets := crypto.GenerateSignatureSecrets(s)
	return basics.Address(secrets.SignatureVerifier), secrets
}

// TestSimulateRoundsErrors checks that a round that can't be simulated is reported in its entry,
// whatever Node.Simulate returns for it, while the other rounds succeed.
func TestSimulateRoundsErrors(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	n := newRoundsNode(t, env)
	// the error the ledger returns for a round it no longer has
	n.errAt = map[basics.Round]error{latest - 2: fmt.Errorf("round %d before dbRound %d", latest-2, latest)}
	n.invalidAt = latest - 1
	router := newRoundsRouter(t, n)
	request := oneGroupRequest(roundLoggerCall(env, sender, 1))

	request.Rounds = []basics.Round{latest - 2, latest - 1, latest, latest + 1}
	response := simulateOK(t, router, request, "json")
	require.Zero(t, response.LastRound)
	require.Empty(t, response.TxnGroups)
	results := *response.RoundResults
	require.Len(t, results, 4)
	for i, rnd := range request.Rounds {
		require.Equal(t, rnd, results[i].LastRound, "entry %d", i)
		if rnd == latest {
			require.Nil(t, results[i].Error)
			requireRoundLogged(t, results[i].TxnGroups[0], latest)
			continue
		}
		require.NotNil(t, results[i].Error, "entry %d", i)
		require.Empty(t, results[i].TxnGroups, "entry %d", i)
		require.Nil(t, results[i].InitialStates, "entry %d", i)
	}
	require.Equal(t, fmt.Sprintf("round %d before dbRound %d", latest-2, latest), *results[0].Error)
	// an InvalidRequestError, a 400 for a single-round request, is that entry's error
	require.Contains(t, *results[1].Error, "extra budget")
	// a round the node doesn't have yet
	require.Contains(t, *results[3].Error, fmt.Sprintf("ledger does not have entry %d", latest+1))

	t.Run("no round simulated", func(t *testing.T) {
		request := request
		request.Rounds = []basics.Round{latest + 1, latest + 2}
		request.AllowMoreLogging = true
		response := simulateOK(t, router, request, "json")
		require.Equal(t, simulation.ResultLatestVersion, response.Version)
		// the overrides a single-round result reports
		single := request
		single.Rounds = nil
		require.Equal(t, simulateOK(t, router, single, "json").EvalOverrides, response.EvalOverrides)
		require.NotNil(t, response.EvalOverrides)
		for _, result := range *response.RoundResults {
			require.NotNil(t, result.Error)
		}
	})
}

// TestSimulateRoundsUnavailable checks, on a real ledger, that a round older than the ledger's
// in-memory history is reported in its entry. It waits for a tracker flush, which takes seconds.
func TestSimulateRoundsUnavailable(t *testing.T) {
	partitiontest.PartitionTest(t)
	if testing.Short() {
		t.Skip("waits for a tracker flush")
	}
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	// round 0 would mean the latest round
	env.TransferAlgos(sender.Addr, sender.Addr, 0)
	start := env.Ledger.Latest()
	for range 2 * env.Config.MaxAcctLookback {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	n := newRoundsNode(t, env)
	router := newRoundsRouter(t, n)
	request := oneGroupRequest(roundLoggerCall(env, sender, 1))

	// The start round leaves the ledger's in-memory history once the trackers flush, which they do
	// on a new block at most every few seconds.
	single := request
	single.Round = start
	for deadline := time.Now().Add(30 * time.Second); ; {
		if postSimulate(t, router, encodeSimulateRequest(t, single, "msgpack"), "msgpack").Code != http.StatusOK {
			break
		}
		require.True(t, time.Now().Before(deadline), "round %d is still available", start)
		time.Sleep(100 * time.Millisecond)
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	n.status.LastRound = latest

	request.Rounds = []basics.Round{start, latest}
	results := *simulateOK(t, router, request, "json").RoundResults
	require.Len(t, results, 2)
	require.Equal(t, start, results[0].LastRound)
	require.NotNil(t, results[0].Error)
	require.Contains(t, *results[0].Error, "dbRound")
	require.Empty(t, results[0].TxnGroups)
	require.Nil(t, results[1].Error)
	requireRoundLogged(t, results[1].TxnGroups[0], latest)
}

// TestSimulateRoundsBadRequest checks the problems that fail a whole rounds request.
func TestSimulateRoundsBadRequest(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	stxn := roundLoggerCall(env, sender, 1)

	// duplicates count against the limit
	many := make([]basics.Round, config.GetDefaultLocal().MaxAPISimulateRounds+1)
	for i := range many {
		many[i] = latest
	}
	for _, tc := range []struct {
		name      string
		configure func(*config.Local)
		request   v2.PreEncodedSimulateRequest
		err       string
	}{
		{"round and rounds", nil, v2.PreEncodedSimulateRequest{Round: latest}, "round and rounds cannot both be set"},
		{"rounds disabled", func(c *config.Local) { c.MaxAPISimulateRounds = 0 }, v2.PreEncodedSimulateRequest{}, "rounds is disabled on this node (MaxAPISimulateRounds is 0)"},
		{"too many rounds", func(c *config.Local) { c.MaxAPISimulateRounds = 2 }, v2.PreEncodedSimulateRequest{Rounds: []basics.Round{latest, latest - 1, latest - 2}}, "too many rounds: got 3, the limit is 2"},
		{"too many duplicate rounds", nil, v2.PreEncodedSimulateRequest{Rounds: many}, fmt.Sprintf("too many rounds: got %d, the limit is %d", len(many), len(many)-1)},
		{"one round listed twice over a limit of 1", func(c *config.Local) { c.MaxAPISimulateRounds = 1 }, v2.PreEncodedSimulateRequest{Rounds: []basics.Round{latest, 0}}, "too many rounds: got 2, the limit is 1"},
		{"fix signers", nil, v2.PreEncodedSimulateRequest{FixSigners: true}, "FixSigners requires AllowEmptySignatures to be enabled"},
		{"trace without developer API", nil, v2.PreEncodedSimulateRequest{ExecTraceConfig: simulation.ExecTraceConfig{Enable: true}}, "the local configuration of the node has `EnableDeveloperAPI` turned off, while requesting for execution trace"},
		{"extra budget", nil, v2.PreEncodedSimulateRequest{ExtraOpcodeBudget: simulation.MaxExtraOpcodeBudget + 1}, "extra budget 320001 \\u003e simulation extra budget limit 320000"},
		{"extra budget over config limit", func(c *config.Local) { c.MaxAPISimulateExtraOpcodeBudget = 100 }, v2.PreEncodedSimulateRequest{ExtraOpcodeBudget: 101}, "extra budget 101 \\u003e simulation extra budget limit 100"},
		{"no groups", nil, v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{}}, "expected at least 1 transaction group, got 0"},
		{"too many groups", func(c *config.Local) { c.MaxAPISimulateGroups = 1 }, v2.PreEncodedSimulateRequest{TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{stxn}}, {Txns: []transactions.SignedTxn{stxn}}}}, "too many transaction groups: got 2, the limit is 1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var configure []func(*config.Local)
			if tc.configure != nil {
				configure = append(configure, tc.configure)
			}
			n := newRoundsNode(t, env, configure...)
			request := tc.request
			if request.TxnGroups == nil {
				request.TxnGroups = []v2.PreEncodedSimulateRequestTransactionGroup{{Txns: []transactions.SignedTxn{stxn}}}
			}
			if request.Rounds == nil {
				request.Rounds = []basics.Round{latest}
			}
			rec := postSimulate(t, newRoundsRouter(t, n), encodeSimulateRequest(t, request, "json"), "json")
			require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
			require.Contains(t, rec.Body.String(), tc.err)
			require.Zero(t, n.calls.Load())
		})
	}

	// A bad format fails the request before any round is simulated, as it does a single-round one.
	for _, rounds := range [][]basics.Round{nil, {latest, latest - 1}} {
		n := newRoundsNode(t, env)
		request := oneGroupRequest(stxn)
		request.Rounds = rounds
		rec := postSimulate(t, newRoundsRouter(t, n), encodeSimulateRequest(t, request, "msgpack"), "bogus")
		require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
		require.Contains(t, rec.Body.String(), "failed to parse the format option")
		require.Zero(t, n.calls.Load())
	}
}

// simulateRoundsDirect calls the handler directly, with the request context ctx.
func simulateRoundsDirect(ctx context.Context, t *testing.T, handler *v2.Handlers, request v2.PreEncodedSimulateRequest) (*httptest.ResponseRecorder, error) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(protocol.EncodeReflect(&request))).WithContext(ctx)
	rec := httptest.NewRecorder()
	err := handler.SimulateTransaction(echo.New().NewContext(req, rec), model.SimulateTransactionParams{})
	return rec, err
}

// TestSimulateRoundsLimiter checks cancellation, panics and the node-wide limiter.
func TestSimulateRoundsLimiter(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	// the subtests run in parallel, after this function returns
	t.Cleanup(env.Close)
	sender := env.Accounts[0]
	for range 3 {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	request := oneGroupRequest(roundLoggerCall(env, sender, 1))
	request.Rounds = []basics.Round{latest - 2, latest - 1, latest}

	const slots = 4
	newHandler := func(n *roundsNode) (*v2.Handlers, *semaphore.Weighted) {
		limiter := semaphore.NewWeighted(slots)
		return &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{}), SimulateLimiter: limiter}, limiter
	}

	t.Run("canceled request", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env)
		handler, limiter := newHandler(n)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		rec, err := simulateRoundsDirect(ctx, t, handler, request)
		require.NoError(t, err)
		require.Zero(t, rec.Body.Len())
		require.False(t, rec.Flushed)
		require.Zero(t, n.calls.Load())
		require.True(t, limiter.TryAcquire(slots))
	})

	t.Run("canceled while every slot is held", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env)
		handler, limiter := newHandler(n)
		require.True(t, limiter.TryAcquire(slots))
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() {
			rec, err := simulateRoundsDirect(ctx, t, handler, request)
			if err == nil && rec.Body.Len() != 0 {
				err = errors.New("unexpected response body")
			}
			done <- err
		}()
		select {
		case err := <-done:
			t.Fatalf("returned before the cancel: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		cancel()
		select {
		case err := <-done:
			require.NoError(t, err)
		case <-time.After(5 * time.Second):
			t.Fatal("canceled request did not return")
		}
		require.Zero(t, n.calls.Load())
		limiter.Release(slots)
	})

	t.Run("panic", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env)
		n.panicAt = latest - 1
		handler, limiter := newHandler(n)
		rec, err := simulateRoundsDirect(context.Background(), t, handler, request)
		require.NoError(t, err)
		response := decodeSimulateResponse(t, rec, "json")
		results := *response.RoundResults
		require.Len(t, results, 3)
		require.Nil(t, results[0].Error)
		require.NotNil(t, results[1].Error)
		require.Contains(t, *results[1].Error, fmt.Sprintf("simulating round %d: panic: injected panic at round %d", latest-1, latest-1))
		require.Equal(t, latest-1, results[1].LastRound)
		require.Nil(t, results[2].Error)
		// every slot was released
		require.True(t, limiter.TryAcquire(slots))
		limiter.Release(slots)

		// a following request gets every slot
		n.panicAt = 0
		rec, err = simulateRoundsDirect(context.Background(), t, handler, request)
		require.NoError(t, err)
		for _, result := range *decodeSimulateResponse(t, rec, "json").RoundResults {
			require.Nil(t, result.Error)
		}
		require.True(t, limiter.TryAcquire(slots))
	})

	t.Run("no limiter", func(t *testing.T) {
		t.Parallel()
		n := newRoundsNode(t, env)
		handler := &v2.Handlers{Node: n, Log: logging.TestingLog(t), Shutdown: make(chan struct{})}
		rec, err := simulateRoundsDirect(context.Background(), t, handler, request)
		require.NoError(t, err)
		for _, result := range *decodeSimulateResponse(t, rec, "json").RoundResults {
			require.Nil(t, result.Error)
		}
	})

	t.Run("parallelism does not change the response", func(t *testing.T) {
		t.Parallel()
		var bodies []string
		for _, parallelism := range []int{1, 4, 0} {
			n := newRoundsNode(t, env, func(c *config.Local) { c.SimulateRoundsParallelism = parallelism })
			rec := postSimulate(t, newRoundsRouter(t, n), encodeSimulateRequest(t, request, "json"), "json")
			require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
			bodies = append(bodies, rec.Body.String())
		}
		require.Equal(t, bodies[0], bodies[1])
		require.Equal(t, bodies[0], bodies[2])
	})
}

// TestSimulateRoundsResourceLimitMultiplier checks that the config multiplier applies at every round.
func TestSimulateRoundsResourceLimitMultiplier(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	defer env.Close()
	sender := env.Accounts[0]
	env.TransferAlgos(sender.Addr, sender.Addr, 0)
	latest := env.Ledger.Latest()
	proto := env.TxnInfo.CurrentProtocolParams()

	// One more account than one app call may reference. The group is filled up to MaxTxGroupSize
	// with payments, so the app call's own limits are the group's limits.
	accounts := make([]basics.Address, proto.MaxAppTotalTxnReferences+1)
	args := make([][]byte, len(accounts))
	for i := range accounts {
		accounts[i] = basics.Address{0xbb, byte(i + 1)}
		args[i] = accounts[i][:]
	}
	validity := func(txn txntest.Txn) *txntest.Txn {
		txn.FirstValid = 0
		txn.LastValid = 1000
		return &txn
	}
	txns := []*txntest.Txn{validity(env.TxnInfo.NewTxn(txntest.Txn{
		Type:   protocol.ApplicationCallTx,
		Sender: sender.Addr,
		ApprovalProgram: `#pragma version 10
int 0
store 0
loop:
load 0
txn NumAppArgs
<
bz end
load 0
txnas ApplicationArgs
acct_params_get AcctBalance
pop
pop
load 0
int 1
+
store 0
b loop
end:
int 1`,
		ClearStateProgram: "#pragma version 10\nint 1",
		ApplicationArgs:   args,
	}))}
	for i := 1; i < proto.MaxTxGroupSize; i++ {
		txns = append(txns, validity(env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: sender.Addr,
			Note:     []byte{byte(i)},
		})))
	}
	txntest.Group(txns...)
	stxns := make([]transactions.SignedTxn, len(txns))
	for i, txn := range txns {
		stxns[i] = txn.Txn().Sign(sender.Sk)
	}
	request := oneGroupRequest(stxns...)
	request.AllowUnnamedResources = true
	request.Rounds = []basics.Round{latest - 1, latest}

	for _, m := range []uint64{1, 2} {
		n := newRoundsNode(t, env, func(c *config.Local) { c.SimulateResourceLimitMultiplier = m })
		response := simulateOK(t, newRoundsRouter(t, n), request, "msgpack")
		for i, result := range *response.RoundResults {
			require.Nil(t, result.Error, "multiplier %d, entry %d", m, i)
			group := result.TxnGroups[0]
			if m == 1 {
				require.NotNil(t, group.FailureMessage, "entry %d", i)
				require.Contains(t, *group.FailureMessage, fmt.Sprintf("unavailable Account %s", accounts[len(accounts)-1]))
				continue
			}
			require.Nil(t, group.FailureMessage, "entry %d", i)
			require.NotNil(t, group.UnnamedResourcesAccessed)
			require.Len(t, *group.UnnamedResourcesAccessed.Accounts, len(accounts))
		}
	}
}

// TestSimulateRoundsConcurrent runs several rounds requests with parallel groups at once, with more
// rounds than CPUs, while the ledger moves on.
func TestSimulateRoundsConcurrent(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	numRounds := min(runtime.NumCPU()+1, simulation.MaxSimulateRounds)
	env := simulationtesting.PrepareSimulatorTest(t, func(c *config.Local) {
		// keep every round of the test in memory
		c.MaxAcctLookback = uint64(numRounds + 8)
	})
	defer env.Close()
	sender := env.Accounts[0]
	for range numRounds {
		env.TransferAlgos(sender.Addr, sender.Addr, 0)
	}
	latest := env.Ledger.Latest()
	rounds := make([]basics.Round, numRounds)
	for i := range rounds {
		rounds[i] = latest - basics.Round(i)
	}
	request := v2.PreEncodedSimulateRequest{
		TxnGroups: []v2.PreEncodedSimulateRequestTransactionGroup{
			{Txns: []transactions.SignedTxn{roundLoggerCall(env, sender, 1)}},
			{Txns: []transactions.SignedTxn{roundLoggerCall(env, env.Accounts[1], 2)}},
		},
		ParallelGroups: true,
		Rounds:         rounds,
	}
	// Each request would run 2 rounds at once, but the node runs at most 3 simulations.
	const parallelism = 3
	n := newRoundsNode(t, env, func(c *config.Local) {
		c.MaxAPISimulateRounds = uint64(numRounds)
		c.SimulateRoundsParallelism = 2
		c.SimulateParallelism = parallelism
	})
	n.delay = 5 * time.Millisecond
	router := newRoundsRouter(t, n)
	body := encodeSimulateRequest(t, request, "msgpack")

	done := make(chan struct{})
	go func() {
		defer close(done)
		for range 3 {
			env.TransferAlgos(sender.Addr, sender.Addr, 0)
		}
	}()
	const requests = 3
	recs := make([]*httptest.ResponseRecorder, requests)
	var wg sync.WaitGroup
	for i := range recs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recs[i] = postSimulate(t, router, body, "msgpack")
		}()
	}
	wg.Wait()
	<-done

	for _, rec := range recs {
		response := decodeSimulateResponse(t, rec, "msgpack")
		require.Equal(t, recs[0].Body.Bytes(), rec.Body.Bytes())
		results := *response.RoundResults
		require.Len(t, results, numRounds)
		for i, result := range results {
			require.Nil(t, result.Error, "entry %d", i)
			require.Equal(t, rounds[i], result.LastRound)
			require.Len(t, result.TxnGroups, 2)
			for g, group := range result.TxnGroups {
				require.Nil(t, group.FailureMessage, "entry %d, group %d", i, g)
				requireRoundLogged(t, group, rounds[i])
			}
		}
	}
	require.True(t, strings.HasPrefix(recs[0].Header().Get("Content-Type"), "application/msgpack"))
	// The router's limiter is shared by all requests: their rounds never ran more than
	// SimulateParallelism at once.
	require.LessOrEqual(t, n.maxInFlight.Load(), int64(parallelism))
	require.GreaterOrEqual(t, n.maxInFlight.Load(), int64(2))
	require.Equal(t, int64(requests*numRounds), n.calls.Load())
}
