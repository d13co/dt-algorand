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

//go:build !windows

package main

import (
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/algorand/go-deadlock"

	"github.com/algorand/go-algorand/config"
	"github.com/algorand/go-algorand/data/bookkeeping"
	"github.com/algorand/go-algorand/ledger/timetravel"
	"github.com/algorand/go-algorand/protocol"
	"github.com/algorand/go-algorand/test/partitiontest"
)

// TestMain runs ttsnap for a bootstrap phase that TestBootstrapAsRoot
// re-executes the test binary for, as the data directory's owner.
func TestMain(m *testing.M) {
	if os.Getenv(bootstrapPhaseEnv) != "" {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// fakeAlgod answers /versions, /v2/status and POST /v2/catchup/ as algod
// does.
type fakeAlgod struct {
	srv        *httptest.Server
	mu         deadlock.Mutex
	lastRound  uint64
	catchpoint string
	catchups   []string
}

func newFakeAlgod(t *testing.T, genesis bookkeeping.Genesis) *fakeAlgod {
	a := &fakeAlgod{}
	gh := genesis.Hash()
	a.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a.mu.Lock()
		defer a.mu.Unlock()
		switch {
		case r.URL.Path == "/versions":
			fmt.Fprintf(w, `{"genesis_id":%q,"genesis_hash_b64":%q}`, genesis.ID(), base64.StdEncoding.EncodeToString(gh[:]))
		case r.URL.Path == "/v2/status":
			fmt.Fprintf(w, `{"last-round":%d,"catchpoint":%q}`, a.lastRound, a.catchpoint)
		case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v2/catchup/"):
			// algod unescapes the label twice: echo the path parameter, then
			// oapi-codegen's binder, and catchup escapes it to match
			label, _ := url.PathUnescape(strings.TrimPrefix(r.URL.EscapedPath(), "/v2/catchup/"))
			label, _ = url.PathUnescape(label)
			a.catchups = append(a.catchups, label)
			w.WriteHeader(http.StatusCreated)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(a.srv.Close)
	return a
}

func (a *fakeAlgod) set(lastRound uint64, catchpoint string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastRound, a.catchpoint = lastRound, catchpoint
}

func (a *fakeAlgod) posted() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.catchups...)
}

// testLabel is a catchpoint label for round rnd.
func testLabel(rnd uint64) string {
	return fmt.Sprintf("%d#%s", rnd, base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(make([]byte, 32)))
}

// bootstrapNode makes a consumer data directory with config, and bootstrap
// options whose start command "starts" a (the fake algod writes its
// algod.net) and counts its runs in the file started.
func bootstrapNode(t *testing.T, n *testNet, a *fakeAlgod, cfg string) (bootstrapOpts, string) {
	dataDir := n.consumer()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, config.ConfigFilename), []byte(cfg), 0o644))
	host := strings.TrimPrefix(a.srv.URL, "http://")
	start := fmt.Sprintf("echo %s > %s/algod.net && echo x >> %s/started", host, dataDir, dataDir)
	return bootstrapOpts{
		fetch:   fetchOpts{dataDir: dataDir, base: n.srv.URL, keys: n.keys, workers: 2, parallel: 2, progressLog: io.Discard},
		holders: true, start: start, wait: 10 * time.Second, pollEvery: 10 * time.Millisecond,
		label: testLabel(n.rounds), log: io.Discard,
	}, dataDir
}

func startCount(t *testing.T, dataDir string) int {
	b, err := os.ReadFile(filepath.Join(dataDir, "started"))
	if os.IsNotExist(err) {
		return 0
	}
	require.NoError(t, err)
	return strings.Count(string(b), "x")
}

func holdersState(t *testing.T, dir string) timetravel.HoldersState {
	st, err := timetravel.Open(dir, timetravel.Options{ReadOnly: true})
	require.NoError(t, err)
	defer st.Close()
	return st.HoldersIndexState()
}

func TestBootstrap(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	m := n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)

	require.NoError(t, o.run())
	rec, err := installed(dataDir, n.genesis)
	require.NoError(t, err)
	require.Equal(t, m.History, rec.History)
	require.Equal(t, timetravel.HoldersComplete, holdersState(t, n.installedHistory(dataDir)))
	require.Equal(t, 1, startCount(t, dataDir))
	require.Equal(t, []string{o.label}, a.posted())

	// a rerun while the node catches up, then once it caught up (at the
	// catchpoint's round, which may be before the snapshot's) or synced past
	// the snapshot, installs, starts and catches up nothing
	for _, st := range []struct {
		round      uint64
		catchpoint string
	}{{0, o.label}, {n.rounds/2 + 1, ""}, {n.rounds + 1, ""}} {
		a.set(st.round, st.catchpoint)
		require.NoError(t, o.run())
	}
	require.Equal(t, 1, startCount(t, dataDir))
	require.Equal(t, []string{o.label}, a.posted())

	// a node that syncs from genesis before the catchup: one more
	a.set(n.rounds/2, "")
	require.NoError(t, o.run())
	require.Len(t, a.posted(), 2)
}

