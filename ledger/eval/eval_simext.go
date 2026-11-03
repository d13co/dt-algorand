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

// Simulate at a past round (non-standard fork feature): only the consensus rules apply.

// newBlockPolicies is the state's generate: whether the policies that apply only to new blocks
// (pool admission, block assembly) hold, such as the refusal to close an account whose minimum
// balance is above the base (checkCloseMinBalance). They are node policy, not consensus, and newer
// than much of the chain, so simulate at a past round drops them (skipNewBlockPolicies): a txn
// that block validation accepted then is accepted. The evaluator still generates the block.
func (eval *BlockEvaluator) newBlockPolicies() bool {
	return eval.generate && !eval.skipNewBlockPolicies
}
