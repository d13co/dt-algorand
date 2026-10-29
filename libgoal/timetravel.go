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
	algodclient "github.com/algorand/go-algorand/daemon/algod/api/client"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
)

// AtRound returns a copy of the client whose account, asset, application,
// box and supply lookups read the ledger as of round r, on a node with
// EnableTimeTravel (algodclient.RestClient.AtRound).
func (c Client) AtRound(r basics.Round) Client {
	c.round = &r
	return c
}

// Round returns the round the client reads at, if AtRound pinned one.
func (c *Client) Round() (basics.Round, bool) {
	if c.round == nil {
		return 0, false
	}
	return *c.round, true
}

// AccountVersions lists the versions of an account's objects, narrowed by
// exclude (algodclient.RestClient.AccountVersions; time travel).
func (c *Client) AccountVersions(address string, exclude string, p algodclient.VersionsParams) (resp model.AccountVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.AccountVersions(address, exclude, p)
	}
	return
}

// AccountAssetVersions lists the versions of an account's holding of an
// asset, and of the asset's params if the account created it (time travel).
func (c *Client) AccountAssetVersions(address string, assetID basics.AssetIndex, p algodclient.VersionsParams) (resp model.AccountVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.AccountAssetVersions(address, assetID, p)
	}
	return
}

// AccountApplicationVersions lists the versions of an account's local state
// of an application, and of its params if the account created it (time
// travel).
func (c *Client) AccountApplicationVersions(address string, appID basics.AppIndex, p algodclient.VersionsParams) (resp model.AccountVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.AccountApplicationVersions(address, appID, p)
	}
	return
}

// AssetVersions lists the versions of an asset's params (time travel).
func (c *Client) AssetVersions(assetID basics.AssetIndex, p algodclient.VersionsParams) (resp model.AssetVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.AssetVersions(assetID, p)
	}
	return
}

// ApplicationVersions lists the versions of an application's params (time
// travel).
func (c *Client) ApplicationVersions(appID basics.AppIndex, p algodclient.VersionsParams) (resp model.ApplicationVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.ApplicationVersions(appID, p)
	}
	return
}

// ApplicationBoxesVersions lists the changes of an application's boxes
// (time travel).
func (c *Client) ApplicationBoxesVersions(appID basics.AppIndex, p algodclient.VersionsParams) (resp model.BoxesVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.ApplicationBoxesVersions(appID, p)
	}
	return
}

// AssetHolders gets a page of the holders of an asset at round, or if round
// is 0 at the client's AtRound round, if any, else the latest
// (algodclient.RestClient.AssetHolders; time travel).
func (c *Client) AssetHolders(assetID basics.AssetIndex, round basics.Round, limit uint64, next string) (resp model.AssetHoldersResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.AssetHolders(assetID, round, limit, next)
	}
	return
}

// ApplicationBoxVersions lists the versions of a box, name in goal's app call
// argument form (time travel).
func (c *Client) ApplicationBoxVersions(appID basics.AppIndex, name string, p algodclient.VersionsParams) (resp model.BoxVersionsResponse, err error) {
	algod, err := c.ensureAlgodClient()
	if err == nil {
		resp, err = algod.ApplicationBoxVersions(appID, name, p)
	}
	return
}