// TestBootstrapStaleAlgodNet checks that an algod.net a crash left, now
// naming another network's node, doesn't pass for the node of the data
// directory.
func TestBootstrapStaleAlgodNet(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	other := n.genesis
	other.Network = "othernet"
	a := newFakeAlgod(t, other)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.net"), []byte(strings.TrimPrefix(a.srv.URL, "http://")+"\n"), 0o644))
	require.False(t, nodeAnswers(dataDir, n.genesis))
	require.True(t, nodeAnswers(dataDir, other))
	running, err := o.install()
	require.NoError(t, err)
	require.False(t, running, "installed")
	require.Equal(t, timetravel.HoldersComplete, holdersState(t, n.installedHistory(dataDir)))
}

// TestBootstrapRunningWithoutSnapshot checks that a node that runs on a
// history no snapshot installed is refused, rather than taken as done.
func TestBootstrapRunningWithoutSnapshot(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.net"), []byte(strings.TrimPrefix(a.srv.URL, "http://")+"\n"), 0o644))
	_, err := o.install()
	require.ErrorContains(t, err, "no snapshot installed")
	require.ErrorContains(t, o.run(), "no snapshot installed")
	require.Empty(t, a.posted())
}

// TestBootstrapResumes checks that a rerun after the install, with the node
// stopped, skips the download (which would refuse the installed history)
// and the complete index, and starts the node.
func TestBootstrapResumes(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	running, err := o.install()
	require.NoError(t, err)
	require.False(t, running)

	n.addRounds(5)
	n.publish()
	require.NoError(t, o.run())
	require.Equal(t, 1, startCount(t, dataDir))
	require.Len(t, a.posted(), 1)
	require.Equal(t, timetravel.HoldersComplete, holdersState(t, n.installedHistory(dataDir)))

	// a full snapshot's record is not a history bootstrap catches up
	rec := filepath.Join(dataDir, n.genesis.ID(), installedFile)
	b, err := os.ReadFile(rec)
	require.NoError(t, err)
	m, err := decodeManifest(b)
	require.NoError(t, err)
	m.Kind = kindFull
	b, err = encodeManifest(m)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(rec, b, 0o644))
	require.NoError(t, os.Remove(filepath.Join(dataDir, "algod.net")))
	_, err = o.install()
	require.ErrorContains(t, err, "full snapshot is installed")
}

