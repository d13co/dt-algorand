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

// Bounded decoding of simulate request bodies. Non-standard fork feature, kept in its own file to
// keep upstream merges of handlers.go clean.

import (
	"errors"
	"fmt"
	"reflect"
	"strings"

	"github.com/algorand/go-codec/codec"
	"github.com/algorand/msgp/msgp"

	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/ledger/simulation"
	"github.com/algorand/go-algorand/protocol"
)

// simulateRequestLists is what a simulate request body decodes into: the fields of
// PreEncodedSimulateRequest, in its order (which a body encoded as an array relies on), then the
// removed fields. Its lists and the removed fields are left encoded, so that they are checked
// against the limits before they are decoded.
//
// The removed fields are the options that earlier releases of the fork took and POST /v2/query
// replaced. A request that sets one is refused with an error that names it, rather than simulated
// without it. Their tags are the only list of them: removedFields reads the keys from there.
//
// TODO(dt6): drop the removed fields (and removedFields, removedFieldSet, maxRemovedFieldBytes) two
// releases after v5.0.5-dt4, the last release that took them. Without them, a request that still
// sends one fails to decode with an unknown-field error, even when it sends a zero value.
type simulateRequestLists struct {
	TxnGroups             codec.Raw                  `codec:"txn-groups"`
	Round                 basics.Round               `codec:"round,omitempty"`
	AllowEmptySignatures  bool                       `codec:"allow-empty-signatures,omitempty"`
	AllowMoreLogging      bool                       `codec:"allow-more-logging,omitempty"`
	AllowUnnamedResources bool                       `codec:"allow-unnamed-resources,omitempty"`
	ExtraOpcodeBudget     int                        `codec:"extra-opcode-budget,omitempty"`
	ExecTraceConfig       simulation.ExecTraceConfig `codec:"exec-trace-config,omitempty"`
	FixSigners            bool                       `codec:"fix-signers,omitempty"`
	Rounds                codec.Raw                  `codec:"rounds,omitempty"`
	ParallelGroups        codec.Raw                  `codec:"parallel-groups,omitempty"`
	SkipValidityWindow    codec.Raw                  `codec:"skip-validity-window,omitempty"`
	RewriteValidityWindow codec.Raw                  `codec:"rewrite-validity-window,omitempty"`
	Protocol              codec.Raw                  `codec:"protocol,omitempty"`
}

// simulateGroupLists is PreEncodedSimulateRequestTransactionGroup with its txns left encoded.
type simulateGroupLists struct {
	Txns codec.Raw `codec:"txns"`
}

// maxRemovedFieldBytes is the longest encoding of a removed field that is decoded to learn whether
// it is set. Every value that leaves it unset (null, false, 0, "", an empty list or map) is shorter;
// a longer one is set.
const maxRemovedFieldBytes = 64

// removedFields returns the keys of the removed fields that l sets, in field order. They are the
// fields after those of PreEncodedSimulateRequest. A value longer than maxRemovedFieldBytes is set
// without being decoded.
func (l *simulateRequestLists) removedFields(handle codec.Handle) ([]string, error) {
	v := reflect.ValueOf(l).Elem()
	var set []string
	for i := reflect.TypeFor[PreEncodedSimulateRequest]().NumField(); i < v.NumField(); i++ {
		raw := v.Field(i).Interface().(codec.Raw)
		if len(raw) == 0 {
			continue
		}
		if len(raw) <= maxRemovedFieldBytes {
			var value any
			if err := decode(handle, raw, &value); err != nil {
				return nil, err
			}
			if !removedFieldSet(value) {
				continue
			}
		}
		key, _, _ := strings.Cut(v.Type().Field(i).Tag.Get("codec"), ",")
		set = append(set, key)
	}
	return set, nil
}

// removedFieldSet reports whether a removed field's decoded value asks for something: null, false,
// 0, "" and an empty array or map don't. A value of the wrong type does.
func removedFieldSet(v any) bool {
	switch v := v.(type) {
	case nil:
		return false
	case bool:
		return v
	case string:
		return v != ""
	case []byte:
		return len(v) > 0
	case int64:
		return v != 0
	case uint64:
		return v != 0
	case float64:
		return v != 0
	case []any:
		return len(v) > 0
	case map[any]any:
		return len(v) > 0
	case map[string]any:
		return len(v) > 0
	}
	return true
}

