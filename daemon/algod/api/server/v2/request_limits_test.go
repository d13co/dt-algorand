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

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestRequestTimeLimit(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		timeoutSeconds int
		limit          time.Duration
		limitSeconds   uint64
	}{
		{-1, 0, 0}, // no limit
		{0, 0, 0},
		{1, 500 * time.Millisecond, 1}, // half: shorter than twice the 1 s margin
		{2, time.Second, 1},
		{10, 9 * time.Second, 9},      // a 1 s margin
		{120, 108 * time.Second, 108}, // a tenth
	} {
		cfg := config.GetDefaultLocal()
		cfg.RestWriteTimeoutSeconds = tc.timeoutSeconds
		require.Equal(t, tc.limit, requestTimeLimit(cfg), tc.timeoutSeconds)
		require.Equal(t, tc.limitSeconds, timeLimitSeconds(requestTimeLimit(cfg)), tc.timeoutSeconds)
	}
}
