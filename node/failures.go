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

package node

// The failures kept for replay (EnableFailureCapture). Non-standard fork feature.

import (
	"path/filepath"
	"time"

	"github.com/algorand/go-algorand/data/failures"
)

// FailureStoreFilename is the file of the failures kept by EnableFailureCapture, in the genesis
// directory.
const FailureStoreFilename = "failures.sqlite"

// openFailureStore opens the failure store when the config asks for it. A store that doesn't
// open leaves the node without it: it is a debugging aid, not a reason to stop.
func (node *AlgorandFullNode) openFailureStore() {
	if !node.config.EnableFailureCapture {
		return
	}
	path := filepath.Join(node.genesisDirs.RootGenesisDir, FailureStoreFilename)
	cfg := failures.Config{
		Retention: time.Duration(min(node.config.FailureCaptureRetentionSeconds, uint64(1<<63-1)/uint64(time.Second))) * time.Second,
		MaxBytes:  node.config.FailureCaptureMaxBytes,
	}
	store, err := failures.Open(path, cfg, node.log)
	if err != nil {
		node.log.Errorf("failure capture is off: %v", err)
		return
	}
	node.failures = store
}

func (node *AlgorandFullNode) closeFailureStore() {
	if node.failures != nil {
		node.failures.Close()
	}
}

// FailureStore returns the store of the failures the node keeps, or nil when it keeps none.
func (node *AlgorandFullNode) FailureStore() *failures.Store {
	return node.failures
}
