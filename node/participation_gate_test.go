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

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/crypto"
	"github.com/algorand/go-algorand/data/account"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/logging"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
	"github.com/algorand/go-algorand/util/db"
)

func gateGenesis(network protocol.NetworkID, schemaID string) bookkeeping.Genesis {
	return bookkeeping.Genesis{
		SchemaID:    schemaID,
		Proto:       protocol.ConsensusCurrentVersion,
		Network:     network,
		FeeSink:     sinkAddr.String(),
		RewardsPool: poolAddr.String(),
	}
}

func gateConfig(enable bool) config.Local {
	cfg := config.GetDefaultLocal()
	cfg.DisableNetworking = true
	cfg.DNSBootstrapID = ""
	cfg.DangerouslyEnableParticipation = enable
	return cfg
}

// writeGatePartKey writes a participation key file named name into dir and returns its path.
func writeGatePartKey(t *testing.T, dir, name string) string {
	require.NoError(t, os.MkdirAll(dir, 0700))
	path := filepath.Join(dir, config.PartKeyFilename(name, 1, 100))
	access, err := db.MakeAccessor(path, false, false)
	require.NoError(t, err)
	var parent basics.Address
	crypto.RandBytes(parent[:])
	part, err := account.FillDBWithParticipationKeys(access, parent, 1, 100, 10)
	require.NoError(t, err)
	part.Close() // closes access
	return path
}

func startGateNode(t *testing.T, rootDir string, cfg config.Local, genesis bookkeeping.Genesis) *AlgorandFullNode {
	n, err := MakeFull(logging.TestingLog(t), rootDir, cfg, []string{}, genesis)
	require.NoError(t, err)
	require.NoError(t, n.Start())
	return n
}

// TestParticipationRefusedPartKeyFile: on a gated network, a partkey file in the data directory stops the node from
// starting unless DangerouslyEnableParticipation is set. Other networks are not gated.
func TestParticipationRefusedPartKeyFile(t *testing.T) {
	partitiontest.PartitionTest(t)

	genesis := gateGenesis(config.Testnet, "v1.0")
	require.Equal(t, "testnet-v1.0", genesis.ID())
	rootDir := t.TempDir()
	keyFile := writeGatePartKey(t, filepath.Join(rootDir, genesis.ID()), "gate")

	n, err := MakeFull(logging.TestingLog(t), rootDir, gateConfig(false), []string{}, genesis)
	require.Nil(t, n)
	require.ErrorIs(t, err, ErrParticipationRefused)
	require.ErrorContains(t, err, keyFile)
	require.ErrorContains(t, err, "testnet-v1.0")
	// the refused node opened no ledger, and created no participation registry
	require.NoFileExists(t, filepath.Join(rootDir, genesis.ID(), "ledger.tracker.sqlite"))
	require.NoFileExists(t, filepath.Join(rootDir, genesis.ID(), config.ParticipationRegistryFilename))

	n = startGateNode(t, rootDir, gateConfig(true), genesis)
	keys, err := n.ListParticipationKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	n.Stop()

	// a network that isn't gated loads the same key without the flag
	other := gateGenesis(config.Devtestnet, "v1.0")
	otherRoot := t.TempDir()
	writeGatePartKey(t, filepath.Join(otherRoot, other.ID()), "gate")
	n = startGateNode(t, otherRoot, gateConfig(false), other)
	keys, err = n.ListParticipationKeys()
	require.NoError(t, err)
	require.Len(t, keys, 1)
	n.Stop()
}

// TestParticipationRefusedRegistry: a key installed through the REST path while the flag was set lives only in the
// participation registry; with the flag off the node refuses to start and names the registry, in ColdDataDir.
func TestParticipationRefusedRegistry(t *testing.T) {
	partitiontest.PartitionTest(t)

	genesis := gateGenesis(config.Mainnet, "v1.0")
	rootDir := t.TempDir()
	coldDir := t.TempDir()
	keyBinary, err := os.ReadFile(writeGatePartKey(t, t.TempDir(), "gate"))
	require.NoError(t, err)

	cfg := gateConfig(true)
	cfg.ColdDataDir = coldDir
	// without StateproofDir and CrashDBDir, a ColdDataDir node fails its second start: config.moveDirIfExists refuses
	// because stateproof.sqlite and crash.sqlite exist in the hot dir, though there is nothing to move
	cfg.StateproofDir = t.TempDir()
	cfg.CrashDBDir = t.TempDir()
	n := startGateNode(t, rootDir, cfg, genesis)
	partID, err := n.InstallParticipationKey(keyBinary)
	require.NoError(t, err)
	record, err := n.GetParticipationKey(partID)
	require.NoError(t, err)
	n.Stop()

	cfg.DangerouslyEnableParticipation = false
	n, err = MakeFull(logging.TestingLog(t), rootDir, cfg, []string{}, genesis)
	require.Nil(t, n)
	require.ErrorIs(t, err, ErrParticipationRefused)
	require.ErrorContains(t, err, filepath.Join(coldDir, genesis.ID(), config.ParticipationRegistryFilename))
	require.ErrorContains(t, err, record.Account.String())

	// with a registry, a partkey file is named alongside its keys
	keyFile := writeGatePartKey(t, filepath.Join(rootDir, genesis.ID()), "gate")
	n, err = MakeFull(logging.TestingLog(t), rootDir, cfg, []string{}, genesis)
	require.Nil(t, n)
	require.ErrorIs(t, err, ErrParticipationRefused)
	require.ErrorContains(t, err, keyFile)
	require.ErrorContains(t, err, record.Account.String())
}

// TestParticipationRefusedInstall: on a gated network without keys the node starts, and refuses to install keys.
func TestParticipationRefusedInstall(t *testing.T) {
	partitiontest.PartitionTest(t)

	genesis := gateGenesis(config.Betanet, "v1.0")
	rootDir := t.TempDir()
	keyBinary, err := os.ReadFile(writeGatePartKey(t, t.TempDir(), "gate"))
	require.NoError(t, err)

	n := startGateNode(t, rootDir, gateConfig(false), genesis)
	defer n.Stop()
	require.True(t, n.ParticipationRefused())
	_, err = n.InstallParticipationKey(keyBinary)
	require.ErrorIs(t, err, ErrParticipationRefused)

	keys, err := n.ListParticipationKeys()
	require.NoError(t, err)
	require.Empty(t, keys)
	files, err := os.ReadDir(filepath.Join(rootDir, genesis.ID()))
	require.NoError(t, err)
	for _, f := range files {
		require.False(t, config.IsPartKeyFilename(f.Name()), f.Name())
		require.False(t, strings.HasPrefix(f.Name(), "tempPartKeyBinary."), f.Name()) // createTemporaryParticipationKey
	}
}
