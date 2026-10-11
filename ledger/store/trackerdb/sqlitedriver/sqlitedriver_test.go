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

package sqlitedriver

import (
	"context"
	"database/sql"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestReadIdleConns checks that the read handle keeps the connections of
// concurrent lookups, like the eval prefetcher's, instead of closing all but 2.
func TestReadIdleConns(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	store, _ := OpenForTesting(t, false)
	defer store.Close()
	rdb := store.(*trackerSQLStore).pair.Rdb.Handle

	const n = 9 // the prefetcher's goroutines and the evaluator
	conns := make([]*sql.Conn, n)
	for i := range conns {
		var err error
		conns[i], err = rdb.Conn(context.Background())
		require.NoError(t, err)
	}
	for _, c := range conns {
		require.NoError(t, c.Close())
	}
	stats := rdb.Stats()
	require.Zero(t, stats.MaxIdleClosed)
	require.Equal(t, n, stats.Idle)
}