// decodeSimulateRequest decodes body, msgpack or else JSON, whatever its Content-Type. It refuses
// the removed fields, more than maxGroups groups, and groups of no txns or more than maxGroupSize,
// before it decodes the txns. As in a full decode, a key listed twice keeps its last value.
// Msgpack txns are decoded as the node decodes txns from the network.
func decodeSimulateRequest(body []byte, maxGroups, maxGroupSize int) (PreEncodedSimulateRequest, error) {
	var lists simulateRequestLists
	var handle codec.Handle = protocol.CodecHandle
	if err := decode(handle, body, &lists); err != nil {
		lists, handle = simulateRequestLists{}, protocol.JSONStrictHandle
		if err = decode(handle, body, &lists); err != nil {
			return PreEncodedSimulateRequest{}, err
		}
	}
	isJSON := handle == protocol.JSONStrictHandle

	removed, err := lists.removedFields(handle)
	if err != nil {
		return PreEncodedSimulateRequest{}, err
	}
	if len(removed) > 0 {
		return PreEncodedSimulateRequest{}, fmt.Errorf("simulate no longer takes %s: use POST /v2/query for read-only calls at several rounds, in isolation or under another protocol", strings.Join(removed, ", "))
	}

	r := PreEncodedSimulateRequest{
		Round:                 lists.Round,
		AllowEmptySignatures:  lists.AllowEmptySignatures,
		AllowMoreLogging:      lists.AllowMoreLogging,
		AllowUnnamedResources: lists.AllowUnnamedResources,
		ExtraOpcodeBudget:     lists.ExtraOpcodeBudget,
		ExecTraceConfig:       lists.ExecTraceConfig,
		FixSigners:            lists.FixSigners,
	}
	n, err := encodedListLen(lists.TxnGroups, isJSON)
	if err != nil {
		return PreEncodedSimulateRequest{}, fmt.Errorf("txn-groups: %w", err)
	}
	if n > maxGroups {
		// The error simulation.Request.Validate would give.
		return PreEncodedSimulateRequest{}, fmt.Errorf("too many transaction groups: got %d, the limit is %d", n, maxGroups)
	}
	if n == 0 {
		return r, nil
	}
	var groups []simulateGroupLists
	if err = decode(handle, lists.TxnGroups, &groups); err != nil {
		return PreEncodedSimulateRequest{}, fmt.Errorf("txn-groups: %w", err)
	}
	r.TxnGroups = make([]PreEncodedSimulateRequestTransactionGroup, len(groups))
	for i, g := range groups {
		n, err = encodedListLen(g.Txns, isJSON)
		if err != nil {
			return PreEncodedSimulateRequest{}, fmt.Errorf("txn-groups[%d].txns: %w", i, err)
		}
		if n == 0 {
			return PreEncodedSimulateRequest{}, errors.New("empty txgroup")
		}
		if n > maxGroupSize {
			return PreEncodedSimulateRequest{}, fmt.Errorf("transaction group size %d exceeds protocol max %d", n, maxGroupSize)
		}
		var txns []transactions.SignedTxn
		if isJSON {
			err = decode(handle, g.Txns, &txns)
		} else {
			txns, err = decodeSignedTxnsMsgp(g.Txns)
		}
		if err != nil {
			return PreEncodedSimulateRequest{}, fmt.Errorf("txn-groups[%d].txns: %w", i, err)
		}
		r.TxnGroups[i].Txns = txns
	}
	return r, nil
}

// decodeSignedTxnsMsgp decodes b, a msgpack array of txns, with their generated msgp decoder, as the
// node decodes txns from the network.
func decodeSignedTxnsMsgp(b []byte) (txns []transactions.SignedTxn, err error) {
	defer func() {
		if x := recover(); x != nil {
			txns, err = nil, fmt.Errorf("failed to decode txns: %v", x)
		}
	}()
	n, isNil, rest, err := msgp.ReadArrayHeaderBytes(b)
	if err != nil {
		return nil, fmt.Errorf("failed to decode txns: %w", err)
	}
	if isNil {
		return nil, nil
	}
	txns = make([]transactions.SignedTxn, n)
	for i := range txns {
		if rest, err = txns[i].UnmarshalMsg(rest); err != nil {
			return nil, fmt.Errorf("failed to decode txn %d: %w", i, err)
		}
	}
	return txns, nil
}
