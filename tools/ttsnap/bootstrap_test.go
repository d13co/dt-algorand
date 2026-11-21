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
	"bufio"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
// re-executes the test binary for, as the data directory's owner, and holds
// a store open for holdStore.
func TestMain(m *testing.M) {
	if os.Getenv(bootstrapPhaseEnv) != "" {
		main()
		os.Exit(0)
	}
	if dir := os.Getenv(holdStoreEnv); dir != "" {
		st, err := timetravel.Open(dir, timetravel.Options{})
		if err != nil {
			fmt.Println(err)
			os.Exit(1)
		}
		fmt.Println("open")
		_, _ = io.Copy(io.Discard, os.Stdin)
		st.Close()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

const holdStoreEnv = "TTSNAP_TEST_HOLD_STORE"

// holdStore has another process, of the program name, open the store at dir,
// as algod does when it opens its ledger, until the returned function is
// called.
func holdStore(t *testing.T, dir, name string) (pid int, closeStore func()) {
	exe, err := os.Executable()
	require.NoError(t, err)
	prog := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.Symlink(exe, prog))
	cmd := exec.Command(prog, "-test.run=^$")
	cmd.Env = append(os.Environ(), holdStoreEnv+"="+dir)
	in, err := cmd.StdinPipe()
	require.NoError(t, err)
	out, err := cmd.StdoutPipe()
	require.NoError(t, err)
	require.NoError(t, cmd.Start())
	line, err := bufio.NewReader(out).ReadString('\n')
	require.NoError(t, err)
	require.Equal(t, "open\n", line)
	var once sync.Once
	closeStore = func() {
		once.Do(func() {
			in.Close()
			_ = cmd.Wait()
		})
	}
	t.Cleanup(closeStore)
	return cmd.Process.Pid, closeStore
}

// onLine closes ch when a write contains prefix.
type onLine struct {
	prefix string
	ch     chan struct{}
}

func (w onLine) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), w.prefix) {
		close(w.ch)
	}
	return len(p), nil
}

// writeAlgodFiles writes the algod.net and algod.pid of a running algod at
// a, which runs in this process.
func writeAlgodFiles(t *testing.T, dataDir string, a *fakeAlgod) {
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.net"), []byte(strings.TrimPrefix(a.srv.URL, "http://")+"\n"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.pid"), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644))
}

// fakeAlgod answers /versions, /v2/status and POST /v2/catchup/ as algod
// does.
type fakeAlgod struct {
	srv        *httptest.Server
	mu         deadlock.Mutex
	lastRound  uint64
	catchpoint string
	catchups   []string
	token      string // that /v2/status wants, if any
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
		case r.URL.Path == "/v2/status" && a.token != "" && r.Header.Get("X-Algo-API-Token") != a.token:
			http.Error(w, "Invalid API Token", http.StatusUnauthorized)
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
// algod.net, and algod.pid names this process) and counts its runs in the
// file started.
func bootstrapNode(t *testing.T, n *testNet, a *fakeAlgod, cfg string) (bootstrapOpts, string) {
	dataDir := n.consumer()
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, config.ConfigFilename), []byte(cfg), 0o644))
	host := strings.TrimPrefix(a.srv.URL, "http://")
	start := fmt.Sprintf("echo %s > %s/algod.net && echo %d > %s/algod.pid && echo x >> %s/started", host, dataDir, os.Getpid(), dataDir, dataDir)
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
// naming another network's node or another node of the network, doesn't pass
// for the node of the data directory.
func TestBootstrapStaleAlgodNet(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	other := n.genesis
	other.Network = "othernet"
	a := newFakeAlgod(t, other)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	writeAlgodFiles(t, dataDir, a)
	require.False(t, nodeAnswers(dataDir, n.genesis))
	require.True(t, nodeAnswers(dataDir, other))
	running, err := o.install()
	require.NoError(t, err)
	require.False(t, running, "installed")
	require.Equal(t, timetravel.HoldersComplete, holdersState(t, n.installedHistory(dataDir)))

	// the network's, with another token
	same := newFakeAlgod(t, n.genesis)
	same.token = strings.Repeat("b", 64)
	writeAlgodFiles(t, dataDir, same)
	require.False(t, nodeAnswers(dataDir, n.genesis), "no token")
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.token"), []byte(strings.Repeat("a", 64)), 0o644))
	require.False(t, nodeAnswers(dataDir, n.genesis), "another token")
	same.token = strings.Repeat("a", 64)
	require.True(t, nodeAnswers(dataDir, n.genesis))

	// even with the token (a copy of the data directory), not once the
	// algod of algod.pid is gone
	dead := exec.Command("true")
	require.NoError(t, dead.Run())
	require.NoError(t, os.WriteFile(filepath.Join(dataDir, "algod.pid"), []byte(strconv.Itoa(dead.Process.Pid)+"\n"), 0o644))
	require.False(t, nodeAnswers(dataDir, n.genesis), "dead pid")
	require.NoError(t, os.Remove(filepath.Join(dataDir, "algod.pid")))
	require.False(t, nodeAnswers(dataDir, n.genesis), "no algod.pid")
}

