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

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestConvertQueryRequestBoxes: a box of the called app has index 0, another app's its place in
// foreign-apps plus 1; an app not listed is added, without touching the request's slice.
func TestConvertQueryRequestBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	apps := make([]basics.AppIndex, 1, 4)
	apps[0] = 20
	r := PreEncodedQueryRequest{
		AppID: 10,
		Calls: []PreEncodedQueryCall{
			{ForeignApps: apps, Boxes: []PreEncodedQueryBox{
				{App: 0, Name: []byte("a")}, {App: 10, Name: []byte("b")}, {App: 20, Name: []byte("c")},
				{App: 30, Name: []byte("d")}, {App: 30, Name: []byte("e")},
			}},
			{ForeignApps: apps},
		},
	}
	request, err := convertQueryRequest(r)
	require.NoError(t, err)
	require.Equal(t, []transactions.BoxRef{
		{Index: 0, Name: []byte("a")}, {Index: 0, Name: []byte("b")}, {Index: 1, Name: []byte("c")},
		{Index: 2, Name: []byte("d")}, {Index: 2, Name: []byte("e")},
	}, request.Calls[0].Boxes)
	require.Equal(t, []basics.AppIndex{20, 30}, request.Calls[0].ForeignApps)
	require.Equal(t, []basics.AppIndex{20}, request.Calls[1].ForeignApps)
	require.Equal(t, []basics.AppIndex{20}, apps)

	_, err = convertQueryRequest(PreEncodedQueryRequest{Calls: []PreEncodedQueryCall{{Accounts: []string{"nope"}}}})
	require.ErrorContains(t, err, `call 0: account "nope"`)
	_, err = convertQueryRequest(PreEncodedQueryRequest{Budget: 1 << 40})
	require.ErrorContains(t, err, "over the limit")
}
