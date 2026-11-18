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

package rpcs

import (
	"encoding/binary"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/agreement"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/network"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// strippedLedger has blocks 1..latest, where rounds up to strippedTo are stored without their certificate.
type strippedLedger struct {
	latest, strippedTo basics.Round
}

func (l strippedLedger) EncodedBlockCert(rnd basics.Round) ([]byte, []byte, error) {
	if rnd > l.latest {
		return nil, nil, ledgercore.ErrNoEntry{Round: rnd, Latest: l.latest}
	}
	blk := bookkeeping.Block{BlockHeader: bookkeeping.BlockHeader{Round: rnd}}
	if rnd <= l.strippedTo {
		return protocol.Encode(&blk), nil, nil
	}
	return protocol.Encode(&blk), protocol.Encode(&agreement.Certificate{Round: rnd}), nil
}

func (l strippedLedger) Latest() basics.Round { return l.latest }

func TestStrippedCertServing(t *testing.T) {
	partitiontest.PartitionTest(t)

	l := strippedLedger{latest: 100, strippedTo: 50}

	// to peers, a round without its certificate is missing, with the latest round to go by
	_, err := RawBlockBytes(l, 10)
	require.Equal(t, ledgercore.ErrNoEntry{Round: 10, Latest: 100}, err)
	data, err := RawBlockBytes(l, 60)
	require.NoError(t, err)
	var ebc EncodedBlockCert
	require.NoError(t, protocol.Decode(data, &ebc))
	require.Equal(t, basics.Round(60), ebc.Certificate.Round)

	topics, n := topicBlockBytes(logging.TestingLog(t), l, 10, BlockAndCertValue)
	require.Zero(t, n)
	errMsg, ok := topics.GetValue(network.ErrorKey)
	require.True(t, ok)
	require.Equal(t, blockNotAvailableErrMsg, string(errMsg))
	latest, ok := topics.GetValue(LatestRoundKey)
	require.True(t, ok)
	require.Equal(t, uint64(100), binary.BigEndian.Uint64(latest))
	_, ok = topics.GetValue(BlockDataKey)
	require.False(t, ok)

	topics, n = topicBlockBytes(logging.TestingLog(t), l, 60, BlockAndCertValue)
	require.Positive(t, n)
	_, ok = topics.GetValue(CertDataKey)
	require.True(t, ok)

	// to the REST API, it is the block with an empty certificate
	data, err = RawBlockBytesAnyCert(l, 10)
	require.NoError(t, err)
	ebc = EncodedBlockCert{}
	require.NoError(t, protocol.Decode(data, &ebc))
	require.Equal(t, basics.Round(10), ebc.Block.Round())
	require.Equal(t, agreement.Certificate{}, ebc.Certificate)
	data, err = RawBlockBytesAnyCert(l, 60)
	require.NoError(t, err)
	ebc = EncodedBlockCert{}
	require.NoError(t, protocol.Decode(data, &ebc))
	require.Equal(t, basics.Round(60), ebc.Certificate.Round)
	_, err = RawBlockBytesAnyCert(l, 101)
	require.ErrorAs(t, err, &ledgercore.ErrNoEntry{})
}
