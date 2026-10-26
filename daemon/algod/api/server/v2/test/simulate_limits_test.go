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
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/daemon/algod/api/server"
	v2 "github.com/algorand/go-algorand/daemon/algod/api/server/v2"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/ledger/simulation"
	simulationtesting "github.com/algorand/go-algorand/ledger/simulation/testing"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// getLimits asks h for GET /v2/transactions/simulate/limits, as a client would.
func getLimits(t *testing.T, h http.Handler) model.SimulateLimitsResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v2/transactions/simulate/limits", nil))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var limits model.SimulateLimitsResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &limits))
	return limits
}

// getSimulateLimits asks a node with cfg for its simulate limits.
func getSimulateLimits(t *testing.T, cfg config.Local) model.SimulateLimitsResponse {
	t.Helper()
	n := makeMockNodeWithConfig(nil, t.Name(), nil, cannedStatusReportGolden, false, cfg)
	return getLimits(t, server.NewRouter(logging.TestingLog(t), n, make(chan struct{}), "", "", nil, 1000))
}

func TestSimulateLimits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	defaultQuery := &model.QueryLimits{MaxCalls: 1024, MaxCallBudget: 320_000, MaxCallLogBytes: 64 << 10, MaxResponseBytes: 64 << 20, MaxRounds: 16}
	for _, tc := range []struct {
		name      string
		configure func(*config.Local)
		want      model.SimulateLimitsResponse
	}{
		{"defaults", func(*config.Local) {}, model.SimulateLimitsResponse{
			MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 320_000, ResourceLimitMultiplier: 1,
			TimeLimitSeconds: 108, Query: defaultQuery,
		}},
		{"query disabled", func(c *config.Local) {
			c.MaxAPIQueryCalls = 0
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 320_000, ResourceLimitMultiplier: 1,
			TimeLimitSeconds: 108,
		}},
		{"query floors and ceilings", func(c *config.Local) {
			c.MaxAPIQueryCalls = 3
			c.MaxAPIQueryCallBudget = 0
			c.MaxAPIQueryCallLogBytes = 0
			c.MaxAPIQueryResponseBytes = 0
			c.MaxAPIQueryRounds = 1_000_000
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 320_000, ResourceLimitMultiplier: 1,
			TimeLimitSeconds: 108,
			Query:            &model.QueryLimits{MaxCalls: 3, MaxCallBudget: 1, MaxRounds: simulation.MaxQueryRounds},
		}},
		{"below the floors", func(c *config.Local) {
			c.MaxAPISimulateGroups = 0
			c.MaxAPISimulateRounds = 0
			c.MaxAPISimulateExtraOpcodeBudget = 0
			c.SimulateResourceLimitMultiplier = 0
			c.RestWriteTimeoutSeconds = 0
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: 1, MaxRounds: 0, MaxExtraOpcodeBudget: 320_000, ResourceLimitMultiplier: 1, Query: defaultQuery,
		}},
		{"above the ceilings", func(c *config.Local) {
			c.MaxAPISimulateGroups = 1_000_000
			c.MaxAPISimulateRounds = 1_000_000
			c.MaxAPISimulateExtraOpcodeBudget = 1 << 40
			c.SimulateResourceLimitMultiplier = 1_000_000
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: simulation.MaxSimulateGroups, MaxRounds: simulation.MaxSimulateRounds,
			MaxExtraOpcodeBudget: simulation.MaxExtraOpcodeBudgetCeiling, ResourceLimitMultiplier: simulation.MaxResourceLimitMultiplier,
			TimeLimitSeconds: 108, Query: defaultQuery,
		}},
		{"set values and features", func(c *config.Local) {
			c.MaxAPISimulateGroups = 3
			c.MaxAPISimulateRounds = 5
			c.MaxAPISimulateExtraOpcodeBudget = 1_000
			c.SimulateResourceLimitMultiplier = 8
			c.EnableTimeTravel = true
			c.EnableDeveloperAPI = true
			c.RestWriteTimeoutSeconds = 65
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: 3, MaxRounds: 5, MaxExtraOpcodeBudget: 1_000, ResourceLimitMultiplier: 8,
			TimeLimitSeconds: 58,
			TimeTravel:       true, DeveloperApi: true,
			Query: defaultQuery,
		}},
		{"short write timeout", func(c *config.Local) {
			c.RestWriteTimeoutSeconds = 1
		}, model.SimulateLimitsResponse{
			MaxTxnGroups: 16, MaxRounds: 16, MaxExtraOpcodeBudget: 320_000, ResourceLimitMultiplier: 1,
			TimeLimitSeconds: 1,
			Query:            defaultQuery,
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := config.GetDefaultLocal()
			tc.configure(&cfg)
			require.Equal(t, tc.want, getSimulateLimits(t, cfg))
		})
	}
}

