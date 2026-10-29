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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	apiclient "github.com/algorand/go-algorand/daemon/algod/api/client"
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

// TestKeyVersions checks the lines of goal app versions --key and goal
// account versions --app-id --key, and that listing follows empty pages.
func TestKeyVersions(t *testing.T) { // nolint:paralleltest // sets the versions flags and output
	partitiontest.PartitionTest(t)

	line := func(en versionEntry, key string) string {
		var b bytes.Buffer
		require.NoError(t, printKeyVersion(&b, en, []byte(key)))
		return b.String()
	}
	at := versionEntry{round: 5, timestamp: 60, label: "app:6"}
	en := at
	en.value = &model.TealValue{Type: uint64(basics.TealUintType), Uint: 42}
	require.Equal(t, "5  1970-01-01 00:01:00  app:6 str:count = 42\n", line(en, "count"))
	en.value = &model.TealValue{Type: uint64(basics.TealBytesType), Bytes: "aGk="}
	require.Equal(t, "5  1970-01-01 00:01:00  app:6 b64:AA== = str:hi\n", line(en, "\x00"))
	require.Equal(t, "5  1970-01-01 00:01:00  app:6 str: removed\n", line(at, ""))
	en = at
	en.deleted = true
	require.Equal(t, "5  1970-01-01 00:01:00  app:6 str:count deleted\n", line(en, "count"))

	var addr basics.Address
	addr[0] = 1
	ens := localKeyEntries(addr, model.AccountVersion{Round: 7, Timestamp: 60, AppsLocalState: &[]model.ApplicationLocalStateVersion{
		{AppId: 9, KeyValue: &model.TealValue{Type: uint64(basics.TealUintType), Uint: 1}}}})
	require.Len(t, ens, 1)
	require.Equal(t, fmt.Sprintf("7  1970-01-01 00:01:00  local:%s:9 str:n = 1\n", addr), line(ens[0], "n"))

	require.NoError(t, checkBoxPrefix("", "str:u"))
	require.NoError(t, checkBoxPrefix("str:b", ""))
	require.ErrorContains(t, checkBoxPrefix("str:b", "str:u"), "cannot be combined with --name")
	require.Error(t, checkBoxPrefix("", "nope"))

	// pages: empty with a next token, then one version; without --all the
	// listing follows the empty page and stops after the version
	var queries []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Query().Get("next") {
		case "":
			w.Write([]byte(`{"round":100,"versions":[],"next-token":"40"}`))
		case "40":
			w.Write([]byte(`{"round":100,"versions":[{"round":50,"timestamp":60,"deleted":false,"key-value":{"type":2,"uint":3,"bytes":""}}],"next-token":"51"}`))
		default:
			t.Errorf("unexpected page %s", r.URL.RawQuery)
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.net"), []byte(strings.TrimPrefix(srv.URL, "http://")), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.token"), []byte(strings.Repeat("a", 64)), 0600))
	c, err := libgoal.MakeClientWithBinDir(dir, dir, "", libgoal.AlgodClient)
	require.NoError(t, err)
	var out bytes.Buffer
	defer func(w io.Writer) { versionsOut, versionsKey = w, "" }(versionsOut)
	versionsOut, versionsKey = &out, "str:count"
	listVersions(appVersionsCmd, c, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
		resp, err := c.ApplicationVersions(6, p)
		var ens []versionEntry
		for _, v := range resp.Versions {
			ens = append(ens, versionEntry{round: v.Round, timestamp: v.Timestamp, label: "app:6", deleted: v.Deleted, value: ptrValue(v.KeyValue)})
		}
		return resp, resp.NextToken, ens, err
	})
	require.Equal(t, "50  1970-01-01 00:01:00  app:6 str:count = 3\n", out.String())
	require.Len(t, queries, 2)
	require.Contains(t, queries[0], "key=str%3Acount")
	require.NotContains(t, queries[0], "include=values", "the key's value is enough")
}
