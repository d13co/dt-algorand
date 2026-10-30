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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestAllowForeignBoxReads: with AllowForeignBoxReads, which only POST /v2/query sets, an app reads
// the boxes of an app that allowed no foreign reads, but still may not write them. Non-standard
// fork feature.
func TestAllowForeignBoxReads(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	ep, _, ledger := makeSampleEnv()
	ep.UnnamedResources = &mockUnnamedResourcePolicy{allowEverything: true}
	// 999 has another creator than the calling app, 888, and set neither ForeignBoxReads nor
	// FamilyBoxAccess.
	ledger.NewApp(basics.Address{2}, 888, basics.AppParams{})
	ledger.NewApp(basics.Address{1}, 999, basics.AppParams{})
	require.NoError(t, ledger.NewBox(999, "b", []byte("boxval"), basics.AppIndex(999).Address()))

	reads := []string{
		`int 999; byte "b"; app_box_get; assert; byte "boxval"; ==`,
		`int 999; byte "b"; int 1; int 3; app_box_extract; byte "oxv"; ==`,
		`int 999; byte "b"; app_box_len; assert; int 6; ==`,
		// A missing box: exists 0 and an empty value.
		`int 999; byte "nope"; app_box_get; !; assert; len; !`,
	}
	for _, read := range reads {
		ep.AllowForeignBoxReads = false
		testApp(t, read, ep, "foreign app 888 may not read box of 999")
		ep.AllowForeignBoxReads = true
		testApp(t, read, ep)
	}

	// Writes stay refused.
	testApp(t, `int 999; byte "b"; byte "xxxxxx"; app_box_put; int 1`, ep, "foreign app 888 may not write box of 999")
	testApp(t, `int 999; byte "b"; app_box_del`, ep, "foreign app 888 may not write box of 999")
	testApp(t, `int 999; byte "c"; int 4; app_box_create`, ep, "foreign app 888 may not write box of 999")
	testApp(t, `int 999; byte "b"; int 0; byte "x"; app_box_replace; int 1`, ep, "foreign app 888 may not write box of 999")
	testApp(t, `int 999; byte "b"; int 0; int 1; byte "x"; app_box_splice; int 1`, ep, "foreign app 888 may not write box of 999")
	testApp(t, `int 999; byte "b"; int 8; app_box_resize; int 1`, ep, "foreign app 888 may not write box of 999")

	// An app that does not exist is still an error, not a missing box.
	testApp(t, `int 1234; byte "b"; app_box_get; pop; pop; int 1`, ep, "no app 1234")
}
