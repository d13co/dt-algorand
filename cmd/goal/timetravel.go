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
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/algorand/go-algorand/cmd/util/datadir"
	"github.com/algorand/go-algorand/cmd/util/ttview"
	apiclient "github.com/algorand/go-algorand/daemon/algod/api/client"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/libgoal"
)

// Commands that read the ledger as of a past round, on a node with
// EnableTimeTravel, and list the versions of objects.

// ttRound is the --round of the commands that read the ledger.
var ttRound uint64

const roundFlagHelp = "Read the ledger as of this round (needs a node with EnableTimeTravel; 0 is the genesis round)"

var (
	versionsMinRound, versionsMaxRound uint64
	versionsLimit                      uint64
	versionsNext, versionsExclude      string
	versionsDesc, versionsAll          bool
	versionsRoundsOnly, versionsJSON   bool
	versionsAssetID, versionsAppID     uint64
)

var (
	holdersLimit            uint64
	holdersNext             string
	holdersAll, holdersJSON bool
)

func init() {
	for _, cmd := range []*cobra.Command{
		infoCmd, balanceCmd, dumpCmd, rewardsCmd, assetDetailsCmd, applicationDetailsCmd,
		infoAssetCmd, infoAppCmd, readStateAppCmd, appBoxInfoCmd, supplyCmd, assetHoldersCmd,
	} {
		cmd.Flags().Uint64Var(&ttRound, "round", 0, roundFlagHelp)
	}

	assetCmd.AddCommand(assetHoldersCmd)
	f := assetHoldersCmd.Flags()
	f.Uint64Var((*uint64)(&assetID), "assetid", 0, "Asset ID (required)")
	assetHoldersCmd.MarkFlagRequired("assetid")
	f.Uint64Var(&holdersLimit, "limit", 0, "Holders per page (default: the node's, 1,000)")
	f.StringVar(&holdersNext, "next", "", "Continue from a previous page's next token, with that page's --round")
	f.BoolVar(&holdersAll, "all", false, "Follow the next tokens to the last page, at the first page's round")
	f.BoolVar(&holdersJSON, "json", false, "Print each page as the node returns it, one JSON document per line")

	accountCmd.AddCommand(accountVersionsCmd)
	assetCmd.AddCommand(assetVersionsCmd)
	appCmd.AddCommand(appVersionsCmd)
	appBoxCmd.AddCommand(appBoxVersionsCmd)
	for _, cmd := range []*cobra.Command{accountVersionsCmd, assetVersionsCmd, appVersionsCmd, appBoxVersionsCmd} {
		f := cmd.Flags()
		f.Uint64Var(&versionsMinRound, "min-round", 0, "List versions at this round or later")
		f.Uint64Var(&versionsMaxRound, "max-round", 0, "List versions at this round or earlier (default: the latest round)")
		f.BoolVar(&versionsDesc, "desc", false, "Newest first (prints whole values instead of changes)")
		f.Uint64Var(&versionsLimit, "limit", 0, "Versions per page (default: the node's, 100)")
		f.StringVar(&versionsNext, "next", "", "Continue from a previous page's next token")
		f.BoolVar(&versionsAll, "all", false, "Follow the next tokens to the last page")
		f.BoolVar(&versionsRoundsOnly, "rounds-only", false, "List the rounds of the versions without their values")
		f.BoolVar(&versionsJSON, "json", false, "Print each page as the node returns it, one JSON document per line")
	}
	accountVersionsCmd.Flags().StringVarP(&accountAddress, "address", "a", "", "Account address (required)")
	accountVersionsCmd.MarkFlagRequired("address")
	accountVersionsCmd.Flags().Uint64Var(&versionsAssetID, "assetid", 0, "Only the account's holding of this asset (and the asset's params if the account created it)")
	accountVersionsCmd.Flags().Uint64Var(&versionsAppID, "app-id", 0, "Only the account's local state of this app (and the app's params if the account created it)")
	accountVersionsCmd.Flags().StringVar(&versionsExclude, "exclude", "", "Narrow the versions as GET /v2/accounts/{address} does: all (the base record only), or created-apps-params and/or created-assets-params, comma-separated")
	assetVersionsCmd.Flags().Uint64Var((*uint64)(&assetID), "assetid", 0, "Asset ID (required)")
	assetVersionsCmd.MarkFlagRequired("assetid")
	appVersionsCmd.Flags().Uint64Var((*uint64)(&appIdx), "app-id", 0, "Application ID (required)")
	appVersionsCmd.MarkFlagRequired("app-id")
	appBoxVersionsCmd.Flags().StringVarP(&boxName, "name", "n", "", "Application box name, in the same form as app-arg (default: every box of the application)")
}

