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
	"encoding/binary"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/basics"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/ledgercore"
	ledgertesting "github.com/algorand/go-algorand/ledger/testing"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

const testSigner = "snapshots@test"

// testNet is a network with a genesis file, a producer genesis directory
// with a history, an ssh key and a snapshot tree served over HTTP.
type testNet struct {
	t        *testing.T
	genesis  bookkeeping.Genesis
	genBlock bookkeeping.Block
	producer string // genesis dir
	key      string
	signers  string
	out      string
	srv      *httptest.Server
	hook     atomic.Pointer[func(*http.Request)] // sees each request
	rounds   uint64
	// deltas are the rounds' state deltas, which the server also serves as
	// an algod's /v2/deltas; tamper changes what it serves
	deltas sync.Map // uint64 -> ledgercore.StateDelta
	tamper atomic.Bool
}

func newTestNet(t *testing.T) *testNet {
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		t.Skip("needs ssh-keygen")
	}
	n := &testNet{t: t}
	sink, pool := ledgertesting.RandomAddress(), ledgertesting.RandomAddress()
	n.genesis = bookkeeping.Genesis{
		SchemaID: "v1", Network: "ttsnaptest", Proto: protocol.ConsensusCurrentVersion,
		FeeSink: sink.String(), RewardsPool: pool.String(),
		Allocation: []bookkeeping.GenesisAllocation{
			{Address: sink.String(), State: bookkeeping.GenesisAccountData{MicroAlgos: basics.MicroAlgos{Raw: 1e12}, Status: basics.NotParticipating}},
			{Address: pool.String(), State: bookkeeping.GenesisAccountData{MicroAlgos: basics.MicroAlgos{Raw: 1e12}, Status: basics.NotParticipating}},
		},
	}
	bal, err := n.genesis.Balances()
	require.NoError(t, err)
	n.genBlock, err = bookkeeping.MakeGenesisBlock(n.genesis.Proto, bal, n.genesis.ID(), n.genesis.Hash())
	require.NoError(t, err)

	n.producer = filepath.Join(t.TempDir(), n.genesis.ID())
	require.NoError(t, os.MkdirAll(n.producer, 0o755))
	n.addRounds(30)

	dir := t.TempDir()
	n.key = filepath.Join(dir, "key")
	out, err := exec.Command("ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-C", testSigner, "-f", n.key).CombinedOutput()
	require.NoError(t, err, string(out))
	pub, err := os.ReadFile(n.key + ".pub")
	require.NoError(t, err)
	n.signers = filepath.Join(dir, "allowed_signers")
	require.NoError(t, os.WriteFile(n.signers, []byte(fmt.Sprintf("%s namespaces=%q %s", testSigner, sigNamespace, pub)), 0o644))

	n.out = t.TempDir()
	files := http.FileServer(http.Dir(n.out))
	n.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hook := n.hook.Load(); hook != nil {
			(*hook)(r)
		}
		if rnd, ok := strings.CutPrefix(r.URL.Path, "/v2/deltas/"); ok {
			n.serveDelta(w, rnd)
			return
		}
		files.ServeHTTP(w, r)
	}))
	t.Cleanup(n.srv.Close)
	return n
}

// addRounds extends the producer's history by k hash-chained rounds, each
// changing one box.
func (n *testNet) addRounds(k uint64) {
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{})
	require.NoError(n.t, err)
	defer st.Close()
	hdr := n.genBlock.BlockHeader
	if last, ok := st.Last(); ok {
		b, ok, err := st.Header(last)
		require.NoError(n.t, err)
		require.True(n.t, ok)
		require.NoError(n.t, protocol.Decode(b, &hdr))
	} else {
		require.NoError(n.t, st.AddRound(0, nil, protocol.Encode(&hdr), nil))
	}
	for range k {
		prev := hdr.Hash()
		hdr.Round++
		hdr.Branch = prev
		r := uint64(hdr.Round)
		sd := ledgercore.StateDelta{
			KvMods: map[string]ledgercore.KvValueDelta{fmt.Sprintf("bx:%08d", r%7): {Data: binary.BigEndian.AppendUint64(nil, r)}},
			Totals: ledgercore.AccountTotals{RewardsLevel: r},
		}
		n.deltas.Store(r, sd)
		require.NoError(n.t, st.AddRound(r, timetravel.Changes(&sd), protocol.Encode(&hdr), nil))
	}
	n.rounds = uint64(hdr.Round)
	require.NoError(n.t, st.Sync())
}

