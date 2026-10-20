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

package eval

import (
	"github.com/algorand/go-algorand/data/transactions"
)

// Simulate's skip-validity-window mode (non-standard fork feature): txns are evaluated as sent,
// even outside their FirstValid..LastValid window.

// alive is BlockHeader.Alive for txn evaluation. With skipValidityWindow, it checks a copy of
// header whose window is the block round, so only the genesis ID and hash checks apply.
func (eval *BlockEvaluator) alive(header transactions.Header) error {
	if eval.skipValidityWindow {
		header.FirstValid = eval.block.Round()
		header.LastValid = eval.block.Round()
	}
	return eval.block.Alive(header)
}

// txnForTail returns the txn that the block's state records as included: roundCowState.addTx
// keeps its lease until its LastValid. With skipValidityWindow, a LastValid before the block
// round would let a second txn with the same lease into the block, so the copy's LastValid is at
// least the block round. addTx reads only Sender, Lease and LastValid.
func (eval *BlockEvaluator) txnForTail(txn transactions.Transaction) transactions.Transaction {
	if eval.skipValidityWindow {
		txn.LastValid = max(txn.LastValid, eval.block.Round())
	}
	return txn
}
