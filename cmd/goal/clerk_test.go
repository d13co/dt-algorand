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

package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/txntest"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func abs(t *testing.T, path string) string {
	t.Helper()
	absPath, err := filepath.Abs(path)
	require.NoError(t, err)
	return absPath
}

func TestDeterminePathToSourceFromSourceMap(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	testCases := []struct {
		name       string
		sourceFile string
		outFile    string

		expectedPath string
	}{
		{
			name:         "same directory",
			sourceFile:   filepath.FromSlash("data/program.teal"),
			outFile:      filepath.FromSlash("data/program.teal.tok"),
			expectedPath: "program.teal",
		},
		{
			name:         "output one level up",
			sourceFile:   filepath.FromSlash("data/program.teal"),
			outFile:      filepath.FromSlash("data/output/program.teal.tok"),
			expectedPath: filepath.FromSlash("../program.teal"),
		},
		{
			name:         "output one level down",
			sourceFile:   filepath.FromSlash("data/program.teal"),
			outFile:      "program.teal.tok",
			expectedPath: filepath.FromSlash("data/program.teal"),
		},
		{
			name:         "input stdin",
			sourceFile:   stdinFileNameValue,
			outFile:      "program.teal.tok",
			expectedPath: "<stdin>",
		},
		{
			name:         "output stdout",
			sourceFile:   filepath.FromSlash("data/program.teal"),
			outFile:      stdoutFilenameValue,
			expectedPath: abs(t, filepath.FromSlash("data/program.teal")),
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			sources := []string{tc.sourceFile}
			if tc.sourceFile != stdinFileNameValue {
				sources = append(sources, abs(t, tc.sourceFile))
			}
			outs := []string{tc.outFile}
			if tc.outFile != stdoutFilenameValue {
				outs = append(outs, abs(t, tc.outFile))
			}

			for sourceIndex, source := range sources {
				for outIndex, out := range outs {
					actualPath, err := determinePathToSourceFromSourceMap(source, out)
					require.NoError(t, err, "sourceIndex: %d, outIndex: %d", sourceIndex, outIndex)
					require.Equal(t, tc.expectedPath, actualPath, "sourceIndex: %d, outIndex: %d", sourceIndex, outIndex)
				}
			}
		})
	}
}

// TestMakeSimulateRequestGroups checks that goal clerk simulate splits a txn file into groups and
// passes --parallel-groups on.
func TestMakeSimulateRequestGroups(t *testing.T) { // nolint:paralleltest // Sets the command's flag variables.
	partitiontest.PartitionTest(t)

	sender := basics.Address{1}
	pay := func(note byte) txntest.Txn {
		return txntest.Txn{Type: protocol.PaymentTx, Sender: sender, Receiver: sender, Note: []byte{note}}
	}
	a, b, c := pay(1), pay(2), pay(3)
	group := txntest.Group(&a, &b)
	ungrouped := c.SignedTxn()

	var data []byte
	for _, stxn := range append(group, ungrouped) {
		data = append(data, protocol.Encode(&stxn)...)
	}
	file := filepath.Join(t.TempDir(), "txns")
	require.NoError(t, os.WriteFile(file, data, 0600))

	defer func(name string, parallel bool) { txFilename, simulateParallelGroups = name, parallel }(txFilename, simulateParallelGroups)
	txFilename = file
	simulateParallelGroups = true

	request := makeSimulateRequest(decodeTxnsFromFile(txFilename))
	require.True(t, request.ParallelGroups)
	require.Len(t, request.TxnGroups, 2)
	require.Equal(t, group, request.TxnGroups[0].Txns)
	require.Equal(t, []transactions.SignedTxn{ungrouped}, request.TxnGroups[1].Txns)
}