func (n *testNet) serveDelta(w http.ResponseWriter, rnd string) {
	var r uint64
	if _, err := fmt.Sscan(rnd, &r); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	v, ok := n.deltas.Load(r)
	if !ok {
		http.NotFound(w, nil)
		return
	}
	sd := v.(ledgercore.StateDelta)
	if n.tamper.Load() {
		sd.Totals.RewardsLevel++
	}
	w.Write(protocol.EncodeReflect(&sd))
}

func (n *testNet) publish() *Manifest {
	_, _, err := prepare(n.producer, false, nil)
	require.NoError(n.t, err)
	m, err := publish(n.producer, n.out, n.key, false, nil)
	require.NoError(n.t, err)
	return m
}

// consumer makes a new node's data directory.
func (n *testNet) consumer() string {
	dir := n.t.TempDir()
	require.NoError(n.t, os.WriteFile(filepath.Join(dir, config.GenesisJSONFile), protocol.EncodeJSON(&n.genesis), 0o644))
	require.NoError(n.t, os.WriteFile(filepath.Join(dir, config.ConfigFilename), []byte(`{"EnableTimeTravel": true}`), 0o644))
	return dir
}

func (n *testNet) fetch(dataDir string, mod ...func(*fetchOpts)) (*Manifest, error) {
	o := fetchOpts{dataDir: dataDir, base: n.srv.URL, keys: n.signers, signer: testSigner, parallel: 2, progressLog: io.Discard}
	for _, f := range mod {
		f(&o)
	}
	m, _, err := fetch(o)
	return m, err
}

func (n *testNet) installedHistory(dataDir string) string {
	return filepath.Join(dataDir, n.genesis.ID(), historyDir)
}

// rewriteLatest replaces the latest manifest with what edit makes of it,
// signed with the producer's key.
func (n *testNet) rewriteLatest(edit func(*Manifest)) {
	p := n.latestManifest()
	b, err := os.ReadFile(p)
	require.NoError(n.t, err)
	m, err := decodeManifest(b)
	require.NoError(n.t, err)
	edit(m)
	b, err = encodeManifest(m)
	require.NoError(n.t, err)
	require.NoError(n.t, writeSigned(p, b, n.key))
}

// latestManifest is the path of the manifest the latest pointer names.
func (n *testNet) latestManifest() string {
	b, err := os.ReadFile(filepath.Join(n.out, filepath.FromSlash(latestPath(n.genesis.ID(), kindHistory))))
	require.NoError(n.t, err)
	name, err := parseLatest(b, kindHistory)
	require.NoError(n.t, err)
	return filepath.Join(n.out, "snapshots", n.genesis.ID(), name)
}

func TestPublishFetch(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	m := n.publish()
	require.Equal(t, basics.Round(n.rounds), m.History.Last)

	dataDir := n.consumer()
	got, err := n.fetch(dataDir)
	require.NoError(t, err)
	require.Equal(t, m.History, got.History)

	st, err := timetravel.Open(n.installedHistory(dataDir), timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	last, err := verifyHeaders(st, n.genesis.Hash(), nil)
	require.NoError(t, err)
	require.Equal(t, n.rounds, last)
	v, ok, err := st.Get(timetravel.KvKey("bx:00000003"), 10)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, binary.BigEndian.AppendUint64(nil, 10), v)
	require.NoError(t, st.Close())

	rec, err := installed(dataDir, n.genesis)
	require.NoError(t, err)
	require.Equal(t, m.History.Last, rec.History.Last)
	_, err = os.Stat(n.installedHistory(dataDir) + stageSuffix)
	require.True(t, os.IsNotExist(err), "the staging dir is renamed into place")

	_, err = n.fetch(dataDir)
	require.NoError(t, err, "a rerun of the same snapshot finishes what is left, here nothing")

	n.addRounds(5)
	n.publish()
	_, err = n.fetch(dataDir)
	require.ErrorContains(t, err, "already exists", "an installed history is not replaced")
}

