# ttimport

`ttimport` builds a non-archival [time travel](../../ledger/timetravel/README.md) node (`EnableTimeTravel: true`, `Archival: false`) from an archival node's block database instead of the network. It writes the account DB and the history of every round into a new data directory, then copies over the last blocks algod needs. algod started on that directory continues from the network.

A time travel node's history comes from every round's state delta since genesis: a node either builds it, or installs another node's with [ttsnap](../ttsnap/README.md) and catches up from a catchpoint. Syncing from genesis over the network takes weeks on mainnet. On fnet's first 40k rounds, `ttimport` replayed ~8,700 rounds/s, ~35× a network sync and 1.6× applying the blocks one by one with `AddBlock`, and produced a byte-identical history.

## How it works

- The ledger opens the archive in place as its block database. It replays the archive the way algod replays its own blocks after a restart: no certificate or signature checks, so use an archive you trust. The replay starts from wherever the data directory's account DB stands.
- `ttimport` never writes to the archive. Both `Archival` and catchpoint tracking are off for the run, so the ledger neither resets nor prunes the archive. Replay adds no blocks, so the ledger writes none to it.
- The replay ends at the archive's last round as of when the ledger opens it. The data directory's own block database then gets, in one transaction, the blocks that catchpoint catchup would fetch: about 1,000 rounds before the account DB's round, more while state proofs lag, up to the last round.

## What you need

- **An archival node's block database for the same network**, holding every round from genesis: `<archival data dir>/<genesis id>/ledger.block.sqlite`, or under its `BlockDBDir` or `ColdDataDir`.
  - The archival node can keep running. Blocks it writes during the import are left for a rerun, which replays only the new rounds. This was tested with a writer appending blocks to the archive the way algod's block queue does.
