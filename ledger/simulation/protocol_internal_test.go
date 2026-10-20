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

package simulation

import (
	"maps"
	"reflect"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestProtocolParamsClassified makes every ConsensusParams field either change the semantics of
// transactions or be ignored by the semantics-changed note, so a new parameter forces a decision.
func TestProtocolParamsClassified(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	typ := reflect.TypeFor[config.ConsensusParams]()
	var fields []string
	for i := range typ.NumField() {
		fields = append(fields, typ.Field(i).Name)
	}
	classified := append(slices.Clone(semanticParams), ignoredParams...)
	for _, name := range fields {
		count := 0
		for _, c := range classified {
			if c == name {
				count++
			}
		}
		require.Equal(t, 1, count, "ConsensusParams.%s must be in exactly one of semanticParams and ignoredParams", name)
	}
	for _, name := range classified {
		require.Contains(t, fields, name, "%s is not a ConsensusParams field", name)
	}
}

func TestProtocolReachable(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		from, to protocol.ConsensusVersion
		want     bool
	}{
		{protocol.ConsensusV24, protocol.ConsensusV42, true},
		{protocol.ConsensusV33, protocol.ConsensusV35, true},
		{protocol.ConsensusV34, protocol.ConsensusV35, true},
		{protocol.ConsensusV35, protocol.ConsensusV36, true},
		{protocol.ConsensusV35, protocol.ConsensusV34, false},
		{protocol.ConsensusV37, protocol.ConsensusV36, false},
		{protocol.ConsensusV42, protocol.ConsensusV41, false},
		{protocol.ConsensusV41, protocol.ConsensusV41, false},
		{protocol.ConsensusVFnet1, protocol.ConsensusVFnet5, true},
		{protocol.ConsensusVFnet3, protocol.ConsensusV41, true},
		{protocol.ConsensusV40, protocol.ConsensusVFnet5, true},
		{protocol.ConsensusV40, protocol.ConsensusVFnet1, false},
		{protocol.ConsensusV42, protocol.ConsensusFuture, false},
	} {
		require.Equal(t, tc.want, protocolReachable(tc.from, tc.to), "%s -> %s", tc.from, tc.to)
	}
}

// TestShortProtocolNames keeps the vNN names up to the current protocol.
func TestShortProtocolNames(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	require.Contains(t, slices.Collect(maps.Values(shortProtocolNames)), protocol.ConsensusCurrentVersion,
		"add the current protocol to shortProtocolNames, and to the docs of protocol in algod.oas2.json, goal clerk simulate and ledger/timetravel/README.md")
	for name, v := range shortProtocolNames {
		_, ok := config.Consensus[v]
		require.True(t, ok, name)
	}
}

func TestDecideProtocol(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	applied, err := decideProtocol(protocol.ConsensusV35, protocol.ConsensusV35)
	require.NoError(t, err)
	require.False(t, applied)

	applied, err = decideProtocol(protocol.ConsensusV35, protocol.ConsensusV42)
	require.NoError(t, err)
	require.True(t, applied)

	applied, err = decideProtocol(protocol.ConsensusV42, protocol.ConsensusFuture)
	require.NoError(t, err)
	require.True(t, applied)

	_, err = decideProtocol(protocol.ConsensusV35, protocol.ConsensusV34)
	require.ErrorAs(t, err, &InvalidRequestError{})
	require.ErrorContains(t, err, "is not a later protocol")
}

func TestValidateProtocol(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	v, err := parseProtocol("v38")
	require.NoError(t, err)
	require.Equal(t, protocol.ConsensusV38, v)
	v, err = parseProtocol(string(protocol.ConsensusV41))
	require.NoError(t, err)
	require.Equal(t, protocol.ConsensusV41, v)
	v, err = parseProtocol("fnet3")
	require.NoError(t, err)
	require.Equal(t, protocol.ConsensusVFnet3, v)

	for _, name := range []string{"", ProtocolLatest, "v42", "fnet5"} {
		require.NoError(t, validateProtocol(name, false), name)
	}
	for _, name := range []string{"bogus", "v99", "V42"} {
		require.ErrorAs(t, validateProtocol(name, true), &InvalidRequestError{}, name)
	}
	require.ErrorAs(t, validateProtocol("future", false), &InvalidRequestError{})
	require.NoError(t, validateProtocol("future", true))
}

