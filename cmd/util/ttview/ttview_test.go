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

package ttview

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"math"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/avm-abi/apps"

	"github.com/algorand/go-algorand/daemon/algod/api/server/v2/generated/model"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/test/partitiontest"
)

func TestAppCallBytes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		input    []byte
		expected string
	}{
		{[]byte("unicode"), "str:unicode"},
		{[]byte{1, 2, 3, 4}, "b64:AQIDBA=="},
		{[]byte{}, "str:"},
		{[]byte("a:b:c"), "str:a:b:c"},
		{[]byte{'a', 0xff}, "b64:Yf8="}, // not UTF-8
	} {
		got := AppCallBytes(tc.input)
		require.Equal(t, tc.expected, got)
		acb, err := apps.NewAppCallBytes(got)
		require.NoError(t, err)
		raw, err := acb.Raw()
		require.NoError(t, err)
		require.Equal(t, tc.input, raw, "round trip")
	}
}

func TestFormatBytes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var addr basics.Address
	addr[0], addr[31] = 1, 2
	require.Equal(t, "addr:"+addr.String(), FormatBytes(addr[:]))
	require.Equal(t, "str:"+strings.Repeat("x", 32), FormatBytes([]byte(strings.Repeat("x", 32))))
	require.Equal(t, "b64:AAE=", FormatBytes([]byte{0, 1}))
	long := FormatBytes(make([]byte, 200))
	require.True(t, strings.HasPrefix(long, "<200 bytes, sha256 "), long)
	require.NotEqual(t, long, FormatBytes(append(make([]byte, 199), 1)))
}

func TestFlatten(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	gs := model.TealKeyValueStore{
		{Key: b64("counter"), Value: model.TealValue{Type: 2, Uint: 42}},
		{Key: b64("owner"), Value: model.TealValue{Type: 1, Bytes: b64("bob")}},
	}
	app := model.Application{Id: 12, Params: &model.ApplicationParams{
		Creator:         "CREATOR",
		ApprovalProgram: []byte{6, 0x81, 1},
		GlobalState:     &gs,
	}}
	fields, err := Flatten(app, Options{})
	require.NoError(t, err)
	require.Equal(t, []Field{
		{"id", "12"},
		{"params.approval-program", "b64:BoEB"},
		{"params.creator", "CREATOR"},
		{"params.global-state[str:counter]", "42"},
		{"params.global-state[str:owner]", "str:bob"},
	}, fields)

	trimmed, err := Flatten(app, Options{Trim: "params."})
	require.NoError(t, err)
	require.Equal(t, "global-state[str:counter]", trimmed[3].Path)

	// keyed arrays, big numbers, the response round and base64 copies left out
	total := uint64(math.MaxUint64)
	name := "USD"
	acct := model.Account{
		Address: "A", Amount: math.MaxUint64, Round: 7,
		Assets:        &[]model.AssetHolding{{AssetID: 31, Amount: 5}, {AssetID: 9, Amount: 1}},
		CreatedAssets: &[]model.Asset{{Index: 31, Params: &model.AssetParams{Total: total, Name: &name, NameB64: &[]byte{'U'}}}},
	}
	fields, err = Flatten(acct, Options{})
	require.NoError(t, err)
	got := map[string]string{}
	for _, f := range fields {
		got[f.Path] = f.Value
	}
	require.Equal(t, "18446744073709551615", got["amount"])
	require.Equal(t, "5", got["assets[31].amount"])
	require.Equal(t, "1", got["assets[9].amount"])
	require.Equal(t, "18446744073709551615", got["created-assets[31].params.total"])
	require.Equal(t, "USD", got["created-assets[31].params.name"])
	require.NotContains(t, got, "round")
	require.NotContains(t, got, "created-assets[31].params.name-b64")

	// keys are 32 bytes, but not addresses
	acct.Participation = &model.AccountParticipation{VoteParticipationKey: make([]byte, 32), SelectionParticipationKey: make([]byte, 32)}
	fields, err = Flatten(acct, Options{})
	require.NoError(t, err)
	for _, f := range fields {
		if strings.HasPrefix(f.Path, "participation.") && strings.HasSuffix(f.Path, "-key") {
			require.True(t, strings.HasPrefix(f.Value, "b64:"), "%s: %s", f.Path, f.Value)
		}
	}

	// box: name and value are bytes when the caller says so
	box := model.Box{Name: []byte("b"), Value: []byte{0, 0, 0, 1}, Round: 3}
	fields, err = Flatten(box, Options{Bytes: []string{"name", "value"}})
	require.NoError(t, err)
	require.Equal(t, []Field{{"name", "str:b"}, {"value", "b64:AAAAAQ=="}}, fields)

	raw, err := json.Marshal(box)
	require.NoError(t, err)
	fromRaw, err := Flatten(json.RawMessage(raw), Options{Bytes: []string{"name", "value"}})
	require.NoError(t, err)
	require.Equal(t, fields, fromRaw)
}

func TestDiff(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	old := []Field{{"a", "1"}, {"b", "2"}, {"gone", "x"}}
	new := []Field{{"a", "1"}, {"b", "3"}, {"c", "4"}}
	require.Equal(t, []Change{
		{Path: "b", Old: "2", New: "3"},
		{Path: "c", New: "4", Added: true},
		{Path: "gone", Old: "x", Removed: true},
	}, Diff(old, new))
	require.Empty(t, Diff(new, new))
}

func TestHistory(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var buf bytes.Buffer
	h := History{W: &buf}
	app := func(n uint64) model.Application {
		gs := model.TealKeyValueStore{{Key: b64("n"), Value: model.TealValue{Type: 2, Uint: n}}}
		return model.Application{Id: 5, Params: &model.ApplicationParams{Creator: "C", GlobalState: &gs}}
	}
	require.NoError(t, h.Baseline("app:5", nil))
	require.NoError(t, h.Add(10, 0, "app:5", false, app(1)))
	require.NoError(t, h.Add(11, 60, "app:5", false, app(2)))
	require.NoError(t, h.Add(12, 120, "app:5", true, nil))
	require.NoError(t, h.Add(3, 0, "other", false, app(7))) // no baseline: in full
	require.Equal(t, `10  1970-01-01 00:00:00  app:5 created
  id                          5
  params.creator              C
  params.global-state[str:n]  1
11  1970-01-01 00:01:00  app:5 updated
  params.global-state[str:n]  1 -> 2
12  1970-01-01 00:02:00  app:5 deleted
3  1970-01-01 00:00:00  other updated
  id                          5
  params.creator              C
  params.global-state[str:n]  7
`, buf.String())
}

func TestWriteTable(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var buf bytes.Buffer
	require.NoError(t, WriteTable(&buf, []string{"id", "name"}, [][]string{{"1", "a"}, {"200", "bb"}}))
	require.Equal(t, "ID   NAME\n1    a\n200  bb\n", buf.String())
}

func b64(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
