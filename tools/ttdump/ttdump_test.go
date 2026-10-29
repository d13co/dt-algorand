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
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/ledger/store/trackerdb"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestSpecRoundTrip(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var addr basics.Address
	addr[0], addr[31] = 7, 9
	a := addr.String()
	for _, s := range []string{
		"acct:" + a,
		"holding:" + a + ":31",
		"local:" + a + ":12",
		"asset:31", "app:12",
		"box:12:str:name",
		"box:12:str:a:b:c", // colons in the name
		"box:12:b64:AAEC",  // zero bytes
		"box:0:str:",       // empty name
		"kv:str:other",     // not a box
		"totals",
		"created:" + a + ":asset:31",
		"created:" + a + ":app:12",
		"hex:ff0102",
	} {
		o, err := parseSpec(s)
		require.NoError(t, err, s)
		require.Equal(t, s, o.String(), "round trip")
		require.Equal(t, o.key, objectOf(o.key).key)
		require.Equal(t, s, objectOf(o.key).String(), "from the key")
	}

	o, err := parseSpec(a) // bare address
	require.NoError(t, err)
	require.Equal(t, "acct:"+a, o.String())
	o, err = parseSpec("box:12:addr:" + a)
	require.NoError(t, err)
	require.Equal(t, apps.MakeBoxKey(12, string(addr[:])), o.kv)
	o, err = parseSpec("kv:str:" + apps.MakeBoxKey(12, "x"))
	require.NoError(t, err)
	require.Equal(t, "box:12:str:x", o.String(), "a box's raw kv key is shown as the box")
	o, err = parseSpec("hex:" + fmt.Sprintf("%x", timetravel.AppParamsKey(5)))
	require.NoError(t, err)
	require.Equal(t, "app:5", o.String())

	for _, bad := range []string{"", "acct:nope", "holding:" + a, "asset:x", "box:12", "box:x:str:a", "created:" + a + ":thing:1", "totals:1", "nope:1", "hex:zz"} {
		_, err := parseSpec(bad)
		require.Error(t, err, bad)
	}
}

var (
	addrA = basics.Address{1}
	addrB = basics.Address{2}
)

const (
	asa = basics.AssetIndex(1001)
	app = basics.AppIndex(1002)
)

// testStore writes a small history: accounts A and B, A's asset that B
// holds, A's app that B opts into, with a box. Rounds 0..6.
func testStore(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), "ledger.timetravel")
	s, err := timetravel.Open(dir, timetravel.Options{DedupEntries: -1}) // keep the no-op writes
	require.NoError(t, err)
	defer s.Close()
	_, err = s.IndexHolders(context.Background(), nil)
	require.NoError(t, err)

	acct := func(a basics.Address, algos uint64, assets, apps uint64) timetravel.Change {
		var ba trackerdb.BaseAccountData
		ad := ledgercore.AccountData{AccountBaseData: ledgercore.AccountBaseData{
			MicroAlgos: basics.MicroAlgos{Raw: algos}, TotalAssets: assets, TotalAppLocalStates: apps}}
		ba.SetCoreAccountData(&ad)
		return timetravel.Change{Key: timetravel.AccountKey(a), Value: protocol.Encode(&ba)}
	}
	totals := func(n uint64) timetravel.Change {
		t := ledgercore.AccountTotals{}
		t.Offline.Money.Raw = n
		return timetravel.Change{Key: timetravel.TotalsKey, Value: protocol.Encode(&t)}
	}
	appParams := func(n uint64) timetravel.Change {
		p := basics.AppParams{ApprovalProgram: []byte{6, 0x81, 1}, ClearStateProgram: []byte{6, 0x81, 1},
			GlobalState: basics.TealKeyValue{"n": {Type: basics.TealUintType, Uint: n}}}
		return timetravel.Change{Key: timetravel.AppParamsKey(app), Value: append(addrA[:], protocol.Encode(&p)...)}
	}
	asaParams := basics.AssetParams{Total: 1000, UnitName: "x", Manager: addrA}
	box := timetravel.KvKey(apps.MakeBoxKey(uint64(app), "b"))
	box2 := timetravel.KvKey(apps.MakeBoxKey(uint64(app), "c"))
	rounds := [][]timetravel.Change{
		{totals(100), acct(addrA, 60, 0, 0), acct(addrB, 40, 0, 0)},
		{totals(100), acct(addrA, 59, 0, 0),
			{Key: timetravel.AssetParamsKey(asa), Value: append(addrA[:], protocol.Encode(&asaParams)...)},
			{Key: timetravel.CreatedKey(addrA, basics.AssetCreatable, basics.CreatableIndex(asa)), Value: []byte{}}},
		{totals(100), acct(addrB, 39, 1, 0), {Key: timetravel.HoldingKey(addrB, asa), Value: protocol.Encode(&basics.AssetHolding{Amount: 0})}},
		{totals(100), appParams(1), {Key: timetravel.CreatedKey(addrA, basics.AppCreatable, basics.CreatableIndex(app)), Value: []byte{}},
			{Key: box, Value: []byte("v1")}},
		{totals(100), appParams(2), {Key: box, Value: []byte("v2")}, {Key: box2, Value: []byte("c1")},
			{Key: timetravel.LocalKey(addrB, app), Value: protocol.Encode(&basics.AppLocalState{Schema: basics.StateSchema{NumUint: 1}})}},
		{totals(100), {Key: box, Deleted: true}, {Key: box2, Deleted: true}, {Key: timetravel.HoldingKey(addrB, asa), Value: protocol.Encode(&basics.AssetHolding{Amount: 5})}},
		{totals(99), {Key: timetravel.HoldingKey(addrB, asa), Deleted: true}, acct(addrB, 39, 0, 1)},
	}
	for rnd, changes := range rounds {
		hdr := bookkeeping.BlockHeader{Round: basics.Round(rnd), TimeStamp: 1_700_000_000 + int64(rnd)*3, TxnCounter: uint64(1000 + rnd)}
		hdr.CurrentProtocol = protocol.ConsensusCurrentVersion
		hdr.RewardsLevel = 0
		var expired *uint64
		if rnd == 2 {
			e := uint64(17)
			expired = &e
		}
		require.NoError(t, s.AddRound(uint64(rnd), changes, protocol.Encode(&hdr), expired))
	}
	return dir
}

