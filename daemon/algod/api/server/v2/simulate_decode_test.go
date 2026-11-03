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

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestSimulateRequestListsFields: simulateRequestLists has the fields of PreEncodedSimulateRequest,
// in that order and with the same tags, with its txn groups left encoded; then the removed fields,
// left encoded under keys of their own.
func TestSimulateRequestListsFields(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	request := reflect.TypeFor[PreEncodedSimulateRequest]()
	lists := reflect.TypeFor[simulateRequestLists]()
	require.Greater(t, lists.NumField(), request.NumField())
	keys := map[string]bool{}
	for i := range lists.NumField() {
		l := lists.Field(i)
		key, _, _ := strings.Cut(l.Tag.Get("codec"), ",")
		require.NotEmpty(t, key, l.Name)
		require.False(t, keys[key], key)
		keys[key] = true
		if i >= request.NumField() {
			require.Equal(t, reflect.TypeFor[codec.Raw](), l.Type, l.Name)
			require.Contains(t, l.Tag.Get("codec"), ",omitempty", l.Name)
			continue
		}
		f := request.Field(i)
		require.Equal(t, f.Name, l.Name)
		require.Equal(t, f.Tag, l.Tag, f.Name)
		if f.Name == "TxnGroups" {
			require.Equal(t, reflect.TypeFor[codec.Raw](), l.Type, f.Name)
		} else {
			require.Equal(t, f.Type, l.Type, f.Name)
		}
	}
	group := reflect.TypeOf(PreEncodedSimulateRequestTransactionGroup{})
	require.Equal(t, 1, group.NumField())
	require.Equal(t, group.Field(0).Tag, reflect.TypeOf(simulateGroupLists{}).Field(0).Tag)
}

// simulateTestTxns are txns that set the lists a txn has.
func simulateTestTxns() []transactions.SignedTxn {
	var a, b basics.Address
	a[0], b[0] = 1, 2
	msig := crypto.MultisigSig{Version: 1, Threshold: 1, Subsigs: []crypto.MultisigSubsig{{Key: crypto.PublicKey{1}}, {Key: crypto.PublicKey{2}, Sig: crypto.Signature{3}}}}
	appl := transactions.Transaction{
		Type:   protocol.ApplicationCallTx,
		Header: transactions.Header{Sender: a, Fee: basics.MicroAlgos{Raw: 1000}, FirstValid: 1, LastValid: 10, Note: []byte("n,]")},
		ApplicationCallTxnFields: transactions.ApplicationCallTxnFields{
			ApplicationID:   5,
			ApplicationArgs: [][]byte{[]byte("x"), nil, []byte(`"],`)},
			Accounts:        []basics.Address{b},
			ForeignApps:     []basics.AppIndex{6, 7},
			ForeignAssets:   []basics.AssetIndex{8},
			Boxes:           []transactions.BoxRef{{Index: 1, Name: []byte("b")}},
			Access:          []transactions.ResourceRef{{Address: b}, {App: 9}, {}},
		},
	}
	pay := transactions.Transaction{
		Type:             protocol.PaymentTx,
		Header:           transactions.Header{Sender: b, FirstValid: 1, LastValid: 10},
		PaymentTxnFields: transactions.PaymentTxnFields{Receiver: a, Amount: basics.MicroAlgos{Raw: 7}},
	}
	return []transactions.SignedTxn{
		{Txn: appl, Sig: crypto.Signature{9}},
		{Txn: pay, Msig: msig, AuthAddr: a},
		{Txn: pay, Lsig: transactions.LogicSig{Logic: []byte{1, 0x20}, Args: [][]byte{[]byte("a"), {}}, Msig: msig}},
		{Txn: pay, Lsig: transactions.LogicSig{Logic: []byte{1}, LMsig: msig}},
	}
}

