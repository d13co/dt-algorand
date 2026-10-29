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

package ledger

import (
	"context"
	"errors"
	"fmt"

	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
)

// HistoryCommitment is the commitment of the time travel history up to a
// round, with that round's header hash.
//
//msgp:ignore HistoryCommitment
type HistoryCommitment struct {
	timetravel.Commitment
	GenesisHash crypto.Digest
	HeaderHash  bookkeeping.BlockHash
	// Installed names the snapshot the history was installed from (ttsnap
	// fetch), empty if the node built it.
	Installed string
}

// HistoryCommitment computes the commitment of the time travel history up to
// round rnd (see timetravel.Store.Commitment), which a history snapshot of
// that round must have, reading with workers goroutines (0: GOMAXPROCS). It
// reads the whole history: minutes on mainnet.
func (l *Ledger) HistoryCommitment(ctx context.Context, rnd basics.Round, workers int) (HistoryCommitment, error) {
	if l.history == nil {
		return HistoryCommitment{}, errors.New("the history commitment requires EnableTimeTravel")
	}
	if err := l.HistoryUntrusted(); err != nil {
		return HistoryCommitment{}, err
	}
	// an installed store reaches past the ledger until the ledger replays it
	if latest := l.Latest(); rnd > latest {
		return HistoryCommitment{}, fmt.Errorf("round %d is after the latest round %d", rnd, latest)
	}
	st := l.history.store
	b, ok, err := st.Header(uint64(rnd))
	if err != nil {
		return HistoryCommitment{}, err
	}
	if !ok {
		return HistoryCommitment{}, fmt.Errorf("the history has no header for round %d yet", rnd)
	}
	var hdr bookkeeping.BlockHeader
	if err = protocol.Decode(b, &hdr); err != nil {
		return HistoryCommitment{}, err
	}
	installed, _, err := st.Installed()
	if err != nil {
		return HistoryCommitment{}, err
	}
	c, err := st.Commitment(ctx, uint64(rnd), timetravel.CommitmentOptions{Workers: workers})
	if err != nil {
		return HistoryCommitment{}, err
	}
	return HistoryCommitment{Commitment: c, GenesisHash: l.GenesisHash(), HeaderHash: hdr.Hash(), Installed: installed}, nil
}