// TestFetchRefuses checks that a manifest that is not the producer's, or that
// lists files outside the allowlist, is refused before anything is written.
func TestFetchRefuses(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()

	nothingWritten := func(dataDir string) {
		_, err := os.Stat(filepath.Join(dataDir, n.genesis.ID()))
		require.True(t, os.IsNotExist(err), "nothing is written to the data directory")
	}

	t.Run("signer", func(t *testing.T) {
		dataDir := n.consumer()
		_, err := n.fetch(dataDir, func(o *fetchOpts) { o.signer = "someone@else" })
		require.ErrorContains(t, err, "signature does not verify")
		nothingWritten(dataDir)
	})

	t.Run("edited", func(t *testing.T) {
		dataDir := n.consumer()
		p := n.latestManifest()
		b, err := os.ReadFile(p)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(p, []byte(strings.Replace(string(b), `"last": 30`, `"last": 31`, 1)), 0o644))
		defer os.WriteFile(p, b, 0o644)
		_, err = n.fetch(dataDir)
		require.ErrorContains(t, err, "signature does not verify")
		nothingWritten(dataDir)
	})

	for name, path := range map[string]string{
		"traversal": historyDir + "/../../../evil",
		"nested":    historyDir + "/sub/000001.sst",
		"absolute":  "/etc/passwd",
		"tracker":   trackerFile, // not in a history snapshot
		"lock":      historyDir + "/LOCK",
	} {
		t.Run(name, func(t *testing.T) {
			n := newTestNet(t)
			n.publish()
			n.rewriteLatest(func(m *Manifest) { m.Files[0].Path = path })
			dataDir := n.consumer()
			_, err := n.fetch(dataDir)
			require.ErrorContains(t, err, "can't hold")
			nothingWritten(dataDir)
		})
	}

	t.Run("stale", func(t *testing.T) {
		n := newTestNet(t)
		n.publish()
		n.rewriteLatest(func(m *Manifest) { m.Created = "2020-01-01T00:00:00Z" })
		dataDir := n.consumer()
		_, err := n.fetch(dataDir, func(o *fetchOpts) { o.maxAge = 24 * time.Hour })
		require.ErrorContains(t, err, "-max-age")
		nothingWritten(dataDir)
		_, err = n.fetch(dataDir) // no limit
		require.NoError(t, err)
	})

	t.Run("pointer", func(t *testing.T) {
		n := newTestNet(t)
		n.publish()
		p := filepath.Join(n.out, filepath.FromSlash(latestPath(n.genesis.ID(), kindHistory)))
		for _, bad := range []string{"../../blobs/x", "full-30.json", "history-30.json/../x", ""} {
			require.NoError(t, os.WriteFile(p, []byte(bad), 0o644))
			dataDir := n.consumer()
			_, err := n.fetch(dataDir)
			require.ErrorContains(t, err, "latest history pointer", bad)
			nothingWritten(dataDir)
		}
		_, _, err := gc(n.out, 1)
		require.Error(t, err, "gc refuses a bad pointer rather than guess what it names")
	})

	t.Run("network", func(t *testing.T) {
		n := newTestNet(t)
		n.publish()
		n.rewriteLatest(func(m *Manifest) { m.GenesisID = "othernet-v1" })
		dataDir := n.consumer()
		_, err := n.fetch(dataDir)
		require.ErrorContains(t, err, "is of network")
		nothingWritten(dataDir)
	})
}

