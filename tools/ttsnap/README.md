# ttsnap

`ttsnap` lets a new [time travel](../../ledger/timetravel/README.md) node start from a recent round instead of syncing from genesis. A producer, a time travel node on btrfs, publishes snapshots of its history; a new node downloads the history (about 70 GB on mainnet), then catches up from a catchpoint like any non-archival node. The catchpoint brings the current state, verified against its label as usual; the node checks the history against it before applying it, deletes the history's rounds after the catchpoint's balances round, and writes them again from its own evaluation of the blocks.

## New node

The installer README's [From a snapshot](../../installer/dtalgorand/README.md#from-a-snapshot) has the steps for the dtalgorand packages, on Linux and macOS. In short, on a data directory whose node has never started, as the node's user:

```sh
ttsnap fetch -url https://tt-snapshots-mainnet.d13.co -d <data dir>   # download and install the history
ttimport -holders-index -d <data dir>                                 # if config.json sets TimeTravelHoldersIndex
# start algod, wait until goal node status answers, then:
ttsnap catchup -d <data dir>                                          # catch up from the network's latest catchpoint
```

`https://tt-snapshots-mainnet.d13.co` serves mainnet's snapshots. Keep `EnableTimeTravel: true` and `Archival: false` (archival nodes can't catch up from a catchpoint), and set `TimeTravelDBDir` before `fetch` if the history goes on another disk. A ttsnap built from source has no keys file of its own: pass `-keys installer/dtalgorand/ttsnap_keys` from the source tree.

### fetch

