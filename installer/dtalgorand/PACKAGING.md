# dtalgorand packaging

How the dtalgorand packages are laid out, built and released: the Debian package on apt.d13.co and the Homebrew formula in `d13co/tap`. To install a package and set up a node, see [README.md](README.md).

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

## Building the .deb

```sh
installer/dtalgorand/build_deb.sh tmp/dtalgorand
```

The script builds the binaries into `tmp/dtalgorand/<arch>/bin`, not `$GOPATH/bin`, and writes `dtalgorand_<version>_<arch>.deb` to the given directory. It builds for the host's architecture only (cgo), so build the arm64 package on an arm64 host. Pass the same `DEB_VERSION` to both builds to give them the same version.

The version is the upstream stable release the fork is based on plus a fork revision, `<x.y.z>-dt<n>`, e.g. `5.0.2-dt1`. The script takes `x.y.z` from the nearest upstream `v<x.y.z>-stable` tag in git history, and the binaries report it as their version (`algod -v` says `5.0.2`, as upstream's does). d13co/dt-algorand doesn't carry upstream's tags, so fetch them first: `git fetch --no-tags https://github.com/algorand/go-algorand.git 'refs/tags/v*-stable:refs/tags/v*-stable'`. If HEAD has a `v<x.y.z>-dt<n>` tag, that is the package version; otherwise it is a dev build, `<x.y.z>-dt<n>+dev<build number>` with `n` the last release on that base (0 if none) and the build number from `scripts/buildnumber.py` (days since 2018-05-25, then the hour), so it sorts after that release and before the next. Set `DEB_VERSION` to pin the package version, e.g. `DEB_VERSION=5.0.2-dt1+dev306622 installer/dtalgorand/build_deb.sh tmp/dtalgorand`.

## Building the macOS tarball

```sh
installer/dtalgorand/build_darwin.sh tmp/dtalgorand
```

Run it on a Mac: the binaries need cgo (libsodium, SQLite). It builds into `tmp/dtalgorand/darwin-<arch>/bin` and writes `dtalgorand_<version>_darwin-<arch>.tar.gz`, versioned as the .deb is (`installer/dtalgorand/version.sh`; `PKG_VERSION` pins it). The tarball holds the binaries under their stock names in `bin/` and, in `share/`, what the .deb puts in the data directory, plus `ttsnap/keys` (the .deb's `/usr/share/dtalgorand/ttsnap/keys`); the formula installs `share/` as `share/dtalgorand`. The binaries run on macOS 14 and later (`MACOSX_DEPLOYMENT_TARGET`, 14.0 by default); releases build on `macos-15`. They are signed ad hoc, which Apple silicon requires and Homebrew-installed command-line tools need no more than: Homebrew doesn't mark its downloads as quarantined, so Gatekeeper doesn't check them.

The `darwin` workflow builds the tarball on pull requests that touch the packaging, installs it with the formula from a local tap, and starts the node under launchd.

## Releasing

Push a `v<x.y.z>-dt<n>` tag on `dtalgorand` and the release workflow builds the .deb, attaches it to a GitHub release and, once approved, publishes it to apt.d13.co. In parallel it builds the macOS tarball on a Mac runner, attaches it to the same release and, once approved, points the `d13co/tap` formula at it. Number the fork revisions from 1 on each upstream base: after merging upstream's `v5.0.3-stable`, the next release is `v5.0.3-dt1`; a fork-only fix on top of it is `v5.0.3-dt2`. The workflow fails if the tag's `x.y.z` isn't the upstream release `dtalgorand` is based on.

## The tap

The formula lives in [d13co/homebrew-tap](https://github.com/d13co/homebrew-tap), as `Formula/dtalgorand.rb`, rendered from [homebrew/dtalgorand.rb.in](homebrew/dtalgorand.rb.in) by [homebrew/render.sh](homebrew/render.sh). The release workflow's `homebrew` job commits it there. It needs, once:

1. The `d13co/homebrew-tap` repository (public, so that `brew` can clone it), with a first commit (a README): the job checks it out.
2. A deploy key with write access on it: `ssh-keygen -t ed25519 -N '' -C dt-algorand-release -f tap_key`, add `tap_key.pub` as a deploy key of the tap with write access, and `tap_key` as the `HOMEBREW_TAP_DEPLOY_KEY` secret of a `homebrew` environment on d13co/dt-algorand (restricted to `v*` tags, with required reviewers, as `apt` is). Then delete both files.

## File descriptors on macOS

algod raises its own file descriptor limit, so launchd's default (256) doesn't matter: Go raises the soft limit at startup up to `kern.maxfilesperproc` (macOS refuses more, even below the hard limit), and algod then raises it to what its connections, the ledger and the time travel store need. The time travel store can hold up to 4096 files open; if the hard limit leaves less room, algod logs a warning and runs anyway.
