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

package ledgercore

import (
	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
)

// CatchpointLookback returns how far a catchpoint's balances round is
// before its round under proto, as catchpoint catchup computes it
// (catchpointCatchupAccessorImpl.StoreBalancesRound).
func CatchpointLookback(proto config.ConsensusParams) basics.Round {
	if proto.CatchpointLookback != 0 {
		return basics.Round(proto.CatchpointLookback)
	}
	return basics.Round(proto.MaxBalLookback)
}