func ttdump(t *testing.T, args ...string) string {
	var out, errOut bytes.Buffer
	err := run(args, &out, &errOut)
	require.NoError(t, err, "ttdump %s: %s", strings.Join(args, " "), errOut.String())
	return out.String()
}

// norm collapses the spaces that align columns, line by line.
func norm(s string) string {
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.Join(strings.Fields(l), " ")
	}
	return strings.Join(lines, "\n")
}

func ttdumpErr(t *testing.T, args ...string) error {
	var out, errOut bytes.Buffer
	return run(args, &out, &errOut)
}

func jsonLines(t *testing.T, s string) []map[string]any {
	var out []map[string]any
	for line := range strings.SplitSeq(strings.TrimSpace(s), "\n") {
		if line == "" {
			continue
		}
		var m map[string]any
		require.NoError(t, json.Unmarshal([]byte(line), &m), line)
		out = append(out, m)
	}
	return out
}

func TestCommands(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	dir := testStore(t)
	st := func(args ...string) string { return norm(ttdump(t, append([]string{"-store", dir}, args...)...)) }
	stj := func(args ...string) []map[string]any {
		return jsonLines(t, ttdump(t, append([]string{"-store", dir, "-json"}, args...)...))
	}
	boxSpec := fmt.Sprintf("box:%d:str:b", app)

	info := stj("info")[0]
	require.EqualValues(t, 0, info["first-round"])
	require.EqualValues(t, 6, info["last-round"])
	require.Contains(t, st("info"), "0..6")

	// get: the value, the round it changed, flags after the object
	g := stj("get", fmt.Sprintf("app:%d", app), "-r", "3")[0]
	require.EqualValues(t, 3, g["changed-at"])
	require.EqualValues(t, 3, g["round"])
	text := st("get", fmt.Sprintf("app:%d", app))
	require.Contains(t, text, "changed at round 4")
	require.Contains(t, text, "global-state[str:n] 2")
	g = stj("get", boxSpec, "-r", "4")[0]
	require.Equal(t, "djI=", g["value"].(map[string]any)["value"]) // "v2"
	g = stj("get", boxSpec)[0]
	require.Equal(t, true, g["deleted"])
	require.EqualValues(t, 5, g["changed-at"])
	g = stj("get", boxSpec, "-r", "2")[0]
	require.EqualValues(t, 3, g["created-at"])
	require.Nil(t, g["value"])
	require.Contains(t, st("get", "totals", "-r", "4"), "changed at round 0", "no-op writes are not versions")
	g = stj("get", "acct:"+addrA.String(), "-r", "0")[0]
	require.EqualValues(t, 60, g["value"].(map[string]any)["amount"])
	require.Error(t, ttdumpErr(t, "-store", dir, "get", "totals", "-r", "7"), "past the last round")

	// versions: diffs in text, REST items in JSON
	text = st("versions", fmt.Sprintf("app:%d", app))
	require.Contains(t, text, fmt.Sprintf("3 2023-11-14 22:13:29 app:%d created", app))
	require.Contains(t, text, "global-state[str:n] 1 -> 2")
	vs := stj("versions", boxSpec)
	require.Len(t, vs, 3)
	require.EqualValues(t, 5, vs[2]["round"])
	require.Equal(t, true, vs[2]["deleted"])
	text = st("versions", boxSpec, "-from", "4")
	require.Contains(t, text, "value str:v1 -> str:v2", "-from diffs against the value before it")
	text = st("versions", boxSpec, "-from", "0", "-n", "1")
	require.Contains(t, text, "created")
	require.NotContains(t, text, "deleted")
	vs = stj("versions", "acct:"+addrB.String())
	rounds := []float64{}
	for _, v := range vs {
		rounds = append(rounds, v["round"].(float64))
	}
	require.Equal(t, []float64{0, 2, 4, 5, 6}, rounds, "base record, holding and local state versions")
	text = st("versions", "acct:"+addrB.String(), "-from", "5")
	require.Contains(t, text, fmt.Sprintf("holding:%s:%d updated", addrB, asa))
	require.Contains(t, text, "amount 0 -> 5")
	require.Contains(t, text, fmt.Sprintf("holding:%s:%d deleted", addrB, asa))
	vs = stj("versions", "totals")
	require.Len(t, vs, 2)
	require.EqualValues(t, 6, vs[1]["round"])
	// every box of the app
	feed := fmt.Sprintf("boxes:%d", app)
	vs = stj("versions", feed)
	require.Len(t, vs, 3)
	require.EqualValues(t, 4, vs[1]["round"])
	require.Len(t, vs[1]["boxes"], 2)
	require.Equal(t, map[string]any{"name": "Yw==", "deleted": false, "value": "YzE="}, vs[1]["boxes"].([]any)[1]) // "c", "c1"
	require.Equal(t, map[string]any{"name": "Yw==", "deleted": true}, vs[2]["boxes"].([]any)[1])
	text = st("versions", feed, "-from", "4")
	require.Contains(t, text, "value str:v1 -> str:v2")
	require.Contains(t, text, fmt.Sprintf("box:%d:str:c created", app))
	require.Contains(t, text, fmt.Sprintf("box:%d:str:c deleted", app))
	require.Len(t, stj("versions", feed, "-desc", "-n", "1"), 1)
	require.Error(t, ttdumpErr(t, "-store", dir, "versions", "boxes:x"))
	require.EqualValues(t, 0, info["box-index-from"])
	require.Contains(t, st("info"), "box index complete")
	idx := stj("raw", "-prefix", "b")
	require.Len(t, idx, 5, "an entry per box write")
	require.Equal(t, boxSpec, idx[0]["object"])
	require.EqualValues(t, 3, idx[0]["round"])
	// the holders index: addrB held asa from round 2 to round 6
	holding := fmt.Sprintf("holding:%s:%d", addrB, asa)
	require.Equal(t, "complete", info["holders-index"])
	require.Contains(t, st("info"), "holders index complete")
	hidx := stj("raw", "-prefix", "a")
	require.Len(t, hidx, 1)
	require.Equal(t, holding, hidx[0]["object"])
	require.Equal(t, []any{2.0, 6.0}, hidx[0]["intervals"])
	require.Contains(t, st("raw", "-prefix", "a"), holding+" 2 rounds 2 6")

	// account: the REST account with its resources
	acct := stj("account", addrB.String(), "-r", "5")[0]
	require.EqualValues(t, 5, acct["round"])
	require.Len(t, acct["assets"], 1)
	require.Len(t, acct["apps-local-state"], 1)
	acct = stj("account", addrA.String(), "-r", "4")[0]
	require.Len(t, acct["created-assets"], 1)
	require.Len(t, acct["created-apps"], 1)

	// ls at a round and ever
	ls := stj("ls", "accounts")
	require.Len(t, ls, 2)
	require.Contains(t, st("ls", "boxes", fmt.Sprint(app), "-r", "4"), "str:v2")
	require.Contains(t, st("ls", "boxes", fmt.Sprint(app)), "nothing at round 6")
	ever := stj("ls", "boxes", fmt.Sprint(app), "-ever")
	require.Len(t, ever, 2)
	require.Equal(t, boxSpec, ever[0]["object"])
	require.EqualValues(t, 3, ever[0]["first-round"])
	require.EqualValues(t, 5, ever[0]["last-round"])
	require.Equal(t, false, ever[0]["live"])
	require.Len(t, stj("ls", "holdings", addrB.String(), "-r", "5"), 1)
	require.Len(t, stj("ls", "created", addrA.String()), 2)
	require.Len(t, stj("ls", "accounts", "-n", "1"), 1)
	require.Len(t, stj("ls", "accounts", "-after", "acct:"+addrA.String()), 1)

	// headers
	h := stj("header", "2")[0]
	require.EqualValues(t, 17, h["expired-stake"])
	require.Len(t, h["digest"], 16)
	require.Len(t, stj("header", "1", "3"), 3)
	require.Contains(t, st("header", "0", "6"), "TXN COUNTER")

	// raw and stats
	raws := stj("raw", "-prefix", boxSpec)
	require.NotEmpty(t, raws)
	require.Equal(t, boxSpec, raws[0]["object"])
	require.Len(t, stj("raw", "-prefix", "H"), 7)
	require.Len(t, stj("raw", "-n", "2"), 2)
	stats := stj("stats")[0]
	total := 0.0
	for _, k := range stats["kinds"].([]any) {
		km := k.(map[string]any)
		total += km["records"].(float64)
		if km["kind"] == "T" {
			require.EqualValues(t, 7, km["records"])
			require.EqualValues(t, 5, km["noops"], "totals rewritten unchanged in rounds 1..5")
		}
	}
	require.Positive(t, total)
}
