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

package main

import (
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/util/db"
)

// fillRounds reads the rounds' headers from the archive and their state deltas
// from an algod API.
type fillRounds struct {
	blocks *sql.DB
	api    string
	client http.Client
}

func (f *fillRounds) Header(rnd basics.Round) (hdr bookkeeping.BlockHeader, err error) {
	var buf []byte
	if err = f.blocks.QueryRow("SELECT hdrdata FROM blocks WHERE rnd = ?", rnd).Scan(&buf); err != nil {
		return hdr, fmt.Errorf("the archive's header of round %d: %w", rnd, err)
	}
	err = protocol.Decode(buf, &hdr)
	return hdr, err
}

func (f *fillRounds) Delta(rnd basics.Round) (*ledgercore.StateDelta, error) {
	url := fmt.Sprintf("%s/v2/deltas/%d?format=msgpack", f.api, rnd)
	var err error
	for try := 0; try < 3; try++ {
		if try > 0 {
			time.Sleep(time.Second)
		}
		var resp *http.Response
		if resp, err = f.client.Get(url); err != nil {
			continue
		}
		var body []byte
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			continue
		}
		if resp.StatusCode != http.StatusOK {
			err = fmt.Errorf("GET %s: %s: %s", url, resp.Status, strings.TrimSpace(string(body)))
			continue
		}
		var sd ledgercore.StateDelta
		if err = protocol.DecodeReflect(body, &sd); err != nil {
			return nil, fmt.Errorf("GET %s: %w", url, err)
		}
		return &sd, nil
	}
	return nil, err
}

// fill adds to the time travel store of dataDir the rounds that an algod
// without time travel committed to its account DB (ledger.FillTimeTravelGap).
func fill(blocksPath, dataDir, api string) error {
	genesis, err := bookkeeping.LoadGenesisFromFile(filepath.Join(dataDir, config.GenesisJSONFile))
	if err != nil {
		return err
	}
	cfg, err := config.LoadConfigFromDisk(dataDir)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	dirs, err := cfg.EnsureAndResolveGenesisDirs(dataDir, genesis.ID(), logging.Base())
	if err != nil {
		return err
	}
	if _, err = os.Stat(blocksPath); err != nil {
		return err
	}
	src, err := db.MakeAccessor(blocksPath, true, false)
	if err != nil {
		return fmt.Errorf("%s: %w", blocksPath, err)
	}
	defer src.Close()
	rounds := &fillRounds{blocks: src.Handle, api: strings.TrimRight(api, "/"), client: http.Client{Timeout: time.Minute}}
	hdr, err := rounds.Header(0)
	if err != nil {
		return err
	}
	if hdr.GenesisHash != genesis.Hash() {
		return fmt.Errorf("%s belongs to another network (genesis hash %v)", blocksPath, hdr.GenesisHash)
	}

	printed := time.Now()
	from, to, err := ledger.FillTimeTravelGap(ledger.DirsAndPrefix{DBFilePrefix: config.LedgerFilenamePrefix, ResolvedGenesisDirs: dirs}, cfg, rounds, logging.Base(), func(rnd basics.Round) {
		if time.Since(printed) >= progressInterval {
			printed = time.Now()
			fmt.Printf("added round %d\n", rnd)
		}
	})
	if err != nil {
		return fmt.Errorf("%w (is algod running on %s?)", err, dataDir)
	}
	if from > to {
		fmt.Printf("the time travel store already reaches the account DB's round %d\n", to)
		return nil
	}
	fmt.Printf("checked the store's last rounds against the deltas, added rounds %d..%d\n", from, to)
	return nil
}
