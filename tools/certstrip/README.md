# certstrip

Agreement certificates are most of an archival node's block DB: 48–77% of its bytes on mainnet (about 20–67 KB per round, ≥1.5 TB in all). Blocks are needed to answer for history; certificates only prove to a peer syncing from genesis that each block was agreed on. An archival node can drop them with `CertificateRetentionRounds` and keep serving every block through its REST API.

`certstrip` is for the case where the archive came from someone else: a ready-made archival snapshot (e.g. [Nodely's](https://nodely.io/docs/public/snapshots/)), which has no checksums or signatures. It checks the snapshot against the network before the certificates go, compacts the block DB once they are gone, and can do the stripping offline without checking anything.

## `CertificateRetentionRounds`

Set in the node's `config.json`, with `Archival`:

```json
{ "Archival": true, "CertificateRetentionRounds": 100000 }
```

- `0`, the default, keeps every certificate.
- Otherwise the node discards the certificate of every block more than that many rounds behind its latest round. The first start with it set strips the existing archive in the background, bottom up in small transactions (hours on mainnet; the log reports progress), then one round per new block.
- Values below 25,000 (or twice `CatchpointInterval` plus the catchpoint lookback, if more) are refused at startup. That keeps the certificates that peers doing a catchpoint catchup ask for.
- A discarded certificate can't be brought back: setting the option back to 0 stops the stripping, it doesn't refetch anything.

What changes for others:

- **Peers** fetching a stripped round get "no block for this round", over HTTP (404, or a redirect to `BlockServiceCustomFallbackEndpoints` if set: point it at a full archiver) and over the WebSocket block service. They move on to another peer, as for a round past the tip, rather than ranking the node as one that sends bad blocks.
- **P2P**: the node no longer advertises the `archival` capability, so peers looking for an archiver don't find it.
- **REST**: `GET /v2/blocks/{round}?format=msgpack` returns stripped rounds with an empty certificate. JSON, header-only and every other endpoint are unchanged.

Old certificates are needed by nodes that catch up block by block from further back than the retention: archival nodes syncing from genesis, and any node that was off for longer than the retention (about 19 hours at 25,000 rounds on mainnet). They ask other peers; archival nodes find archivers through the `_archive` DNS SRV records and the P2P capability. Catchpoint catchup only needs recent rounds.

**Disk space**: stripping in place leaves the freed pages inside the block DB, where new blocks reuse them for months; the file doesn't shrink. To get the space back, stop the node and run `certstrip compact` (below), or `VACUUM INTO` a new file and swap it in. A plain `VACUUM` in WAL mode needs about twice the DB's size free.

## Trusting a snapshot

Everything in a snapshot is untrusted. `certstrip` has two ways to check it.

**Label mode** (the default): one catchpoint label that the network agrees on authenticates the tracker DB and, through the hash chain, every block below it. A catchpoint label for round R digests that round's block hash, the balances trie root (accounts, assets, apps, boxes), the totals, the state proof verification data and the online stake. So if the node's own label for R equals the network's, its header for R is the network's; each header holds the hash of the one before (`Branch`), and each header commits to its block's transactions and their results (`TxnCommitments`: IDs, signatures and ApplyData). Old certificate signatures sign those same header hashes, so they add nothing once the chain is anchored. It needs the node to catch up from the network to its next catchpoint: up to `CatchpointInterval` (10,000) rounds, plus the 320-round catchpoint lookback on a node whose catchpoints were just reset.

**`-full`**: replay the chain from genesis into a scratch ledger, checking every certificate against the online stake it rebuilds. It needs nothing but `genesis.json`, but it takes days on mainnet: the online stake of old rounds exists nowhere in a snapshot. It needs the certificates, so it can't run once they're stripped.

Both rest only on SHA-512/256 collision resistance. Neither re-checks that the transactions followed the protocol's rules; what matters is that this is the history the network agreed on.

## Restoring a snapshot, checked

1. Download and unpack the snapshot into an algod data directory (Nodely's are split archives of the genesis directory: `cat <name>.tar.zst.* | zstd -d | tar x`), with the network's `genesis.json` and a `config.json` with `"Archival": true`. Don't start algod yet.

2. Walk the block DB. This reads every header and block (0.5–1 TB on mainnet; disk-bound) and stops at the first bad round. Interrupted, it resumes where it stopped.

   ```
   certstrip verify-chain -d <data dir>
   ```

   It checks, for every round from genesis: the round is the previous one plus one; the block and header blobs are canonically encoded (byte for byte what they decode to, since the node serves the stored bytes); the header column is the block's header (the ledger reads headers from it alone); the round and protocol columns match; the payset matches the header's commitments; the header follows the previous one (`Branch`, upgrade state, timestamps…); round 0 is the block `genesis.json` makes. It records the walk's tip and its hash in the state directory (below).

3. Reset the tracker state that a label wouldn't catch:

   ```
   certstrip reset-trie -d <data dir>
   ```

   It clears the balances trie, so that algod rebuilds it from the accounts at startup: a snapshot with doctored accounts and the original trie would otherwise produce the network's label. It rebuilds the creatables index from the accounts and the txtail from the walked blocks, since the label covers neither. It deletes the snapshot's catchpoint files and records, which the node would otherwise serve, and its `crash.sqlite` and `stateproof.sqlite`, which belong to another node. It drops every trigger and view of the block and tracker DBs, which could rewrite checked rows whenever algod writes, and any catchpoint catchup in progress (its state and staging tables, and the block DB's `catchpointblocks`), which algod would resume at startup and finish by swapping in unchecked blocks and accounts. It refuses a time travel store, which no label covers. It lists any participation keys (`*.partkey`, `partregistry.sqlite`) it finds without removing them: a snapshot of a participating node may carry its keys, and voting with them would equivocate; remove them unless they're yours.

4. Start algod (`CertificateRetentionRounds` still 0, catchpoints tracked as an archival node does by default). It rebuilds the trie (likely tens of minutes on mainnet), catches up from the network, checking certificates as usual, and makes a catchpoint at the next multiple of `CatchpointInterval`. Its `last-catchpoint` in `/v2/status` (or `goal node status`) shows the label.

5. Get the network's label for that round from two independent sources or more, e.g. Algorand's published labels (`https://algorand-catchpoints.s3.us-east-2.amazonaws.com/channel/<network>/latest.catchpoint`, which holds the latest only) and `last-catchpoint` of a node you run or of a provider other than the snapshot's. Then:

   ```
   certstrip anchor -d <data dir> -label 'algorand-s3=<round>#<hash>' -label 'my-node=<round>#<hash>'
   ```

   Each `-label` names its source; the names are recorded with the anchor, and `certstrip` can't check that they are independent, so that part is up to you. Run it while that is still the node's last catchpoint: the node keeps only its last label, and makes a new one every `CatchpointInterval` rounds (about 8 hours on mainnet). It refuses labels that differ, a label for a round not past the reset, and a node label that differs from the network's. It then checks that the blocks algod added link the walked tip to the labelled round, and records the anchored round. The node can keep running. `certstrip` itself makes no network requests.

6. Strip: set `CertificateRetentionRounds` and restart algod, which strips in the background. Or, with algod stopped, write a compacted copy and swap it in:

   ```
   certstrip compact -d <data dir> -retention 100000 -replace
   ```

   `compact` refuses to strip past the anchored round. It needs free space for the copy; the old block DB is left as `ledger.block.sqlite.old` to delete once algod runs fine.

After step 5, the tracker state at the anchored round, every block up to it, and everything algod builds on them are the network's. Blocks after it come from the network through catchup.

### State files

`certstrip` records what it checked (the walk's tip and hash, the reset round, the anchored round) in JSON files under `$XDG_STATE_HOME/certstrip` (default `~/.local/state/certstrip`, `-state-dir` to change it), in a directory named after the network and the block DB's path. Never in the data directory: that came with the snapshot, which could carry files claiming checks that never ran. Run every step as the same user, and as the user that runs algod: the commands that write into the data directory (`reset-trie`, `compact`, `-replace-tracker`) refuse to run as root on a directory another user owns.

## Stripping without checking

Checking is optional. To strip a snapshot's certificates and keep whatever trust it had:

- set `CertificateRetentionRounds` on the node, or
- `certstrip compact -unverified` with algod stopped, to strip and reclaim the space in one pass, or
- by hand with algod stopped: `UPDATE blocks SET certdata = NULL WHERE rnd BETWEEN :lo AND :hi`, in ascending batches from round 1. Keep the stripped rounds one run from round 1 up: the node finds where stripping stopped by binary search. (It also samples for holes below that point and, finding one, scans for the first certificate once.)

Label mode still works after stripping: the walk reads only headers and blocks. `-full` doesn't.

## `-full`

```
certstrip verify-chain -full -d <data dir> -work <scratch dir> [-replace-tracker]
```

Replays every round into a non-archival ledger under `-work`: the walk's checks, then the certificate against the scratch ledger's stake (its votes checked in parallel, a couple of rounds ahead of the ledger, as catchup does), then adding the block. It stops at the first failure with the round and reason, and resumes from the scratch ledger after an interruption. Once it reaches the tip, `-replace-tracker` (algod stopped) installs the rebuilt tracker DB in place of the snapshot's, which is kept as `.bak`, and removes the snapshot's catchpoints, `crash.sqlite` and `stateproof.sqlite`. It records the walk as a full one, which lets `compact` strip without an anchor.

## Not covered

- The label doesn't cover the creatables index or the txtail; `reset-trie` rebuilds both from data that is.
- The certificates the node keeps (the last `CertificateRetentionRounds`) aren't checked in label mode for rounds that came with the snapshot; only those algod fetched itself were. Peers check every certificate they fetch, so a forged one can't mislead them; it only makes them rank this node as a bad peer. `-full` checks them all.
- `verify-chain` doesn't check certificates, and `-full` doesn't re-validate transactions (signatures, ApplyData): it applies certified blocks as they are, like `ledger.AddBlock`.
- A snapshot's tar parts can't be checked before unpacking: Nodely publishes no checksums.
