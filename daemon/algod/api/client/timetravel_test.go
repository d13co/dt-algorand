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

package client

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// request is what the recorder got.
type request struct {
	path  string
	query url.Values
}

// recorder is a server that answers {} and records every request.
func recorder(t *testing.T) (RestClient, *[]request) {
	var got []request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, request{r.URL.Path, r.URL.Query()})
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	}))
	t.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	require.NoError(t, err)
	return MakeRestClient(*u, "token"), &got
}

const testAddr = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAY5HFKQ"

// lookups are the client's requests, whether they read the ledger as of a
// pinned round, and their path.
var lookups = []struct {
	name   string
	call   func(c RestClient)
	pinned bool
	path   string
}{
	{"AccountInformation", func(c RestClient) { c.AccountInformation(testAddr, true) }, true, "/v2/accounts/" + testAddr},
	{"RawAccountInformation", func(c RestClient) { c.RawAccountInformation(testAddr) }, true, "/v2/accounts/" + testAddr},
	{"AccountAssetInformation", func(c RestClient) { c.AccountAssetInformation(testAddr, 5) }, true, "/v2/accounts/" + testAddr + "/assets/5"},
	{"AccountApplicationInformation", func(c RestClient) { c.AccountApplicationInformation(testAddr, 6) }, true, "/v2/accounts/" + testAddr + "/applications/6"},
	{"RawAccountApplicationInformation", func(c RestClient) { c.RawAccountApplicationInformation(testAddr, 6) }, true, "/v2/accounts/" + testAddr + "/applications/6"},
	{"AccountAssetsInformation", func(c RestClient) { c.AccountAssetsInformation(testAddr, "", 10) }, true, "/v2/accounts/" + testAddr + "/assets"},
	{"RawAccountAssetsInformation", func(c RestClient) { c.RawAccountAssetsInformation(testAddr, "", 10) }, true, "/v2/accounts/" + testAddr + "/assets"},
	{"AccountApplicationsInformation", func(c RestClient) { c.AccountApplicationsInformation(testAddr, "", 10, true) }, true, "/v2/accounts/" + testAddr + "/applications"},
	{"AssetInformation", func(c RestClient) { c.AssetInformation(5) }, true, "/v2/assets/5"},
	{"ApplicationInformation", func(c RestClient) { c.ApplicationInformation(6) }, true, "/v2/applications/6"},
	{"GetApplicationBoxByName", func(c RestClient) { c.GetApplicationBoxByName(6, "str:b") }, true, "/v2/applications/6/box"},
	{"ApplicationBoxes", func(c RestClient) { c.ApplicationBoxes(6, 10) }, true, "/v2/applications/6/boxes"},
	{"ApplicationBoxesPage", func(c RestClient) { c.ApplicationBoxesPage(6, 10, "", "", false, 0) }, true, "/v2/applications/6/boxes"},
	{"AssetHolders", func(c RestClient) { c.AssetHolders(5, 0, 0, "") }, true, "/v2/assets/5/holders"},
	{"LedgerSupply", func(c RestClient) { c.LedgerSupply() }, true, "/v2/ledger/supply"},
	{"Status", func(c RestClient) { c.Status() }, false, "/v2/status"},
	{"SuggestedParams", func(c RestClient) { c.SuggestedParams() }, false, "/v2/transactions/params"},
	{"PendingTransactionsByAddr", func(c RestClient) { c.PendingTransactionsByAddr(testAddr, 1) }, false, "/v2/accounts/" + testAddr + "/transactions/pending"},
	{"AssetVersions", func(c RestClient) { c.AssetVersions(5, VersionsParams{}) }, false, "/v2/assets/5/versions"},
}

func TestAtRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, pin := range []*basics.Round{nil, new(basics.Round), func() *basics.Round { r := basics.Round(1234); return &r }()} {
		c, got := recorder(t)
		if pin != nil {
			c = c.AtRound(*pin)
		}
		for _, l := range lookups {
			n := len(*got)
			l.call(c)
			require.Len(t, *got, n+1, l.name)
			req := (*got)[n]
			require.Equal(t, l.path, req.path, l.name)
			if pin != nil && l.pinned {
				require.Equal(t, []string{strconv.FormatUint(uint64(*pin), 10)}, req.query["round"], l.name)
			} else {
				require.NotContains(t, req.query, "round", l.name)
			}
		}
	}

	// a box listing's own round wins over the pin
	c, got := recorder(t)
	c.AtRound(7).ApplicationBoxesPage(6, 10, "", "", false, 9)
	require.Equal(t, []string{"9"}, (*got)[0].query["round"])
	// the raw variants keep their format
	c.AtRound(7).RawAccountInformation(testAddr)
	require.Equal(t, "msgpack", (*got)[1].query.Get("format"))
	require.Equal(t, "7", (*got)[1].query.Get("round"))
	// a query in the server URL is kept
	c.serverURL.RawQuery = "x=1"
	c.AtRound(7).AssetInformation(5)
	require.Equal(t, "1", (*got)[2].query.Get("x"))
	require.Equal(t, "7", (*got)[2].query.Get("round"))
	c.serverURL.RawQuery = ""
	// so does a holders page's
	c.AtRound(7).AssetHolders(5, 9, 3, testAddr)
	require.Equal(t, request{"/v2/assets/5/holders", url.Values{"round": {"9"}, "limit": {"3"}, "next": {testAddr}}}, (*got)[3])
}

func TestVersionsParams(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	c, got := recorder(t)
	last := func() request { return (*got)[len(*got)-1] }
	zero, ten := basics.Round(0), basics.Round(10)
	_, err := c.AccountVersions(testAddr, "all", VersionsParams{MinRound: &zero, MaxRound: &ten, Desc: true, Limit: 5, Next: "3", Values: true})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/accounts/" + testAddr + "/versions", url.Values{"min-round": {"0"}, "max-round": {"10"},
		"order": {"desc"}, "limit": {"5"}, "next": {"3"}, "include": {"values"}, "exclude": {"all"}}}, last())

	_, err = c.AssetVersions(5, VersionsParams{})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/assets/5/versions", url.Values{}}, last(), "defaults send nothing")
	_, err = c.ApplicationVersions(6, VersionsParams{Values: true})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/applications/6/versions", url.Values{"include": {"values"}}}, last())
	_, err = c.ApplicationBoxVersions(6, "str:b", VersionsParams{})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/applications/6/box/versions", url.Values{"name": {"str:b"}}}, last())
	_, err = c.ApplicationBoxesVersions(6, VersionsParams{Desc: true})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/applications/6/boxes/versions", url.Values{"order": {"desc"}}}, last())
	_, err = c.AccountAssetVersions(testAddr, 5, VersionsParams{MaxRound: &ten})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/accounts/" + testAddr + "/assets/5/versions", url.Values{"max-round": {"10"}}}, last())
	_, err = c.AccountApplicationVersions(testAddr, 6, VersionsParams{})
	require.NoError(t, err)
	require.Equal(t, request{"/v2/accounts/" + testAddr + "/applications/6/versions", url.Values{}}, last())
	_, err = c.AtRound(3).ApplicationVersions(6, VersionsParams{})
	require.NoError(t, err)
	require.NotContains(t, last().query, "round", "versions take their range, not the pin")
}
