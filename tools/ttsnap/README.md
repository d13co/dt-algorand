# ttsnap

`ttsnap` lets a new [time travel](../../ledger/timetravel/README.md) node start from a recent round instead of syncing from genesis. A producer, a time travel node on btrfs, publishes snapshots of its history; a new node downloads the history (~65 GB on mainnet), then catches up from a catchpoint like any non-archival node. The catchpoint brings the current state, verified against its label as usual; the node checks the history against it before applying it and checks every round it replays.

## Trust

| what | how it is checked |
|---|---|
| the files are the published ones | SHA-256 of each file, listed in the manifest |
| the manifest is the producer's | `ssh-keygen -Y` signature, against the public keys in `/usr/share/dtalgorand/ttsnap/allowed_signers` (or `-keys`), before anything is written |
| every header is the chain's | `fetch`, before it installs anything (and `ttsnap verify` later): every header is of this network (its genesis hash) and hashes to the next one's `Branch`. algod then checks that header 0 is the genesis block, when it opens the ledger, and the history's header of B against the catchpoint, whose blocks chain to the labelled one |
| the history at the catchpoint's balances round B | the node compares the history's header and totals of round B with the catchpoint's (the catchpoint's header of B is hash-chained to the labelled block) before it swaps anything; a history that does not fit aborts the catchup and leaves the node as it was |
| the rounds after B | the node replays them from blocks and compares each one the history already holds: header, expired stake, the digest of the round's changes and the stored value of every object the round changed. A difference, or a record that can't be decoded, marks the history untrusted, durably: the node keeps running, but serves nothing from the history (`?round=` and `/versions` answer 503; old deltas, past-round box listings and simulations fail) until the store is replaced, or `ttsnap verify -clear-diverged` clears the mark. A round that can't be read is not served until a restart verifies it again. The node serves no history for rounds it hasn't reached yet |
| the rounds before B | attested by the producer, and so are the values of objects last changed before B (the totals at B are checked, the other objects at B are not yet). Records the producer added to rounds the node replays, beyond what each round changed, are not detected either. `ttsnap verify -deltas <archival algod> -sample N` compares N random rounds with an archival node's `/v2/deltas` |

A full snapshot (`-full`) also carries the producer's tracker and block DBs, so the node starts without a catchpoint. Its account state is the producer's and nothing checks it: `fetch -full` needs `-trust-producer`. Use it between your own nodes.

## New node

