# dtalgorand

The packages of this fork ([time travel](../../ledger/timetravel/README.md)): a Debian package on apt.d13.co, which installs alongside the stock `algorand` package, and, for macOS on Apple silicon, a Homebrew formula in `d13co/tap` built from the same release. This page covers installing them and setting up a node; [Building and releasing](#building-and-releasing) is for maintainers.

## Requirements

- **OS:** Linux on amd64 or arm64 (the .deb is for Debian and Ubuntu; elsewhere, build from source), or macOS 14 or later on Apple silicon.
- **Disk:** about 90 GB for a mainnet node today, growing. A synced mainnet node measured on 2026-10-30 held 65.5 GiB of history (3.6 GiB of it the asset holders index), an 11 GiB account DB and 0.7 GiB of recent blocks; a history installed from a snapshot is a few GB larger. Plan for 100 GB or more on an NVMe SSD: queries at past rounds read the history at random. Before it starts, `ttsnap fetch` checks that the history's disk has room for the download (about 70 GB, which becomes the history) plus 5% and 1 GiB.
- **Memory:** what a stock node needs (Algorand recommends 8 GB or more), plus a few hundred MB for the history's cache and memtables.

## Installing

### Debian and Ubuntu

Add the apt.d13.co repository and install the package (amd64 or arm64):

```sh
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.d13.co/key.asc -o /etc/apt/keyrings/d13co.asc
gpg --show-keys /etc/apt/keyrings/d13co.asc   # exactly one primary key: F66E77133063650C159F405AC4D2171FB5B60471
echo "deb [signed-by=/etc/apt/keyrings/d13co.asc] https://apt.d13.co stable main" |
  sudo tee /etc/apt/sources.list.d/d13co.list
sudo apt update
sudo apt install dtalgorand
```

apt trusts every key in `d13co.asc`, hence the check before `apt update`.

The package does not enable or start `dtalgorand.service`. It turns on unattended upgrades from apt.d13.co (`/etc/apt/apt.conf.d/51dtalgorand-upgrades`), and an upgrade restarts the nodes that are running.

### macOS

```sh
brew install d13co/tap/dtalgorand
```

Don't run `brew services start dtalgorand` yet: a first start begins a sync from genesis. Choose a path first (next section).

## Setting up a node

The node's first start begins a sync from genesis, so pick a path before starting it. A new install's `config.json` already sets `"EnableTimeTravel": true` and `"TimeTravelHoldersIndex": true` (the [asset holders index](../../ledger/timetravel/README.md#holders-index) for `GET /v2/assets/{asset-id}/holders`). Time travel can't be turned on later for a node that synced without it.

### Choosing a path

| path | time on mainnet | trusts | needs |
|---|---|---|---|
| [**From a snapshot**](#from-a-snapshot) (recommended) | an hour or two on a fast link: a 70 GB download, a 5-minute index build, then a catchpoint catchup | the snapshot's producer, for the history before the catchpoint. The headers are checked to chain from genesis, and the current state comes from the catchpoint, verified as usual | nothing else |
| [**From an archival node's blocks**](#from-an-archival-nodes-blocks) (`ttimport`) | days, depending on the disks | the archive: its blocks are replayed without certificate checks | an archival node's block DB holding every round since genesis (~3.5 TB on mainnet), with write access to its directory |
| [**Sync from genesis**](#sync-from-genesis) | weeks | nothing beyond what any node trusts: certificates are checked | a machine that stays on for weeks |

Below, `D` is the data directory: `/var/lib/dtalgorand` with the .deb, `$(brew --prefix)/var/dtalgorand` with Homebrew. On Linux the node runs as the `dtalgorand` user, so run the tools as that user (`sudo -u dtalgorand`); with plain `sudo` they leave files owned by root that the node can't open. On macOS the node runs as you, under `brew services`, with no `sudo`.

**Optionally, before anything else, put the history on a fast disk.** It goes in `<TimeTravelDBDir>/mainnet-v1.0/ledger.timetravel`.

```sh
# Linux (the unit's ProtectHome keeps the node out of /home)
sudo install -d -o dtalgorand -g dtalgorand /mnt/fast/dtalgorand
sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p TimeTravelDBDir -v /mnt/fast/dtalgorand

# macOS (keep the disk mounted whenever the node runs, and out of Time Machine)
mkdir -p /Volumes/<disk>/dtalgorand
dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p TimeTravelDBDir -v /Volumes/<disk>/dtalgorand
tmutil addexclusion /Volumes/<disk>/dtalgorand
```

### From a snapshot

[ttsnap](../../tools/ttsnap/README.md) installs another node's history, and the node then catches up from a catchpoint like any non-archival node. See ttsnap's [Trust](../../tools/ttsnap/README.md#trust) section for what is checked.

1. Download and install the latest snapshot. It needs a data directory whose node has never started (see [Starting over](#starting-over) otherwise). The download resumes when rerun.

   ```sh
   sudo -u dtalgorand ttsnap fetch -url https://tt-snapshots-mainnet.d13.co -d /var/lib/dtalgorand    # Linux
   ttsnap fetch -url https://tt-snapshots-mainnet.d13.co -d $(brew --prefix)/var/dtalgorand           # macOS
   ```

   The producer keeps its last two snapshots, so a download has between about 8 and 15 hours, depending on when it starts, before its snapshot is deleted. One that takes longer fails: rerun `fetch` to move to the latest snapshot, which keeps the files the two share.

2. Build the asset holders index, with the node still stopped (about 5 minutes on mainnet). The snapshot doesn't carry it. The index serves `GET /v2/assets/{asset-id}/holders` (`dtgoal asset holders`); without it the node runs as usual, logs a warning at start, and that endpoint answers 503. To do without it, set `TimeTravelHoldersIndex` to `false` instead (`dtalgocfg set -d D -p TimeTravelHoldersIndex -v false`, as `dtalgorand` on Linux), and skip this step.

   ```sh
   sudo -u dtalgorand ttimport -holders-index -d /var/lib/dtalgorand    # Linux
   ttimport -holders-index -d $(brew --prefix)/var/dtalgorand           # macOS
   ```

3. Start the node:

   ```sh
   sudo systemctl enable --now dtalgorand    # Linux
   brew services start dtalgorand            # macOS
   ```

4. Once `dtgoal node status` answers ([Checking the node](#checking-the-node)), catch up from the network's latest catchpoint. algod takes a minute or more to open the history, and `catchup` fails until it has: rerun it then.

   ```sh
   sudo -u dtalgorand ttsnap catchup -d /var/lib/dtalgorand    # Linux
   ttsnap catchup -d $(brew --prefix)/var/dtalgorand           # macOS
   ```

   `catchup` starts the catchup and returns. Follow it with `dtgoal node status` ([Checking the node](#checking-the-node)): it is done when the `Catchpoint` lines are gone and the node syncs normally. Before applying the catchpoint, the node deletes the snapshot's rounds after the catchpoint's balances round (minutes on mainnet) and later writes them again from its own evaluation. If `catchup` says the network's latest catchpoint is past the snapshot, see [When the latest catchpoint is past the snapshot](../../tools/ttsnap/README.md#when-the-latest-catchpoint-is-past-the-snapshot).

### From an archival node's blocks

[ttimport](../../tools/ttimport/README.md) builds the history by replaying an archival node's block DB, much faster than syncing the same rounds from the network. The archival node must be an `Archival: true` node synced from genesis: a node that started from a catchpoint lacks the early rounds, and the import then fails partway with `ledger does not have entry N`. The archival node can keep running during the import.

ttimport never changes the archive's blocks, but SQLite opens the archive for writing and puts its `-wal` and `-shm` files next to it, so whoever runs ttimport needs write access to the archive's directory.

1. Run the import. It prints a progress line every 10 seconds. Ctrl-C stops it, and rerunning the same command resumes it.

   On Linux, run it as root, since an algod's ledger directory is private to its user. When root creates the `-wal` and `-shm` files, SQLite gives them to the archive's owner, so the archival node is unaffected.

   ```sh
   sudo ttimport -blocks /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite -d /var/lib/dtalgorand
   ```

   On macOS, run it as yourself. You need write access to the archive's directory: copy the archive to a disk you own if it belongs to another user.

   ```sh
   ttimport -blocks <archival data dir>/mainnet-v1.0/ledger.block.sqlite -d $(brew --prefix)/var/dtalgorand
   ```

2. On Linux, give the result to `dtalgorand`. This also applies after a failed or interrupted run, which leaves files that only root can read. Include the `TimeTravelDBDir` directory if you set one.

   ```sh
   sudo chown -R dtalgorand:dtalgorand /var/lib/dtalgorand    # and /mnt/fast/dtalgorand, if used
   ```

3. Start the node (`sudo systemctl enable --now dtalgorand`, or `brew services start dtalgorand`). It catches up the rounds after the archive's last one from the network.

The package's `config.json` turns the holders index on, so the import keeps it from round 0 and it needs no build. ttimport's README covers [the alternative](../../tools/ttimport/README.md#building-the-asset-holders-index): import with it off, then build it once.

### Sync from genesis

Start the node; nothing else is needed:

```sh
sudo systemctl enable --now dtalgorand    # Linux
brew services start dtalgorand            # macOS
```

This takes weeks on mainnet. The node checks certificates as any node does, and keeps the holders index from round 0. A Mac stops the node while it sleeps (see [macOS notes](#macos-notes)), so a laptop is a poor fit for this path.

### Checking the node

```sh
sudo -u dtalgorand dtgoal node status -d /var/lib/dtalgorand    # Linux
dtgoal node status -d $(brew --prefix)/var/dtalgorand           # macOS
```

- **Synced:** `Last committed block` matches the network's (for example `curl -s https://mainnet-api.4160.nodely.dev/v2/status`), and `Sync Time` stays near 0.
- **During a catchpoint catchup:** `Catchpoint` lines show its progress.

The REST API's address is in `D/algod.net` and its token in `D/algod.token`. algod picks port 8080 if it is free and a random port otherwise, so a stock `algorand` node on the same host can take 8080. To pin the port, set `EndpointAddress` and restart the node: `sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p EndpointAddress -v 127.0.0.1:8081` on Linux, `dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p EndpointAddress -v 127.0.0.1:8081` on macOS. To check that time travel works:

```sh
D=/var/lib/dtalgorand    # or $(brew --prefix)/var/dtalgorand
curl -H "X-Algo-API-Token: $(cat $D/algod.token)" "http://$(cat $D/algod.net)/v2/ledger/supply?round=1000"
```

### Starting over

`ttsnap fetch` refuses a data directory whose node has started once (`ledger.timetravel already exists`). An interrupted `fetch` or import doesn't need this: rerun it to resume. To start from scratch, keep `config.json` and `genesis.json` and delete the ledger, which throws away whatever was downloaded or synced:

1. Stop the node, and keep it from starting at boot until you are done: `sudo systemctl disable --now dtalgorand`, or `brew services stop dtalgorand`.
2. If the node holds participation keys (see [Participation](#participation)), move them aside first: `partregistry.sqlite` with its `-wal` and `-shm` files, and any `*.partkey` files, in `D/mainnet-v1.0`.
3. Delete the genesis directory, and the `mainnet-v1.0` directory under each `*Dir` setting of `config.json` you set (`TimeTravelDBDir`, `HotDataDir`, `ColdDataDir`, `TrackerDBDir`, `BlockDBDir`, `CatchpointDir`):

   ```sh
   sudo rm -rf /var/lib/dtalgorand/mainnet-v1.0 /mnt/fast/dtalgorand/mainnet-v1.0    # Linux, history on /mnt/fast
   rm -rf $(brew --prefix)/var/dtalgorand/mainnet-v1.0                               # macOS
   ```

4. Follow the path again from its first step. Its start step starts the node (on Linux, `enable --now` enables it again).

## Participation

This fork changes the ledger, evaluator and REST layers to serve historical queries, and is not audited for running validators. On mainnet, testnet and betanet the node therefore refuses participation keys unless `config.json` sets `"DangerouslyEnableParticipation": true`. Other networks, such as private networks, devmode, fnet and devnet, are not affected.

While the flag is off on those networks:

- The node won't start if it holds participation keys (a follower, `EnableFollowMode`, starts anyway: it can't use them): partkey files in the genesis directory, or keys in the participation registry (`partregistry.sqlite`, in `ColdDataDir` if set). algod exits with code 78 and names the keys it found; the systemd units don't restart it (`RestartPreventExitStatus=78`), while launchd starts it again about every 10 seconds (run `brew services stop dtalgorand` and read `D/node.log`). Under `goal node start` the message is in `algod-err.log` and `node.log`; goal only reports that algod exited early.
- Installing keys (`POST /v2/participation`, `goal account addpartkey`, `installpartkey`, `renewpartkey`) fails. `POST /v2/participation/generate/{address}` is refused before it generates anything. `goal account addpartkey` and `renewpartkey` generate the keys locally, which takes minutes, so they check the data directory's `config.json` and the node's network first and stop before generating; if goal can't read the config, the node still refuses the keys when goal installs them.

Don't point dtalgorand at a validator's data directory: give it its own, or run upstream algod on the validator's. Deleting the keys to get the node to start is irreversible: keys installed through the REST API (as goal installs them) exist only in the participation registry, and the account would need a new key registration. Once keys are installed with the flag on, turning it off stops the node from starting until they are moved away, and that includes expired `.partkey` files, which algod doesn't delete. To remove keys from the registry, start the node once with the flag on and run `goal account deletepartkey`, or move `partregistry.sqlite` (and its `-wal` and `-shm` files) aside while the node is stopped.

## macOS notes

- The node runs as the logged-in user, under launchd (`brew services`), and stops while the Mac sleeps, as does a `ttsnap fetch` (rerun it to resume). Keep the Mac awake on power in System Settings, or run `caffeinate -s -w $(cat $(brew --prefix)/var/dtalgorand/algod.pid)`.
- Time Machine backs up the history unless you exclude it: `tmutil addexclusion <dir>`.
- Unlike the .deb, an upgrade doesn't restart the node: run `brew services restart dtalgorand`.
- `brew uninstall dtalgorand` removes the binaries only: after `brew services stop dtalgorand`, delete `D` (and the history's directory, if `TimeTravelDBDir` put it elsewhere) and `$(brew --prefix)/var/log/dtalgorand.log` by hand. `brew untap d13co/tap` removes the tap.
- A ttsnap built from source has no keys file of its own: pass `-keys installer/dtalgorand/ttsnap_keys` from the source tree.

### File descriptors

algod raises its own file descriptor limit, so launchd's default (256) doesn't matter: Go raises the soft limit at startup up to `kern.maxfilesperproc` (macOS refuses more, even below the hard limit), and algod then raises it to what its connections, the ledger and the time travel store need. The time travel store can hold up to 4096 files open; if the hard limit leaves less room, algod logs a warning and runs anyway.

## Layout

### Debian

| Path | |
|---|---|
| `/usr/lib/dtalgorand/bin/` | `algod`, `goal`, `kmd`, `algoh`, `algocfg`, `algokey`, `diagcfg`, `node_exporter` under their stock names: goal and algoh run algod and kmd from their own directory |
| `/usr/bin/dt*` | links to them: `dtalgod`, `dtgoal`, `dtkmd`, `dtalgoh`, `dtalgocfg`, `dtalgokey`, `dtdiagcfg` |
| `/usr/bin/ttimport` | [ttimport](../../tools/ttimport/README.md) |
| `/usr/bin/ttsnap` | [ttsnap](../../tools/ttsnap/README.md): install a history snapshot, then catch up from a catchpoint |
| `/usr/share/dtalgorand/ttsnap/keys` | the Falcon-1024 keys of the snapshot producers and attesters `ttsnap fetch` trusts |
| `/var/lib/dtalgorand/` | the node's data directory, with mainnet's `genesis.json` and the other networks' under `genesis/` |
| `dtalgorand.service`, `dtalgorand@.service` | the node, and nodes for other data directories (see the unit's comments) |
| `/etc/apt/apt.conf.d/51dtalgorand-upgrades` | unattended upgrades from `apt.d13.co`, suite `stable` |

The node runs as the `dtalgorand` user, whose home is `/var/lib/dtalgorand`: algod and goal keep files in `$HOME/.algorand`.

### Homebrew

| Path | |
|---|---|
| `$(brew --prefix)/opt/dtalgorand/libexec/` | the binaries under their stock names |
| `$(brew --prefix)/bin/` | links to them: `dtalgod`, `dtgoal`, ..., and `ttimport`, `ttsnap` under their own names |
| `$(brew --prefix)/opt/dtalgorand/share/dtalgorand/ttsnap/keys` | the snapshot keys `ttsnap fetch` trusts, found from the binary's location |
| `$(brew --prefix)/var/dtalgorand/` | the node's data directory, set up as the .deb's is; a new one starts with `"EnableTimeTravel": true` and `"TimeTravelHoldersIndex": true` |
| `$(brew --prefix)/var/log/dtalgorand.log` | the service's stdout and stderr (algod logs to `node.log` in the data directory) |

## Building and releasing

### Building the .deb

```sh
installer/dtalgorand/build_deb.sh tmp/dtalgorand
```

The script builds the binaries into `tmp/dtalgorand/<arch>/bin`, not `$GOPATH/bin`, and writes `dtalgorand_<version>_<arch>.deb` to the given directory. It builds for the host's architecture only (cgo), so build the arm64 package on an arm64 host. Pass the same `DEB_VERSION` to both builds to give them the same version.

The version is the upstream stable release the fork is based on plus a fork revision, `<x.y.z>-dt<n>`, e.g. `5.0.2-dt1`. The script takes `x.y.z` from the nearest upstream `v<x.y.z>-stable` tag in git history, and the binaries report it as their version (`algod -v` says `5.0.2`, as upstream's does). d13co/dt-algorand doesn't carry upstream's tags, so fetch them first: `git fetch --no-tags https://github.com/algorand/go-algorand.git 'refs/tags/v*-stable:refs/tags/v*-stable'`. If HEAD has a `v<x.y.z>-dt<n>` tag, that is the package version; otherwise it is a dev build, `<x.y.z>-dt<n>+dev<build number>` with `n` the last release on that base (0 if none) and the build number from `scripts/buildnumber.py` (days since 2018-05-25, then the hour), so it sorts after that release and before the next. Set `DEB_VERSION` to pin the package version, e.g. `DEB_VERSION=5.0.2-dt1+dev306622 installer/dtalgorand/build_deb.sh tmp/dtalgorand`.

### Building the macOS tarball

```sh
installer/dtalgorand/build_darwin.sh tmp/dtalgorand
```

Run it on a Mac: the binaries need cgo (libsodium, SQLite). It builds into `tmp/dtalgorand/darwin-<arch>/bin` and writes `dtalgorand_<version>_darwin-<arch>.tar.gz`, versioned as the .deb is (`installer/dtalgorand/version.sh`; `PKG_VERSION` pins it). The tarball holds the binaries under their stock names in `bin/` and, in `share/`, what the .deb puts in the data directory, plus `ttsnap/keys` (the .deb's `/usr/share/dtalgorand/ttsnap/keys`); the formula installs `share/` as `share/dtalgorand`. The binaries run on macOS 14 and later (`MACOSX_DEPLOYMENT_TARGET`, 14.0 by default); releases build on `macos-15`. They are signed ad hoc, which Apple silicon requires and Homebrew-installed command-line tools need no more than: Homebrew doesn't mark its downloads as quarantined, so Gatekeeper doesn't check them.

The `darwin` workflow builds the tarball on pull requests that touch the packaging, installs it with the formula from a local tap, and starts the node under launchd.

### Releasing

Push a `v<x.y.z>-dt<n>` tag on `dtalgorand` and the release workflow builds the .deb, attaches it to a GitHub release and, once approved, publishes it to apt.d13.co. In parallel it builds the macOS tarball on a Mac runner, attaches it to the same release and, once approved, points the `d13co/tap` formula at it. Number the fork revisions from 1 on each upstream base: after merging upstream's `v5.0.3-stable`, the next release is `v5.0.3-dt1`; a fork-only fix on top of it is `v5.0.3-dt2`. The workflow fails if the tag's `x.y.z` isn't the upstream release `dtalgorand` is based on.

### The tap

The formula lives in [d13co/homebrew-tap](https://github.com/d13co/homebrew-tap), as `Formula/dtalgorand.rb`, rendered from [homebrew/dtalgorand.rb.in](homebrew/dtalgorand.rb.in) by [homebrew/render.sh](homebrew/render.sh). The release workflow's `homebrew` job commits it there. It needs, once:

1. The `d13co/homebrew-tap` repository (public, so that `brew` can clone it), with a first commit (a README): the job checks it out.
2. A deploy key with write access on it: `ssh-keygen -t ed25519 -N '' -C dt-algorand-release -f tap_key`, add `tap_key.pub` as a deploy key of the tap with write access, and `tap_key` as the `HOMEBREW_TAP_DEPLOY_KEY` secret of a `homebrew` environment on d13co/dt-algorand (restricted to `v*` tags, with required reviewers, as `apt` is). Then delete both files.
