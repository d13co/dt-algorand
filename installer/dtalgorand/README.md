# dtalgorand

The Debian package of this fork ([time travel](../../ledger/timetravel/README.md)), published on apt.d13.co. It installs alongside the stock `algorand` package.

## Building

```sh
installer/dtalgorand/build_deb.sh tmp/dtalgorand
```

The script builds the binaries into `tmp/dtalgorand/<arch>/bin`, not `$GOPATH/bin`, and writes `dtalgorand_<version>_<arch>.deb` to the given directory. It builds for the host's architecture only (cgo), so build the arm64 package on an arm64 host. Pass the same `BUILDNUMBER` to both builds to give them the same version.

The version is `<VersionMajor>.<VersionMinor>.<build number>` from `config/version.go` and `scripts/buildnumber.py` (days since 2018-05-25, then the hour), so later builds sort higher.

## Layout

| Path | |
|---|---|
| `/usr/lib/dtalgorand/bin/` | `algod`, `goal`, `kmd`, `algoh`, `algocfg`, `algokey`, `diagcfg`, `node_exporter` under their stock names: goal and algoh run algod and kmd from their own directory |
| `/usr/bin/dt*` | links to them: `dtalgod`, `dtgoal`, `dtkmd`, `dtalgoh`, `dtalgocfg`, `dtalgokey`, `dtdiagcfg` |
| `/usr/bin/ttimport` | [ttimport](../../tools/ttimport/README.md) |
| `/var/lib/dtalgorand/` | the node's data directory, with mainnet's `genesis.json` and the other networks' under `genesis/` |
| `dtalgorand.service`, `dtalgorand@.service` | the node, and nodes for other data directories (see the unit's comments) |
| `/etc/apt/apt.conf.d/51dtalgorand-upgrades` | unattended upgrades from `apt.d13.co`, suite `stable` |

The node runs as the `dtalgorand` user, whose home is `/var/lib/dtalgorand`: algod and goal keep files in `$HOME/.algorand`.

## Starting the node

The package does not enable or start `dtalgorand.service`. A time travel node syncs from genesis, and a node that synced without time travel cannot turn it on later. On a new install, the package writes a `config.json` with `"EnableTimeTravel": true`. Before the first start:

1. Optionally, put the history on a fast disk. `ProtectHome` keeps the node out of `/home`.

   ```sh
   sudo install -d -o dtalgorand -g dtalgorand /mnt/fast/dtalgorand
   sudo -u dtalgorand dtalgocfg set -d /var/lib/dtalgorand -p TimeTravelDBDir -v /mnt/fast/dtalgorand
   ```

2. Sync from the network (weeks on mainnet), or import an archival node's blocks first. An algod's ledger directory is private to its user, so import as root and hand the result to `dtalgorand`:

   ```sh
   sudo ttimport -blocks <archival data dir>/mainnet-v1.0/ledger.block.sqlite -d /var/lib/dtalgorand
   sudo chown -R dtalgorand:dtalgorand /var/lib/dtalgorand /mnt/fast/dtalgorand
   ```

3. Start the node:

   ```sh
   sudo systemctl enable --now dtalgorand
   sudo -u dtalgorand dtgoal node status -d /var/lib/dtalgorand
   ```

Upgrades restart the nodes that are running.