// withRound returns client pinned to --round, if cmd has it, once the node
// has shown that it reads past rounds (probeRound).
func withRound(cmd *cobra.Command, client libgoal.Client) libgoal.Client {
	if !cmd.Flags().Changed("round") {
		return client
	}
	r := basics.Round(ttRound)
	pinned := client.AtRound(r)
	if err := probeRound(pinned, r); err != nil {
		reportErrorf("Cannot read the ledger as of round %d: %v", r, err)
	}
	fmt.Fprintf(os.Stderr, "As of round %d\n", r)
	return pinned
}

// probeRound checks that the node answers as of round r, through a client
// pinned to r. A node without time travel rejects the round; an algod that
// predates time travel ignores it and answers at its latest round, which the
// probe catches unless r is that round, when the answers are the latest ones
// anyway.
func probeRound(pinned libgoal.Client, r basics.Round) error {
	var zero basics.Address
	probe, err := pinned.AccountInformation(zero.String(), true)
	if err != nil {
		return err
	}
	if probe.Round != r {
		return fmt.Errorf("the node answered at round %d: it does not read past rounds (time travel)", probe.Round)
	}
	return nil
}

var accountVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "List the versions of an account (time travel)",
	Long: `List the rounds at which an account changed, and what changed: its base record, asset holdings, app local states, and the params of the assets and apps it created. Needs a node with EnableTimeTravel.

Each version is a line with its round, time, the object that changed and how, followed by the fields that changed since that object's previous version.`,
	Args: validateNoPosArgsFn,
	Run: func(cmd *cobra.Command, _ []string) {
		client := ensureAlgodClient(datadir.EnsureSingleDataDir())
		if versionsAssetID != 0 && versionsAppID != 0 {
			reportErrorf("Only one of --assetid and --app-id can be given")
		}
		if (versionsAssetID != 0 || versionsAppID != 0) && versionsExclude != "" {
			reportErrorf("--exclude narrows the versions of the whole account, not of one asset or app")
		}
		addr, err := basics.UnmarshalChecksumAddress(accountAddress)
		if err != nil {
			reportErrorf(errorParseAddr, err)
		}
		listVersions(cmd, client, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
			var resp model.AccountVersionsResponse
			var err error
			switch {
			case versionsAssetID != 0:
				resp, err = client.AccountAssetVersions(accountAddress, basics.AssetIndex(versionsAssetID), p)
			case versionsAppID != 0:
				resp, err = client.AccountApplicationVersions(accountAddress, basics.AppIndex(versionsAppID), p)
			default:
				resp, err = client.AccountVersions(accountAddress, versionsExclude, p)
			}
			var ens []versionEntry
			for _, v := range resp.Versions {
				ens = append(ens, accountVersionEntries(addr, v)...)
			}
			return resp, resp.NextToken, ens, err
		})
	},
}

var assetVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "List the versions of an asset's params (time travel)",
	Long:  `List the rounds at which an asset was created, reconfigured or destroyed, with what changed. Needs a node with EnableTimeTravel.`,
	Args:  validateNoPosArgsFn,
	Run: func(cmd *cobra.Command, _ []string) {
		client := ensureAlgodClient(datadir.EnsureSingleDataDir())
		label := fmt.Sprintf("asset:%d", assetID)
		listVersions(cmd, client, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
			resp, err := client.AssetVersions(assetID, p)
			var ens []versionEntry
			for _, v := range resp.Versions {
				ens = append(ens, versionEntry{v.Round, v.Timestamp, label, v.Deleted, ptrValue(v.Asset), assetBaseline(assetID)})
			}
			return resp, resp.NextToken, ens, err
		})
	},
}