// TestBootstrapLedgerOpening checks that a rerun while algod opens its
// ledger (it has the history open, and has not written algod.net yet) takes
// the node as running, even with no index step to fail, and that the wait
// gives up as soon as that algod exits.
func TestBootstrapLedgerOpening(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true}`)
	o.holders = false
	running, err := o.install()
	require.NoError(t, err)
	require.False(t, running)

	// another program reading the store is not waited on
	pid, closeStore := holdStore(t, n.installedHistory(dataDir), "ttsnap")
	_, err = o.install()
	require.ErrorContains(t, err, fmt.Sprintf("process %d (ttsnap) has the history", pid))
	closeStore()

	pid, closeStore = holdStore(t, n.installedHistory(dataDir), "dtalgod")
	held, ok := lockHolder(filepath.Join(n.installedHistory(dataDir), storeLock))
	require.True(t, ok)
	require.Equal(t, pid, held)
	running, err = o.install()
	require.NoError(t, err)
	require.True(t, running, "opening")

	// algod exits once the wait has seen it hold the store
	o.wait = time.Minute
	waiting := make(chan struct{})
	o.log = onLine{"waiting for algod", waiting}
	go func() {
		<-waiting
		closeStore()
	}()
	began := time.Now()
	require.ErrorContains(t, o.run(), fmt.Sprintf("algod (pid %d, which had the history open) exited", pid))
	require.Less(t, time.Since(began), 30*time.Second)
	require.Equal(t, 0, startCount(t, dataDir))
	_, ok = lockHolder(filepath.Join(n.installedHistory(dataDir), storeLock))
	require.False(t, ok)
}

// TestFetchLocked checks that an install into a data directory that another
// one is installing into is refused.
func TestFetchLocked(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true}`)
	lock := n.installedHistory(dataDir) + installLockSuffix
	require.NoError(t, os.MkdirAll(filepath.Dir(lock), 0o755))
	l, err := lockFile(lock)
	require.NoError(t, err)
	_, _, err = fetch(o.fetch)
	require.ErrorContains(t, err, "another ttsnap is installing")
	_, err = os.Stat(n.installedHistory(dataDir) + stageSuffix)
	require.True(t, os.IsNotExist(err), "no stage left behind")
	require.NoError(t, l.Close())
	_, _, err = fetch(o.fetch)
	require.NoError(t, err)
	_, err = os.Stat(lock)
	require.True(t, os.IsNotExist(err), "the lock file goes with the lock")
}

func TestSystemdUnit(t *testing.T) {
	partitiontest.PartitionTest(t)
	for start, unit := range map[string]string{
		debStart: "dtalgorand",
		systemctlStart + shellQuote(templateInstance("/srv/dt node")): `dtalgorand@-srv-dt\x20node`,
		"systemctl start dtalgorand":                                  "dtalgorand",
		"systemctl restart dtalgorand":                                "dtalgorand",
		"systemctl --user start x":                                    "",
		"systemctl stop dtalgorand":                                   "",
		brewStart:                                                     "",
		"goal node start -d x":                                        "",
		systemctlStart + "a b":                                        "",
	} {
		o := bootstrapOpts{start: start}
		require.Equal(t, unit, o.systemdUnit(), start)
	}
}

