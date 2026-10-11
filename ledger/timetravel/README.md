# Time travel (non-standard fork)

`EnableTimeTravel` turns a node into a *state-history* node: it keeps the value of every ledger object (accounts, asset holdings and params, app local states and params, boxes, totals) at every round since genesis, plus every block header, and serves lookups, the REST API and simulation **at any past round**. It is meant to replace an archival node for state queries: with `Archival: false` the old block bodies and certificates (≈3.5 TB on mainnet) are pruned, and the history costs ≈65 GB (57–73 GB, see [Mainnet estimates](#mainnet-estimates)).

This is a non-compliant fork feature: it changes no consensus rule, but the node answers questions a standard node cannot, and some historical answers are approximations (see [Limitations](#limitations)).

## Using it

```json
{ "EnableTimeTravel": true, "Archival": false, "TimeTravelCheckpointInterval": 1 }
```

- The node must sync from genesis, over the network or from an archive with `ttimport` (below): the history is built from every block's state delta. Fast catchup (`goal node catchup`) is refused, and a ledger whose tracker DB is past genesis with an empty history fails to open. So does one whose tracker DB is ahead of its block DB, which a stock node resets, resyncing from genesis; delete the tracker DB to do that on purpose.
- The store (Pebble) lives in `<dir>/<genesis id>/ledger.timetravel/`, where `<dir>` is `TimeTravelDBDir`, else `BlockDBDir`, else `ColdDataDir`, else the data directory. Queries at past rounds read it at random, so point `TimeTravelDBDir` at a fast disk to keep their latency down. To move an existing store, stop the node, move the `ledger.timetravel` directory, and set `TimeTravelDBDir`. `goal node clone` copies it when it lives in the data directory.
- `tools/ttimport` builds a non-archival node's ledger and history from an existing archival node's block database instead of the network: `ttimport -blocks <archival data dir>/<genesis id>/ledger.block.sqlite -d <new data dir>`. The ledger replays the archive in place like algod after a restart (no certificate or signature checks, nothing written to the archive), then the data directory gets the last ~1,000 blocks, and algod started on it continues from the network. On fnet's first 40k rounds it replayed at ~8,700 rounds/s (~35× a network sync) and produced a byte-identical history (identical reads in any case: after a restart, the raw records can differ by the [no-op writes](#no-op-writes) a fresh cache lets through). See [tools/ttimport/README.md](../../tools/ttimport/README.md) for the how-to.
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

Not served for pruned rounds (the price of `Archival: false`): full blocks (`/v2/blocks/{R}` without `header-only`), `/v2/blocks/{R}/hash`, `/txids`, `/logs`, `/transactions/{txid}/proof`, `/lightheader/proof`, and `/v2/stateproofs/{R}`, which read the full blocks after R. `goal clerk simulate --round R` works unchanged.

Object histories (`timetravel` tag in the API spec, registered only with `EnableTimeTravel`): `GET .../versions` lists the rounds at which an object's stored value changed, paginated, with the new values on `include=values`. A write of an identical value is not a version; a deletion is (`deleted: true`). Pending rewards are not versions: each version's account is the account at that round, as `?round=` returns it. See [VERSIONS.md](VERSIONS.md) for the design.

| endpoint | versions of |
|---|---|
| `GET /v2/accounts/{addr}/versions` | everything `/v2/accounts/{addr}` returns: base record, holdings, local states, created assets' and apps' params (`exclude=all`, `created-apps-params`, `created-assets-params` narrow it as they do there). Each version lists only the objects that changed in that round, so `?round=min-round` plus the versions gives the account at any round of the range |
| `GET /v2/accounts/{addr}/assets/{id}/versions`, `/applications/{id}/versions` | the holding or local state, plus the params if the account created the asset or app |
| `GET /v2/assets/{id}/versions`, `GET /v2/applications/{id}/versions` | the params (global state included) |
| `GET /v2/applications/{id}/box/versions?name=` | the box |

They take `min-round`, `max-round` (inclusive, default 0 and the latest round), `order=asc|desc`, `limit` (default 100, at most 10,000, or 1,000 with values) and `next` (the previous page's `next-token`, present if and only if more versions remain); pages with values also stop before the version that would take their stored values past 1 MB, unless it is the first (the JSON is larger: base64, decoded state). History never changes, so pages stay consistent while blocks arrive. A leaf object's versions are one range scan (its versions are adjacent in the store); an account merges one scan per object it has ever had, capped by `MaxAPIResourcesPerAccount`, counting every resource the account ever had, deleted ones included (400 beyond it).

`GET /v2/deltas/{R}` works for any round whose block is still stored (every round with `Archival: true`, otherwise the last ~1,000, more with `MaxBlockHistoryLookback`): rounds older than the trackers' in-memory deltas are re-evaluated (see [Node integration](#node-integration)).

`Archival: true` also works: the node keeps every block and certificate and serves every stock archival endpoint unchanged, with `?round=` and historical simulate on top. The cost is the archival block DB (~3.5 TB) plus the history (~65 GB, of which ~12 GB are headers that duplicate the block DB's; historical lookups read them from the history all the same).

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

With N = 1 (the default) every version is a checkpoint record: no patches, no index, one seek per read, and **writes are blind appends** (patching needs a read of the base for every change; skipping [no-op writes](#no-op-writes) reads only memory).

Since a read is one seek whatever the age of the version, the store never re-writes the whole state: there are no periodic snapshots of unchanged objects.

### No-op writes

Deltas rewrite objects that did not change: every block writes the totals and the rewards pool account, changed or not, and apps rewrite identical global state and boxes. In fnet's first 25.3 M rounds these were 55% of the records (50.2 M of 91.8 M); on mainnet the pool alone writes one in every round that distributes no rewards. The size measurements and estimates below include them.

- `AddRound` skips a change that leaves its object in the state its latest record holds, when an in-memory cache of recently written objects knows that state: 2^18 objects (~30 MB), each by the SHA-256 of its value. Writes still read nothing from the store, and the cache costs about one more hash of the round's values. Objects the cache has forgotten are stored as before, as is the first rewrite of each object after a restart. The round's digest still covers every change.
- `ttimport -dedup -d <data dir>` removes them from the store of a stopped node, including those written before the cache existed, and drops the tombstones of objects that never existed (created and deleted in one round). On fnet it removed 50.3 M records (3.21 → 2.94 GiB) in 1.5 minutes, and every read compared answered the same.

### Node integration

- `ledger/timetravel.go`: a ledger tracker. `newBlock` encodes the round's changes and header; `prepareCommit` starts writing every round up to the commit's new base, alongside the tracker DB transaction, and `commitRound` waits for the write and its fsync inside that transaction **before** it commits, so the history always reaches at least the tracker DB round. After a crash, replayed rounds already in the store are skipped.
- The round-taking `Ledger` lookups of state and headers (`LookupWithoutRewards`, `LookupAccount`, `LookupAsset/Application`, `LookupKv`, `LookupKeysByPrefix`, `LookupKvPairsByPrefix`, `GetCreatorForRound`, `Totals`, `LookupAgreement`, `OnlineCirculation`, `BlockHdr`) first ask the trackers and fall back to the history when they answer "round older than my window" (`RoundOffsetError`, or a pruned header). `Block`, `BlockCert`, `GetKnockOfflineCandidates` and the state proof lookups have no fallback. Consensus paths never read the history; the in-window answers are unchanged.
- Simulation pulls objects lazily through those lookups at `round`, so referenced and unnamed resources are projected on demand. Its only change is duplicate detection: at a round the ledger already has, `CheckDupAt` checks the txtail's leases and txids of the rounds before the simulated one only, ignoring the later ones.
- Reads go through a view that merges the rounds the tracker holds in memory (from `newBlock` until their commit, already encoded as store keys) over the store, so every round up to the tip is readable, including listings at rounds between the tracker DB round and the tip.
- Old state deltas: `GetStateDeltaForRound` falls back to evaluating the round's block against the history at the previous round, exactly as `AddBlock` does (no validation: the block is certified), at most two at a time. Each round's header record also stores an 8-byte digest of the round's sorted changes; a re-evaluated delta must reproduce it exactly, or the request fails with a 500 instead of returning a delta that disagrees (missing or extra writes included). A round whose block is gone is a 404.
- REST: `?round=R` wraps the node's ledger in a view whose "latest" is R (`daemon/algod/api/server/v2/timetravel.go`); the handlers are unchanged apart from how they obtain the ledger, and two errors: 404 for a supply whose online stake was not recorded, 500 for a delta whose re-evaluation fails its digest check. `/v2/applications/{id}/boxes` takes `round` in the stock API and keeps its own handling on every node; on a time travel node its box listing falls back to the history.
- Online stake: since `ExcludeExpiredCirculation` (v38, mainnet round 30,097,387), the online stake of vote round V (`OnlineCirculation(BalanceRound(V), V)`, used by agreement, `/v2/ledger/supply` and the `online_stake` opcode) is the online money at the balance round minus the stake of the accounts online then whose keys expire before V. The trackers work it out from ~320 rounds of online-account history, which the history does not keep; instead the tracker keeps the online accounts in memory (loaded from the tracker DB's `onlineaccounts` table, then updated from each round's delta) and records each round's expired stake for vote round round + 320 (`E` keyspace, written with the round). The set keeps running sums of the expired accounts' stake and a heap of the others by key expiry, so a round costs only its changed and newly expired accounts. `TestTimeTravelOnlineCirculation` checks every round against the trackers.

### Limitations

- The online stake is only recorded for rounds written since the history recorded it, and only for the vote round agreement pairs with each balance round. Otherwise `/v2/ledger/supply?round=` answers 404, and the `online_stake` opcode fails in a simulation. Rounds before v38 need no record: their online stake is the online money.
- Simulating at an old round detects duplicate txids and leases only against transactions still in the txtail (about the last 1,000 rounds), and knocks offline only the absent or expired accounts the simulated transactions touch.
- Re-evaluating a delta whose round has no recorded online stake gives `online_stake` the online money (an upper bound); if that changes the block's writes, the digest check fails (an error, never a wrong delta). `voter_params_get` reads only the account's online data, which the history answers exactly.
- The store pre-allocates WAL files: up to four of 70 MiB (110% of the memtable), ~280 MB of fixed overhead.

### TODO

- Research ways to make catchpoints (fast catchup) work, instead of requiring a sync from genesis. Candidates: start the history at the catchpoint round and serve only later rounds; ship history snapshots (Pebble checkpoints or SST files) alongside catchpoints; serve rounds after the catchpoint right away while a background job backfills the older rounds forward from genesis (replaying archival peers' blocks, or importing deltas from a node that serves them; going backwards from the catchpoint is not possible, since deltas carry no previous values).

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
- **With block compression they don't.** In object-major order an object's consecutive versions are neighbours in the same compression block, so zstd removes the same redundancy that patches do, and patches (high-entropy offsets and literals) compress poorly, while checkpoints re-store values. At 32 KB blocks patches still save 18–46%; at 128 KB blocks full versions win in 2024 (−12%) and 2026 (−7%) and lose by 4% in 2022. Over all of mainnet, with per-era ratios: 53 GB of state for full versions vs 57 GB for the best patch setting (see [Mainnet estimates](#mainnet-estimates) for the full-history figure).
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

Snappy at 32 KB: 2,962 B/round (5.4×). 64 KB blocks are the latency-leaning alternative (36% more disk).

Bigger blocks keep compressing better (adjacent versions of the same object share a block). Reads stay sub-millisecond with a cold cache; the block cache absorbs hot objects.

### Mainnet estimates

Comparing settings: each run's disk/logical ratio, applied to the sampled history (rounds < 35 M use the 2022 run, 35–55 M the 2024 one, the rest the 2026 one):

| setting | state history, 65.8 M rounds, per-era ratios | history writes during a genesis sync |
|---|---|---|
| N=1, uncompressed | 689 GB | 3.6 h |
| N=1, 32 KB | 92 GB | 3.6 h |
| N=256, 32 KB | 72 GB | 17.8 h |
| N=1024, 32 KB | 65 GB | 16.4 h |
| N=1024 incremental, 32 KB | 63 GB | 44.7 h |
| N=1024, 128 KB | 57 GB | 16.6 h (≈6 days with a cold cache) |
| **N=1, 128 KB (default)** | **53 GB** | **3.5 h** |

These figures rank the settings but are not absolute sizes. One ratio per era misweights the record classes. Fnet's time-travel store, the one full history built so far (25.3 M rounds by `ttimport`, 3.44 GB), also shows that a full history compresses better than separate runs. Fnet's own size says little about mainnet's: 74% of it is headers, since fnet is nearly idle. The full-history estimate for the default setting corrects two errors that roughly cancel:

- **Class mix (+12 GB).** App params compress ~26×, accounts and holdings ~4×. The 2022 run is 83% app params by logical bytes, but rounds below 35 M are only 62%, so the run's blended ratio is too low for its era. Applying each run's per-class ratios to the sampled history gives 65.5 GB of state instead of 53. The reweighting shrinks the share of app params, where patches save the most, so it should not reverse the ranking above. The patch settings were not re-measured per class.
- **Full-history adjacency (−14 GB).** Disk is close to a·records + b·runs, where a run is one object's versions within a window: the first version of a run compresses against unrelated neighbours, later ones against the previous version. In a full history an object has one run, so each extra run that the per-era windows imply costs b too much. Fitted on fnet windows of 50 k rounds or less, the model predicts fnet's full 25.3 M-round store within 0.1%. On mainnet, the number of objects ever is bounded by the catchpoint's live counts at 65.8 M (22.6 M accounts, 130 M holdings) plus the sampled deletions. Merging runs saves ~14 GB, mostly on accounts and holdings.

| class | per-era runs | full history (bounds) |
|---|---|---|
| app params | 18.6 GB | 17.1 GB (16.2–17.8) |
| accounts | 18.5 GB | 13.7 GB (11.5–14.9) |
| holdings | 14.8 GB | 10.3 GB (8.9–11.7) |
| local states | 4.2 GB | 3.4 GB (3.3–3.5) |
| asset params | 4.0 GB | 3.0 GB (2.5–3.7) |
| boxes | 3.3 GB | 1.8 GB (1.8–2.1) |
| totals | 1.1 GB | 1.1 GB |
| creator index | 1.0 GB | 0.6 GB (0.5–0.8) |
| **state, fully compacted** | **65.5 GB** | **51 GB (45–57)** |
| headers and digests | | 12.05 GB (11.0–13.3) |
| expired stake (`E`, rounds ≥ 30,097,387) | | 0.2 GB |
| fully compacted | | 63 GB (56–70) |
| **live store** | | **65 GB (57–73)** |

A live store is 2% larger than a fully compacted one (fnet: 3.44 vs 3.37 GB; 5% on state records). Header compression depends on the header's size: raw headers are ~400–480 B before round ~46.5 M, ~530 B with incentives, and ~680 B from ~54 M. They take ~120 B per round on disk early on and ~315 B now. The bounds combine:

- the number of objects ever, from the live count up to the live count plus every sampled deletion;
- the per-run saving, ±10% (the fnet fit);
- each class's sampling error;
- the header ratio of each size regime;
- live overhead of 2–5%.

They do not cover one risk: each era's per-class ratios come from a single run.

Expected live store size while a node syncs from genesis (or `ttimport` replays):

| round | 10 M | 20 M | 30 M | 40 M | 50 M | 60 M | 65.8 M |
|---|---|---|---|---|---|---|---|
| GB | 2.6 (2.1–3.0) | 11.4 (9.5–12.9) | 20.3 (17.5–22.7) | 31.5 (27.5–34.8) | 46.7 (40.9–51.8) | 59.1 (52.0–65.3) | 65.4 (56.8–73.1) |

Archival comparison, from the sampled blocks: an archival block DB holds **~3.48 TB** raw (block bodies ~1.26 TB, **certificates 2.16 TB**, now ~65 KB per round, and headers stored twice). A time-travel node with `Archival: false` keeps headers only: 33 GB raw, **~12 GB** compressed with their digests. Total: **~65 GB** (57–73) vs ~3.5 TB of blocks that cannot answer a single historical state query.

Sync from genesis: the history adds ~0.15–0.27 ms per round of write work in the commit goroutine (~3 hours over 65.8 M rounds, against days of block evaluation). Patch settings add 10–26 hours with a warm cache and ~6 days with a realistic cold one.

## Reproducing

[tools/ttlab/README.md](../../tools/ttlab/README.md) describes the input data, each command's options and its output.

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