var assetHoldersCmd = &cobra.Command{
	Use:   "holders",
	Short: "List the accounts that hold an asset (time travel)",
	Long: `List the accounts that hold an asset, at the latest round or --round, in address order: one line per holder with its address, its amount in base units, and "frozen" if its holding is frozen. Accounts opted in with a zero balance are listed. Needs a node with EnableTimeTravel and TimeTravelHoldersIndex.

A page can be short, or even empty, while more holders remain: --all follows the next tokens to the last page, reading every page at the first page's round.`,
	Args: validateNoPosArgsFn,
	Run: func(cmd *cobra.Command, _ []string) {
		client := withRound(cmd, ensureAlgodClient(datadir.EnsureSingleDataDir()))
		next := holdersNext
		for {
			resp, err := client.AssetHolders(assetID, 0, holdersLimit, next)
			if err != nil {
				reportErrorf(errorRequestFail, err)
			}
			if holdersJSON {
				b, err := json.Marshal(resp)
				if err != nil {
					reportErrorf("%v", err)
				}
				fmt.Println(string(b))
			} else {
				for _, h := range resp.Holders {
					frozen := ""
					if h.IsFrozen {
						frozen = " frozen"
					}
					fmt.Printf("%s %d%s\n", h.Address, h.Amount, frozen)
				}
			}
			if resp.NextToken == nil {
				return
			}
			if !holdersAll {
				fmt.Fprintf(os.Stderr, "More holders: --round %d --next %s\n", resp.Round, *resp.NextToken)
				return
			}
			// the pages after the first at its round, even without --round
			client, next = client.AtRound(resp.Round), *resp.NextToken
		}
	},
}

var appVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "List the versions of an application's params and global state (time travel)",
	Long:  `List the rounds at which an application was created, updated, deleted or changed its global state, with what changed. Needs a node with EnableTimeTravel.`,
	Args:  validateNoPosArgsFn,
	Run: func(cmd *cobra.Command, _ []string) {
		client := ensureAlgodClient(datadir.EnsureSingleDataDir())
		label := fmt.Sprintf("app:%d", appIdx)
		listVersions(cmd, client, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
			resp, err := client.ApplicationVersions(appIdx, p)
			var ens []versionEntry
			for _, v := range resp.Versions {
				ens = append(ens, versionEntry{v.Round, v.Timestamp, label, v.Deleted, ptrValue(v.Application), appBaseline(appIdx)})
			}
			return resp, resp.NextToken, ens, err
		})
	},
}

var appBoxVersionsCmd = &cobra.Command{
	Use:   "versions",
	Short: "List the versions of an application's boxes (time travel)",
	Long:  `List the rounds at which an application box was created, written or deleted, with its values. Without --name, list the changes of every box of the application, round by round. Needs a node with EnableTimeTravel.`,
	Args:  validateNoPosArgsFn,
	Run: func(cmd *cobra.Command, _ []string) {
		client := ensureAlgodClient(datadir.EnsureSingleDataDir())
		if boxName == "" {
			listVersions(cmd, client, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
				resp, err := client.ApplicationBoxesVersions(appIdx, p)
				var ens []versionEntry
				for _, v := range resp.Versions {
					for _, b := range v.Boxes {
						ens = append(ens, boxVersionEntry(v.Round, v.Timestamp, b.Name, b.Deleted, b.Value))
					}
				}
				return resp, resp.NextToken, ens, err
			})
			return
		}
		name, err := newAppCallBytes(boxName).Raw()
		if err != nil {
			reportErrorf(errorInvalidBoxName, boxName, err)
		}
		listVersions(cmd, client, func(p apiclient.VersionsParams) (any, *string, []versionEntry, error) {
			resp, err := client.ApplicationBoxVersions(appIdx, boxName, p)
			var ens []versionEntry
			for _, v := range resp.Versions {
				ens = append(ens, boxVersionEntry(v.Round, v.Timestamp, name, v.Deleted, v.Value))
			}
			return resp, resp.NextToken, ens, err
		})
	},
}

