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

package libgoal

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	algodclient "github.com/algorand/go-algorand/daemon/algod/api/client"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestAtRound checks that a client pinned with AtRound sends the round with
// its ledger lookups only, and that versions requests reach the node.
func TestAtRound(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	got := map[string]url.Values{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got[r.URL.Path] = r.URL.Query()
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/v2/assets/5" {
			w.Write([]byte(`{"index":5,"params":{"creator":"","decimals":0,"total":0}}`))
			return
		}
		w.Write([]byte("{}"))
	}))
	defer srv.Close()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.net"), []byte(strings.TrimPrefix(srv.URL, "http://")), 0600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "algod.token"), []byte(strings.Repeat("a", 64)), 0600))
	c, err := MakeClientWithBinDir(dir, dir, "", AlgodClient)
	require.NoError(t, err)

	_, pinned := c.Round()
	require.False(t, pinned)
	at := c.AtRound(0)
	r, pinned := at.Round()
	require.True(t, pinned)
	require.Zero(t, r)
	_, pinned = c.Round()
	require.False(t, pinned, "AtRound returns a copy")

	var addr basics.Address
	lookups := []struct {
		path string
		call func(c Client) error
	}{
		{"/v2/assets/5", func(c Client) error { _, err := c.AssetInformation(5); return err }},
		{"/v2/accounts/" + addr.String(), func(c Client) error { _, err := c.AccountInformation(addr.String(), true); return err }},
		{"/v2/accounts/" + addr.String() + "/applications/6", func(c Client) error {
			_, err := c.AccountApplicationInformation(addr.String(), 6)
			return err
		}},
		{"/v2/applications/6/box", func(c Client) error { _, err := c.GetApplicationBoxByName(6, "str:b"); return err }},
		{"/v2/ledger/supply", func(c Client) error { _, err := c.LedgerSupply(); return err }},
		{"/v2/assets/5/holders", func(c Client) error { _, err := c.AssetHolders(5, 0, 0, ""); return err }},
	}
	for _, l := range lookups {
		require.NoError(t, l.call(at), l.path)
		require.Equal(t, "0", got[l.path].Get("round"), l.path)
		require.NoError(t, l.call(c), l.path)
		require.NotContains(t, got[l.path], "round", "%s unpinned", l.path)
	}
	_, err = at.Status()
	require.NoError(t, err)
	require.NotContains(t, got["/v2/status"], "round")

	_, err = c.ApplicationBoxVersions(6, "str:b", algodclient.VersionsParams{Values: true})
	require.NoError(t, err)
	require.Equal(t, url.Values{"name": {"str:b"}, "include": {"values"}}, got["/v2/applications/6/box/versions"])
	_, err = c.ApplicationBoxesVersions(6, algodclient.VersionsParams{Values: true})
	require.NoError(t, err)
	require.Equal(t, url.Values{"include": {"values"}}, got["/v2/applications/6/boxes/versions"])
	_, err = c.AccountVersions(addr.String(), "all", algodclient.VersionsParams{})
	require.NoError(t, err)
	require.Equal(t, "all", got["/v2/accounts/"+addr.String()+"/versions"].Get("exclude"))
}
