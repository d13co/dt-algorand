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
	"fmt"

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
}

func (p VersionsParams) query() versionsQuery {
	q := versionsQuery{MinRound: p.MinRound, MaxRound: p.MaxRound, Limit: p.Limit, Next: p.Next}
	if p.Desc {
		q.Order = "desc"
	}
	if p.Values {
		q.Include = "values"
	}
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

type assetHoldersParams struct {
	Limit uint64       `url:"limit,omitempty"`
	Next  string       `url:"next,omitempty"`
	Round basics.Round `url:"round,omitempty"`
}

// AssetHolders gets a page of the accounts that hold an asset, at round, or
// if round is 0 at the client's AtRound round, if any, else the latest
// (EnableTimeTravel and TimeTravelHoldersIndex). limit 0 is the node's
// default; next is the previous page's next-token. Pin the round of the first
// page's response for the next ones.
func (client RestClient) AssetHolders(assetID basics.AssetIndex, round basics.Round, limit uint64, next string) (response model.AssetHoldersResponse, err error) {
	if round == 0 {
		client = client.pinned()
	}
	err = client.get(&response, fmt.Sprintf("/v2/assets/%d/holders", assetID), assetHoldersParams{Limit: limit, Next: next, Round: round})
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
