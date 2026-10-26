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
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
)

// VersionsParams select a page of an object's versions on a time travel
// node (the /versions endpoints).
type VersionsParams struct {
	MinRound, MaxRound *basics.Round // inclusive; nil: 0 and the latest round
	Desc               bool          // newest first
	Limit              uint64        // 0: the node's default
	Next               string        // the previous page's next-token
	Values             bool          // include the new values
	// Keys lists the state keys each app params or local state version
	// changed; only the app state endpoints take it
	Keys bool
	// Rounds asks for the objects as of each of these rounds instead of
	// their versions; it excludes MinRound and MaxRound
	Rounds []basics.Round
}

type versionsQuery struct {
	MinRound *basics.Round `url:"min-round,omitempty"`
	MaxRound *basics.Round `url:"max-round,omitempty"`
	Order    string        `url:"order,omitempty"`
	Limit    uint64        `url:"limit,omitempty"`
	Next     string        `url:"next,omitempty"`
	Include  string        `url:"include,omitempty"`
	Exclude  string        `url:"exclude,omitempty"`
	Name     string        `url:"name,omitempty"`
	Rounds   string        `url:"rounds,omitempty"`
}

func (p VersionsParams) query() versionsQuery {
	q := versionsQuery{MinRound: p.MinRound, MaxRound: p.MaxRound, Limit: p.Limit, Next: p.Next}
	if p.Desc {
		q.Order = "desc"
	}
	var inc []string
	if p.Values {
		inc = append(inc, "values")
	}
	if p.Keys {
		inc = append(inc, "keys")
	}
	q.Include = strings.Join(inc, ",")
	rs := make([]string, len(p.Rounds))
	for i, r := range p.Rounds {
		rs[i] = strconv.FormatUint(uint64(r), 10)
	}
	q.Rounds = strings.Join(rs, ",")
	return q
}

// AccountVersions lists the versions of everything GET /v2/accounts/{address}
// returns: the account's base record, holdings, local states, and created
// assets and apps, narrowed by exclude as there: "" or "none", "all", or
// "created-apps-params" and/or "created-assets-params", comma-separated.
func (client RestClient) AccountVersions(address string, exclude string, p VersionsParams) (response model.AccountVersionsResponse, err error) {
	q := p.query()
	q.Exclude = exclude
	err = client.get(&response, fmt.Sprintf("/v2/accounts/%s/versions", address), q)
	return
}

// AccountAssetVersions lists the versions of an account's holding of an
// asset and, if the account created it, of the asset's params.
func (client RestClient) AccountAssetVersions(address string, assetID basics.AssetIndex, p VersionsParams) (response model.AccountVersionsResponse, err error) {
	err = client.get(&response, fmt.Sprintf("/v2/accounts/%s/assets/%d/versions", address, assetID), p.query())
	return
}

// AccountApplicationVersions lists the versions of an account's local state
// of an application and, if the account created it, of its params.
func (client RestClient) AccountApplicationVersions(address string, appID basics.AppIndex, p VersionsParams) (response model.AccountVersionsResponse, err error) {
	err = client.get(&response, fmt.Sprintf("/v2/accounts/%s/applications/%d/versions", address, appID), p.query())
	return
}

// AssetVersions lists the versions of an asset's params.
func (client RestClient) AssetVersions(assetID basics.AssetIndex, p VersionsParams) (response model.AssetVersionsResponse, err error) {
	err = client.get(&response, fmt.Sprintf("/v2/assets/%d/versions", assetID), p.query())
	return
}

// ApplicationVersions lists the versions of an application's params, global
// state included.
func (client RestClient) ApplicationVersions(appID basics.AppIndex, p VersionsParams) (response model.ApplicationVersionsResponse, err error) {
	err = client.get(&response, fmt.Sprintf("/v2/applications/%d/versions", appID), p.query())
	return
}

// ApplicationBoxesVersions lists the rounds at which any box of an
// application changed, with the boxes that did.
func (client RestClient) ApplicationBoxesVersions(appID basics.AppIndex, p VersionsParams) (response model.BoxesVersionsResponse, err error) {
	err = client.get(&response, fmt.Sprintf("/v2/applications/%d/boxes/versions", appID), p.query())
	return
}

// ApplicationBoxVersions lists the versions of a box. name is in goal's app
// call argument form, e.g. "b64:..." or "str:...".
func (client RestClient) ApplicationBoxVersions(appID basics.AppIndex, name string, p VersionsParams) (response model.BoxVersionsResponse, err error) {
	q := p.query()
	q.Name = name
	err = client.get(&response, fmt.Sprintf("/v2/applications/%d/box/versions", appID), q)
	return
}

func init() {
	// The query body is JSON, not form values.
	rawRequestPaths["/v2/query"] = true
}

// Query runs read-only TEAL calls at one or more rounds (POST /v2/query).
// Only dtalgorand nodes have it: GET /v2/transactions/simulate/limits
// reports a query object when they do.
func (client RestClient) Query(request model.QueryRequest) (response model.QueryResponse, err error) {
	body, err := json.Marshal(request)
	if err != nil {
		return response, err
	}
	err = client.post(&response, "/v2/query", nil, body, false)
	return
}

// SimulateLimits gets the limits of simulate and query requests.
func (client RestClient) SimulateLimits() (response model.SimulateLimitsResponse, err error) {
	err = client.get(&response, "/v2/transactions/simulate/limits", nil)
	return
}