- It refuses a data directory that already has a history (`ledger.timetravel already exists`), which any start of the node creates: see [Starting over](../../installer/dtalgorand/README.md#starting-over).
- It checks first that the history's disk has room for what is left to download, plus 5% and 1 GiB.
- Downloads resume when rerun. The history downloads into `ledger.timetravel.ttsnap-partial/` next to its target, and is renamed into place once every file checks out, the headers chain from genesis and the commitment is the manifest's (minutes on mainnet with NVMe). A full snapshot's tracker and block DBs download as `*.ttsnap-partial` files next to theirs. A rerun of an install that stopped while moving files finishes it.
- `-parallel` sets the files downloaded at once (4), `-workers` the goroutines that check the commitment (one per CPU).
- `-keys` selects another keys file, and `-min-attest N` requires N attesters' signatures of the snapshot's statement.
- The latest pointer is not signed, so a mirror could keep serving an old one: `fetch` refuses a latest snapshot older than `-max-age` (7 days). `-manifest` installs a given one.
- The producer keeps its last two snapshots, so a download has between about 8 and 15 hours, depending on when it starts. One that takes longer fails once its files are gone: rerun `fetch` to move to the latest snapshot, which keeps the files the two share.
- It drops an asset holders index the snapshot carries, since the commitment doesn't cover it, so build one with `ttimport -holders-index` before starting the node, as `fetch` reminds you when `config.json` sets `TimeTravelHoldersIndex`. About 5 minutes on mainnet; the catchup then keeps it complete.
- It writes `ttsnap.json` to the genesis directory (`<data dir>/<genesis id>/`), recording the installed manifest.

### catchup

`ttsnap catchup -d <data dir>` asks the running algod to catch up from the network's latest catchpoint (or `-label <round>#<hash>`), then returns. Follow the catchup with `goal node status` (`dtgoal` from the packages): it is done when the `Catchpoint` lines are gone and the node syncs normally.

The catchpoint's balances round (its round − 320) must be at most the history's last round. Catchpoints come every 10,000 rounds (about 7.7 hours on mainnet), and mainnet's producer starts a snapshot in the 290 rounds before each one (about 13 minutes), after its balances round, so each snapshot reaches the next catchpoint and the one before it. The network's latest catchpoint fits the latest snapshot, and keeps fitting the one you installed until the catchpoint after those two is published, about 7.7 hours after the snapshot.

Before it applies the catchpoint, the catchup deletes the history's rounds after the balances round (minutes on mainnet; a shutdown interrupts it and the catchup resumes on the next start, an abort waits for it), and the node writes them again. Before the catchup, the node serves the installed history only for the rounds it has synced. `goal node catchup <label>` works the same way; algod refuses a catchpoint the history does not reach.

### When the latest catchpoint is past the snapshot

This happens after an install that took longer than about 7.7 hours, or when the producer missed a run. `catchup` then stops, names the catchpoint the producer recorded in the manifest (the latest one before the snapshot), and prints the command that uses it. The choices:

- `-snapshot-label` uses the producer's catchpoint. The producer vouches for the account state; attesters don't check the label.
- `-label <round>#<hash>` takes one from a source you trust, such as a catchpoint-tracking node of your own.
- Wait for the next snapshot, then [start over](../../installer/dtalgorand/README.md#starting-over) and install it.

### verify

With algod stopped, `ttsnap verify -d <data dir>` checks the headers (minutes on mainnet), and with `-deltas` compares `-sample` random rounds (`-seed` fixes them) with an archival node's `/v2/deltas`:

```sh
# Linux
sudo systemctl stop dtalgorand
sudo -u dtalgorand ttsnap verify -d /var/lib/dtalgorand -deltas https://mainnet-api.4160.nodely.dev -sample 1000
sudo systemctl start dtalgorand

# macOS
brew services stop dtalgorand
ttsnap verify -d $(brew --prefix)/var/dtalgorand -deltas https://mainnet-api.4160.nodely.dev -sample 1000
brew services start dtalgorand
```

`-clear-diverged` removes, once the checks pass, the mark algod leaves on a history it found to differ from the chain (see [Trust](#trust)).

## Trust

| what | how it is checked |
|---|---|
| the files are the published ones | SHA-256 of each file, listed in the manifest |
| the manifest is the producer's | Falcon-1024 signature by a producer key of the package's keys file (`/usr/share/dtalgorand/ttsnap/keys`; with Homebrew, `$(brew --prefix)/opt/dtalgorand/share/dtalgorand/ttsnap/keys`) or of `-keys`, before anything is written |
| the history's content is what the producer says | `fetch` recomputes the history's [commitment](#commitment) from the downloaded files and compares it with the manifest's, before it installs anything. It refuses a store that holds anything readers could see besides what the commitment covers (records after the snapshot's round, keys outside the store's keyspaces, unknown meta records), deletes the records the commitment leaves out (no-ops, lone tombstones), drops an asset holders index the snapshot carries (build one with `ttimport -holders-index`) and rebuilds the box index, so the installed store answers like any other with that commitment |
| the history's content is what other nodes have | with `-min-attest N`, N attester keys of the keys file must have signed the snapshot's [statement](#statements) (its round, header hash and commitment), checked before the download. An attester signs only what its own history, built without a snapshot, says. The package's keys file holds no attester keys yet, so any `-min-attest` fails for now |
| every header is the chain's | `fetch`, before it installs anything (and `ttsnap verify` later): every header is of this network (its genesis hash) and hashes to the next one's `Branch`. algod then checks that header 0 is the genesis block, when it opens the ledger, and the history's header of B against the catchpoint, whose blocks chain to the labelled one |
| the catchpoint | its label is what the node's account state is checked against: by default the network's latest, from Algorand's catchpoint bucket as `goal node catchup` takes it. The label the producer recorded in the manifest is used only with `ttsnap catchup -snapshot-label`, and is the producer's word: attesters don't check it |
| the history at the catchpoint's balances round B | the node compares the history's header and totals of round B with the catchpoint's (the catchpoint's header of B is hash-chained to the labelled block) before it swaps anything; a history that does not fit aborts the catchup and leaves the node as it was |
| the rounds after B | written by the node: before it swaps anything, the catchup deletes every record of the history after B (it reads the whole history to find them, minutes on mainnet), and the node writes those rounds from its own evaluation of the blocks, as a node that synced them would. Comparing the rounds the history holds with the evaluation instead would see only the objects each round changed, not records added for others. A history with a record key that is not an object's (`timetravel.ValidObjectKey`), which no node writes and which reads could take for a version of any round, aborts the catchup and is marked untrusted, durably: the node keeps running, but serves nothing from the history (`?round=` and `/versions` answer 503; old deltas, past-round box listings and simulations fail) until the store is replaced, or `ttsnap verify -clear-diverged` clears the mark. The node serves no history for rounds it hasn't reached yet. A node that syncs the installed history without a catchpoint catchup serves its rounds as the producer's, as it does the rounds before B, and compares with its evaluation only the objects each round changed |
| the rounds before B | attested by the producer, and by the attesters with `-min-attest`: the commitment covers every record of every round, so a history that differs from theirs in any record, records added included, does not match their statements. `ttsnap verify -deltas <archival algod> -sample N` compares N random rounds with an archival node's `/v2/deltas` |

A full snapshot (`-full`) also carries the producer's tracker and block DBs, so the node starts without a catchpoint. Its account state is the producer's and nothing checks it: `fetch -full` needs `-trust-producer`. Use it between your own nodes.

## Producer

Producing snapshots needs Linux, for btrfs; installing them (above) and [attesting](#attesters) also work on macOS.

The producer is a time travel node whose genesis directory, history included, lives in one btrfs subvolume, so that one snapshot covers the tracker DB, the blocks and the history at the same instant. Taken while algod runs, a snapshot opens like a ledger after a crash: blocks are written before the trackers commit them, and the history is synced before the tracker DB commits. `TestTimeTravelSnapshotSoak` (ledger package, `TT_SNAPSHOT_SOAK=N`) copies a running ledger N times at random instants and checks that every copy opens and answers like the original.

1. Move the genesis directory onto a btrfs image laid out for snapshots ([scripts/btrfs_hot_ledger.sh](../../scripts/btrfs_hot_ledger.sh)): the ledger in an `@live` subvolume bind-mounted on the genesis directory, snapshots in `@snap` next to it. An image from the script's earlier layout is converted in place.
2. Make a signing key, and give its public key line to the consumers (in the package's [keys file](../../installer/dtalgorand/ttsnap_keys)):

   ```sh
   ttsnap keygen -o /root/ttsnap_key -name d13 -role producer
   ```
3. Run [scripts/dt_snapshot.sh](../../scripts/dt_snapshot.sh) from a timer: it looks up the network's latest catchpoint label (set `NETWORK=mainnet` with the layout of step 1: when `@live` is the genesis directory the script can't tell the network, and publishes without a label, which leaves `ttsnap catchup -snapshot-label` nothing to use), snapshots `@live`, runs `ttsnap prepare` and `ttsnap publish` on the snapshot, keeps a few snapshots and garbage-collects the tree. Serve `OUT` as static files (anything that answers range requests), or copy it to a bucket with `UPLOAD`: blobs first, then the manifests, then the latest pointers, and deletions last, so that a reader never finds a manifest whose blobs aren't there yet. Manifests and blobs never change once written; only the pointers do.

   ```sh
   OUT=/srv/snapshots KEY=/root/ttsnap_key \
   UPLOAD='rclone copy "$OUT/blobs" r2:snapshots/blobs && rclone copy --exclude "latest-*" "$OUT/snapshots" r2:snapshots/snapshots && rclone copy "$OUT/snapshots" r2:snapshots/snapshots && rclone sync "$OUT" r2:snapshots' \
   scripts/dt_snapshot.sh
   ```

   [installer/tt-snapshot](../../installer/tt-snapshot/README.md) sets this up with systemd, as mainnet's producer runs it: a snapshot just before each catchpoint, after its balances round, so that it reaches the catchpoint (see [New node](#new-node)), and an upload to a Cloudflare R2 bucket.

The commands it runs:

- `ttsnap prepare [-full] <genesis dir>` takes a writable snapshot (never a running node's directory): it opens the history, which replays the store's WAL, flushes it to tables and deletes obsolete files, and folds the SQLite WALs into their DBs. It refuses a history that doesn't start at genesis, whose box index is incomplete, or (`-full`) whose DBs are not of one instant.
- `ttsnap publish -o <out> -key <key file> [-full] [-catchpoint <label>] [-workers N] <genesis dir>` copies every file to `<out>/blobs/<sha256[:2]>/<sha256>`, unless it is there already (most of the history's tables are, from earlier snapshots), computes the history's commitment, writes the statement and the producer's signature of it, then the signed manifest `<out>/snapshots/<genesis id>/<kind>-<round>.json`, and points `latest-<kind>` at it. It refuses a snapshot older than the latest one, and leaves out a catchpoint label the history does not reach.
- `ttsnap gc -o <out> -keep N` keeps the newest N (3) snapshots of each kind, deletes the blobs no manifest lists and the statements of the snapshots it removed.
- `ttsnap info [-full] <genesis dir>` prints what a genesis directory holds.

The long commands (`prepare`, `publish`, `fetch`, `verify`) report progress on stderr: what they are doing and, while they copy, download or check, how far they are every 10 seconds (with the rate and about how long is left where they can tell). Results stay on stdout.

## Attesters

An attester runs a time travel node whose history it built itself: synced from genesis, or imported from an archival node's blocks with `ttimport`. It signs the statement its own history makes about a snapshot's round, if it is the snapshot's; consumers with `-min-attest` require such signatures. The node computes the commitment through an admin endpoint (`GET /v2/timetravel/commitment/<round>`, not in the REST spec, one at a time); a node that is not running is read directly.

An attester can run on macOS, with the Homebrew package (`-d $(brew --prefix)/var/dtalgorand`), as long as its node built its own history (synced or imported with `ttimport`): one installed with `ttsnap fetch` can't attest.

```sh
ttsnap keygen -o ~/attester_key -name <your name> -role attester   # give the printed line to the consumers
ttsnap attest -key ~/attester_key -d <data dir> -url https://tt-snapshots-mainnet.d13.co -o <out>
```

`attest` writes `<out>/snapshots/<genesis id>/statements/<round>/<key id>.sig`, for the producer's tree (or the on-chain registry). It refuses a node whose history was installed from a snapshot (`fetch` marks the store, and writes `ttsnap.json` to the genesis directory), and a history whose statement differs from the snapshot's. `-manifest` attests a given snapshot instead of the latest, `-round R` a round of its own, and `-workers` sets the goroutines that read a stopped node's history; `ttsnap commitment -d <data dir> -round R` prints the statement without signing it. The endpoint reads with half the node's CPUs; stopping algod cancels it.

A history written before the node recorded expired stake (the `E` records) lacks them for some rounds: its commitment differs from a current node's, as its content does. Rebuild it with `ttimport` before attesting.

## Commitment

The commitment is a SHA-256 digest of a history's content up to a round, the same for every node with that history, whatever its files: synced or imported, compacted or not, with or without the no-op records the dedup cache missed, with more rounds after. It covers every version of every object at or before the round, the header record of every round and the expired stake records, each in key order, and leaves out the derived and local records (box index, meta). Each object hashes to a digest; the digests are cut into chunks after each digest whose first two bytes are zero, so that the chunks depend on the content alone and the key space can be split among workers in any way (see `ledger/timetravel/commitment.go`). Content made to avoid chunk boundaries (box values ground on chain) costs parallelism, not memory or correctness: a worker past 1M digests without one hands them over in batches. A store where one object's key starts with another's, which no node writes, is refused: their versions would interleave.

Two copies of fnet's history taken two weeks apart, written by different builds and one with 23k more rounds, give the same commitment at the earlier one's last round; so does a copy before and after `ttimport -dedup` or `-index-boxes`, and a fetched copy after the install removed 2,775 no-op records.

It reads the whole history: fnet's 25.3M rounds (3.0 GB on disk, 20.5 GB of keys and values) take 45–50 s on one core and 9 s with 16 goroutines on an idle 16-thread desktop, from the page cache (about 70 CPU-seconds). Mainnet's history (about 70 GB on disk, 0.75 TB of keys and values) should take about 6 minutes with 16 goroutines when the disk keeps up (NVMe), and the read speed of the disk otherwise.

## Statements

A statement is 134 bytes: `dtalgorand-ttsnap/statement/1\n`, the genesis hash, the round (8 bytes, big endian), the round's header hash and the commitment. It depends on the chain alone, so every correct history makes the same one. Its signatures are deterministic Falcon-1024 (det1024) signatures of these bytes, the scheme the AVM's `falcon_verify` checks: the same key and statement always give the same signature, and a contract can check one as is (a public key is 1,793 bytes, a signature at most 1,423). A manifest signature signs `dtalgorand-ttsnap/manifest/1\n` and the manifest's SHA-256, so neither can pass for the other.

Signature files read `falcon1024 <key id> <base64 signature>`; a key ID is the first 8 bytes of the public key's SHA-256, in hex.

## Snapshot tree

```
blobs/<sha256[:2]>/<sha256>                        every file, by content
snapshots/<genesis id>/history-<round>.json        manifest (+ .sig), never rewritten
snapshots/<genesis id>/latest-history              the name of the latest manifest
snapshots/<genesis id>/full-<round>.json           full snapshots, if published (latest-full)
snapshots/<genesis id>/statements/<round>/         statement.hex, and <key id>.sig per signer
```

A manifest lists the network, the history's rounds, last header hash, box index start and Pebble format, its commitment, a catchpoint label it reaches, and each file's path, size and SHA-256. Paths are limited to the history store's file names (and for a full snapshot the tracker and block DBs); `fetch` refuses anything else, and decides where each file goes from the local `config.json` alone.
