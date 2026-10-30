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
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/libgoal"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestProbeRound checks that --round is refused by a node without time
// travel, and by an algod that ignores ?round and answers at its latest round.
func TestProbeRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		name   string
		answer func(w http.ResponseWriter, r *http.Request)
		err    string
	}{
		{"time travel", func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprintf(w, `{"round":%s}`, r.URL.Query().Get("round"))
		}, ""},
		{"ignores the round", func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"round":99}`))
		}, "the node answered at round 99: it does not read past rounds"},
		{"no time travel", func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"message":"looking up past rounds requires EnableTimeTravel"}`))
		}, "requires EnableTimeTravel"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				tc.answer(w, r)
			}))
			defer srv.Close()
			dir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.net"), []byte(strings.TrimPrefix(srv.URL, "http://")), 0600))
			require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.token"), []byte(strings.Repeat("a", 64)), 0600))
			c, err := libgoal.MakeClientWithBinDir(dir, dir, "", libgoal.AlgodClient)
			require.NoError(t, err)

			err = probeRound(c.AtRound(7), basics.Round(7))
			if tc.err == "" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, tc.err)
			}
		})
	}
}

// TestAccountVersionsAsOf checks that goal account versions --rounds with
// --assetid or --app-id prints a line for a round at which neither the
// holding or local state nor the params exist, which the API leaves empty.
func TestAccountVersionsAsOf(t *testing.T) { // nolint:paralleltest // sets the --assetid and --app-id flags
	partitiontest.PartitionTest(t)

	var addr basics.Address
	addr[0] = 1
	defer func() { versionsAssetID, versionsAppID = 0, 0 }()
	printed := func(ens []versionEntry) string {
		var b bytes.Buffer
		for _, en := range ens {
			require.NoError(t, printAsOf(&b, en, viewOptions(en.label)))
		}
		return b.String()
	}
	empty := model.AccountVersion{Round: 5, Timestamp: 60}

	versionsAssetID = 7
	require.Equal(t, fmt.Sprintf("5  1970-01-01 00:01:00  holding:%s:7 none\n", addr), printed(accountVersionEntries(addr, empty, true)))
	require.Empty(t, accountVersionEntries(addr, empty, false), "a range page has no such entries")
	changed := basics.Round(20)
	held := model.AccountVersion{Round: 50, Timestamp: 60, Assets: &[]model.AssetHoldingVersion{{AssetId: 7, ChangedRound: &changed, AssetHolding: &model.AssetHolding{AssetID: 7, Amount: 3}}}}
	out := printed(accountVersionEntries(addr, held, true))
	require.Contains(t, out, fmt.Sprintf("holding:%s:7 last changed at 20", addr))
	require.NotContains(t, out, "none")

	versionsAssetID, versionsAppID = 0, 9
	require.Equal(t, fmt.Sprintf("5  1970-01-01 00:01:00  local:%s:9 none\n", addr), printed(accountVersionEntries(addr, empty, true)))
}
