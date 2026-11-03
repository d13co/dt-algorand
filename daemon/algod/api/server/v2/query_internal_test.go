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

package v2

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-codec/codec"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestConvertQueryRequestBoxes: a box of the called app has index 0, another app's its place in
// foreign-apps plus 1; an app not listed is added, without touching the request's slice.
func TestConvertQueryRequestBoxes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	apps := make([]basics.AppIndex, 1, 4)
	apps[0] = 20
	r := PreEncodedQueryRequest{
		AppID: 10,
		Calls: []PreEncodedQueryCall{
			{ForeignApps: apps, Boxes: []PreEncodedQueryBox{
				{App: 0, Name: []byte("a")}, {App: 10, Name: []byte("b")}, {App: 20, Name: []byte("c")},
				{App: 30, Name: []byte("d")}, {App: 30, Name: []byte("e")},
			}},
			{ForeignApps: apps},
		},
	}
	request, err := convertQueryRequest(r)
	require.NoError(t, err)
	require.Equal(t, []transactions.BoxRef{
		{Index: 0, Name: []byte("a")}, {Index: 0, Name: []byte("b")}, {Index: 1, Name: []byte("c")},
		{Index: 2, Name: []byte("d")}, {Index: 2, Name: []byte("e")},
	}, request.Calls[0].Boxes)
	require.Equal(t, []basics.AppIndex{20, 30}, request.Calls[0].ForeignApps)
	require.Equal(t, []basics.AppIndex{20}, request.Calls[1].ForeignApps)
	require.Equal(t, []basics.AppIndex{20}, apps)

	_, err = convertQueryRequest(PreEncodedQueryRequest{Calls: []PreEncodedQueryCall{{Accounts: []string{"nope"}}}})
	require.ErrorContains(t, err, `call 0: account "nope"`)
	_, err = convertQueryRequest(PreEncodedQueryRequest{Budget: 1 << 40})
	require.ErrorContains(t, err, "over the limit")
}

// TestQueryRequestListsFields: queryRequestLists has the fields of PreEncodedQueryRequest, in the
// same order and with the same tags, its lists left encoded.
func TestQueryRequestListsFields(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	full, lists := reflect.TypeOf(PreEncodedQueryRequest{}), reflect.TypeOf(queryRequestLists{})
	require.Equal(t, full.NumField(), lists.NumField())
	for i := range full.NumField() {
		f, l := full.Field(i), lists.Field(i)
		require.Equal(t, f.Name, l.Name)
		require.Equal(t, f.Tag, l.Tag, f.Name)
		if f.Name != "Calls" && f.Name != "Rounds" {
			require.Equal(t, f.Type, l.Type, f.Name)
		}
	}
}

// queryTestLimits are the default limits, with room for the lists the tests count.
func queryTestLimits() simulation.QueryLimits {
	return simulation.QueryLimits{MaxCalls: 1 << 20, MaxRounds: 256, MaxTotalCalls: 1 << 30}
}

// TestDecodeQueryRequest: a request decodes as a full decode would, in both codecs, and the counts
// of its lists are what it holds.
func TestDecodeQueryRequest(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	// Strings with the characters the JSON count must skip.
	tricky := []string{`a,b`, `]}`, `"[,`, `\\`, `\",`}
	for _, n := range []int{0, 1, 15, 16, 17, 70000} {
		calls := make([]PreEncodedQueryCall, n)
		for i := range calls {
			calls[i] = PreEncodedQueryCall{Args: [][]byte{[]byte("x")}, Accounts: tricky[:i%len(tricky)+1],
				Boxes: []PreEncodedQueryBox{{App: 1, Name: []byte("b,]")}}}
		}
		rounds := make([]basics.Round, min(n, 256))
		for i := range rounds {
			rounds[i] = basics.Round(i * 1000)
		}
		req := PreEncodedQueryRequest{Program: []byte{1, 2}, GlobalSchema: &PreEncodedQuerySchema{NumUint: 1},
			AppID: 3, Sender: "s", Rounds: rounds, Protocol: "p", Budget: 7, Calls: calls}
		for _, handle := range []codecHandleCase{{"msgpack", protocol.CodecHandle}, {"json", protocol.JSONStrictHandle}} {
			body, err := encode(handle.h, &req)
			require.NoError(t, err)
			var want PreEncodedQueryRequest
			require.NoError(t, decode(handle.h, body, &want))
			got, err := decodeQueryRequest(body, queryTestLimits())
			require.NoError(t, err, handle.name)
			require.Equal(t, want, got, handle.name)

			var lists queryRequestLists
			require.NoError(t, decode(handle.h, body, &lists))
			isJSON := handle.h == protocol.JSONStrictHandle
			count, err := encodedListLen(lists.Calls, isJSON)
			require.NoError(t, err)
			require.Equal(t, n, count, handle.name)
			count, err = encodedListLen(lists.Rounds, isJSON)
			require.NoError(t, err)
			require.Equal(t, len(rounds), count, handle.name)
		}
	}
	for _, body := range []string{`{"calls":null}`, `{"calls":[ ]}`, `{"calls":[{}], "rounds":null}`} {
		_, err := decodeQueryRequest([]byte(body), queryTestLimits())
		require.NoError(t, err, body)
	}
	_, err := decodeQueryRequest([]byte(`{"calls":{"a":{}}}`), queryTestLimits())
	require.ErrorContains(t, err, "expected a list")
}