- **Disk for the new data directory:**
  - the history: ~65 GB on mainnet (57–73, see the [estimate](../../ledger/timetravel/README.md#mainnet-estimates)), 3.2 GB for fnet's 25.3 M rounds;
  - the account DB of a non-archival node;
  - about 1,000 blocks.
  
  The history is read at random by queries at past rounds, so put it on a fast disk (`TimeTravelDBDir`).
- **Binaries from this fork:** `ttimport`, and the `algod` and `goal` that run the node afterwards. `make build` installs all three in `$GOPATH/bin`. Once `crypto/libs` is built, `go build -o . ./tools/ttimport ./cmd/algod ./cmd/goal` builds them into the current directory.

## Building a node

1. Create the data directory with the network's `genesis.json` and a `config.json`:

   ```sh
   mkdir ~/tt
   cp <archival data dir>/genesis.json ~/tt/
   cat > ~/tt/config.json <<'EOF'
   {
     "EnableTimeTravel": true,
     "Archival": false
   }
   EOF
   ```

   Set anything that decides where the ledger's files go **now**: `TimeTravelDBDir`, `HotDataDir`, `ColdDataDir`, `TrackerDBDir`, `BlockDBDir`. `ttimport` writes to the directories they resolve to, and algod later looks for its files there. None of them may resolve to the archive's directory. `TimeTravelCheckpointInterval` is also fixed once the history exists; keep the default of 1.

2. Run the import:

   ```sh
   ttimport -blocks /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite -d ~/tt
   ```

   On a 40k-round fnet archive:

   ```
   replaying rounds 1..40000 of the source; start algod on /home/me/tt only after this finishes
   replayed to round 40000 in 3s (13663 rounds/s)
   /home/me/tt/fnet-v1/ledger.block.sqlite now holds rounds 38995..40000
   ```

   A replay that can take days prints a progress line every 10 seconds:
   - the replayed round and its block's date;
   - rounds/s and txns/s over the interval, and overall rounds/s;
   - the share of the interval spent reading blocks, writing the account DB, and writing the history;
   - resident memory, split into the Go runtime's (with the heap's live bytes) and the rest, which is mostly the history's block cache and memtables and SQLite's page caches.

   Blocks are read on a goroutine a few blocks ahead, so a block-read share near 100% means the replay is waiting on the archive's disk. Commits run while the replay continues, and each writes the history alongside its account DB transaction, which waits for that write before committing. So the write shares overlap, the account-write share includes the history write, and an account-write share near 100% means the replay is waiting on commits. On an HDD archive this gets much worse once memory runs short. Early mainnet rounds replayed at ~5–7k rounds/s on HDDs.

3. If the import is interrupted, rerun the same command. Ctrl-C kills the run (it cannot stop cleanly), and a rerun resumes from the account DB's last commit. A replay commits at least every 10,000 rounds, so at most that many are replayed again. Until a run completes, the data directory's block database is behind its account DB, and algod refuses to start:

   ```
   couldn't initialize the node: the account DB is at round 19998 but the block DB ends at round 0: rerun tools/ttimport if it was interrupted, or delete the account DB to resync from genesis
   ```

4. Start the node. If the archival node kept running, a last rerun just before starting brings the directory up to the archive's current tip, so algod has fewer rounds to fetch. A rerun on a complete directory replays only the rounds after the account DB's round, and leaves the block database alone when it already reaches the archive's tip.

   ```sh
   goal node start -d ~/tt
   goal node status -d ~/tt
   curl -H "X-Algo-API-Token: $(cat ~/tt/algod.token)" "$(cat ~/tt/algod.net)/v2/ledger/supply?round=1000"
   ```

   algod catches up the rounds after the archive's last one from the network, verifying certificates as usual. The node no longer needs the archive.

## Removing no-op records

Stores written before the ledger skipped [no-op writes](../../ledger/timetravel/README.md#no-op-writes) hold a record for the totals and the rewards pool in nearly every round. With the node stopped, remove them:

```sh
goal node stop -d ~/tt
ttimport -dedup -d ~/tt
goal node start -d ~/tt
```

On fnet's 25.3 M rounds:

```
removing no-op records from /home/me/tt/fnet-v1/ledger.timetravel (rounds 0..25314401, 3.21 GiB)
scanned 91824136 records, removed 50291820
removed 50291820 records in 22s; compacting
compacted in 59s: 3.21 GiB -> 2.94 GiB
```

Reads and `/versions` answer the same afterwards. The one difference: objects created and deleted within a single round (apps, assets, holdings, local states) no longer count toward `MaxAPIResourcesPerAccount` in `/v2/accounts/{address}/versions`. Ctrl-C is safe, and a rerun finishes the job. While algod runs on the directory, the store is locked and `-dedup` refuses it. The compaction at the end rewrites the store in consecutive key ranges, deleting each range's old files when it finishes, so it needs free disk on the store's partition for a range rather than for a second copy of the store: 1.9 GiB at the peak on an 8.7 GiB test store, where one compaction of the whole store took 6.8 GiB. On mainnet (~65 GB) expect roughly 3–8 GiB per range, and up to ~15 GiB with Pebble's own compactions running alongside. `-dedup` also rebuilds the box index (next section), so a store that needs both needs only `-dedup`; after a Ctrl-C the index covers only the rounds added since, until a rerun.

## Building the box index

`/v2/applications/{id}/boxes/versions` reads the store's [box index](../../ledger/timetravel/README.md#box-index). Stores that held boxes before a build with the index first opened them have it only from that round on, and the endpoint answers 503 for earlier ranges (`ttdump info` shows where the index starts). With the node stopped, build it:

```sh
goal node stop -d ~/tt
ttimport -index-boxes -d ~/tt
goal node start -d ~/tt
```

It scans the box records once and writes an entry per box version, then compacts the index range by range, as `-dedup` does; a rerun rebuilds it again. Ctrl-C is safe: the index then covers only the rounds added after it, until a rerun. A store that holds no box yet needs nothing: the index is complete from genesis. On mainnet boxes arrived with v36, around round 25 M, so an import that has not reached that round can switch to a build with the index and skip this.

## Filling a gap left by an algod without time travel

An algod without time travel (upstream `algod`, say, which ignores `EnableTimeTravel`) started on the directory catches up and commits rounds to the account DB alone. The ledger then refuses the directory:

```
ttimport: reloadLedger.loadFromDisk tracker *ledger.historyTracker failed to loadFromDisk : time travel store /home/me/tt/mainnet-v1.0/ledger.timetravel ends at round 65949151 but the ledger is at round 65949318
```

With algod stopped, `-fill` adds the missing rounds to the store, with their state deltas from an algod API that serves `/v2/deltas` for them (any archival node with deltas, such as `https://mainnet-api.4160.nodely.dev`) and their headers from the archive:

```sh
ttimport -fill https://mainnet-api.4160.nodely.dev -blocks /var/lib/algorand/mainnet-v1.0/ledger.block.sqlite -d ~/tt
```

It first rebuilds the last 10 stored rounds from their deltas and adds nothing unless their changes, headers and expired stake match the store. The expired stake comes from the account DB's online account history, which reaches back `MaxBalLookback` (320) rounds, so the gap must be shorter than ~310 rounds: past that, only an import from genesis rebuilds the store. Then rerun the import, or start the node.

## Building the asset holders index

`/v2/assets/{id}/holders` reads the store's [asset holders index](../../ledger/timetravel/README.md#holders-index), which exists only once built. Set `"TimeTravelHoldersIndex": true` in the data directory's `config.json` (`-holders-index` refuses otherwise, since algod deletes the index at launch when the option is off), then with the node stopped:

```sh
goal node stop -d ~/tt
ttimport -holders-index -d ~/tt
goal node start -d ~/tt
```

It scans the holding records once and writes an entry per holding that ever existed, then compacts the index; a rerun rebuilds it again. On a mainnet store (61.7 GiB, 115 M holdings ever) the index is ~3.6 GiB, and the build should take about 5 minutes, writes and compaction. From then on every round algod or a `ttimport` import adds keeps it up to date, so it can be built at any point of an import (stop it, build, rerun it to resume), and the earlier the faster. Ctrl-C is safe: it leaves no index (the endpoint answers 503), until a rerun. A build without the index that adds rounds to the store (an older algod) leaves it stale, and it needs `-holders-index` again; so does turning `TimeTravelHoldersIndex` off and on. `ttdump info` shows the index's state.

## Flags

| flag | |
|---|---|
| `-blocks` | the archive's block database; the file must be named `ledger.block.sqlite` |
| `-d` | the data directory to build: `genesis.json`, and `config.json` with `EnableTimeTravel` and without `Archival` |
| `-dedup` | instead of importing, remove the no-op records from the time travel store of `-d` (no `-blocks`), and rebuild its box index |
| `-index-boxes` | instead of importing, build the box index of the time travel store of `-d` (no `-blocks`) |
| `-holders-index` | instead of importing, build the asset holders index of the time travel store of `-d` (no `-blocks`), whose `config.json` sets `TimeTravelHoldersIndex` |
| `-fill <algod API URL>` | instead of importing, add to the time travel store of `-d` the rounds its account DB is ahead by, with deltas from the API and headers from `-blocks` |
| `-v` | also print the ledger's info log |
| `-cpuprofile <file>` | CPU profile of the run, kept on Ctrl-C |
| `-memprofile <file>` | heap profile, rewritten whenever resident memory reaches a new high (from 1 GiB, in 256 MiB steps) and on Ctrl-C, so that one survives an OOM kill |

## Refusals

`ttimport` refuses, before replaying:

- a `config.json` without `EnableTimeTravel`, or with `Archival`;
- a source not named `ledger.block.sqlite`, a missing source, and a source with no blocks;
- a source from another network, judged by its last block header's genesis hash against `genesis.json`;
- a data directory whose ledger directories resolve into the source's directory.

The ledger refuses a data directory that is ahead of the source. For an archival time travel node (`Archival: true`), sync from genesis over the network instead.
