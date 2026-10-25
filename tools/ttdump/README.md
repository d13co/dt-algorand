# ttdump

`ttdump` explores the [time travel](../../ledger/timetravel/README.md) store of a node: any object at any round, an object's versions with what changed between them, listings at past rounds, block headers, and the store's raw records and statistics.

It opens the store read-only, but Pebble locks the store while it is open, so **algod must be stopped** (and algod cannot start while ttdump runs). On a running node, goal's `--round` flags and `versions` commands ask algod instead.

```sh
ttdump -d ~/tt info                       # a data directory: finds the store as algod does
ttdump -store /fast/fnet-v1/ledger.timetravel info   # or the store itself
```

With `-d`, ttdump also loads the network's `consensus.json`, as algod does. Copying the store of a running node gives an inconsistent copy; stop the node first.

The store of a stopped node ends at the round of the node's last account DB commit, which trails the node's last round by at least `MaxAcctLookback` (4) rounds and up to a few seconds' worth: algod writes the rounds after it again when it restarts. `info` shows the store's last round.

## Objects

Commands name objects by a spec, and print objects in the same form, so any object in the output can be pasted into the next command.

| spec | object |
|---|---|
| `acct:ADDR`, or just `ADDR` | an account's base record |
| `holding:ADDR:ASSET` | an asset holding |
| `local:ADDR:APP` | an app local state |
| `asset:ID`, `app:ID` | asset or app params (an app's global state included) |
| `box:APP:NAME` | a box; NAME in goal's app argument form: `str:`, `b64:`, `int:`, `addr:`, ... |
| `kv:NAME` | a kv entry by its raw key, in the same form |
| `totals` | the account totals |
| `created:ADDR:asset:ID`, `created:ADDR:app:ID` | a creator index entry |
| `hex:HEX` | any object key |

## Output

Text by default. `-json` (before or after the command) prints what algod's REST API returns for the same question, one JSON document, or one per line for listings and versions:

- `get` and `ls` values: `model.Account` (the base record, as `GET /v2/accounts/{addr}?exclude=all&round=R`), `AssetHolding`, `ApplicationLocalState`, `Asset`, `Application`, `Box`;
- `account`: `GET /v2/accounts/{addr}?round=R`;
- `versions`: the items of the `/versions` endpoints with `include=values`.

The totals, which `/v2/ledger/supply` cannot show in full, are `ledgercore.AccountTotals` in codec JSON, and headers are codec JSON as in `/v2/blocks/{R}?header-only=true`.

In text, values are flattened to paths: nested fields join with `.`, holdings and created assets or apps are keyed by id (`assets[31566704].amount`), and global and local state by key (`global-state[str:counter]`). Byte values print as `str:` when printable, `addr:` when 32 bytes, `b64:` otherwise, and as a length and hash prefix above 128 bytes.

## Commands

Rounds default to the store's last round. A round past it, or before the store's first round, is an error.

### info

```
$ ttdump -d ~/tt info
store        /home/me/tt/fnet-v1/ledger.timetravel
rounds       0..25314401 (2019-...  .. 2026-...)
checkpoints  every 1 rounds
disk         3.21 GiB
box index    complete
holders      no index (TimeTravelHoldersIndex, ttimport -holders-index)

estimated disk use (sstables only, not the WAL or memtables):
KEYSPACE  KIND  CONTENTS              DISK
E               expired online stake  ...
H               headers and digests   ...
a               asset holders index   ...
b               box index             ...
c         A     asset params          ...
...
```

Fast: the per-keyspace figures are Pebble's estimates, no scan. `box index` says from which round the [box index](../../ledger/timetravel/README.md#box-index) is complete, when it does not cover the whole store (`ttimport -index-boxes` builds the rest). `holders` is the state of the [asset holders index](../../ledger/timetravel/README.md#holders-index): complete, stale (a build without it added rounds, its meta record is bad, or the store's N isn't 1) or missing (`ttimport -holders-index` builds it); the JSON field `holders-index` says which.

### get OBJECT [-r R]

The object at round R and the round it last changed, or when it was deleted or will be created:

```
$ ttdump -d ~/tt get app:1002 -r 6
app:1002 at round 6 (2023-11-14 22:13:38): changed at round 4 (2023-11-14 22:13:32)
  id                                  1002
  approval-program                    b64:BoEB
  creator                             AEAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAKE3PRHE
  global-state[str:n]                 2
  ...
$ ttdump -d ~/tt get box:1002:str:b -r 2
box:1002:str:b at round 2 (2023-11-14 22:13:26): absent; created at round 3
```

Writes that left an object unchanged are not versions, so "changed at" is the round its value last changed. On a store written before the ledger skipped [no-op writes](../../ledger/timetravel/README.md#no-op-writes), finding that round walks back over them; `ttimport -dedup` removes them.

### account ADDR [-r R] [-exclude all|created-apps-params|created-assets-params]

The account with its holdings, local states and created assets and apps, as `GET /v2/accounts/{addr}?round=R`, pending rewards included. ttdump has no `MaxAPIResourcesPerAccount` limit.

### versions OBJECT [-from R] [-to R] [-desc] [-n N] [-full] [-exclude ...]

Each version: a line with its round, time, object and what happened, then the fields that changed since that object's previous value (the first version diffs against the value at `-from − 1`). For an account, the versions of everything `account` shows; `-exclude` narrows them as for `account`.

```
$ ttdump -d ~/tt versions acct:AIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGFFWAF4 -from 5
5  2023-11-14 22:13:35  holding:AIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGFFWAF4:1001 updated
  amount  0 -> 5
6  2023-11-14 22:13:38  acct:AIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGFFWAF4 updated
  total-apps-opted-in    0 -> 1
  total-assets-opted-in  1 -> 0
6  2023-11-14 22:13:38  holding:AIAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAGFFWAF4:1001 deleted
```

`boxes:APP` lists the changes of every box of the app, round by round, from the store's [box index](../../ledger/timetravel/README.md#box-index), as `/v2/applications/{id}/boxes/versions` does.

`-full` (and `-desc`, newest first) prints whole values instead. The totals, creator index entries and `hex:` objects, which the REST API has no versions of, list the store's versions as they are.

### ls WHAT [-r R] [-ever] [-after OBJECT] [-n N]

`accounts`, `assets`, `apps`, `boxes APP`, `kv [PREFIX]`, `holdings ADDR`, `locals ADDR` or `created ADDR`: the objects that exist at round R, a table with a few columns per kind. At most `-n` (1000; 0 for all); a truncated listing ends with the `-after` that continues it, on stderr.

`-ever` lists every object with a record at any round, deleted ones included, with the rounds of its first and last records, the number of records, and whether it exists at the last round. With a checkpoint interval above 1, checkpoint copies count as records.

### header R [R2]

One round: the header, the digest of the round's changes, and the expired online stake recorded with it. A range: a table of round, time, protocol, transaction counter, digest and expired stake.

### stats [-kind K] [-top N]

Scans every versioned record (`-kind` limits it to one kind: `a h l A P k T C`) and prints, per keyspace and kind: records, objects, tombstones, no-op records (a record equal to the object's previous one; with a checkpoint interval of 1 only), key and value bytes, and the most records of one object. Then the `-top` (10) objects with the most records. A progress line goes to stderr every 10 seconds; on mainnet, expect it to take a while.

### raw [-prefix P] [-after HEX] [-n N]

The records in key order, at most `-n` (100): key, keyspace, object, round, tag (`F` full value, `D` deleted, `X` patch), value length and its first bytes, or for an asset holders index entry the rounds at which its holding was created and deleted. `-prefix` is an object (its records in both versioned keyspaces), a keyspace (`a` asset holders index, `b` box index, `c` checkpoints, `p` patches, `r` round index, `H` headers, `E` expired stake, `m` meta) or `hex:` a raw key prefix. The store's layout is in [ledger/timetravel/store.go](../../ledger/timetravel/store.go).
