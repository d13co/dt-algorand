# dtalgorand

The packages of this fork ([time travel](../../ledger/timetravel/README.md)): a Debian package on apt.d13.co, which installs alongside the stock `algorand` package, and, for macOS on Apple silicon, a Homebrew formula in `d13co/tap` built from the same release. This page covers installing them and setting up a node. [PACKAGING.md](PACKAGING.md) describes where the packages put their files, and how they are built and released.

## Requirements

- **OS:** Linux on amd64 (the .deb is for Debian and Ubuntu; elsewhere, and on arm64, build from source), or macOS 14 or later on Apple silicon.
- **Disk:** about 90 GB for a mainnet node today, growing. A synced mainnet node measured on 2026-10-30 held 65.5 GiB of history (3.6 GiB of it the asset holders index), an 11 GiB account DB and 0.7 GiB of recent blocks; a history installed from a snapshot is about the same size (older releases of `ttsnap fetch` left the snapshot's old box index on disk, about 1 GB, until Pebble compacted it away; with algod stopped, `ttimport -d <data dir> -index-boxes` reclaims it). Plan for 100 GB or more on an NVMe SSD: queries at past rounds read the history at random. Before it starts, `ttsnap fetch` checks that the history's disk has room for the download (about 70 GB, which becomes the history) plus 5% and 1 GiB.
- **Memory:** what a stock node needs (Algorand recommends 8 GB or more), plus a few hundred MB for the history's cache and memtables.

## Installing

### Debian and Ubuntu

Add the apt.d13.co repository and install the package (amd64 only for now):

```sh
sudo install -d -m 0755 /etc/apt/keyrings
sudo curl -fsSL https://apt.d13.co/key.asc -o /etc/apt/keyrings/d13co.asc
gpg --show-keys /etc/apt/keyrings/d13co.asc
echo "deb [signed-by=/etc/apt/keyrings/d13co.asc] https://apt.d13.co stable main" |
  sudo tee /etc/apt/sources.list.d/d13co.list
sudo apt update
sudo apt install dtalgorand
```

`gpg --show-keys` must list exactly one primary key, `F66E77133063650C159F405AC4D2171FB5B60471`: apt trusts every key in `d13co.asc`, hence the check before `apt update`.

The commands have a `dt` prefix (`dtalgod`, `dtgoal`, `dtalgocfg`, ...) so that they don't clash with the stock package's; `ttsnap` and `ttimport` keep their names. Without `-d`, `dtgoal`, `dtalgocfg`, `dtalgod`, `dtalgoh` and `dtdiagcfg` take the data directory from `$DTALGORAND_DATA` (see [Finishing the install](#finishing-the-install)) and ignore `$ALGORAND_DATA`, which may point at a stock node. Likewise, without `-k`, `dtgoal` takes kmd's data directory from `$DTALGORAND_KMD`, not `$ALGORAND_KMD`. The node's data directory is `/var/lib/dtalgorand`, and the node runs as the `dtalgorand` user, whose home it is.

The package does not enable or start `dtalgorand.service`. It turns on unattended upgrades from apt.d13.co (`/etc/apt/apt.conf.d/51dtalgorand-upgrades`), and an upgrade restarts the nodes that are running.

### macOS

```sh
brew install d13co/tap/dtalgorand
```

The commands are linked with the same `dt` prefix in `$(brew --prefix)/bin`, and the node's data directory is `$(brew --prefix)/var/dtalgorand`.

The formula doesn't start the node: it gives it a launchd service for `brew services`. `brew services start dtalgorand` writes `~/Library/LaunchAgents/homebrew.mxcl.dtalgorand.plist`, and launchd then runs algod as you, starts it again at every login, and restarts it if it exits. It is a login agent, not a boot daemon: after a reboot, the node waits until you log in, and it stops when you log out. Its output goes to `$(brew --prefix)/var/log/dtalgorand.log`, and algod's own log to `node.log` in the data directory. `brew services stop dtalgorand` stops the node and removes the plist, so it no longer starts at login.

Don't run `brew services start dtalgorand` yet: a first start begins a sync from genesis. Finish the install (next section), then choose a path ([Setting up a node](#setting-up-a-node)).

### Finishing the install

On Linux, there is nothing to set: the commands run as the `dtalgorand` user through `sudo -u dtalgorand`, which doesn't pass your environment on, so they keep `-d`.

On macOS, point the dtalgorand commands at the node's data directory, so that they work without `-d`. Add `DTALGORAND_DATA` to `~/.zshrc` (zsh is the default shell; with bash, write `~/.bash_profile` instead of `~/.zshrc` in the commands below). The `grep` skips the `echo` when the file already has an `export DTALGORAND_DATA=` line, so running the line again doesn't add a second copy:

```sh
grep -qs '^export DTALGORAND_DATA=' ~/.zshrc || echo "export DTALGORAND_DATA=$(brew --prefix)/var/dtalgorand" >> ~/.zshrc
```

The line holds the path as it was when you ran it: `/opt/homebrew/var/dtalgorand` with a default Homebrew. Open a new terminal, or run `source ~/.zshrc`, then check that the commands find the data directory:

```sh
dtalgocfg get -p EnableTimeTravel
```

It prints `true` (zsh may show `true%`: the output doesn't end with a newline). If it prints `false`, the variable points somewhere other than the data directory: fix the line in `~/.zshrc` by hand (the command above leaves an existing line alone). Without the variable, it says that the data directory isn't specified.

`dtgoal`, `dtalgocfg`, `dtalgod`, `dtalgoh` and `dtdiagcfg` read `DTALGORAND_DATA` when `-d` isn't given, and `dtgoal` finds kmd's data directory from it too. `ttsnap` and `ttimport` don't: they always need `-d`. The commands below pass `-d` everywhere, so they work with or without the variable.

## Setting up a node

The node's first start begins a sync from genesis, so pick a path before starting it. A new install's `config.json` already sets `"EnableTimeTravel": true` and `"TimeTravelHoldersIndex": true` (the [asset holders index](../../ledger/timetravel/README.md#holders-index) for `GET /v2/assets/{asset-id}/holders`). Time travel can't be turned on later for a node that synced without it.

To do without the holders index (3.6 GiB on mainnet), set `TimeTravelHoldersIndex` to `false` before the first start. Turning it off later deletes the index at the node's next start, and turning it back on means rebuilding it with `ttimport -holders-index` while the node is stopped (about 5 minutes on mainnet).

### Choosing a path

| path | time on mainnet | trusts | needs |
|---|---|---|---|
| [**From a snapshot**](#from-a-snapshot) (recommended) | an hour or two on a fast link: a 70 GB download, a 5-minute index build, then a catchpoint catchup | the snapshot's producer, for the history before the catchpoint. The headers are checked to chain from genesis, and the current state comes from the catchpoint, verified as usual | nothing else |
| [**From an archival node's blocks**](#from-an-archival-nodes-blocks) (`ttimport`) | days, depending on the disks | the archive: its blocks are replayed without certificate checks | an archival node's block DB holding every round since genesis (~3.5 TB on mainnet), with write access to its directory |
| [**Sync from genesis**](#sync-from-genesis) | weeks | nothing beyond what any node trusts: certificates are checked | a machine that stays on for weeks |

For an archival node, see [Making the node archival](#making-the-node-archival).

Below, `D` is the data directory: `/var/lib/dtalgorand` with the .deb, `$(brew --prefix)/var/dtalgorand` with Homebrew. On Linux the node runs as the `dtalgorand` user, so run the tools as that user (`sudo -u dtalgorand`); with plain `sudo` they leave files owned by root that the node can't open. On macOS the node runs as you, under `brew services`, with no `sudo`.

**Optionally, before anything else, put the history on a fast disk.** It goes in `<TimeTravelDBDir>/mainnet-v1.0/ledger.timetravel`.

On Linux, put it outside `/home` (the unit's `ProtectHome` keeps the node out of it):

```sh
sudo install -d -o dtalgorand -g dtalgorand /mnt/fast/dtalgorand
sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p TimeTravelDBDir -v /mnt/fast/dtalgorand
```

On macOS, use a disk that stays mounted whenever the node runs, and exclude it from Time Machine:

```sh
mkdir -p /Volumes/<disk>/dtalgorand
dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p TimeTravelDBDir -v /Volumes/<disk>/dtalgorand
tmutil addexclusion /Volumes/<disk>/dtalgorand
```

### From a snapshot

[ttsnap](../../tools/ttsnap/README.md) installs another node's history, and the node then catches up from a catchpoint like any non-archival node. See ttsnap's [Trust](../../tools/ttsnap/README.md#trust) section for what is checked. The mainnet producer's history is one of three histories built without a snapshot (two `ttimport` imports of different archival nodes' blocks, one sync from genesis), and all three agreed on content.

`ttsnap bootstrap` runs the four steps below in one command, and a rerun picks up where it stopped.

On Linux, run it as root: it does everything as `dtalgorand`, except starting the service.

```sh
sudo ttsnap bootstrap -d /var/lib/dtalgorand
```

On macOS:

```sh
ttsnap bootstrap -d $(brew --prefix)/var/dtalgorand
```

It returns once the catchup has started: follow it as in step 4. To do without the holders index, set `TimeTravelHoldersIndex` to `false` as in step 2 and add `-holders-index=false`. See ttsnap's [bootstrap](../../tools/ttsnap/README.md#bootstrap) for the details. The steps, one at a time:

1. Download and install the latest snapshot. It needs a data directory whose node has never started (see [Starting over](#starting-over) otherwise). The download resumes when rerun.

   On Linux:

   ```sh
   sudo -u dtalgorand ttsnap fetch -d /var/lib/dtalgorand
   ```

   On macOS:

   ```sh
   ttsnap fetch -d $(brew --prefix)/var/dtalgorand
   ```

   The producer keeps its last two snapshots, so a download has between about 8 and 15 hours, depending on when it starts, before its snapshot is deleted. One that takes longer fails: rerun `fetch` to move to the latest snapshot, which keeps the files the two share.

2. Build the asset holders index, with the node still stopped (about 5 minutes on mainnet). The snapshot doesn't carry it. The index serves `GET /v2/assets/{asset-id}/holders` (`dtgoal asset holders`); without it the node runs as usual, logs a warning at start, and that endpoint answers 503. To do without it, set `TimeTravelHoldersIndex` to `false` instead (`dtalgocfg set -d D -p TimeTravelHoldersIndex -v false`, as `dtalgorand` on Linux), and skip this step.

   On Linux:

   ```sh
   sudo -u dtalgorand ttimport -holders-index -d /var/lib/dtalgorand
   ```

   On macOS:

   ```sh
   ttimport -holders-index -d $(brew --prefix)/var/dtalgorand
   ```

3. Start the node:

   On Linux:

   ```sh
   sudo systemctl enable --now dtalgorand
   ```

   On macOS:

   ```sh
   brew services start dtalgorand
   ```

4. Once `dtgoal node status` answers ([Checking the node](#checking-the-node)), catch up from the network's latest catchpoint. algod takes a minute or more to open the history, and `catchup` fails until it has: rerun it then.

   On Linux:

   ```sh
   sudo -u dtalgorand ttsnap catchup -d /var/lib/dtalgorand
   ```

   On macOS:

   ```sh
   ttsnap catchup -d $(brew --prefix)/var/dtalgorand
   ```

   `catchup` starts the catchup and returns. Follow it with `dtgoal node status` ([Checking the node](#checking-the-node)): it is done when the `Catchpoint` lines are gone and the node syncs normally. Before applying the catchpoint, the node deletes the snapshot's rounds after the catchpoint's balances round (minutes on mainnet) and later writes them again from its own evaluation. If `catchup` says the network's latest catchpoint is past the snapshot, see [When the latest catchpoint is past the snapshot](../../tools/ttsnap/README.md#when-the-latest-catchpoint-is-past-the-snapshot).

### From an archival node's blocks

[ttimport](../../tools/ttimport/README.md) builds the history by replaying an archival node's block DB, much faster than syncing the same rounds from the network. The archival node must be an `Archival: true` node synced from genesis: a node that started from a catchpoint lacks the early rounds, and the import then fails partway with `ledger does not have entry N`. The archival node can keep running during the import.

ttimport never changes the archive's blocks, but SQLite opens the archive for writing and puts its `-wal` and `-shm` files next to it, so whoever runs ttimport needs write access to the archive's directory.

1. Run the import. It prints a progress line every 10 seconds. Ctrl-C stops it, and rerunning the same command resumes it.

   On Linux, run it as `dtalgorand`, so that the node can open what it writes. ttimport refuses to run as root on the node's data directory.

   ```sh
   sudo -u dtalgorand ttimport -blocks /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite -d /var/lib/dtalgorand
   ```

   On macOS, run it as yourself. You need write access to the archive's directory: copy the archive to a disk you own if it belongs to another user.

   ```sh
   ttimport -blocks <archival data dir>/mainnet-v1.0/ledger.block.sqlite -d $(brew --prefix)/var/dtalgorand
   ```

2. Start the node (`sudo systemctl enable --now dtalgorand`, or `brew services start dtalgorand`). It catches up the rounds after the archive's last one from the network.

The package's `config.json` turns the holders index on, so the import keeps it from round 0 and it needs no build. ttimport's README covers [the alternative](../../tools/ttimport/README.md#building-the-asset-holders-index): import with it off, then build it once.

### Sync from genesis

Start the node; nothing else is needed:

On Linux:

```sh
sudo systemctl enable --now dtalgorand
```

On macOS:

```sh
brew services start dtalgorand
```

This takes weeks on mainnet. The node checks certificates as any node does, and keeps the holders index from round 0. A Mac stops the node while it sleeps (see [macOS notes](#macos-notes)), so a laptop is a poor fit for this path.

Optionally, once the node has caught up (and after an import from an archival node too), stop it and remove the no-op records the node didn't catch as it wrote: on mainnet this saved 4% (68.4 → 65.6 GiB) and took about 1.5 hours on a small VPS, plus the box index rebuild. See [Removing no-op records](../../tools/ttimport/README.md#removing-no-op-records).

On Linux, as `dtalgorand`, so that the files it writes stay the node's:

```sh
sudo systemctl stop dtalgorand
sudo -u dtalgorand ttimport -dedup -d /var/lib/dtalgorand
sudo systemctl start dtalgorand
```

On macOS:

```sh
brew services stop dtalgorand
ttimport -dedup -d $(brew --prefix)/var/dtalgorand
brew services start dtalgorand
```

### Making the node archival

An archival dtalgorand node (`Archival: true`) keeps every block and certificate, and serves every stock archival endpoint with time travel on top. It can't catch up from a catchpoint. Setting `Archival` to `true` before the first start of a [sync from genesis](#sync-from-genesis) builds one directly, in weeks. If you have an archival node synced from genesis, it is faster to set up a node from a snapshot or with ttimport, let it sync, then hand the archive's block DB over to it: swap in the archive's block DB and turn `Archival` on while the node is stopped. The account DB and the history stay the node's; only the blocks come from the archive, which the node serves without checking their certificates, as ttimport replays them.

Make both changes before the next start. Either one alone loses blocks:

- With `Archival: true` and its own block DB, which doesn't start at round 0, the node drops its blocks and resets the block DB to genesis, then refuses to start because the account DB is ahead of it. Swapping in the archive recovers from this. Don't delete the account DB, as the error suggests: that means a sync from genesis.
- With the archive's block DB and `Archival: false`, the node prunes the archive down to its last ~1,000 blocks.

An archive that doesn't start at round 0 is reset as well, so check it first (step 2).

The steps below use the stock `algorand` package's archive in `/var/lib/algorand/mainnet-v1.0` on Linux. On macOS they use `ARCHIVE` for the directory that holds the archive's `mainnet-v1.0`: set it first, as in `ARCHIVE=/path/to/archival/data`. The archive is in the archival node's data directory unless the node's `config.json` sets `BlockDBDir`, or `ColdDataDir` without it. Then the archive is in that directory's `mainnet-v1.0`: use it in steps 2 and 4 (on Linux, `grep -E 'BlockDBDir|ColdDataDir' /var/lib/algorand/config.json` shows both settings). The steps assume dtalgorand's block DB is in its data directory, with `BlockDBDir` and `ColdDataDir` unset (to put the archive elsewhere, see [Leaving the archive on its disk](#leaving-the-archive-on-its-disk)). The archival node gives up its block DB: it can't run afterwards unless it syncs again.

1. Stop dtalgorand, then the archival node, so that the archive reaches at least the node's round. Keep dtalgorand from starting until step 5: a start between steps 3 and 4 loses its own blocks, and a start with the archive in place and `Archival` off prunes the archive.

   On Linux:

   ```sh
   sudo systemctl disable --now dtalgorand
   sudo systemctl disable --now algorand
   ```

   On macOS, stop dtalgorand (`brew services stop` also keeps it from starting at login), then stop the archival node however it was started:

   ```sh
   brew services stop dtalgorand
   ```

2. Check the rounds: the archive's first round must be 0, and its last at least the node's account DB round. If the archive is behind, the node refuses to start, saying the account DB is ahead of the block DB, and changes nothing: start the archival node for a while and repeat from step 1.

   On Linux (install `sqlite3` if it is missing: `sudo apt install sqlite3`):

   ```sh
   sudo sqlite3 -readonly /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite 'SELECT min(rnd), max(rnd) FROM blocks'
   sudo sqlite3 -readonly /var/lib/dtalgorand/mainnet-v1.0/ledger.tracker.sqlite "SELECT rnd FROM acctrounds WHERE id='acctbase'"
   ```

   On macOS:

   ```sh
   sqlite3 -readonly "$ARCHIVE"/mainnet-v1.0/ledger.block.sqlite 'SELECT min(rnd), max(rnd) FROM blocks'
   sqlite3 -readonly $(brew --prefix)/var/dtalgorand/mainnet-v1.0/ledger.tracker.sqlite "SELECT rnd FROM acctrounds WHERE id='acctbase'"
   ```

3. Turn `Archival` on, before the archive moves in. With `Archival` on, the default `CatchpointTracking` (0) also makes the node build the catchpoint trie of every account on its next start, then write a catchpoint file every 10,000 rounds: the commands set it to `-1` to skip that (leave that part out if you want the catchpoints). The last command must print `true`.

   On Linux:

   ```sh
   sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p Archival -v true && sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p CatchpointTracking -v -1
   sudo -u dtalgorand dtalgocfg get -d /var/lib/dtalgorand -p Archival
   ```

   On macOS:

   ```sh
   dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p Archival -v true && dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p CatchpointTracking -v -1
   dtalgocfg get -d $(brew --prefix)/var/dtalgorand -p Archival
   ```

4. Move the node's own block DB into a new `own-blocks` directory, and the archive's in its place, with their `-wal` and `-shm` files if any. On the same disk, `mv` renames the archive at once. From another disk it copies it: the node's disk needs room for the whole archive (~3.5 TB on mainnet), and the copy takes hours. To skip the copy, [leave the archive on its disk](#leaving-the-archive-on-its-disk) instead.

   On Linux, as root, since the node's genesis directory is the `dtalgorand` user's alone:

   ```sh
   sudo -u dtalgorand mkdir /var/lib/dtalgorand/mainnet-v1.0/own-blocks
   sudo sh -c 'mv /var/lib/dtalgorand/mainnet-v1.0/ledger.block.sqlite* /var/lib/dtalgorand/mainnet-v1.0/own-blocks/'
   sudo sh -c 'mv /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite* /var/lib/dtalgorand/mainnet-v1.0/ && chown dtalgorand:dtalgorand /var/lib/dtalgorand/mainnet-v1.0/ledger.block.sqlite*'
   ```

   On macOS, the archive's files may belong to another user, so they move as root and then become yours:

   ```sh
   mkdir $(brew --prefix)/var/dtalgorand/mainnet-v1.0/own-blocks
   mv $(brew --prefix)/var/dtalgorand/mainnet-v1.0/ledger.block.sqlite* $(brew --prefix)/var/dtalgorand/mainnet-v1.0/own-blocks/
   sudo sh -c "mv '$ARCHIVE'/mainnet-v1.0/ledger.block.sqlite* '$(brew --prefix)'/var/dtalgorand/mainnet-v1.0/ && chown $(id -un) '$(brew --prefix)'/var/dtalgorand/mainnet-v1.0/ledger.block.sqlite*"
   ```

   If a copy from another disk is interrupted, run only the last command again: the one before it would move the partial copy into `own-blocks`, over the node's own block DB. `mv` removes each of the archive's files only once it is copied, so the archive still holds what wasn't copied, and the new copy replaces the partial one.

5. Start the node.

   On Linux:

   ```sh
   sudo systemctl enable --now dtalgorand
   ```

   On macOS:

   ```sh
   brew services start dtalgorand
   ```

If the archive was ahead, the node replays its extra rounds on start, writing their history as it goes, then syncs from the network. Once it does, `GET /v2/blocks/1` answers, and the archival node's other files and `own-blocks` can be deleted.

#### Leaving the archive on its disk

Instead of moving the archive to the node's disk, give it a directory of its own on its disk, outside the archival node's data directory, and point `BlockDBDir` at it in step 3. The history's location follows `BlockDBDir` unless `TimeTravelDBDir` is set, so if you haven't set `TimeTravelDBDir`, set it to the node's data directory, where the history is now. In step 4, move the archive into that directory's `mainnet-v1.0` instead of the node's: a rename on the same disk.

On Linux, the archive's disk must be outside `/home` (the unit's `ProtectHome` hides `/home`, `/root` and `/run/user`). The commands below take it at `/mnt/archive`, with the archive in `/mnt/archive/algorand/mainnet-v1.0`. In step 3, after turning `Archival` on:

```sh
sudo install -d -o dtalgorand -g dtalgorand /mnt/archive/dtalgorand/mainnet-v1.0
sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p BlockDBDir -v /mnt/archive/dtalgorand
```

Only if you haven't set `TimeTravelDBDir` (`sudo -u dtalgorand dtalgocfg get -d /var/lib/dtalgorand -p TimeTravelDBDir` prints nothing), keep the history in the data directory:

```sh
sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p TimeTravelDBDir -v /var/lib/dtalgorand
```

In step 4, instead of its commands:

```sh
sudo -u dtalgorand mkdir /var/lib/dtalgorand/mainnet-v1.0/own-blocks
sudo sh -c 'mv /var/lib/dtalgorand/mainnet-v1.0/ledger.block.sqlite* /var/lib/dtalgorand/mainnet-v1.0/own-blocks/'
sudo sh -c 'mv /mnt/archive/algorand/mainnet-v1.0/ledger.block.sqlite* /mnt/archive/dtalgorand/mainnet-v1.0/ && chown dtalgorand:dtalgorand /mnt/archive/dtalgorand/mainnet-v1.0/ledger.block.sqlite*'
```

On macOS, the commands below take the archive's disk at `/Volumes/Archive`, with `ARCHIVE` on it as above. In step 3, after turning `Archival` on:

```sh
mkdir -p /Volumes/Archive/dtalgorand/mainnet-v1.0
dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p BlockDBDir -v /Volumes/Archive/dtalgorand
```

Only if you haven't set `TimeTravelDBDir` (`dtalgocfg get -d $(brew --prefix)/var/dtalgorand -p TimeTravelDBDir` prints nothing), keep the history in the data directory:

```sh
dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p TimeTravelDBDir -v $(brew --prefix)/var/dtalgorand
```

In step 4, instead of its commands:

```sh
mkdir $(brew --prefix)/var/dtalgorand/mainnet-v1.0/own-blocks
mv $(brew --prefix)/var/dtalgorand/mainnet-v1.0/ledger.block.sqlite* $(brew --prefix)/var/dtalgorand/mainnet-v1.0/own-blocks/
sudo sh -c "mv '$ARCHIVE'/mainnet-v1.0/ledger.block.sqlite* /Volumes/Archive/dtalgorand/mainnet-v1.0/ && chown $(id -un) /Volumes/Archive/dtalgorand/mainnet-v1.0/ledger.block.sqlite*"
```

#### Rebuilding the node later

Keep the archive out of the way first: [Starting over](#starting-over) deletes the genesis directories, the block DB's included, and ttsnap and ttimport refuse a config with `Archival` on. Stop the node, move `ledger.block.sqlite*` out of the block DB's genesis directory, set `Archival` to `false`, start over, then repeat the swap. ttimport can import from the moved archive with `-blocks`.

### Checking the node

On Linux:

```sh
sudo -u dtalgorand dtgoal node status -d /var/lib/dtalgorand
```

On macOS:

```sh
dtgoal node status -d $(brew --prefix)/var/dtalgorand
```

- **Synced:** `Last committed block` matches the network's (for example `curl -s https://mainnet-api.4160.nodely.dev/v2/status`), and `Sync Time` stays near 0.
- **During a catchpoint catchup:** `Catchpoint` lines show its progress.

The REST API's address is in `D/algod.net` and its token in `D/algod.token`. algod picks port 8080 if it is free and a random port otherwise, so a stock `algorand` node on the same host can take 8080. To pin the port, set `EndpointAddress` and restart the node: `sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p EndpointAddress -v 127.0.0.1:8081` on Linux, `dtalgocfg set -d $(brew --prefix)/var/dtalgorand -p EndpointAddress -v 127.0.0.1:8081` on macOS. To check that time travel works, on Linux:

```sh
D=/var/lib/dtalgorand
curl -H "X-Algo-API-Token: $(cat $D/algod.token)" "http://$(cat $D/algod.net)/v2/ledger/supply?round=1000"
```

On macOS:

```sh
D=$(brew --prefix)/var/dtalgorand
curl -H "X-Algo-API-Token: $(cat $D/algod.token)" "http://$(cat $D/algod.net)/v2/ledger/supply?round=1000"
```

### Starting over

On an [archival node](#making-the-node-archival), the block DB is the archive, and the steps below delete it: move it away first, as [Rebuilding the node later](#rebuilding-the-node-later) says.

`ttsnap fetch` refuses a data directory whose node has started once (`ledger.timetravel already exists`). An interrupted `fetch` or import doesn't need this: rerun it to resume. To start from scratch, keep `config.json` and `genesis.json` and delete the ledger, which throws away whatever was downloaded or synced:

1. Stop the node, and keep it from starting at boot until you are done: `sudo systemctl disable --now dtalgorand`, or `brew services stop dtalgorand`.
2. If the node holds participation keys (see [Participation](#participation)), move them aside first: any `*.partkey` files in `D/mainnet-v1.0`, and `partregistry.sqlite` with its `-wal` and `-shm` files, which are in `D/mainnet-v1.0` too unless `ColdDataDir` is set (then in its `mainnet-v1.0`).
3. Delete the genesis directory, and the `mainnet-v1.0` directory under each `*Dir` setting of `config.json` you set (`TimeTravelDBDir`, `HotDataDir`, `ColdDataDir`, `TrackerDBDir`, `BlockDBDir`, `CatchpointDir`, `StateproofDir`, `CrashDBDir`):

   On Linux, if you put the history on `/mnt/fast` as above (otherwise add your `<TimeTravelDBDir>/mainnet-v1.0`, or drop the second path if you didn't set it):

   ```sh
   sudo rm -rf /var/lib/dtalgorand/mainnet-v1.0 /mnt/fast/dtalgorand/mainnet-v1.0
   ```

   On macOS:

   ```sh
   rm -rf $(brew --prefix)/var/dtalgorand/mainnet-v1.0
   ```

4. Follow the path again from its first step. Its start step starts the node (on Linux, `enable --now` enables it again).

## Participation

This fork changes the ledger, evaluator and REST layers to serve historical queries, and is not audited for running validators. On mainnet, testnet and betanet the node therefore refuses participation keys unless `config.json` sets `"DangerouslyEnableParticipation": true`. Other networks, such as private networks, devmode, fnet and devnet, are not affected.

While the flag is off on those networks:

- The node won't start if it holds participation keys (a follower, `EnableFollowMode`, starts anyway: it can't use them): partkey files in the genesis directory, or keys in the participation registry (`partregistry.sqlite`, in `ColdDataDir` if set). algod exits with code 78 and names the keys it found; the systemd units don't restart it (`RestartPreventExitStatus=78`), while launchd starts it again about every 10 seconds (run `brew services stop dtalgorand` and read `D/node.log`). Under `dtgoal node start` the message is in `algod-err.log` and `node.log`; dtgoal only reports that algod exited early.
- Installing keys (`POST /v2/participation`, `dtgoal account addpartkey`, `installpartkey`, `renewpartkey`) fails. `POST /v2/participation/generate/{address}` is refused before it generates anything. `dtgoal account addpartkey` and `renewpartkey` generate the keys locally, which takes minutes, so they check the data directory's `config.json` and the node's network first and stop before generating; if dtgoal can't read the config, the node still refuses the keys when dtgoal installs them.

Don't point dtalgorand at a validator's data directory: give it its own, or run upstream algod on the validator's. Deleting the keys to get the node to start is irreversible: keys installed through the REST API (as dtgoal installs them) exist only in the participation registry, and the account would need a new key registration. Once keys are installed with the flag on, turning it off stops the node from starting until they are moved away, and that includes expired `.partkey` files, which algod doesn't delete. To remove keys from the registry, start the node once with the flag on and run `dtgoal account deletepartkey`, or move `partregistry.sqlite` (and its `-wal` and `-shm` files) aside while the node is stopped.

## macOS notes

- The node stops while the Mac sleeps, as does a `ttsnap fetch` (rerun it to resume). Keep the Mac awake on power in System Settings, or run `caffeinate -s -w $(cat $(brew --prefix)/var/dtalgorand/algod.pid)`.
- Time Machine backs up the history unless you exclude it: `tmutil addexclusion <dir>`.
- Unlike the .deb, an upgrade doesn't restart the node: run `brew services restart dtalgorand`.
- Start, stop and restart the node with `brew services`. `dtgoal node start`, `stop` and `restart` refuse, saying the node is managed with systemctl: the data directory's `system.json` is the .deb's, with `"systemd_managed": true`. The message's systemctl advice and link are for the .deb: on macOS, launchd runs the node for `brew services`. Keep the flag: without it, launchd starts again a node that `dtgoal node stop` stopped, and `dtgoal node restart` can start an algod alongside launchd's, so one of them fails to lock `algod.lock`, and if that one is launchd's, launchd starts it again about every 10 seconds. Don't delete `system.json` either: its `"shared_server": true` keeps the wallets you make with `dtgoal` in `~/.algorand/mainnet-v1.0/kmd-v0.5`, and without it `dtgoal` looks for them in the data directory instead.
- `brew uninstall dtalgorand` removes the binaries only: run `brew services stop dtalgorand`, check that `D/algod.pid` is gone, then delete `D` (and the history's directory, if `TimeTravelDBDir` put it elsewhere) and `$(brew --prefix)/var/log/dtalgorand.log` by hand. Wallets made with `dtgoal` are outside `D`, in `~/.algorand/mainnet-v1.0/kmd-v0.5`, which a stock `goal` on the same Mac may also use: delete it only if neither holds wallets you need. `brew untap d13co/tap` removes the tap.
- A ttsnap built from source has no keys file of its own: pass `-keys installer/dtalgorand/ttsnap_keys` from the source tree.