type codecHandleCase struct {
	name string
	h    codec.Handle
}

// TestDecodeQueryRequestLimits: a body of more calls or rounds than the limits is rejected with the
// error Validate would give. A key listed twice keeps its last value, as in a full decode.
func TestDecodeQueryRequestLimits(t *testing.T) {
	partitiontest.PartitionTest(t)

	limits := simulation.QueryLimits{MaxCalls: 1024, MaxRounds: 16, MaxTotalCalls: 1024}
	msgpackList := func(entry byte, n int) []byte {
		b := binary.BigEndian.AppendUint32([]byte{0xdd}, uint32(n))
		return append(b, bytes.Repeat([]byte{entry}, n)...)
	}
	msgpackCalls := append([]byte{0x81, 0xa5, 'c', 'a', 'l', 'l', 's'}, msgpackList(0x80, 1025)...)
	msgpackRounds := append(append([]byte{0x82, 0xa6, 'r', 'o', 'u', 'n', 'd', 's'}, msgpackList(0x01, 17)...),
		0xa5, 'c', 'a', 'l', 'l', 's', 0x91, 0x80)
	jsonCalls := []byte(`{"calls":[` + strings.Repeat(`{},`, 1024) + `{}]}`)
	// Too many calls, then one.
	twice := []byte(`{"calls":[` + strings.Repeat(`{},`, 1024) + `{}],"calls":[{}]}`)

	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"msgpack calls", msgpackCalls, "too many calls"},
		{"msgpack rounds", msgpackRounds, "too many rounds"},
		{"json calls", jsonCalls, "too many calls"},
	} {
		_, err := decodeQueryRequest(tc.body, limits)
		require.ErrorContains(t, err, tc.want, tc.name)
	}
	r, err := decodeQueryRequest(twice, limits)
	require.NoError(t, err)
	require.Len(t, r.Calls, 1)
}

// TestDecodeQueryRequestMalformed: a body whose lists are malformed fails to decode, however many
// entries they seem to have, rather than as over the limits: the codec checks a list's syntax as it
// copies it out, before encodedListLen counts it.
func TestDecodeQueryRequestMalformed(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	limits := simulation.QueryLimits{MaxCalls: 4, MaxRounds: 16, MaxTotalCalls: 64}
	many := strings.Repeat(`{},`, 8)
	for _, body := range []string{
		`{"calls":[{},]}`,
		`{"calls":[,{}]}`,
		`{"calls":[{},,{}]}`,
		`{"calls":[` + many + `]}`,
		`{"calls":[` + strings.Repeat(`,`, 8) + `]}`,
		`{"calls":[` + many + `{}],"rounds":[1,2,]}`,
	} {
		_, err := decodeQueryRequest([]byte(body), limits)
		require.Error(t, err, body)
		require.NotContains(t, err.Error(), "too many", body)
	}
	r, err := decodeQueryRequest([]byte("{\"calls\":[ {} ,\n {} ], \"rounds\": [ 1 , 2 ] }"), limits)
	require.NoError(t, err)
	require.Len(t, r.Calls, 2)
	require.Len(t, r.Rounds, 2)
}
