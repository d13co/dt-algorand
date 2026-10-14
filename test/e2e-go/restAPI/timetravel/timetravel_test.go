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

package timetravel

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/daemon/algod/api/client"
	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/data/transactions/logic"
	"github.com/algorand/go-algorand/libgoal"
	helper "github.com/algorand/go-algorand/test/e2e-go/restAPI"
	"github.com/algorand/go-algorand/test/framework/fixtures"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// snapshot is what the node returned for the test's objects right after a
// round: what a client pinned to that round must get later.
type snapshot struct {
	acct  model.Account
	app   model.Application
	box   model.BoxResponse
	asset model.Asset
}

// TestTimeTravelClients checks the REST client and libgoal against a time
// travel node: lookups pinned to past rounds, versions with paging, and
// tools/ttdump on the stopped node's store against the node's answers.
func TestTimeTravelClients(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)
	a := require.New(fixtures.SynchronizedTest(t))

	var f fixtures.RestClientFixture
	f.Setup(t, filepath.Join("nettemplates", "DevModeTimeTravelNetwork.json"))
	defer f.Shutdown()
	c := f.LibGoalClient
	wh, err := c.GetUnencryptedWalletHandle()
	a.NoError(err)
	richest, err := f.GetRichestAccount()
	a.NoError(err)
	sender := richest.Address

	// every transaction is a round of its own in dev mode
	send := func(tx transactions.Transaction) helperInfo {
		tx, err := c.FillUnsignedTxTemplate(sender, 0, 0, 0, tx)
		a.NoError(err)
		txid, err := c.SignAndBroadcastTransaction(wh, nil, tx)
		a.NoError(err)
		info, err := helper.WaitForTransaction(t, c, txid, 30*time.Second)
		a.NoError(err)
		a.NotNil(info.ConfirmedRound)
		return helperInfo{round: *info.ConfirmedRound, app: info.ApplicationIndex, asset: info.AssetIndex}
	}

	approval, err := logic.AssembleString(`#pragma version 8
txn ApplicationID
bz end
byte "n"; byte "n"; app_global_get; int 1; +; app_global_put
byte "b"; txn ApplicationArgs 0; box_put
end:
int 1`)
	a.NoError(err)
	clear, err := logic.AssembleString("#pragma version 8\nint 1")
	a.NoError(err)
	tx, err := c.MakeUnsignedApplicationCallTx(0, nil, libgoal.RefBundle{}, transactions.NoOpOC, approval.Program, clear.Program,
		basics.StateSchema{NumUint: 1}, basics.StateSchema{}, 0, 0)
	a.NoError(err)
	created := send(tx)
	a.NotNil(created.app)
	app := *created.app
	tx, err = c.ConstructPayment(sender, app.Address().String(), 0, 10_000_000, nil, "", [32]byte{}, 0, 0)
	a.NoError(err)
	funded := send(tx) // creates the app's account
	tx, err = c.MakeUnsignedAssetCreateTx(1000, false, sender, sender, "", "", "tt", "time travel", "", nil, 0)
	a.NoError(err)
	assetCreated := send(tx)
	a.NotNil(assetCreated.asset)
	asa := *assetCreated.asset

	snaps := map[basics.Round]snapshot{}
	var callRounds, configRounds []basics.Round
	for i := 0; i < 6; i++ {
		tx, err = c.MakeUnsignedAppNoOpTx(app, [][]byte{binary.BigEndian.AppendUint64(nil, uint64(i))},
			libgoal.RefBundle{Boxes: []basics.BoxRef{{App: app, Name: "b"}}}, 0)
		a.NoError(err)
		r := send(tx).round
		callRounds = append(callRounds, r)
		snaps[r] = snap(t, c, sender, app, asa)
		if i%2 == 1 {
			reserve := richest.Address
			if i%4 == 1 {
				reserve = app.Address().String()
			}
			tx, err = c.MakeUnsignedAssetConfigTx(sender, asa, &sender, &reserve, nil, nil)
			a.NoError(err)
			r = send(tx).round
			configRounds = append(configRounds, r)
			snaps[r] = snap(t, c, sender, app, asa)
		}
	}
	status, err := c.Status()
	a.NoError(err)
	latest := status.LastRound

	// lookups pinned to past rounds answer as the node did then
	for r, want := range snaps {
		got := snap(t, c.AtRound(r), sender, app, asa)
		a.Equal(want, got, "round %d", r)
	}
	future := c.AtRound(latest + 100)
	_, err = future.AccountInformation(sender, true)
	a.Error(err, "a round past the latest")

	// versions, through several pages
	allPages := func(fetch func(client.VersionsParams) ([]any, *string, error), p client.VersionsParams) []any {
		var out []any
		for {
			items, next, err := fetch(p)
			a.NoError(err)
			out = append(out, items...)
			if next == nil {
				return out
			}
			p.Next = *next
		}
	}
	rounds := func(items []any) []basics.Round {
		var rs []basics.Round
		for _, it := range items {
			var v struct {
				Round basics.Round `json:"round"`
			}
			b, err := json.Marshal(it)
			a.NoError(err)
			a.NoError(json.Unmarshal(b, &v))
			rs = append(rs, v.Round)
		}
		return rs
	}
	appVersions := func(p client.VersionsParams) ([]any, *string, error) {
		resp, err := c.ApplicationVersions(app, p)
		return anys(resp.Versions), resp.NextToken, err
	}
	boxVersions := func(p client.VersionsParams) ([]any, *string, error) {
		resp, err := c.ApplicationBoxVersions(app, "str:b", p)
		return anys(resp.Versions), resp.NextToken, err
	}
	boxesVersions := func(p client.VersionsParams) ([]any, *string, error) {
		resp, err := c.ApplicationBoxesVersions(app, p)
		for _, v := range resp.Versions {
			a.Len(v.Boxes, 1, "the app has one box")
			a.Equal([]byte("b"), v.Boxes[0].Name)
		}
		return anys(resp.Versions), resp.NextToken, err
	}
	assetVersions := func(p client.VersionsParams) ([]any, *string, error) {
		resp, err := c.AssetVersions(asa, p)
		return anys(resp.Versions), resp.NextToken, err
	}
	appRounds := append([]basics.Round{created.round}, callRounds...)
	a.Equal(appRounds, rounds(allPages(appVersions, client.VersionsParams{Limit: 2, Values: true})))
	a.Equal(callRounds, rounds(allPages(boxVersions, client.VersionsParams{Limit: 4, Values: true})))
	a.Equal(callRounds, rounds(allPages(boxesVersions, client.VersionsParams{Limit: 3, Values: true})))
	a.Equal(append([]basics.Round{assetCreated.round}, configRounds...), rounds(allPages(assetVersions, client.VersionsParams{Values: true})))
	desc := rounds(allPages(appVersions, client.VersionsParams{Desc: true, Limit: 3}))
	a.Equal(callRounds[len(callRounds)-1], desc[0])
	from := callRounds[2]
	a.Equal(callRounds[2:], rounds(allPages(boxVersions, client.VersionsParams{MinRound: &from})))
	acctRounds := rounds(allPages(func(p client.VersionsParams) ([]any, *string, error) {
		resp, err := c.AccountVersions(sender, "all", p)
		return anys(resp.Versions), resp.NextToken, err
	}, client.VersionsParams{Values: true}))
	for _, r := range callRounds {
		a.Contains(acctRounds, r, "the sender paid a fee in round %d", r)
	}

	// goal against the node
	dataDir := f.PrimaryDataDir()
	goal := func(args ...string) string {
		out, err := exec.Command(filepath.Join(f.GetBinDir(), "goal"), append(args, "-d", dataDir)...).CombinedOutput()
		a.NoError(err, "goal %v: %s", args, out)
		return string(out)
	}
	r0 := callRounds[0]
	a.Contains(goal("ledger", "supply", "--round", fmt.Sprint(r0)), fmt.Sprintf("As of round %d\nRound: %d", r0, r0))
	a.Contains(goal("account", "balance", "-a", sender, "--round", fmt.Sprint(r0)), fmt.Sprintf("%d microAlgos", snaps[r0].acct.Amount))
	a.Contains(goal("app", "box", "info", "--app-id", fmt.Sprint(app), "-n", "str:b", "--round", fmt.Sprint(r0)),
		"Value: b64:"+base64.StdEncoding.EncodeToString(snaps[r0].box.Value))
	appID := fmt.Sprintf("ID %d", app)
	a.Contains(goal("account", "info", "-a", sender, "--round", fmt.Sprint(created.round)), appID)
	a.NotContains(goal("account", "info", "-a", sender, "--round", fmt.Sprint(created.round-1)), appID)

	var pageRounds []basics.Round // the pages of --json, as the client gets them
	for _, line := range strings.Split(strings.TrimSpace(goal("app", "versions", "--app-id", fmt.Sprint(app), "--json", "--all", "--limit", "2")), "\n") {
		var page model.ApplicationVersionsResponse
		a.NoError(json.Unmarshal([]byte(line), &page), line)
		for _, v := range page.Versions {
			pageRounds = append(pageRounds, v.Round)
		}
	}
	a.Equal(appRounds, pageRounds)
	boxSpec := fmt.Sprintf("box:%d:str:b", app)
	text := goal("app", "box", "versions", "--app-id", fmt.Sprint(app), "-n", "str:b")
	a.Equal(len(callRounds), strings.Count(text, boxSpec+" "), text)
	a.Contains(text, boxSpec+" created")
	a.Contains(text, "value  b64:")
	text = goal("asset", "versions", "--assetid", fmt.Sprint(asa), "--rounds-only")
	lines := strings.Split(strings.TrimSpace(text), "\n")
	a.Len(lines, 1+len(configRounds), text)
	a.True(strings.HasSuffix(lines[0], fmt.Sprintf("asset:%d created", asa)), lines[0])
	a.True(strings.HasSuffix(lines[1], fmt.Sprintf("asset:%d changed", asa)), lines[1])
	// the app's account did not exist before its funding: a creation, not a change from zero
	text = goal("account", "versions", "-a", app.Address().String(), "--exclude", "all", "--min-round", fmt.Sprint(funded.round))
	a.True(strings.HasPrefix(text, fmt.Sprintf("%d ", funded.round)), text)
	a.Contains(strings.SplitN(text, "\n", 2)[0], "acct:"+app.Address().String()+" created")

	// ttdump on the stopped node's store answers as the node did
	ttdump := filepath.Join(f.GetBinDir(), "ttdump")
	a.FileExists(ttdump, "ttdump is built with the other binaries (make install)")
	appVersionItems := allPages(appVersions, client.VersionsParams{Values: true})
	run := func(args ...string) [][]byte {
		cmd := exec.Command(ttdump, append([]string{"-d", dataDir, "-json"}, args...)...)
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		out, err := cmd.Output()
		a.NoError(err, "ttdump %v: %s", args, stderr.String())
		return bytes.Split(bytes.TrimSpace(out), []byte("\n"))
	}
	// A stopped node's store ends at its last account DB commit, which trails
	// the latest round by MaxAcctLookback (4) and happens on a new round once
	// the flush interval (5 s) has passed: add rounds, wait, add one more, and
	// stop the node, until the store has every round compared below.
	nc, err := f.GetNodeController("Node")
	a.NoError(err)
	target := callRounds[len(callRounds)-1]
	for attempt := 0; ; attempt++ {
		pay := func(i int) {
			tx, err := c.ConstructPayment(sender, sender, 0, 0, []byte{byte(attempt), byte(i)}, "", [32]byte{}, 0, 0)
			a.NoError(err)
			send(tx)
		}
		for i := 0; i < 5; i++ {
			pay(i)
		}
		time.Sleep(6 * time.Second)
		pay(5)
		time.Sleep(time.Second) // the commit runs after the round
		a.NoError(nc.StopAlgod())
		var info struct {
			Last basics.Round `json:"last-round"`
		}
		a.NoError(json.Unmarshal(run("info")[0], &info))
		if info.Last >= target {
			break
		}
		a.Less(attempt, 2, "the store is at round %d, not %d", info.Last, target)
		_, err = f.StartNode(dataDir)
		a.NoError(err)
	}
	value := func(line []byte) any { // the value of a get
		var v struct {
			Value json.RawMessage `json:"value"`
		}
		a.NoError(json.Unmarshal(line, &v))
		return normalize(t, v.Value)
	}
	for _, r := range []basics.Round{callRounds[0], configRounds[0], callRounds[len(callRounds)-1]} {
		want := snaps[r]
		rs := fmt.Sprint(r)
		a.Equal(normalize(t, want.acct), normalize(t, run("account", sender, "-r", rs)[0]), "account at %d", r)
		a.Equal(normalize(t, want.app), value(run("get", fmt.Sprintf("app:%d", app), "-r", rs)[0]), "app at %d", r)
		a.Equal(normalize(t, want.asset), value(run("get", fmt.Sprintf("asset:%d", asa), "-r", rs)[0]), "asset at %d", r)
		a.Equal(normalize(t, want.box), value(run("get", fmt.Sprintf("box:%d:str:b", app), "-r", rs)[0]), "box at %d", r)
	}
	versionLines := run("versions", fmt.Sprintf("app:%d", app))
	a.Len(versionLines, len(appVersionItems))
	for i, line := range versionLines {
		a.Equal(normalize(t, appVersionItems[i]), normalize(t, line), "app version %d", i)
	}
}