func TestBootstrapHoldersIndexOption(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)

	// asked for, but the config would have algod delete it: refused before
	// anything is downloaded
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true}`)
	_, err := o.install()
	require.ErrorContains(t, err, "TimeTravelHoldersIndex")
	_, err = os.Stat(filepath.Join(dataDir, n.genesis.ID()))
	require.True(t, os.IsNotExist(err), "nothing is written")

	o.holders = false
	running, err := o.install()
	require.NoError(t, err)
	require.False(t, running)
	require.Equal(t, timetravel.HoldersNone, holdersState(t, n.installedHistory(dataDir)))

	o, dataDir = bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	o.holders = false
	_, err = o.install()
	require.NoError(t, err)
	require.Equal(t, timetravel.HoldersNone, holdersState(t, n.installedHistory(dataDir)))
}

func TestBootstrapWaitTimesOut(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	o.start, o.wait = startNone, 50*time.Millisecond
	require.ErrorContains(t, o.run(), "did not answer")
	require.Empty(t, a.posted())
	require.Equal(t, 0, startCount(t, dataDir))
}

func TestDefaultStart(t *testing.T) {
	partitiontest.PartitionTest(t)
	units := t.TempDir()
	var noBrew []string

	_, _, err := defaultStart(debDataDir, "linux", []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "no unit installed")
	require.NoError(t, os.WriteFile(filepath.Join(units, debUnit), nil, 0o644))
	start, root, err := defaultStart(debDataDir+"/", "linux", []string{t.TempDir(), units}, noBrew)
	require.NoError(t, err)
	require.Equal(t, debStart, start)
	require.True(t, root)
	_, _, err = defaultStart(t.TempDir(), "linux", []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "another data directory")

	prefix := t.TempDir()
	brew := []string{t.TempDir(), prefix}
	start, root, err = defaultStart(filepath.Join(prefix, "var", "dtalgorand"), "darwin", nil, brew)
	require.NoError(t, err)
	require.Equal(t, brewStart, start)
	require.False(t, root)
	_, _, err = defaultStart(filepath.Join(prefix, "var", "other"), "darwin", nil, brew)
	require.ErrorContains(t, err, "-start")

	require.ErrorContains(t, checkStartUser(debStart, true, 1000), "needs root")
	require.NoError(t, checkStartUser(debStart, true, 0))
	require.ErrorContains(t, checkStartUser(brewStart, false, 0), "without sudo")
	require.NoError(t, checkStartUser(brewStart, false, 501))
	require.NoError(t, checkStartUser("goal node start -d x", false, 0))
}

// rootNode makes, under a directory the owner uid can traverse (unlike
// t.TempDir's), a keys file and a data directory that uid owns.
func rootNode(t *testing.T, n *testNet, uid int) (keys, dataDir string) {
	base, err := os.MkdirTemp("", "bootstrap-root")
	require.NoError(t, err)
	t.Cleanup(func() { os.RemoveAll(base) })
	require.NoError(t, os.Chmod(base, 0o755))
	keys = filepath.Join(base, "keys")
	b, err := os.ReadFile(n.keys)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(keys, b, 0o644))
	dataDir = filepath.Join(base, "node")
	require.NoError(t, os.Mkdir(dataDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, config.GenesisJSONFile), protocol.EncodeJSON(&n.genesis), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, config.ConfigFilename), []byte(`{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`), 0o644))
	for _, p := range []string{dataDir, filepath.Join(dataDir, config.GenesisJSONFile), filepath.Join(dataDir, config.ConfigFilename)} {
		require.NoError(t, os.Chown(p, uid, uid))
	}
	return keys, dataDir
}

// TestBootstrapAsRoot checks that bootstrap as root on another user's data
// directory installs and catches up as that user, starts the node as that
// user too unless the start needs root (the package's systemctl), and that a
// phase refuses to run as root. It needs root (a container: go test -c, then
// run it there).
func TestBootstrapAsRoot(t *testing.T) {
	partitiontest.PartitionTest(t)
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	const uid = 65534 // nobody
	n := newTestNet(t)
	n.publish()
	label := testLabel(n.rounds)

	for _, asRoot := range []bool{false, true} {
		a := newFakeAlgod(t, n.genesis)
		keys, dataDir := rootNode(t, n, uid)
		host := strings.TrimPrefix(a.srv.URL, "http://")
		o := bootstrapOpts{
			fetch: fetchOpts{dataDir: dataDir, base: n.srv.URL, keys: keys, progressLog: io.Discard},
			// what the started algod would write, as the owner
			start:       fmt.Sprintf("echo %s > %s/algod.net && chown %d %s/algod.net && echo x >> %s/started && id -u > %s/start-uid", host, dataDir, uid, dataDir, dataDir, dataDir),
			startAsRoot: asRoot,
			wait:        10 * time.Second, pollEvery: 10 * time.Millisecond, log: io.Discard,
		}
		args := []string{"-url", n.srv.URL, "-workers", "2", "-parallel", "2", "-label", label}
		require.NoError(t, o.bootstrap(args))

		require.Equal(t, 1, startCount(t, dataDir))
		u, err := os.ReadFile(filepath.Join(dataDir, "start-uid"))
		require.NoError(t, err)
		want := strconv.Itoa(uid)
		if asRoot {
			want = "0"
		}
		require.Equal(t, want, strings.TrimSpace(string(u)), "the start's user")
		require.Equal(t, []string{label}, a.posted())
		require.Equal(t, timetravel.HoldersComplete, holdersState(t, n.installedHistory(dataDir)))
		err = filepath.WalkDir(filepath.Join(dataDir, n.genesis.ID()), func(p string, _ fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			owner, err := fileOwner(p)
			if err == nil && owner != uid {
				err = fmt.Errorf("%s belongs to %d, not the data directory's owner", p, owner)
			}
			return err
		})
		require.NoError(t, err)

		t.Setenv(bootstrapPhaseEnv, "install")
		require.ErrorContains(t, cmdBootstrap(append(args, "-d", dataDir, "-keys", keys)), "runs as the data directory's owner")
		os.Unsetenv(bootstrapPhaseEnv)
	}
}