// boxVersionEntry is a change of box name of the --app-id app.
func boxVersionEntry(rnd basics.Round, ts int64, name []byte, deleted bool, value *[]byte) versionEntry {
	var val any
	if value != nil {
		val = model.Box{Name: name, Value: *value}
	}
	baseline := func(c libgoal.Client) (any, error) {
		box, err := c.GetApplicationBoxByName(appIdx, "b64:"+base64.StdEncoding.EncodeToString(name))
		if err != nil {
			return nil, err
		}
		return model.Box{Name: box.Name, Value: box.Value}, nil
	}
	return versionEntry{rnd, ts, fmt.Sprintf("box:%d:%s", appIdx, ttview.AppCallBytes(name)), deleted, val, baseline}
}

// versionEntry is one object's change in a version.
type versionEntry struct {
	round     basics.Round
	timestamp int64
	label     string
	deleted   bool
	value     any // nil when deleted or without values
	// baseline looks the object up with a client pinned to a round: what the
	// first version of a page with --min-round is compared with.
	baseline func(libgoal.Client) (any, error)
}

// ptrValue returns p as an any, nil (not a typed nil) if p is nil.
func ptrValue[T any](p *T) any {
	if p == nil {
		return nil
	}
	return p
}

func assetBaseline(id basics.AssetIndex) func(libgoal.Client) (any, error) {
	return func(c libgoal.Client) (any, error) {
		a, err := c.AssetInformation(id)
		return a, err
	}
}

func appBaseline(id basics.AppIndex) func(libgoal.Client) (any, error) {
	return func(c libgoal.Client) (any, error) {
		a, err := c.ApplicationInformation(id)
		return a, err
	}
}

// accountVersionEntries splits an account's version into its objects'
// changes, each compared with that object's previous version.
func accountVersionEntries(addr basics.Address, v model.AccountVersion) []versionEntry {
	var ens []versionEntry
	if b := v.Base; b != nil {
		ens = append(ens, versionEntry{v.Round, v.Timestamp, "acct:" + addr.String(), b.Deleted, ptrValue(b.Account),
			func(c libgoal.Client) (any, error) {
				acct, err := c.AccountInformation(addr.String(), false)
				if err != nil || acct.AmountWithoutPendingRewards == 0 {
					return nil, err // an account without algos does not exist
				}
				return acct, nil
			}})
	}
	if v.Assets != nil {
		for _, h := range *v.Assets {
			ens = append(ens, versionEntry{v.Round, v.Timestamp, fmt.Sprintf("holding:%s:%d", addr, h.AssetId), h.Deleted, ptrValue(h.AssetHolding),
				func(c libgoal.Client) (any, error) {
					r, err := c.AccountAssetInformation(addr.String(), h.AssetId)
					if err != nil || r.AssetHolding == nil {
						return nil, err
					}
					return r.AssetHolding, nil
				}})
		}
	}
	if v.AppsLocalState != nil {
		for _, l := range *v.AppsLocalState {
			ens = append(ens, versionEntry{v.Round, v.Timestamp, fmt.Sprintf("local:%s:%d", addr, l.AppId), l.Deleted, ptrValue(l.AppLocalState),
				func(c libgoal.Client) (any, error) {
					r, err := c.AccountApplicationInformation(addr.String(), l.AppId)
					if err != nil || r.AppLocalState == nil {
						return nil, err
					}
					return r.AppLocalState, nil
				}})
		}
	}
	if v.CreatedAssets != nil {
		for _, a := range *v.CreatedAssets {
			ens = append(ens, versionEntry{v.Round, v.Timestamp, fmt.Sprintf("asset:%d", a.AssetId), a.Deleted, ptrValue(a.Asset), assetBaseline(a.AssetId)})
		}
	}
	if v.CreatedApps != nil {
		for _, a := range *v.CreatedApps {
			ens = append(ens, versionEntry{v.Round, v.Timestamp, fmt.Sprintf("app:%d", a.AppId), a.Deleted, ptrValue(a.Application), appBaseline(a.AppId)})
		}
	}
	return ens
}

