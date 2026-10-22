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

package logic

import (
	"bytes"
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// abortingTracer stops the program with EvalAborted before its opcode number at.
type abortingTracer struct {
	NullEvalTracer
	at, opcodes int
	afterErr    error
}

func (t *abortingTracer) BeforeOpcode(cx *EvalContext) {
	t.opcodes++
	if t.opcodes == t.at {
		panic(EvalAborted{Cause: context.Canceled})
	}
}

func (t *abortingTracer) AfterProgram(cx *EvalContext, pass bool, evalError error) {
	t.afterErr = evalError
}

// TestEvalAborted checks that a Tracer stops a program with EvalAborted, which eval returns as the
// program's error without logging a panic.
func TestEvalAborted(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ops := testProg(t, "loop: int 1; bnz loop", LogicVersion)
	txn := transactions.SignedTxn{Lsig: transactions.LogicSig{Logic: ops.Program}}
	ep := defaultSigParams(txn)
	var logOutput bytes.Buffer
	logger := logging.NewLogger()
	logger.SetOutput(&logOutput)
	ep.logger = logger
	tracer := &abortingTracer{at: 10}
	ep.Tracer = tracer

	pass, err := EvalSignature(0, ep)
	require.False(t, pass)
	var aborted EvalAborted
	require.ErrorAs(t, err, &aborted)
	require.ErrorIs(t, err, context.Canceled)
	require.ErrorContains(t, err, "evaluation aborted: context canceled")
	require.Equal(t, 10, tracer.opcodes)
	// The tracer still sees the end of the program.
	require.Error(t, tracer.afterErr)
	require.Empty(t, logOutput.String())

	// Any other panic is still logged.
	other := &panicTracer{}
	ep = defaultSigParams(txn)
	ep.logger = logger
	ep.Tracer = other
	_, err = EvalSignature(0, ep)
	require.NotErrorAs(t, err, &aborted)
	require.Contains(t, logOutput.String(), "recovered panic in Eval")
}

type panicTracer struct{ NullEvalTracer }

func (t *panicTracer) BeforeOpcode(cx *EvalContext) { panic("injected") }