1. Install the `dtalgorand` package (see [Starting the node](../../installer/dtalgorand/README.md#starting-the-node)) or build algod, goal and ttsnap from this fork. Keep `EnableTimeTravel: true` and `Archival: false`; set `TimeTravelDBDir` now if the history goes on another disk.
2. Download and install the latest history snapshot into the data directory. The node must not have a history yet.

   ```sh
   sudo -u dtalgorand ttsnap fetch -url https://<snapshot host> -d /var/lib/dtalgorand
   ```

   Downloads resume when rerun; files land next to their targets (`*.ttsnap-partial`) and move into place once all of them check out and the history's headers chain from genesis (a few minutes on mainnet). A rerun of an install that stopped while moving files finishes it. `-keys` and `-signer` select other producers' keys. The latest pointer is not signed, so a mirror could keep serving an old one: `fetch` refuses a latest snapshot older than `-max-age` (7 days); `-manifest` installs a given one.
3. Start algod and catch up from the network's latest catchpoint (or `-label <round>#<hash>`):

   ```sh
   sudo systemctl start dtalgorand
   sudo -u dtalgorand ttsnap catchup -d /var/lib/dtalgorand
   ```

   The catchpoint's balances round (its round − 320) must be at most the history's last round. A snapshot published after the catchpoint always is; if the latest catchpoint is newer than the snapshot, wait for the next snapshot or pass an older label. `goal node catchup <label>` works the same way; algod refuses a catchpoint the history does not reach.
4. Optionally, with algod stopped, check the headers (minutes on mainnet) and sample rounds against an archival node:

   ```sh
   ttsnap verify -d /var/lib/dtalgorand -deltas https://mainnet-api.4160.nodely.dev -sample 1000
   ```

## Producer

The producer is a time travel node whose genesis directory, history included, lives in one btrfs subvolume, so that one snapshot covers the tracker DB, the blocks and the history at the same instant. Taken while algod runs, a snapshot opens like a ledger after a crash: blocks are written before the trackers commit them, and the history is synced before the tracker DB commits. `TestTimeTravelSnapshotSoak` (ledger package, `TT_SNAPSHOT_SOAK=N`) copies a running ledger N times at random instants and checks that every copy opens and answers like the original.

1. Move the genesis directory onto a btrfs image laid out for snapshots ([scripts/btrfs_hot_ledger.sh](../../scripts/btrfs_hot_ledger.sh)): the ledger in an `@live` subvolume bind-mounted on the genesis directory, snapshots in `@snap` next to it. An image from the script's earlier layout is converted in place.
2. Make a signing key, and give its public key to the consumers (in the package's `allowed_signers`):

   ```sh
   ssh-keygen -t ed25519 -N '' -C snapshots@dtalgorand -f /root/ttsnap_key
   echo "snapshots@dtalgorand namespaces=\"dtalgorand-ttsnap\" $(cat /root/ttsnap_key.pub)"
   ```
3. Run [scripts/dt_snapshot.sh](../../scripts/dt_snapshot.sh) from a timer, e.g. hourly: it snapshots `@live`, runs `ttsnap prepare` and `ttsnap publish` on the snapshot, keeps a few snapshots and garbage-collects the tree. Serve `OUT` as static files (anything that answers range requests), or copy it to a bucket with `UPLOAD`: blobs first, then the manifests, then the latest pointers, and deletions last, so that a reader never finds a manifest whose blobs aren't there yet. Manifests and blobs never change once written; only the pointers do.

   ```sh
   OUT=/srv/snapshots KEY=/root/ttsnap_key \
   UPLOAD='rclone copy "$OUT/blobs" r2:snapshots/blobs && rclone copy --exclude "latest-*" "$OUT/snapshots" r2:snapshots/snapshots && rclone copy "$OUT/snapshots" r2:snapshots/snapshots && rclone sync "$OUT" r2:snapshots' \
   scripts/dt_snapshot.sh
   ```

The commands it runs:

- `ttsnap prepare [-full] <genesis dir>` takes a writable snapshot (never a running node's directory): it opens the history, which replays the store's WAL, flushes it to tables and deletes obsolete files, and folds the SQLite WALs into their DBs. It refuses a history that doesn't start at genesis, whose box index is incomplete, or (`-full`) whose DBs are not of one instant.
- `ttsnap publish -o <out> -key <key> [-full] <genesis dir>` copies every file to `<out>/blobs/<sha256>`, unless it is there already (most of the history's tables are, from earlier snapshots), then writes the signed manifest `<out>/snapshots/<genesis id>/<kind>-<round>.json` and points `latest-<kind>` at it. It refuses a snapshot older than the latest one.
- `ttsnap gc -o <out> -keep N` keeps the newest N snapshots of each kind and deletes the blobs no manifest lists.
- `ttsnap info [-full] <genesis dir>` prints what a genesis directory holds.

The long commands (`prepare`, `publish`, `fetch`, `verify`) report progress on stderr: what they are doing and, while they copy, download or check, how far they are every 10 seconds (with the rate and about how long is left where they can tell). Results stay on stdout.

## Snapshot tree

```
blobs/<sha256[:2]>/<sha256>                    every file, by content
snapshots/<genesis id>/history-<round>.json    manifest (+ .sig), never rewritten
snapshots/<genesis id>/latest-history          the name of the latest manifest
snapshots/<genesis id>/full-<round>.json       full snapshots, if published (latest-full)
```

A manifest lists the network, the history's rounds, last header hash, box index start and Pebble format, and each file's path, size and SHA-256. Paths are limited to the history store's file names (and for a full snapshot the tracker and block DBs); `fetch` refuses anything else, and decides where each file goes from the local `config.json` alone.
