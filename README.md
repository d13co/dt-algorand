# dt-algorand

An experimental fork of [go-algorand](https://github.com/algorand/go-algorand) that adds **time travel**: a node that keeps the value of every account, asset, app and box at every round since genesis, and serves the REST API and transaction simulation **at any past round**. On mainnet the history takes about 70 GB, against ≈3.5 TB of archived blocks.

It changes no consensus rule, but it is not an official Algorand release: the node answers questions a standard node cannot, and some historical answers are approximations (see [Limitations](ledger/timetravel/README.md#limitations)). Releases follow upstream's stable releases: `v5.0.5-dt4` is upstream `5.0.5` plus fork revision 4. The revision counts the fork's releases and doesn't restart when the upstream base changes. Development happens on the `dtalgorand` branch.

## Running a node

Install the `dtalgorand` package: a Debian package from apt.d13.co, which installs alongside the stock `algorand` package, or `brew install d13co/tap/dtalgorand` on macOS 14 or later on Apple silicon. Builds are on the [releases page](https://github.com/d13co/dt-algorand/releases). Its commands have a `dt` prefix (`dtalgod`, `dtgoal`, ...) and read `$DTALGORAND_DATA` and `$DTALGORAND_KMD` rather than `$ALGORAND_DATA` and `$ALGORAND_KMD`, so they leave a stock node on the same machine alone. A mainnet node needs about 90 GB of disk today, ideally NVMe. Then pick a path before the node's first start, which otherwise begins a sync from genesis:

| path | time on mainnet | needs |
|---|---|---|
| install a history snapshot, then catch up from a catchpoint (recommended) | an hour or two | nothing else |
| import an archival node's blocks with `ttimport` | days | an archival node synced from genesis |
| sync from genesis | weeks | a machine that stays on for weeks |

[installer/dtalgorand/README.md](installer/dtalgorand/README.md#setting-up-a-node) has the requirements, the trade-offs and the steps for each path, on Linux and macOS.

**Not for validators**: the fork is not audited for running validators, so on mainnet, testnet and betanet the node refuses participation keys unless its config sets `"DangerouslyEnableParticipation": true`. See [Participation](installer/dtalgorand/README.md#participation).

## What it adds

- **Use**: [ledger/timetravel/README.md](ledger/timetravel/README.md) covers the configuration (`EnableTimeTravel`), the REST endpoints that take `?round=`, simulation at a past round, the design, and the mainnet size estimates. [VERSIONS.md](ledger/timetravel/VERSIONS.md) describes the API that lists the rounds at which an object changed.
- **Round marker**: every answer given as of a past round carries `X-Algod-TT-Round: R`, so a client can tell that the node honoured `round`; a stock node ignores the parameter and answers as of the latest round. See [REST](ledger/timetravel/README.md#rest).
- **Asset holders**: `GET /v2/assets/{id}/holders` lists an asset's holders at any round, from an index of every holding by asset (`TimeTravelHoldersIndex`, on in a new package install). See [Holders index](ledger/timetravel/README.md#holders-index).
- **goal**: read commands take `--round`, and `versions` subcommands list an object's changes. See [cmd/goal/README.md](cmd/goal/README.md#past-rounds-and-versions-time-travel).
- **Simulate limits**: `SimulateResourceLimitMultiplier` lets an operator raise the limits simulate puts on unnamed resources, for read-only calls that touch many accounts or boxes. See [Unnamed-resource limits in simulation](ledger/timetravel/README.md#unnamed-resource-limits-in-simulation).
- **Simulate budget**: `MaxAPISimulateExtraOpcodeBudget` lets an operator raise (or lower) the 320,000 limit on `extra-opcode-budget`, per group (mind the CPU, timeout and trace-memory costs listed there). See [Extra opcode budget limit in simulation](ledger/timetravel/README.md#extra-opcode-budget-limit-in-simulation).
- **Simulate groups**: one simulate request can hold several transaction groups, run in order as one block or (`parallel-groups`) each alone against the start state. See [Several transaction groups in one simulation](ledger/timetravel/README.md#several-transaction-groups-in-one-simulation).
- **Simulate rounds**: one simulate request can list several rounds (`rounds`) and gets one result per round (`round-results`), simulated in parallel, optionally with the transactions' validity windows skipped or moved to each round (`skip-validity-window`, `rewrite-validity-window`), so the rounds can be more than 1000 apart. See [One simulation at several rounds](ledger/timetravel/README.md#one-simulation-at-several-rounds).
- **Simulate protocol**: `protocol` simulates a round under a later consensus protocol, as if an upgrade to it took effect right after that round (e.g. AVM 13 against 2022 state), and reports where the old chain lacks data the new protocol reads. See [A later protocol at a past round](ledger/timetravel/README.md#a-later-protocol-at-a-past-round).
- **Queries**: `POST /v2/query` runs read-only TEAL: one program, many calls, each alone at each listed round, without signatures, fees or a block, and answers with each call's logs, cost and error. See [Read-only queries](ledger/timetravel/README.md#read-only-queries-post-v2query).
- **Capabilities**: `GET /v2/capabilities` tells clients which fork features the node has turned on and what its simulate and query requests accept. See [Capabilities](ledger/timetravel/README.md#capabilities-get-v2capabilities).
- **Overload limits**: simulate, query and the versions endpoints stop at the node's time limit (504) and when the client goes away, and `SimulateParallelism` caps the simulations running at once (503 with `Retry-After` beyond it). See [Overload limits](ledger/timetravel/README.md#overload-limits).
- **Effort header**: with `EnableEffortHeader`, expensive requests report the work they did in an `X-Algod-Effort` header (opcodes, lookups, history reads, response bytes), so a load balancer can bill on it. Browsers on other origins can read it, and `Retry-After`. See [Effort header](ledger/timetravel/README.md#effort-header).
- **Tools**:
  - [ttimport](tools/ttimport/README.md) builds a time travel node from an archival node's blocks instead of syncing from the network, which takes weeks on mainnet.
  - [ttsnap](tools/ttsnap/README.md) publishes snapshots of a node's history and installs them on new nodes, which then catch up from a catchpoint instead of syncing from genesis.
  - [ttdump](tools/ttdump/README.md) explores the history store of a stopped node.
  - [ttlab](tools/ttlab/README.md) benchmarks storage designs against mainnet state deltas.

The rest of this README is upstream's.

---

# go-algorand (upstream)
| **Branch**      | **Build Status** |
| --------------- | ---------------- |
| **rel/stable**  | [![Nightly Tests](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml/badge.svg?branch=rel%2Fstable)](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml) |
| **rel/beta**    | [![Nightly Tests](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml/badge.svg?branch=rel%2Fbeta)](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml) |
| **rel/nightly** | [![Nightly Tests](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml/badge.svg?branch=rel%2Fnightly)](https://github.com/algorand/go-algorand/actions/workflows/ci-nightly.yml) |


**Algorand's** official implementation in Go.

Algorand is a permissionless, pure proof-of-stake blockchain that delivers decentralization, scalability, security, and transaction finality.

## Getting Started

Visit our [developer website](https://dev.algorand.co/) for the most up-to-date information about using and installing the Algorand platform.

## Building from Source

Development is done using the Go Programming Language. The Go version is specified in the project's [go.mod](go.mod) file. This document assumes you have a functioning environment set up. If you need assistance setting up an environment, please visit the [official Go documentation website](https://golang.org/doc/).

### Linux / OSX

We currently strive to support Debian-based distributions, with Ubuntu 24.04 as our official release target. Building on Arch Linux also works well. Our core engineering team uses Linux and OSX, so both environments are well-supported for development.

**OSX Only**: [Homebrew (brew)](https://brew.sh) must be installed before continuing. [Here](https://docs.brew.sh/Installation) are the installation requirements.

### Initial Environment Setup

```bash
git clone https://github.com/algorand/go-algorand
cd go-algorand
./scripts/configure_dev.sh
```

At this point, you are ready to build go-algorand. We use `make` and have several targets to automate common tasks.

### Build

```bash
make install
```

### Test

```bash
# Unit tests
make test

# Integration tests
make integration
```

### Style and Checks

```bash
make fmt
make lint
make fix
make vet
```

Alternatively, run:

```bash
make sanity
```

## Running a Node

Once the software is built, you'll find binaries in `${GOPATH}/bin`, and a data directory will be initialized at `~/.algorand`. Start your node with:

```bash
${GOPATH}/bin/goal node start -d ~/.algorand
```

Use:

```bash
${GOPATH}/bin/carpenter -d ~/.algorand
```

to see activity. Refer to the [developer website](https://developer.algorand.org/) for instructions on using different tools.

### Providing Your Own Data Directory

You can run a node out of other directories than `~/.algorand` and join networks other than mainnet. Just make a new directory and copy the `genesis.json` file for the network into it. For example:

```bash
mkdir ~/testnet_data
cp installer/genesis/testnet/genesis.json ~/testnet_data/genesis.json
${GOPATH}/bin/goal node start -d ~/testnet_data
```

Genesis files for mainnet, testnet, and betanet can be found in `installer/genesis/`.

## Contributing

Please refer to our [CONTRIBUTING](CONTRIBUTING.md) document.

## Project Layout

`go-algorand` is organized into various subsystems and packages:

### Core

Provides core functionality to the `algod` and `kmd` daemons, as well as other tools and commands:

- **crypto**: Contains the cryptographic constructions used for hashing, signatures, and VRFs. It also includes Algorand-specific details about spending keys, protocol keys, one-time-use signing keys, and how they relate to each other.
- **config**: Holds configuration parameters, including those used locally by the node and those that must be agreed upon by the protocol.
- **data**: Defines various types used throughout the codebase.
  - **basics**: Holds basic types such as MicroAlgos, account data, and addresses.
  - **account**: Defines accounts, including "root" accounts (which can spend money) and "participation" accounts (which can participate in the agreement protocol).
  - **transactions**: Defines transactions that accounts can issue against the Algorand state, including standard payments and participation key registration transactions.
  - **bookkeeping**: Defines blocks, which are batches of transactions atomically committed to Algorand.
  - **pools**: Implements the transaction pool, holding transactions seen by a node in memory before they are proposed in a block.
  - **committee**: Implements the credentials that authenticate a participating account's membership in the agreement protocol.
- **ledger** ([README](ledger/README.md)): Contains the Algorand Ledger state machine, which holds the sequence of blocks. The Ledger executes the state transitions resulting from applying these blocks. It answers queries on blocks (e.g., what transactions were in the last committed block?) and on accounts (e.g., what is my balance?).
- **protocol**: Declares constants used to identify protocol versions, tags for routing network messages, and prefixes for domain separation of cryptographic inputs. It also implements the canonical encoder.
- **network**: Contains the code for participating in a mesh network based on WebSockets. It maintains connections to some number of peers, (optionally) accepts connections from peers, sends point-to-point and broadcast messages, and receives messages, routing them to various handler code (e.g., agreement/gossip/network.go registers three handlers).
  - **rpcs**: Contains the HTTP RPCs used by `algod` processes to query one another.
- **agreement** ([README](agreement/README.md)): Contains the agreement service, which implements Algorand's Byzantine Agreement protocol. This protocol allows participating accounts to quickly confirm blocks in a fork-safe manner, provided that sufficient account stake is correctly executing the protocol.
- **node**: Integrates the components above and handles initialization and shutdown. It provides queries into these components.

### Daemon

Contains the two daemons that provide Algorand clients with services:

- **daemon/algod**: Holds the `algod` daemon, which implements a participating node. `algod` allows a node to participate in the agreement protocol, submit and confirm transactions, and view the state of the Algorand Ledger.
  - **daemon/algod/api** ([README](daemon/algod/api/README.md)): The REST interface used for interactions with `algod`.
- **daemon/kmd** ([README](daemon/kmd/README.md)): Holds the `kmd` daemon, which allows a node to sign transactions. Since `kmd` is separate from `algod`, it enables a user to sign transactions on an air-gapped computer.

### Interfacing

Enables developers to interface with the Algorand system:

- **cmd**: Contains the primary commands defining entry points into the system.
- **libgoal**: Exports a Go interface useful for developers of Algorand clients.
- **tools** ([README](tools/README.md)): Various tools and utilities that don’t have a better place to go.
- **tools/debug**: Holds secondary commands that assist developers during debugging.
- **tools/misc** ([README](tools/misc/README.md)): Small tools that are handy in a pinch.

### Deployment

Helps Algorand developers deploy networks of their own:

- **nodecontrol**
- **docker**
- **netdeploy**

### Utilities

Provides utilities for the various components:

- **logging**: A wrapper around `logrus`.
- **util**: Contains a variety of utilities, including a codec, a SQLite wrapper, a goroutine pool, a timer interface, node metrics, and more.

### Test

- **test** ([README](test/README.md)): Contains end-to-end tests and utilities for the above components.

## License

[![License: AGPL v3](https://img.shields.io/badge/License-AGPL%20v3-blue.svg)](COPYING)

Please see the [COPYING_FAQ](COPYING_FAQ) for details on how to apply our license.

Copyright (C) 2019-2026, Algorand Foundation Ltd.