// viewOptions tells ttview how to show an object's values.
func viewOptions(label string) ttview.Options {
	switch {
	case strings.HasPrefix(label, "box:"):
		return ttview.Options{Bytes: []string{"name", "value"}}
	case strings.HasPrefix(label, "asset:"), strings.HasPrefix(label, "app:"):
		return ttview.Options{Trim: "params."}
	}
	return ttview.Options{}
}

// listVersions prints the pages fetch returns: as JSON, or as the objects'
// changes. It follows next tokens with --all, and otherwise prints the token
// of the next page on stderr.
func listVersions(cmd *cobra.Command, client libgoal.Client, fetch func(apiclient.VersionsParams) (any, *string, []versionEntry, error)) {
	p := apiclient.VersionsParams{Desc: versionsDesc, Limit: versionsLimit, Next: versionsNext, Values: !versionsRoundsOnly}
	if cmd.Flags().Changed("min-round") {
		r := basics.Round(versionsMinRound)
		p.MinRound = &r
	}
	if cmd.Flags().Changed("max-round") {
		r := basics.Round(versionsMaxRound)
		p.MaxRound = &r
	}
	h := ttview.History{W: os.Stdout, Full: versionsDesc}
	seen := map[string]bool{}
	for {
		page, next, entries, err := fetch(p)
		if err != nil {
			reportVersionsError(err)
		}
		if versionsJSON {
			b, err := json.Marshal(page)
			if err != nil {
				reportErrorf("%v", err)
			}
			fmt.Println(string(b))
		} else {
			for _, en := range entries {
				h.Opts = viewOptions(en.label)
				if !seen[en.label] && !h.Full {
					seen[en.label] = true
					if err := h.Baseline(en.label, baselineValue(client, en, p)); err != nil {
						reportErrorf("%v", err)
					}
				}
				if err := h.Add(uint64(en.round), en.timestamp, en.label, en.deleted, en.value); err != nil {
					reportErrorf("%v", err)
				}
			}
		}
		if next == nil || *next == "" {
			return
		}
		if !versionsAll {
			fmt.Fprintf(os.Stderr, "More versions: --next %s\n", *next)
			return
		}
		p.Next = *next
	}
}

// baselineValue is the value of en's object before the range, nil if it did
// not exist then or the range starts at genesis.
func baselineValue(client libgoal.Client, en versionEntry, p apiclient.VersionsParams) any {
	if p.MinRound == nil || *p.MinRound == 0 || en.baseline == nil {
		return nil
	}
	v, err := en.baseline(client.AtRound(*p.MinRound - 1))
	var httpError apiclient.HTTPError
	if errors.As(err, &httpError) && httpError.StatusCode == http.StatusNotFound {
		return nil
	}
	if err != nil {
		reportErrorf("Looking up %s at round %d: %v", en.label, *p.MinRound-1, err)
	}
	return v
}

func reportVersionsError(err error) {
	var httpError apiclient.HTTPError
	if errors.As(err, &httpError) {
		switch {
		case httpError.StatusCode == http.StatusNotFound:
			reportErrorf("The node does not list versions: it needs EnableTimeTravel (%v)", err)
		case httpError.StatusCode == http.StatusBadRequest && strings.Contains(httpError.ErrorString, "too many objects"):
			reportErrorf("%v\nNarrow the versions with --exclude, --assetid or --app-id", err)
		}
	}
	reportErrorf(errorRequestFail, err)
}
