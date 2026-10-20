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
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/transactions"
	"github.com/algorand/go-algorand/protocol"
)

// Simulate's rewrite-validity-window mode (non-standard fork feature).

// rewriteValidityWindow returns groups with every top-level txn's window moved to the block being
// simulated, blockRound, keeping its width. Txids change, so valid group IDs are recomputed and
// the signatures over the body of every txn that changed are dropped. Txns that differ only by
// their window come out identical. The returned outer and group slices are new; each
// SignedTxn is a value copy whose inner slices are still shared with groups (and with every other
// round of the request), so only whole fields are assigned, never slice elements.
//
// Left as they are:
//   - heartbeat txns' windows: their proof is bound to LastValid and their seed to FirstValid.
//     They still get their group's new ID, and lose their signatures if that changes it.
//   - malformed windows (LastValid < FirstValid, or wider than MaxTxnLife), so that WellFormed
//     reports them.
//   - logic sigs: they sign the program, not the txn.
//   - group IDs that are not valid for their group, so that it still fails.
func rewriteValidityWindow(groups [][]transactions.SignedTxn, blockRound basics.Round, proto config.ConsensusParams) [][]transactions.SignedTxn {
	rewritten := make([][]transactions.SignedTxn, len(groups))
	for g, group := range groups {
		validGroup := validGroupID(group)
		out := make([]transactions.SignedTxn, len(group))
		for i, stxn := range group {
			if stxn.Txn.Type != protocol.HeartbeatTx {
				rewriteTxnWindow(&stxn, blockRound, proto)
			}
			out[i] = stxn
		}
		if validGroup {
			id := computeGroupID(out)
			for i := range out {
				if out[i].Txn.Group != id {
					// The signatures over the txn cover Group.
					stripTxnSigs(&out[i])
					out[i].Txn.Group = id
				}
			}
		}
		rewritten[g] = out
	}
	return rewritten
}

// rewriteTxnWindow moves stxn's window to start at blockRound and, if that changes it, strips the
// signatures over the txn body.
func rewriteTxnWindow(stxn *transactions.SignedTxn, blockRound basics.Round, proto config.ConsensusParams) {
	fv, lv := stxn.Txn.FirstValid, stxn.Txn.LastValid
	if lv >= fv && lv-fv <= basics.Round(proto.MaxTxnLife) && fv != blockRound {
		stxn.Txn.FirstValid = blockRound
		stxn.Txn.LastValid = blockRound + (lv - fv)
		stripTxnSigs(stxn)
	}
}

// stripTxnSigs drops stxn's signatures over the txn body. A PQ signature becomes a placeholder,
// which keeps its fee contribution and its envelope check. A logic sig signs the program, so it is
// kept.
func stripTxnSigs(stxn *transactions.SignedTxn) {
	stxn.Sig = crypto.Signature{}
	stxn.Msig = crypto.MultisigSig{}
	stxn.PQsig.Signature = nil
}

// validGroupID reports whether group carries the group ID that checkTxnGroupID accepts: a
// non-zero ID equal to the hash of its txns with Group zeroed.
func validGroupID(group []transactions.SignedTxn) bool {
	if len(group) == 0 || group[0].Txn.Group.IsZero() {
		return false
	}
	id := group[0].Txn.Group
	for _, stxn := range group {
		if stxn.Txn.Group != id {
			return false
		}
	}
	return computeGroupID(group) == id
}

// computeGroupID returns the group ID of group's txns.
func computeGroupID(group []transactions.SignedTxn) crypto.Digest {
	tg := transactions.TxGroup{TxGroupHashes: make([]crypto.Digest, len(group))}
	for i, stxn := range group {
		txn := stxn.Txn
		txn.Group = crypto.Digest{}
		tg.TxGroupHashes[i] = crypto.Digest(txn.ID())
	}
	return crypto.HashObj(tg)
}