// TestFetchBadBlob checks that a blob that does not match its SHA-256 is not
// installed.
func TestFetchBadBlob(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	m := n.publish()
	var sst File
	for _, f := range m.Files {
		if strings.HasSuffix(f.Path, ".sst") {
			sst = f
		}
	}
	require.NotEmpty(t, sst.SHA256)
	p := filepath.Join(n.out, filepath.FromSlash(blobPath(sst.SHA256)))
	b, err := os.ReadFile(p)
	require.NoError(t, err)
	bad := append([]byte(nil), b...)
	bad[len(bad)/2]++
	// the blob is a hard link to the producer's file: replace, don't modify
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.WriteFile(p, bad, 0o644))

	dataDir := n.consumer()
	_, err = n.fetch(dataDir)
	require.ErrorContains(t, err, "SHA-256")
	_, err = os.Stat(n.installedHistory(dataDir))
	require.True(t, os.IsNotExist(err))

	// fixed on the server: a rerun completes
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.WriteFile(p, b, 0o644))
	_, err = n.fetch(dataDir)
	require.NoError(t, err)
}

// TestFetchResumes leaves half of a file downloaded and checks that fetch
// completes it.
func TestFetchResumes(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	m := n.publish()
	dataDir := n.consumer()
	stage := n.installedHistory(dataDir) + stageSuffix
	require.NoError(t, os.MkdirAll(stage, 0o700))
	var big File
	for _, f := range m.Files {
		if f.Size > big.Size {
			big = f
		}
	}
	b, err := os.ReadFile(filepath.Join(n.out, filepath.FromSlash(blobPath(big.SHA256))))
	require.NoError(t, err)
	_, name, _ := strings.Cut(big.Path, "/")
	require.NoError(t, os.WriteFile(filepath.Join(stage, name), b[:len(b)/2], 0o600))

	var ranged atomic.Bool
	hook := func(r *http.Request) {
		if strings.Contains(r.URL.Path, big.SHA256) && r.Header.Get("Range") != "" {
			ranged.Store(true)
		}
	}
	n.hook.Store(&hook)
	_, err = n.fetch(dataDir)
	require.NoError(t, err)
	require.True(t, ranged.Load(), "the partial file was resumed")
}

func TestVerifyHeadersBrokenChain(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{})
	require.NoError(t, err)
	defer st.Close()
	_, err = verifyHeaders(st, n.genesis.Hash(), nil)
	require.NoError(t, err)

	// a header that does not chain onto the one before
	key := binary.BigEndian.AppendUint64([]byte{'H'}, 12)
	rec, closer, err := st.DB().Get(key)
	require.NoError(t, err)
	rec = append([]byte(nil), rec...)
	closer.Close()
	var hdr bookkeeping.BlockHeader
	require.NoError(t, protocol.Decode(rec[timetravel.DigestLen:], &hdr))
	hdr.TimeStamp++
	require.NoError(t, st.DB().Set(key, append(rec[:timetravel.DigestLen:timetravel.DigestLen], protocol.Encode(&hdr)...), nil))
	_, err = verifyHeaders(st, n.genesis.Hash(), nil)
	require.ErrorContains(t, err, "round 13: the header does not chain onto round 12's")

	_, err = verifyHeaders(st, bookkeeping.Genesis{Network: "x"}.Hash(), nil)
	require.ErrorContains(t, err, "round 0: the header is of another network")
}

func TestGC(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	first := n.publish()
	n.addRounds(20)
	second := n.publish()
	require.Greater(t, second.History.Last, first.History.Last)

	manifests, blobs, err := gc(n.out, 1)
	require.NoError(t, err)
	require.Equal(t, 1, manifests)
	require.Positive(t, blobs, "the first snapshot's own files go")
	_, err = os.Stat(filepath.Join(n.out, filepath.FromSlash(manifestPath(n.genesis.ID(), kindHistory, first.History.Last))))
	require.True(t, os.IsNotExist(err))
	for _, f := range second.Files {
		_, err := os.Stat(filepath.Join(n.out, filepath.FromSlash(blobPath(f.SHA256))))
		require.NoError(t, err, f.Path)
	}
	_, err = n.fetch(n.consumer())
	require.NoError(t, err)
}