// limitsNode serves a node on env with cfg changed by configure, and returns its router and the
// limits it reports.
func limitsNode(t *testing.T, env simulationtesting.Environment, configure func(*config.Local)) (http.Handler, model.SimulateLimitsResponse) {
	t.Helper()
	h := newRoundsRouter(t, newRoundsNode(t, env, configure))
	return h, getLimits(t, h)
}

// requireSimulated posts request to h and checks that it was simulated, returning the response.
func requireSimulated(t *testing.T, h http.Handler, request v2.PreEncodedSimulateRequest) v2.PreEncodedSimulateResponse {
	t.Helper()
	return simulateOK(t, h, request, "msgpack")
}

// requireRejected posts request to h and checks that it gets a 400 containing msg.
func requireRejected(t *testing.T, h http.Handler, request v2.PreEncodedSimulateRequest, msg string) {
	t.Helper()
	rec := postSimulate(t, h, encodeSimulateRequest(t, request, "msgpack"), "msgpack")
	require.Equal(t, http.StatusBadRequest, rec.Code, rec.Body.String())
	require.Contains(t, rec.Body.String(), msg)
}

// failure returns group's failure message, or "" when it succeeded.
func failure(group v2.PreEncodedSimulateTxnGroupResult) string {
	if group.FailureMessage == nil {
		return ""
	}
	return *group.FailureMessage
}

