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

package ledgercore

import (
	"context"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestEffort(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// nil counts nothing and doesn't panic
	var none *Effort
	none.AddOpcodes(5)
	none.Lookup(true)
	none.History()
	none.AddScanned(3)
	require.Equal(t, EffortCounts{}, none.Counts())
	require.Nil(t, EffortFromContext(context.Background()))

	e := NewEffort()
	e.AddOpcodes(700)
	e.AddOpcodes(20)
	e.Lookup(false)
	e.Lookup(true)
	e.Lookup(false)
	e.History()
	e.AddScanned(9)
	c := e.Counts()
	c.Eval = 0
	require.Equal(t, EffortCounts{Opcodes: 720, Lookups: 3, History: 2, Scanned: 9}, c)
	require.Regexp(t, regexp.MustCompile(`^opcodes=720, lookups=3, history=2, scanned=9, eval-us=\d+$`), e.Header())

	require.Same(t, e, EffortFromContext(ContextWithEffort(context.Background(), e)))
}
