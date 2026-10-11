# Time travel (non-standard fork)

`EnableTimeTravel` turns a node into a *state-history* node: it keeps the value of every ledger object (accounts, asset holdings and params, app local states and params, boxes, totals) at every round since genesis, plus every block header, and serves lookups, the REST API and simulation **at any past round**. It is meant to replace an archival node for state queries: with `Archival: false` the old block bodies and certificates (≈3.5 TB on mainnet) are pruned, and the history costs ≈65 GB.

This is a non-compliant fork feature: it changes no consensus rule, but the node answers questions a standard node cannot, and some historical answers are approximations (see [Limitations](#limitations)).

## Using it

```json
{ "EnableTimeTravel": true, "Archival": false, "TimeTravelCheckpointInterval": 1 }
```

- The node must sync from genesis: the history is built from every block's state delta. Fast catchup (`goal node catchup`) is refused, and a ledger whose tracker DB is past genesis with an empty history fails to open. So does one whose tracker DB is ahead of its block DB, which a stock node resets, resyncing from genesis; delete the tracker DB to do that on purpose.
- The store (Pebble) lives in `<dir>/<genesis id>/ledger.timetravel/`, where `<dir>` is `TimeTravelDBDir`, else `BlockDBDir`, else `ColdDataDir`, else the data directory. Queries at past rounds read it at random, so point `TimeTravelDBDir` at a fast disk to keep their latency down. To move an existing store, stop the node, move the `ledger.timetravel` directory, and set `TimeTravelDBDir`. `goal node clone` copies it when it lives in the data directory.
- `tools/ttimport` builds the ledger and its history from an existing archival node's block database instead of the network: `go run ./tools/ttimport -blocks <archival data dir>/<genesis id>/ledger.block.sqlite -d <new data dir>` (the new data directory needs `genesis.json` and a `config.json` with `EnableTimeTravel` and without `Archival`). The ledger opens the archive in place as its block database and replays it like algod after a restart, without certificate or signature checks, resuming from where the data directory's trackers stand; it never changes the archive's contents, and refuses an archive from another network or one behind the data directory. It then copies the last blocks algod needs (about 1,000 rounds, more while state proofs lag) into the data directory's own block database. Every 10 seconds it prints the replayed round and its date, rounds/s and txns/s, the share of time spent reading blocks and writing the account DB and the history, and its resident memory; `-v` adds the ledger's info log. Ctrl-C kills the run, and a rerun resumes from the trackers' last commit. Until a run completes, the directory's block database is behind its account DB, and algod refuses to start on it (rerun ttimport). On fnet's first 40k rounds it replayed at ~8,700 rounds/s (1.6× applying the blocks one by one with `AddBlock`, ~35× a network sync) and produced a byte-identical history. Start algod on the directory afterwards to continue from the network.
- `TimeTravelCheckpointInterval` (N) is fixed when the store is created. 1 (the default) is the recommended setting; see [Patches and N](#patches-and-n).

### REST

Read endpoints take `?round=R` and answer as of the end of round R (the `round` fields of the responses say R):

| endpoint | at round R |
|---|---|
| `GET /v2/accounts/{addr}` (incl. `exclude=`), `/assets`, `/applications`, `/assets/{id}`, `/applications/{id}` | account, resources, pending rewards at R |
| `GET /v2/assets/{id}`, `GET /v2/applications/{id}` | params (and global state) at R |
| `GET /v2/applications/{id}/box?name=`, `/boxes` | box value / box listing at R |
| `GET /v2/ledger/supply` | totals at R |
| `GET /v2/blocks/{R}?header-only=true` | header of any round (no `?round=` needed) |
| `POST /v2/transactions/simulate` with `"round": R` in the request | evaluates the group as if it were in block R+1, projecting every object it touches, including unnamed resources (`allow-unnamed-resources`), from the history |

Not served for pruned rounds (the price of `Archival: false`): full blocks (`/v2/blocks/{R}` without `header-only`), `/v2/blocks/{R}/txids`, `/logs`, `/transactions/{txid}/proof`. `goal clerk simulate --round R` works unchanged.

`GET /v2/deltas/{R}` works for any round whose block is still stored (every round with `Archival: true`, the last ~1000 otherwise): rounds older than the trackers' in-memory deltas are re-evaluated (see [Node integration](#node-integration)).

`Archival: true` also works: the node keeps every block and certificate and serves every stock archival endpoint unchanged, with `?round=` and historical simulate on top. The cost is the archival block DB (~3.5 TB) plus the history (~53 GB; the ~12 GB of history headers duplicate the block DB and go unused).

## Design

### Objects and keys

Every changed object of a round is extracted from the block's `StateDelta` and stored as its full, persisted msgpack encoding:

| kind | object key | value |
|---|---|---|
| account | `a` addr | `trackerdb.BaseAccountData` |
| asset holding | `h` addr asset | `basics.AssetHolding` |
| app local state | `l` addr app | `basics.AppLocalState` |
| asset params | `A` asset | creator + `basics.AssetParams` |
| app params | `P` app | creator + `basics.AppParams` |
| creator index | `C` creator type id | empty (lists created assets/apps per account) |
| kv / box | `k` escaped key | raw bytes |
| totals | `T` | `ledgercore.AccountTotals` |

Deletions are tombstones. Kv keys are escaped (`00`→`00 ff`) and terminated (`00 00`) so variable-length keys keep their order and prefix scans (an app's boxes) stay contiguous.

### Layout: object-major versions

History is stored **object-major**: record key = keyspace byte + object key + big-endian round. All versions of an object are adjacent, so "the value of X at R" is one bounded reverse seek (latest record ≤ R), no matter how long ago X last changed. Listing an account's assets/apps or an app's boxes at R is a prefix scan.

Two record kinds, governed by `TimeTravelCheckpointInterval` (N):

- **checkpoint records** (`c` keyspace): every N rounds, the value of every object that changed since the previous checkpoint round. Unchanged objects are not repeated; genesis is the first checkpoint.
- **patches** (`p` keyspace), for the rounds in between: per changed object, a patch against the object's value at the last checkpoint round (or against its previous version, `Incremental`, used only by the evaluation tools). Patches are git-style copy/insert byte deltas over the msgpack encoding, so the same codec handles boxes. A patch that isn't smaller than the value is stored as the value.
- reading X@R: the latest `c` record ≤ c(R), however old, plus the latest `p` record in (c(R), R], applied in memory. Two seeks.
- a per-round index (`r`) of changed keys lets a checkpoint round be materialized after a restart; it is deleted once the checkpoint is written.

With N = 1 (the default) every version is a checkpoint record: no patches, no index, one seek per read, and **writes are blind appends** (patching needs a read of the base for every change).

Since a read is one seek whatever the age of the version, the store never re-writes the whole state: there are no periodic snapshots of unchanged objects.

### Node integration

- `ledger/timetravel.go`: a ledger tracker. `newBlock` encodes the round's changes and header; `prepareCommit` writes every round up to the commit's new base and fsyncs **before** the tracker DB transaction, so the history always reaches at least the tracker DB round. After a crash, replayed rounds already in the store are skipped.
- Every round-taking `Ledger` lookup (`LookupWithoutRewards`, `LookupAccount`, `LookupAsset/Application`, `LookupKv`, `LookupKeysByPrefix`, `LookupKvPairsByPrefix`, `GetCreatorForRound`, `Totals`, `LookupAgreement`, `OnlineCirculation`, `BlockHdr`) first asks the trackers and falls back to the history when they answer "round older than my window" (`RoundOffsetError`, or a pruned header). Consensus paths never read the history; the in-window answers are unchanged.
- Simulation needed no change: the evaluator pulls objects lazily through those lookups at `round`, so referenced and unnamed resources are projected on demand. `CheckDup` is skipped for rounds older than the txtail (no txids are kept).
- Reads go through a view that merges the rounds the tracker holds in memory (from `newBlock` until their commit, already encoded as store keys) over the store, so every round up to the tip is readable, including listings at rounds between the tracker DB round and the tip.
- Old state deltas: `GetStateDeltaForRound` falls back to evaluating the round's block against the history at the previous round, exactly as `AddBlock` does (no validation: the block is certified), at most two at a time. Each round's header record also stores an 8-byte digest of the round's sorted changes; a re-evaluated delta must reproduce it exactly, or the request fails with a 500 instead of returning a delta that disagrees (missing or extra writes included). A round whose block is gone is a 404.
- REST: `?round=R` wraps the node's ledger in a view whose "latest" is R (`daemon/algod/api/server/v2/timetravel.go`); the handlers are unchanged apart from how they obtain the ledger.
- Online stake: since `ExcludeExpiredCirculation` (v38, mainnet round 30,097,387), the online stake of vote round V (`OnlineCirculation(BalanceRound(V), V)`, used by agreement, `/v2/ledger/supply` and the `online_stake` opcode) is the online money at the balance round minus the stake of the accounts online then whose keys expire before V. The trackers work it out from ~320 rounds of online-account history, which the history does not keep; instead the tracker keeps the online accounts in memory (loaded from the tracker DB's `onlineaccounts` table, then updated from each round's delta) and records each round's expired stake for vote round round + 320 (`E` keyspace, written with the round). Summing the online accounts takes ~2 µs per round for mainnet's 1.5k. `TestTimeTravelOnlineCirculation` checks every round against the trackers.

### Limitations

- The online stake is only recorded for rounds written since the history recorded it, and only for the vote round agreement pairs with each balance round. Otherwise `/v2/ledger/supply?round=` answers 404, and the `online_stake` opcode fails in a simulation. Rounds before v38 need no record: their online stake is the online money.
- Simulating at an old round does not detect duplicate txids/leases, and does not knock offline absent accounts.
- Re-evaluating a delta whose round has no recorded online stake gives `online_stake` the online money (an upper bound); if that changes the block's writes, the digest check fails (an error, never a wrong delta). `voter_params_get` reads only the account's online data, which the history answers exactly.
- The store pre-allocates WAL files (~200 MB fixed overhead).

### TODO

- Research ways to make catchpoints (fast catchup) work, instead of requiring a sync from genesis. Candidates: start the history at the catchpoint round and serve only later rounds; ship history snapshots (Pebble checkpoints or SST files) alongside catchpoints; serve rounds after the catchpoint right away while a background job backfills the older rounds forward from genesis (replaying archival peers' blocks, or importing deltas from a node that serves them; going backwards from the catchpoint is not possible, since deltas carry no previous values).
- Resource endpoints to expose the rounds at which they changes, something like `/v2/accounts/ABC?versions` returning paginated full objects

## Evaluation

All measurements use **real mainnet state deltas** (`GET /v2/deltas/{round}` from a public node, which serves the whole history): 3,289 rounds sampled every 20k rounds across all 65.8M rounds (with their blocks and certificates), and contiguous runs of 20k rounds at 25.0M (2022) and 45.0M (2024) and 50k rounds at 65.7M (2026). The first 5k/5k/10k rounds of each run warm up the store and are excluded from per-round figures. Machine: 16 cores, NVMe, 14 GB RAM. Reproduce with `tools/ttlab` (see the end).

### What history is made of

Over mainnet's history there were 2.6 B object changes; storing every version in full is 0.60 TB of values (0.72 TB with keys). **82% of it is app params**: every global-state write re-stores the whole `AppParams`, approval and clear programs included (2.5 KB on average). That is the redundancy patches target. Accounts are 31 GB, local states 29 GB, asset params 32 GB, holdings 6 GB, boxes 10 GB.

### Engine

The same 1.59 M versions (2026 run, N = 1) loaded in round order like a syncing node, 100 rounds per write transaction, and the same 5,000 point-in-time lookups (latest version ≤ R):

| engine | disk | ingest | lookup p50 | p99 |
|---|---|---|---|---|
| **Pebble v2**, zstd 32 KB blocks | **91 MiB** | **~7 s** | **4.7 µs** | **62 µs** |
| SQLite, clustered `(obj, round)` primary key | 975 MiB | 58 s | 11.5 µs | 746 µs |
| DuckDB, no index | 1,075 MiB | 24 s | 3.8 ms | 5.7 ms |
| DuckDB, ART index on `obj` | 1,075 MiB | +1.1 s | 3.7 ms | 5.3 ms |
| DuckDB, ART equality lookup, round filtered client-side | 1,075 MiB | | 2.5 ms | 109 ms |
| DuckDB, table sorted by `(obj, round)` | 1,661 MiB | | 2.0 ms | 9.3 ms |

DuckDB is a columnar analytics engine: its blob compression is weak, and a point lookup costs a query plan and a row-group scan, ART index or not. It would suit scans ("balance of X over time"), not the thousands of point lookups per second that API and simulation traffic need. SQLite has no compression. **Pebble** (an LSM, already a go-algorand dependency in its v1 form; the store uses `pebble/v2`, whose zstd works with the `DataDog/zstd` go-algorand pins) gives ordered keys for one-seek reads, append-friendly writes and per-block compression, and wins on every axis.

### Patches and N

Steady-state disk bytes per round (zstd unless noted), write cost per round, and checkpoint stalls:

| setting | 2022 B/round | 2024 | 2026 | write µs/round (2026) | checkpoint stall | read p50 / p99 µs (2026) |
|---|---|---|---|---|---|---|
| N=1, uncompressed | 17,240 | 26,045 | 15,249 | 151 | – | 4.2 / 50 |
| N=256, uncompressed | | | 10,072 | 700 | 47 ms | 7.3 / 55 |
| N=1, 32 KB | 2,304 | 3,581 | 1,950 | 144 | – | 4.4 / 68 |
| N=256, 32 KB | 1,363 | 3,094 | 1,739 | 781 | 52 ms | 8.4 / 104 |
| N=1024, 32 KB | 1,246 | 2,781 | 1,598 | 710 | 162 ms | 8.4 / 112 |
| N=1024 incremental, 32 KB | 1,279 | 2,818 | 1,267 | 1,659 | 131 ms | 11.3 / 216 |
| N=1024, 128 KB | 1,154 | 2,681 | 1,014 | 574 | 137 ms | 7.9 / 186 |
| **N=1, 128 KB** | 1,206 | **2,352** | **940** | **144** | – | 4.7 / 187 |
| N=1024, 128 KB, 8 MB cache | | | 1,014 | 8,264 | 2,857 ms | 382 / 494 |
| N=1, 128 KB, 8 MB cache | | | 940 | 143 | – | 292 / 375 |

2026 only, 32 KB blocks: N=16 2,135, N=64 1,947, N=4096 1,613 B/round; incremental N=64 1,749, N=256 1,489. Write CPU of patch settings is 4–14× N=1's across the three eras.

- **Uncompressed, patches matter** (−34% at N = 256): they strip the repeated programs out of app-param versions.
- **With block compression they don't.** In object-major order an object's consecutive versions are neighbours in the same compression block, so zstd removes the same redundancy that patches do, and patches (high-entropy offsets and literals) compress poorly, while checkpoints re-store values. At 32 KB blocks patches still save 18–46%; at 128 KB blocks full versions win in 2024 (−12%) and 2026 (−7%) and lose by 4% in 2022. Over all of mainnet: 53 GB for full versions vs 57 GB for the best patch setting.
- Patches cost 4–14× the write CPU with a warm cache and **8.3 ms/round with a cold one** (every change reads its base; N = 1 writes stay at ~0.15 ms), stall commits for 0.15–0.5 s per checkpoint warm (2.9 s cold), and double read latency. Incremental patches are smaller but chain: hot objects average 82 patches per read at N = 1024 and 320 at N = 4096.

**Default: N = 1 (full versions) with zstd and 128 KB blocks.** Patches stay available through `TimeTravelCheckpointInterval`, but cost more than they save.

### Block size and read latency

2026 run, N = 1, zstd; reads are 100k lookups after the run, with an 8 MB block cache (cold: nearly every lookup decompresses blocks) and a 1 GB one (warm):

| block | disk B/round | compression | cold p50 / p90 / p99 µs | warm p50 / p99 µs |
|---|---|---|---|---|
| 4 KB | 7,396 | 2.2× | | 4.1 / 49 |
| 32 KB | 1,950 | 8.2× | 153 / 262 / 331 | 3.9 / 14 |
| 64 KB | 1,274 | 12.5× | 72 / 290 / 319 | 3.8 / 11 |
| **128 KB** | **940** | **17.0×** | 292 / 329 / 375 | 3.5 / 9 |
| 256 KB | 758 | 21.0× | 255 / – / 407 | |

Snappy at 32 KB: 2,962 B/round (5.4×). 64 KB blocks are the latency-leaning alternative (26% more disk).

Bigger blocks keep compressing better (adjacent versions of the same object share a block). Reads stay sub-millisecond with a cold cache; the block cache absorbs hot objects.

### Mainnet estimates

Per-era disk/logical ratios from the runs, applied to the sampled history (rounds < 35 M use the 2022 ratios, 35–55 M the 2024 ones, the rest the 2026 ones):

| setting | state history, 65.8 M rounds | history writes during a genesis sync |
|---|---|---|
| N=1, uncompressed | 689 GB | 3.6 h |
| N=1, 32 KB | 92 GB | 3.6 h |
| N=256, 32 KB | 72 GB | 17.8 h |
| N=1024, 32 KB | 65 GB | 16.4 h |
| N=1024 incremental, 32 KB | 63 GB | 44.7 h |
| N=1024, 128 KB | 57 GB | 16.6 h (≈6 days with a cold cache) |
| **N=1, 128 KB (default)** | **53 GB** | **3.5 h** |

Archival comparison, from the sampled blocks: an archival block DB holds **~3.48 TB** raw (block bodies ~1.26 TB, **certificates 2.16 TB**, now ~65 KB per round, and headers stored twice). A time-travel node with `Archival: false` keeps headers only: 33 GB raw, **~12 GB** compressed (~150–310 B per round on disk). Total for full state history + headers: **~65 GB** (plus ~0.5 GB of per-round digests) vs ~3.5 TB of blocks that cannot answer a single historical state query.

Sync from genesis: the history adds ~0.15–0.27 ms per round of write work in the commit goroutine (~3 hours over 65.8 M rounds, against days of block evaluation). Patch settings add 10–26 hours with a warm cache and ~6 days with a realistic cold one.

## Reproducing

```sh
cd tools/ttlab   # separate module: DuckDB stays out of algod's go.mod
# deltas: GET /v2/deltas/{round}?format=msgpack -> <dir>/<round>.msgp, blocks likewise
go run . stats -deltas sample-deltas -blocks sample-blocks > stats.csv
go run . ingest -data run-2026 -from 65700000 -to 65749999 -warm 10000 -db /tmp/s -n 1 -comp zstd -block 131072 >> m2026.jsonl
go run . engines -store <N=1 store kept with -keep> -out /tmp/eng
go run . catchpoint -file <catchpoint tar prefix from a relay> -db /tmp/cp
python3 estimate.py stats.csv 2022=m2022.jsonl 2024=m2024.jsonl 2026=m2026.jsonl
go run . demo -net <dev-mode network created with goal network create -K>   # REST end to end
```
