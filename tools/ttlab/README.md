# ttlab

ttlab benchmarks time travel storage designs (`ledger/timetravel`) against real mainnet state deltas. It produced the numbers in the [time travel README](../../ledger/timetravel/README.md#evaluation) and reproduces them. It also has an end-to-end check of the REST API on a running time travel node.

It is a separate Go module so that DuckDB and SQLite stay out of algod's `go.mod`. `make tidy` and the tools CI workflow (`go vet`) cover it, but `make lint` and `make test` do not.

## Build

```sh
cd tools/ttlab
go build -o ttlab .    # or: go run . <command> ...
```

`go.mod` replaces `github.com/algorand/go-algorand` with `../..`, so ttlab builds against the checkout it sits in. Like algod it needs libsodium in `crypto/libs`; a fresh worktree can copy it from another checkout. The first build also compiles DuckDB's cgo bindings.

## Commands

| command | does | output |
|---|---|---|
| [`stats`](#stats) | sizes of each sampled round's changes, block, header and certificate | CSV |
| [`ingest`](#ingest) | builds a store from a contiguous run of rounds, then times point-in-time reads | one JSON line |
| [`engines`](#engines) | loads a store into SQLite and DuckDB and runs the same lookups on all three engines | one JSON line per engine |
| [`catchpoint`](#catchpoint) | reads (a prefix of) a catchpoint file: header, average account and KV sizes | JSON |
| [`demo`](#demo) | REST checks against a dev-mode time travel node | `[ok  ]` / `[FAIL]` lines |

[`estimate.py`](#estimatepy) turns `stats` and `ingest` output into whole-history estimates.

## Input data

`stats`, `ingest` and so `engines` read one msgpack file per round, named `<round>.msgp`:

- state deltas, from `GET /v2/deltas/{round}?format=msgpack`;
- blocks with their certificates, from `GET /v2/blocks/{round}?format=msgpack` (only `stats` reads them).

Nodely's public mainnet API (`https://mainnet-api.4160.nodely.dev`, no token) serves both for every round back to genesis and handles about 25 requests in parallel:

```sh
N=https://mainnet-api.4160.nodely.dev
mkdir run-2026 sample-deltas sample-blocks
# a contiguous run, for ingest
seq 65700000 65749999 | xargs -P 25 -I{} curl -sf -o run-2026/{}.msgp "$N/v2/deltas/{}?format=msgpack"
# a sample every 20,000 rounds, for stats
seq 0 20000 65800000 | xargs -P 25 -I{} curl -sf -o sample-deltas/{}.msgp "$N/v2/deltas/{}?format=msgpack"
seq 0 20000 65800000 | xargs -P 25 -I{} curl -sf -o sample-blocks/{}.msgp "$N/v2/blocks/{}?format=msgpack"
```

A 2026 delta file is about 24 KB, so a 50k-round run is about 1.2 GB. `curl -f` skips rounds that fail; `ingest` stops at the first missing file, so download any gaps again.

## stats

```sh
./ttlab stats -deltas sample-deltas -blocks sample-blocks > stats.csv
```

One CSV row per round in `-deltas`: for each object kind the number of changes (`n_<kind>`) and the bytes of their values (`b_<kind>`), then the number of deletions, the encoded header size and the transaction count. If `-blocks` has the same round, the row ends with the block, block header and certificate sizes. Kinds: `a` accounts, `h` asset holdings, `l` app local states, `A` asset params, `P` app params, `k` boxes and other KVs.

## ingest

```sh
./ttlab ingest -data run-2026 -from 65700000 -to 65749999 -warm 10000 \
  -db /tmp/s -comp zstd -block 131072 >> m2026.jsonl
```

Adds rounds `-from` to `-to` to a new store one at a time, as a syncing node does, and compacts it. Then it runs `-q` point-in-time reads in two classes, each at a random round between the object's version and `-to`: `hot` picks objects in proportion to how often they change, `uniform` picks each distinct object with equal chance.

| flag | default | |
|---|---|---|
| `-data` | | delta directory |
| `-from`, `-to` | | rounds to load, inclusive |
| `-db` | | store directory. **Deleted before the run, and again after it unless `-keep` is set.** |
| `-comp` | zstd | `zstd`, `snappy`, `minlz` or `none` |
| `-block` | 32768 | sstable block size in bytes (algod's store uses 131072) |
| `-hdr` | false | also store block headers |
| `-warm` | 0 | rounds at the start left out of the steady-state figures and the reads |
| `-q` | 20000 | reads per class |
| `-cache` | 256 | block cache, MB |
| `-keep` | false | keep the store, e.g. for `engines` |
| `-dedup` | false | skip [no-op writes](../../ledger/timetravel/README.md#no-op-writes) as algod does. Off, every change is stored, as in the time travel README's measurements. |

The output's main fields:

- `write_us_per_round`, `max_round_ms`: time in `AddRound`.
- `disk_mib`: live sstables after a full compaction. `compression_ratio`: logical bytes over that.
- `steady_logical_bytes_per_round`, `steady_disk_bytes_per_round`: bytes per round after `-warm`, overall and by keyspace in `steady_by_class`.
- `read_hot`, `read_uniform`: latency percentiles in µs, and `found_frac`.

## engines

```sh
./ttlab ingest -data run-2026 -from 65700000 -to 65749999 -warm 10000 -db /tmp/s -keep > /dev/null
./ttlab engines -store /tmp/s -out /tmp/eng
```

Copies every version in a store (made with `ingest -keep`) into SQLite (clustered `(obj, rnd)` primary key) and DuckDB under `-out`, in round order with `-batch` (100) rounds per write transaction. Then it runs the same `-q` (5000) lookups of the latest version at or before a round on Pebble, SQLite and DuckDB, and fails if an engine's answer differs from Pebble's. DuckDB reports reads without an index (`read_noindex`), with an ART index (`read_art`), with an equality lookup on that index and the round filtered client-side (`read_art_eq`), and its size after sorting the table (`disk_sorted_mib`).

## estimate.py

```sh
python3 estimate.py stats.csv 2022=m2022.jsonl 2024=m2024.jsonl 2026=m2026.jsonl
```

Scales the `ingest` results up to mainnet's whole history. Each era's run stands for a span of rounds (`ERAS`: 2022 for rounds below 35M, 2024 for 35M to 55M, 2026 above). For each configuration it takes the run's disk bytes per round relative to that era's `-comp none` run, and applies that ratio to the logical bytes `stats.csv` sampled for those rounds. It also prints the size of an archival block database from the sampled blocks.

- `stats.csv` must sample every 20,000 rounds (`SAMPLE`); any other spacing gives wrong totals.
- Every era file needs a `-comp none` run (any `-block`), the baseline for the others.
- A configuration (compression, block size; N and patches for runs from before their removal) is listed only if every era file has it, and the totals cover only the eras passed.
- Era names must be the ones in `ERAS`.

## catchpoint

```sh
curl -s https://algorand-catchpoints.s3.us-east-2.amazonaws.com/channel/mainnet/latest.catchpoint   # label: <round>#<hash>
dig +short SRV _algobootstrap._tcp.mainnet.algorand.network                                        # relays
python3 -c 'import sys; f=lambda n: f(n//36)+"0123456789abcdefghijklmnopqrstuvwxyz"[n%36] if n else ""; print(f(int(sys.argv[1])))' <round>
curl -s http://<relay>:4160/v1/mainnet-v1.0/ledger/<round in base 36> | head -c 30000000 > cp.tar
./ttlab catchpoint -file cp.tar -db /tmp/cp
```

Reads a catchpoint file as a relay streams it and prints its header and the number of accounts, resources and KVs read, with their average encoded size. A prefix of the file is enough, and relays do not support range requests, so cut the stream with `head -c`: 30 MB holds about 62k accounts. With `-db` it also writes the accounts it read into a store as one full checkpoint and reports its logical and on-disk bytes per account. That directory is deleted before and after.

## demo

Needs `algod` and `goal` built from this branch on the `PATH` (`make install`).

```sh
goal network create -K -r ~/ttnet -t test/testdata/nettemplates/DevModeNetwork.json
# add "EnableTimeTravel": true to ~/ttnet/Node/config.json (Archival stays off)
goal network start -r ~/ttnet
tools/ttlab/ttlab demo -net ~/ttnet
goal network stop -r ~/ttnet
```

The demo uses no kmd (hence `-K`, which skips importing the root keys): it signs with the network directory's `Wallet1.rootkey` and `Wallet2.rootkey` and talks to the node named `Node`, so another template must use those names. It creates an app that counts calls in global state and writes a box, sends `-rounds` (1150) transactions, alternating app calls and payments, one per dev-mode round, and records the live state after each. Then it checks old rounds against those records:

- `?round=` on accounts, apps, boxes and `/v2/ledger/supply`;
- simulate at a past round;
- block headers of old rounds (200) and their pruned bodies (404);
- the `/versions` endpoints, ascending and descending, with paging.

The 404 checks reach 101 rounds past the first, and a node without `Archival` keeps the last ~1000 block bodies, so keep `-rounds` above ~1100. If any check fails, the demo exits with status 1.
