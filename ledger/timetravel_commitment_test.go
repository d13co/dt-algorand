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

package ledger

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestLedgerHistoryCommitment(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	genesisInitState, _ := ledgertesting.GenerateInitState(t, protocol.ConsensusCurrentVersion, 10)
	cfg := config.GetDefaultLocal()
	cfg.EnableTimeTravel = true
	l, err := OpenLedger(logging.TestingLog(t), filepath.Join(t.TempDir(), "ledger"), false, genesisInitState, cfg)
	require.NoError(t, err)
	defer l.Close()
	const rounds = 12
	for range rounds {
		addEmptyValidatedBlock(t, l, genesisInitState.Accounts)
	}
	commitRoundLookback(0, l)

	ctx := context.Background()
	c, err := l.HistoryCommitment(ctx, 10, 2)
	require.NoError(t, err)
	want, err := l.history.store.Commitment(ctx, 10, timetravel.CommitmentOptions{Workers: 1})
	require.NoError(t, err)
	require.Equal(t, want, c.Commitment)
	hdr, err := l.BlockHdr(10)
	require.NoError(t, err)
	require.Equal(t, hdr.Hash(), c.HeaderHash)
	require.Equal(t, l.GenesisHash(), c.GenesisHash)
	require.EqualValues(t, 11, c.Headers)
	require.Empty(t, c.Installed)
	require.NoError(t, l.history.store.SetInstalled("history-9.json"))
	c, err = l.HistoryCommitment(ctx, 10, 2)
	require.NoError(t, err)
	require.Equal(t, "history-9.json", c.Installed)

	_, err = l.HistoryCommitment(ctx, rounds+1, 2)
	require.ErrorContains(t, err, "after the latest round")

	cfg.EnableTimeTravel = false
	plain, err := OpenLedger(logging.TestingLog(t), filepath.Join(t.TempDir(), "ledger"), false, genesisInitState, cfg)
	require.NoError(t, err)
	defer plain.Close()
	_, err = plain.HistoryCommitment(ctx, basics.Round(0), 2)
	require.ErrorContains(t, err, "requires EnableTimeTravel")
}