// TestBootstrapRunningWithoutSnapshot checks that a node that runs on a
// history no snapshot installed is refused, rather than taken as done.
func TestBootstrapRunningWithoutSnapshot(t *testing.T) {
	partitiontest.PartitionTest(t)
	n := newTestNet(t)
	n.publish()
	a := newFakeAlgod(t, n.genesis)
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true, "TimeTravelHoldersIndex": true}`)
	writeAlgodFiles(t, dataDir, a)
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

	// without time travel, that first
	o, _ := bootstrapNode(t, n, a, `{"TimeTravelHoldersIndex": true}`)
	_, err := o.install()
	require.ErrorContains(t, err, "must set EnableTimeTravel")

	// asked for, but the config would have algod delete it: refused before
	// anything is downloaded
	o, dataDir := bootstrapNode(t, n, a, `{"EnableTimeTravel": true}`)
	_, err = o.install()
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

	_, _, err := defaultStart(debDataDir, "linux", debUser, []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "no unit installed")
	require.NoError(t, os.WriteFile(filepath.Join(units, debUnit), nil, 0o644))
	start, root, err := defaultStart(debDataDir+"/", "linux", debUser, []string{t.TempDir(), units}, noBrew)
	require.NoError(t, err)
	require.Equal(t, debStart, start)
	require.True(t, root)
	_, _, err = defaultStart(t.TempDir(), "linux", debUser, []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "another data directory")

	// another directory of the package's user, with the template unit
	require.NoError(t, os.WriteFile(filepath.Join(units, debTemplateUnit), nil, 0o644))
	start, root, err = defaultStart("/var/lib/dtalgorand_testnet", "linux", debUser, []string{units}, noBrew)
	require.NoError(t, err)
	require.Equal(t, "systemctl enable --now dtalgorand@-var-lib-dtalgorand_testnet", start)
	require.True(t, root)
	_, _, err = defaultStart("/var/lib/dtalgorand_testnet", "linux", "someone", []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "another user's directory")
	_, _, err = defaultStart("/home/dtalgorand/node", "linux", debUser, []string{units}, noBrew)
	require.ErrorContains(t, err, "-start", "hidden from the unit by ProtectHome")
	require.Equal(t, `dtalgorand@-srv-my\x2dnode-.testnet-a\x20b`, templateInstance("/srv/my-node/.testnet/a b"))

	prefix := t.TempDir()
	brew := []string{t.TempDir(), prefix}
	start, root, err = defaultStart(filepath.Join(prefix, "var", "dtalgorand"), "darwin", "", nil, brew)
	require.NoError(t, err)
	require.Equal(t, brewStart, start)
	require.False(t, root)
	_, _, err = defaultStart(filepath.Join(prefix, "var", "other"), "darwin", "", nil, brew)
	require.ErrorContains(t, err, "-start")

	require.ErrorContains(t, checkStartUser(debStart, true, 1000), "needs root")
	require.NoError(t, checkStartUser(debStart, true, 0))
	require.ErrorContains(t, checkStartUser(brewStart, false, 0), "without sudo")
	require.NoError(t, checkStartUser(brewStart, false, 501))
	require.ErrorContains(t, checkStartUser("brew services restart dtalgorand", false, 0), "not root")
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
	// root's alone: bootstrap reads it for the owner
	require.NoError(t, os.WriteFile(keys, b, 0o600))
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
			// and leaves a process that writes to its output later, as a daemon may
			start:       fmt.Sprintf("echo %s > %s/algod.net && echo %d > %s/algod.pid && chown %d %s/algod.net %s/algod.pid && echo x >> %s/started && id -u > %s/start-uid && (sleep 3 && echo late && touch %s/late-write) &", host, dataDir, os.Getpid(), dataDir, uid, dataDir, dataDir, dataDir, dataDir, dataDir),
			startAsRoot: asRoot,
			wait:        10 * time.Second, pollEvery: 10 * time.Millisecond, log: io.Discard,
		}
		args := []string{"-url", n.srv.URL, "-workers", "2", "-parallel", "2", "-label", label}
		began := time.Now()
		require.NoError(t, o.bootstrap(args))
		require.Less(t, time.Since(began), 30*time.Second, "waited for the start's leftover process")
		// which, like an algod the start leaves running, outlives a write
		// to its output once bootstrap is done
		require.Eventually(t, func() bool { return exists(filepath.Join(dataDir, "late-write")) }, 20*time.Second, 100*time.Millisecond, "the leftover process died writing")

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

	// a root-owned directory, which the service's algod could not open
	_, dataDir := rootNode(t, n, 0)
	o := bootstrapOpts{fetch: fetchOpts{dataDir: dataDir}, start: "true", startAsRoot: true, log: io.Discard}
	require.ErrorContains(t, o.bootstrap(nil), "belongs to root")
}