// TestSimulateLimitsEnforced checks that the limits GET /v2/transactions/simulate/limits reports
// are the ones POST /v2/transactions/simulate enforces: a request at a reported limit is
// simulated, and one past it is not.
func TestSimulateLimitsEnforced(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	env := simulationtesting.PrepareSimulatorTest(t)
	// The subtests run after this function returns.
	t.Cleanup(env.Close)
	sender := env.Accounts[0]
	latest := env.Ledger.Latest()

	pay := func(note uint64) *txntest.Txn {
		txn := env.TxnInfo.NewTxn(txntest.Txn{
			Type:     protocol.PaymentTx,
			Sender:   sender.Addr,
			Receiver: sender.Addr,
			Note:     binary.BigEndian.AppendUint64(nil, note),
		})
		txn.FirstValid = 0
		txn.LastValid = 1000
		return &txn
	}
	// groups returns a request of n groups of one payment each.
	groups := func(n uint64) v2.PreEncodedSimulateRequest {
		var request v2.PreEncodedSimulateRequest
		for i := range n {
			request.TxnGroups = append(request.TxnGroups, v2.PreEncodedSimulateRequestTransactionGroup{
				Txns: []transactions.SignedTxn{pay(i).Txn().Sign(sender.Sk)},
			})
		}
		return request
	}

	t.Run("groups", func(t *testing.T) {
		t.Parallel()
		for _, m := range []uint64{0, 1, 16, 1_000} {
			h, limits := limitsNode(t, env, func(c *config.Local) { c.MaxAPISimulateGroups = m })
			requireSimulated(t, h, groups(limits.MaxTxnGroups))
			requireRejected(t, h, groups(limits.MaxTxnGroups+1), "too many transaction groups")
		}
	})

	t.Run("extra opcode budget", func(t *testing.T) {
		t.Parallel()
		for _, m := range []uint64{0, 1, 320_000, 1 << 40} {
			h, limits := limitsNode(t, env, func(c *config.Local) { c.MaxAPISimulateExtraOpcodeBudget = m })
			request := groups(1)
			request.ExtraOpcodeBudget = int(limits.MaxExtraOpcodeBudget)
			requireSimulated(t, h, request)
			request.ExtraOpcodeBudget++
			requireRejected(t, h, request, "simulation extra budget limit")
		}
	})

	t.Run("rounds", func(t *testing.T) {
		t.Parallel()
		withRounds := func(n uint64) v2.PreEncodedSimulateRequest {
			request := groups(1)
			// Duplicates count against the limit.
			for range n {
				request.Rounds = append(request.Rounds, latest)
			}
			return request
		}
		for _, m := range []uint64{0, 1, 16, 1_000} {
			h, limits := limitsNode(t, env, func(c *config.Local) { c.MaxAPISimulateRounds = m })
			if limits.MaxRounds == 0 {
				requireRejected(t, h, withRounds(1), "rounds is disabled")
				continue
			}
			response := requireSimulated(t, h, withRounds(limits.MaxRounds))
			require.Len(t, *response.RoundResults, int(limits.MaxRounds))
			requireRejected(t, h, withRounds(limits.MaxRounds+1), "too many rounds")
		}
	})

	t.Run("resource limit multiplier", func(t *testing.T) {
		t.Parallel()
		proto := env.TxnInfo.CurrentProtocolParams()
		// In a full group, an app call that lists no resources may reach MaxAppTotalTxnReferences
		// unnamed accounts times the multiplier: its account limit is no lower.
		require.LessOrEqual(t, proto.MaxAppTotalTxnReferences, proto.MaxAppTxnAccounts+proto.MaxAppTxnForeignApps)
		// accounts returns a full group whose app call reads the balance of n accounts that no
		// transaction names, with the most extra budget the default config allows.
		accounts := func(n int) v2.PreEncodedSimulateRequest {
			call := env.TxnInfo.NewTxn(txntest.Txn{
				Type:   protocol.ApplicationCallTx,
				Sender: sender.Addr,
				ApprovalProgram: `#pragma version 10
txn ApplicationArgs 0
btoi
store 1
int 0
store 0
loop:
load 0
load 1
<
bz end
load 0
int 1
+
dup
store 0
itob
int 24
bzero
concat
acct_params_get AcctBalance
pop
pop
b loop
end:
int 1`,
				ClearStateProgram: "#pragma version 10\nint 1",
				ApplicationArgs:   [][]byte{binary.BigEndian.AppendUint64(nil, uint64(n))},
			})
			call.FirstValid = 0
			call.LastValid = 1000
			txns := []*txntest.Txn{&call}
			for i := 1; i < proto.MaxTxGroupSize; i++ {
				txns = append(txns, pay(uint64(i)))
			}
			txntest.Group(txns...)
			stxns := make([]transactions.SignedTxn, len(txns))
			for i, txn := range txns {
				stxns[i] = txn.Txn().Sign(sender.Sk)
			}
			request := oneGroupRequest(stxns...)
			request.AllowUnnamedResources = true
			request.ExtraOpcodeBudget = simulation.MaxExtraOpcodeBudget
			return request
		}
		for _, m := range []uint64{0, 1, 3, 1 << 20} {
			h, limits := limitsNode(t, env, func(c *config.Local) { c.SimulateResourceLimitMultiplier = m })
			n := int(limits.ResourceLimitMultiplier) * proto.MaxAppTotalTxnReferences

			group := requireSimulated(t, h, accounts(n)).TxnGroups[0]
			require.Nil(t, group.FailureMessage, "multiplier %d: %v", m, failure(group))
			require.NotNil(t, group.UnnamedResourcesAccessed, "multiplier %d", m)
			require.Len(t, *group.UnnamedResourcesAccessed.Accounts, n, "multiplier %d", m)

			group = requireSimulated(t, h, accounts(n+1)).TxnGroups[0]
			require.NotNil(t, group.FailureMessage, "multiplier %d", m)
			// The accounts are numbered from 1, so the last one is n+1.
			var last basics.Address
			binary.BigEndian.PutUint64(last[:], uint64(n+1))
			require.Contains(t, *group.FailureMessage, fmt.Sprintf("unavailable Account %s", last), "multiplier %d", m)
		}
	})
}