// TestDecodeSimulateRequest: a request decodes as a full decode would, in both codecs.
func TestDecodeSimulateRequest(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	txns := simulateTestTxns()
	reqs := []PreEncodedSimulateRequest{
		{TxnGroups: []PreEncodedSimulateRequestTransactionGroup{{Txns: txns}}},
		{
			TxnGroups:            []PreEncodedSimulateRequestTransactionGroup{{Txns: txns[:1]}, {Txns: txns[1:]}},
			Round:                3,
			AllowEmptySignatures: true, AllowMoreLogging: true, AllowUnnamedResources: true, FixSigners: true,
			ExtraOpcodeBudget: 700,
			ExecTraceConfig:   simulation.ExecTraceConfig{Enable: true, Stack: true},
		},
	}
	for _, req := range reqs {
		for _, handle := range []codecHandleCase{{"msgpack", protocol.CodecHandle}, {"json", protocol.JSONStrictHandle}} {
			body, err := encode(handle.h, &req)
			require.NoError(t, err)
			var want PreEncodedSimulateRequest
			require.NoError(t, decode(handle.h, body, &want))
			got, err := decodeSimulateRequest(body, 16, 16)
			require.NoError(t, err, handle.name)
			require.Equal(t, want, got, handle.name)
		}
	}
	// msgp's encoding of the txns, as an SDK's.
	body := append([]byte{0x81}, msgpStr("txn-groups")...)
	body = append(body, 0x91, 0x81)
	body = append(body, msgpStr("txns")...)
	body = append(body, 0x90|byte(len(txns)))
	for i := range txns {
		body = append(body, protocol.Encode(&txns[i])...)
	}
	got, err := decodeSimulateRequest(body, 16, 16)
	require.NoError(t, err)
	require.Equal(t, txns, got.TxnGroups[0].Txns)

	for _, tc := range []struct{ body, want string }{
		{`{"txn-groups":[]}`, ""},
		{`{"txn-groups":[{"txns":[]}]}`, "empty txgroup"},
		{`{"txn-groups":[{"txns":null}]}`, "empty txgroup"},
		{`{"txn-groups":[{}]}`, "empty txgroup"},
		{`{"txn-groups":[{"txns":[` + strings.Repeat(`{},`, 16) + `{}]}]}`, "transaction group size 17 exceeds protocol max 16"},
		{`{"txn-groups":[` + strings.Repeat(`{"txns":[{}]},`, 16) + `{"txns":[{}]}]}`, "too many transaction groups: got 17, the limit is 16"},
		{`{"txn-groups":{}}`, "expected a list"},
		{`{"txn-groups":[], "rounds":[]}`, ""},
		{`{"txn-groups":[], "rounds":[1]}`, "simulate no longer takes rounds"},
		{`{"txn-groups":[], "protocol":"` + strings.Repeat("x", 100) + `"}`, "simulate no longer takes protocol"},
		{`{"txn-groups":[], "parallel-groups":false, "skip-validity-window":0, "protocol":""}`, ""},
		// Whitespace in a value is not what makes it long.
		{`{"txn-groups":[], "rounds":[` + strings.Repeat(" ", 80) + `]}`, ""},
		{"{\"txn-groups\":[],\n  \"rounds\": [\n" + strings.Repeat(" ", 80) + "\n  ],\n  \"protocol\":\n" + strings.Repeat(" ", 80) + "\"\"\n}", ""},
		{`{"txn-groups":[], "rounds":[` + strings.Repeat(" ", 80) + `1]}`, "simulate no longer takes rounds"},
		{`{"txn-groups":[], "protocol":"` + strings.Repeat(" ", 80) + `"}`, "simulate no longer takes protocol"},
	} {
		_, err := decodeSimulateRequest([]byte(tc.body), 16, 16)
		if tc.want == "" {
			require.NoError(t, err, tc.body)
		} else {
			require.ErrorContains(t, err, tc.want, tc.body)
		}
	}
}

func msgpStr(s string) []byte { return append([]byte{0xa0 | byte(len(s))}, s...) }

// TestDecodeSimulateRequestLimits: bodies of too many groups or txns, or that set a removed field,
// are refused with the errors the handler gave before. A key listed twice keeps its last value, as
// in a full decode.
func TestDecodeSimulateRequestLimits(t *testing.T) {
	partitiontest.PartitionTest(t)

	list := func(entry []byte, n int) []byte {
		b := binary.BigEndian.AppendUint32([]byte{0xdd}, uint32(n))
		return append(b, bytes.Repeat(entry, n)...)
	}
	groups := func(g []byte) []byte { return append(append([]byte{0x81}, msgpStr("txn-groups")...), g...) }
	oneGroup := func(txns []byte) []byte {
		return groups(append(append([]byte{0x91, 0x81}, msgpStr("txns")...), txns...))
	}
	decode := func(body []byte) (PreEncodedSimulateRequest, error) { return decodeSimulateRequest(body, 16, 16) }
	for _, tc := range []struct {
		name string
		body []byte
		want string
	}{
		{"txns", oneGroup(list([]byte{0x80}, 17)), "transaction group size"},
		{"no txns", oneGroup([]byte{0x90}), "empty txgroup"},
		{"groups", groups(list(append(append([]byte{0x81}, msgpStr("txns")...), 0x91, 0x80), 17)), "too many transaction groups"},
		{"removed rounds", append(append([]byte{0x82}, msgpStr("rounds")...), append(list([]byte{0x01}, 100), append(msgpStr("txn-groups"), 0x90)...)...), "no longer takes rounds"},
		{"json txns", []byte(`{"txn-groups":[{"txns":[` + strings.Repeat(`{},`, 16) + `{}]}]}`), "transaction group size"},
	} {
		_, err := decode(tc.body)
		require.ErrorContains(t, err, tc.want, tc.name)
	}
	// Too many txns, then one.
	twice := []byte(`{"txn-groups":[{"txns":[` + strings.Repeat(`{},`, 16) + `{}]}],"txn-groups":[{"txns":[{}]}]}`)
	r, err := decode(twice)
	require.NoError(t, err)
	require.Len(t, r.TxnGroups, 1)
	require.Len(t, r.TxnGroups[0].Txns, 1)
}