// TestUpgradeAt checks that a header with a synthesized upgrade makes the next block switch to
// the target, and that the next block passes the evaluator's header checks against it.
func TestUpgradeAt(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	for _, tc := range []struct {
		from, to protocol.ConsensusVersion
	}{
		{protocol.ConsensusV35, protocol.ConsensusFuture},
		{protocol.ConsensusV35, protocol.ConsensusV42}, // turns on payouts and SHA-512 hashes
		{protocol.ConsensusV39, protocol.ConsensusV40},
		{protocol.ConsensusV42, protocol.ConsensusFuture},
	} {
		prev := bookkeeping.BlockHeader{
			Round:        1000,
			GenesisID:    "test",
			TimeStamp:    1_700_000_000,
			UpgradeState: bookkeeping.UpgradeState{CurrentProtocol: tc.from},
		}
		prev.GenesisHash[0] = 1
		// A real proposal in flight is replaced.
		prev.NextProtocol = protocol.ConsensusV36
		prev.NextProtocolVoteBefore = 1500
		prev.NextProtocolSwitchOn = 2000

		patched := upgradeAt(prev, tc.to)
		require.Equal(t, tc.from, patched.CurrentProtocol)
		next := bookkeeping.MakeBlock(patched)
		require.Equal(t, tc.to, next.CurrentProtocol, "%s -> %s", tc.from, tc.to)
		require.Empty(t, next.NextProtocol)
		require.NoError(t, next.PreCheck(patched), "%s -> %s", tc.from, tc.to)

		params, prevParams := config.Consensus[tc.to], config.Consensus[tc.from]
		if params.Bonus.BaseAmount != 0 && params.Bonus != prevParams.Bonus {
			require.Equal(t, basics.MicroAlgos{Raw: params.Bonus.BaseAmount}, next.Bonus)
		}
	}
}

func TestStaticNotes(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	notes := staticNotes(config.Consensus[protocol.ConsensusV35], config.Consensus[protocol.ConsensusV42])
	codes := make([]string, len(notes))
	for i, n := range notes {
		codes[i] = n.Code
	}
	require.Equal(t, []string{noteSemanticsChanged, noteNoHeartbeatChallenge, noteNoLoadHistory}, codes)
	require.Contains(t, notes[0].Message, "LogicSigVersion: 7 → 13")
	require.Contains(t, notes[0].Message, "LogicSigMsig: true → false")
	require.NotContains(t, notes[0].Message, "AgreementFilterTimeout")
	// online_stake follows the balance round's protocol, and the bonus is no transaction's.
	require.NotContains(t, notes[0].Message, "ExcludeExpiredCirculation")
	require.NotContains(t, notes[0].Message, "Bonus")

	// v40 to v42: payout rules and heartbeats are the same, so challenges work.
	notes = staticNotes(config.Consensus[protocol.ConsensusV40], config.Consensus[protocol.ConsensusV42])
	for _, n := range notes {
		require.NotEqual(t, noteNoHeartbeatChallenge, n.Code)
	}

	require.Empty(t, staticNotes(config.Consensus[protocol.ConsensusV42], config.Consensus[protocol.ConsensusV42]))
}

func TestProtocolOverrideHits(t *testing.T) {
	partitiontest.PartitionTest(t)
	t.Parallel()

	var po ProtocolOverride
	po.addHit(noteAppVersion, NoteHit{Group: 1, Path: TxnPath{0}})
	po.addHit(noteBlockIncentiveFields, NoteHit{Group: 2, Path: TxnPath{1, 0}})
	po.addHit(noteBlockIncentiveFields, NoteHit{Group: 0, Path: TxnPath{3}})
	po.addHit(noteBlockIncentiveFields, NoteHit{Group: 0, Path: TxnPath{3}}) // duplicate
	// The lowest hits are kept, whatever order they come in.
	for i := 2 * maxNoteHits; i > 0; i-- {
		po.addHit(noteOnlineStake, NoteHit{Group: i, Path: TxnPath{0}})
	}
	po.Notes = append(po.Notes, ProtocolNote{Code: noteSemanticsChanged})
	po.sortNotes()

	require.Len(t, po.Notes, 4)
	require.Equal(t, noteSemanticsChanged, po.Notes[0].Code)
	require.Equal(t, noteBlockIncentiveFields, po.Notes[1].Code)
	require.Equal(t, []NoteHit{{Group: 0, Path: TxnPath{3}}, {Group: 2, Path: TxnPath{1, 0}}}, po.Notes[1].Hits)
	require.Equal(t, hitNoteMessages[noteBlockIncentiveFields], po.Notes[1].Message)
	require.Equal(t, noteOnlineStake, po.Notes[2].Code)
	require.Len(t, po.Notes[2].Hits, maxNoteHits)
	for i, hit := range po.Notes[2].Hits {
		require.Equal(t, NoteHit{Group: i + 1, Path: TxnPath{0}}, hit)
	}
	require.Equal(t, noteAppVersion, po.Notes[3].Code)
}