// TestTimeTravelGoalWithoutTimeTravel checks goal's errors on a node without
// EnableTimeTravel.
func TestTimeTravelGoalWithoutTimeTravel(t *testing.T) {
	partitiontest.PartitionTest(t)
	defer fixtures.ShutdownSynchronizedTest(t)
	a := require.New(fixtures.SynchronizedTest(t))

	var f fixtures.RestClientFixture
	f.Setup(t, filepath.Join("nettemplates", "DevModeNetwork.json"))
	defer f.Shutdown()
	goal := func(args ...string) string {
		out, err := exec.Command(filepath.Join(f.GetBinDir(), "goal"), append(args, "-d", f.PrimaryDataDir())...).CombinedOutput()
		a.Error(err, "goal %v: %s", args, out)
		return string(out)
	}
	a.Contains(goal("ledger", "supply", "--round", "0"), "requires EnableTimeTravel")
	a.Contains(goal("app", "versions", "--app-id", "1"), "does not list versions: it needs EnableTimeTravel")
}

type helperInfo struct {
	round basics.Round
	app   *basics.AppIndex
	asset *basics.AssetIndex
}

func snap(t *testing.T, c libgoal.Client, sender string, app basics.AppIndex, asa basics.AssetIndex) snapshot {
	a := require.New(t)
	var s snapshot
	var err error
	s.acct, err = c.AccountInformation(sender, true)
	a.NoError(err)
	s.app, err = c.ApplicationInformation(app)
	a.NoError(err)
	s.box, err = c.GetApplicationBoxByName(app, "str:b")
	a.NoError(err)
	s.asset, err = c.AssetInformation(asa)
	a.NoError(err)
	return s
}

func anys[T any](s []T) []any {
	out := make([]any, len(s))
	for i := range s {
		out[i] = s[i]
	}
	return out
}

// normalize decodes JSON, or a value's encoding/json form, into maps and
// slices, so that equal JSON compares equal whatever the field order.
func normalize(t *testing.T, v any) any {
	b, ok := v.([]byte)
	if raw, isRaw := v.(json.RawMessage); isRaw {
		b, ok = raw, true
	}
	if !ok {
		var err error
		b, err = json.Marshal(v)
		require.NoError(t, err)
	}
	var out any
	require.NoError(t, json.Unmarshal(b, &out), string(b))
	return out
}
