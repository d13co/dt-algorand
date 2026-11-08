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

package simulation

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestQueryCacheEvict: over its limit, the cache drops the entries of the calls that asked for them
// longest ago, down to 3/4 of its limit, and never those of the running call.
func TestQueryCacheEvict(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	c := queryCache{limit: 1000}
	key := func(name string) queryCacheKey { return queryCacheKey{kind: queryCacheKv, name: name} }
	put := func(name string, n int) { c.put(key(name), name, n) }
	has := func(names ...string) {
		t.Helper()
		require.Len(t, c.entries, len(names))
		total := 0
		for _, name := range names {
			e, ok := c.entries[key(name)]
			require.True(t, ok, name)
			require.Equal(t, name, e.value)
			total += e.bytes
		}
		require.Equal(t, total, c.bytes)
	}

	c.gen = 1
	put("a", 400)
	put("b", 400)
	c.gen = 2
	put("c", 400) // 1200: a and b go
	has("c")

	c.gen = 3
	_, ok := c.get(key("c"))
	require.True(t, ok)
	put("d", 400)
	c.gen = 4
	_, ok = c.get(key("c"))
	require.True(t, ok)
	put("e", 400) // 1200: d, last asked for by call 3, goes; c and e are call 4's
	has("c", "e")
	require.Equal(t, uint64(4), c.full)
	put("f", 100)
	has("c", "e", "f")

	c.gen = 5
	put("g", 200) // 1100: call 4's entries go
	has("g")
	require.Equal(t, uint64(4), c.full) // call 5 got under the limit

	c.gen = 6
	put("h", 2000) // over on its own: g goes, h stays
	has("h")
	require.Equal(t, uint64(6), c.full)
	put("i", 10) // no new scan in the same call
	has("h", "i")

	_, ok = c.get(key("a"))
	require.False(t, ok)
}