func TestAllowedPath(t *testing.T) {
	partitiontest.PartitionTest(t)
	for _, p := range []string{historyDir + "/000123.sst", historyDir + "/1234567.sst", historyDir + "/000004.log", historyDir + "/000004-001.log",
		historyDir + "/MANIFEST-000001", historyDir + "/OPTIONS-000003", historyDir + "/marker.format-version.000004.024",
		historyDir + "/marker.manifest.000001.MANIFEST-000001", historyDir + "/000010.blob"} {
		require.True(t, allowedPath(kindHistory, p), p)
		require.True(t, allowedPath(kindFull, p), p)
	}
	for _, p := range []string{historyDir + "/LOCK", historyDir + "/temporary.000001.dbtmp", historyDir + "/../x", historyDir + "/a/000001.sst",
		"x/000001.sst", "000001.sst", historyDir, historyDir + "/", trackerFile + "-wal", "../" + trackerFile} {
		require.False(t, allowedPath(kindFull, p), p)
	}
	require.False(t, allowedPath(kindHistory, trackerFile))
	require.True(t, allowedPath(kindFull, trackerFile))
	require.True(t, allowedPath(kindFull, blocksFile))
}

func TestJoinURL(t *testing.T) {
	partitiontest.PartitionTest(t)
	u, err := joinURL("https://snap.example/tree/", "blobs/ab/abc")
	require.NoError(t, err)
	require.Equal(t, "https://snap.example/tree/blobs/ab/abc", u)
	for _, p := range []string{"../x", "a/../../x", "/abs", "a//b"} {
		_, err := joinURL("https://snap.example/tree", p)
		require.Error(t, err, p)
	}
	_, err = joinURL("file:///etc", "x")
	require.Error(t, err)
}

// TestVerifySample compares sampled rounds of a history with the deltas an
// algod serves for them.
func TestVerifySample(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	st, err := timetravel.Open(filepath.Join(n.producer, historyDir), timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer st.Close()
	require.NoError(t, verifySample(st, n.srv.URL, 20, 1, http.DefaultClient, nil))

	n.tamper.Store(true)
	err = verifySample(st, n.srv.URL, 1, 1, http.DefaultClient, nil)
	require.ErrorIs(t, err, timetravel.ErrDiverged)

	_, err = fetchDelta(http.DefaultClient, n.srv.URL, 999)
	require.ErrorContains(t, err, "404")
}

// TestPublishOrder checks that a snapshot is published once, and never
// older than the latest one.
func TestPublishOrder(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	_, err := publish(n.producer, n.out, n.key, false, nil)
	require.ErrorContains(t, err, "is published already")

	old := t.TempDir()
	require.NoError(t, os.CopyFS(filepath.Join(old, historyDir), os.DirFS(filepath.Join(n.producer, historyDir))))
	n.addRounds(5)
	n.publish()
	_, err = publish(old, n.out, n.key, false, nil)
	require.ErrorContains(t, err, "publish only newer snapshots")
}

// TestFetchPrunesStage leaves files of another snapshot in the staging dir
// and checks that they don't end up in the store.
func TestFetchPrunesStage(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	dataDir := n.consumer()
	stage := n.installedHistory(dataDir) + stageSuffix
	require.NoError(t, os.MkdirAll(stage, 0o700))
	stale := filepath.Join(stage, "marker.manifest.999999.MANIFEST-999999")
	require.NoError(t, os.WriteFile(stale, nil, 0o600))
	_, err := n.fetch(dataDir)
	require.NoError(t, err)
	_, err = os.Stat(filepath.Join(n.installedHistory(dataDir), filepath.Base(stale)))
	require.True(t, os.IsNotExist(err))
}
