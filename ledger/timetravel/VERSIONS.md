# Object versions API (design)

Status: implemented on branch `timetravel-versions` (see [Implementation](#implementation)). Resolved the README TODO "resource endpoints to expose the rounds at which they change".

`?round=R` answers "what was X at R". This adds "when did X change, and to what": a paginated list of an object's versions over a round range.

## What a version is

The store writes one record per object per round in which the object appears in the round's state delta, except for the [no-op writes](README.md#no-op-writes) it skips. A **version** of an object is a round at which its stored value changed:

- A write of identical bytes is not a version (deltas can carry no-op writes). The scan compares each record with its predecessor, which it already holds.
- A deletion is a version with `deleted: true`: account closed, holding opted out, local state cleared, asset/app destroyed, box deleted. Creation is the first version after a tombstone or no record.
- Derived fields are not versions. An account's `amount` includes pending rewards, which move with the header's rewards level without the stored record changing. Each version reports the object exactly as `GET ...?round=<version round>` would (rewards at that round), and the docs say rewards can accrue between versions.
- Composite resources (`/v2/accounts/{addr}`) version whenever any constituent changes: base record, holdings, local states, created-asset/app index, created-asset/app params. Defined exactly: a round R is a version iff `GET /v2/accounts/{addr}?round=R&exclude=X` differs from round R−1, ignoring pending rewards.

History at or below `Latest()` is immutable (no forks), so version lists are append-only: pagination tokens are stable across new blocks, and pages with an explicit `max-round` are cacheable forever.

## The shapes compared

Costs are for this store: an object's versions are adjacent records `c obj round`, one iterator scans them in order, and each record carries the full value, so the server holds the values whether or not it returns them. A 128 KB block holds ~50 app-param versions (2.5 KB avg) or ~1,000 account versions; a cold block read costs ~0.3 ms, a warm seek ~5 µs.

| shape | leaf object (asset, app, box, holding) | composite account | client work | verdict |
|---|---|---|---|---|
| `rounds[]` only | one range scan, smallest payload | one merged scan | N+1: one `?round=R` per version; for a composite each of those re-materializes the whole account (O(versions × resources)) | as the default for leaves, yes; as the only shape, no |
| entire states | same scan, values already in hand: no extra reads | each version repeats every unchanged holding: quadratic payload | none | yes for leaves (`include=values`); no for composites |
| base + byte/JSON diffs | diffing server-side, or computing msgpack patches of the stored versions | JSON Patch indexes arrays (`assets`, global state are arrays) so indices shift; Merge Patch can't delete array elements | apply a diff format | no |
| base + **object-granular patches** | same as entire states (the leaf is the object) | per round, only the constituents that changed, each in full | replace sub-objects by id | yes for composites |

## Recommendation

1. **Sub-resource endpoints `.../versions`**, not a `?versions` flag on the existing endpoints: the response schema differs, and generated clients can't type a schema that switches on a query flag.
2. **Rounds by default, `include=values` for full values.** Every version entry always carries `round`, `timestamp` and `deleted`; `include=values` adds the new value. For a leaf this is "entire states" at no extra read cost.
3. **The account is base + patches, where a patch is a whole sub-object.** An account version lists which constituents changed (ids always, new values with `include=values`). The base is the existing `GET /v2/accounts/{addr}?round=<min-round>`; immutability makes the two requests consistent without a snapshot token. No composite full states.
4. **The per-resource account endpoints are filters of the composite.** `/v2/accounts/{addr}/assets/{id}/versions` returns the same `AccountVersion` entries restricted to that asset's holding and (if the account created it) its params, matching what `/v2/accounts/{addr}/assets/{id}` returns.
5. **No diffs in v1.** Global-state diffs (in the existing `EvalDelta` format) are the one diff worth adding later, because app params are 82% of history and each full version repeats the programs.

## Endpoints

Registered only when `EnableTimeTravel` is on (absent otherwise, so 404).

| endpoint | objects | entry |
|---|---|---|
| `GET /v2/accounts/{address}/versions` | `a addr`, `h addr *`, `l addr *`, `C addr *`, `A id`/`P id` of created ids; narrowed by `exclude` | `AccountVersion` |
| `GET /v2/accounts/{address}/assets/{asset-id}/versions` | `h addr id`, `A id` if creator | `AccountVersion` |
| `GET /v2/accounts/{address}/applications/{application-id}/versions` | `l addr id`, `P id` if creator | `AccountVersion` |
| `GET /v2/assets/{asset-id}/versions` | `A id` | `AssetVersion` |
| `GET /v2/applications/{application-id}/versions` | `P id` | `ApplicationVersion` |
| `GET /v2/applications/{application-id}/box/versions?name=` | `k` key | `BoxVersion` |
| `GET /v2/applications/{application-id}/boxes/versions` | the `b app` box index, then each entry's `k` record | `BoxesVersion` |

Not versioned: `/v2/ledger/supply` (totals change nearly every round; `?round=` covers it), blocks.

## Parameters

| param | meaning |
|---|---|
| `min-round`, `max-round` | inclusive range; defaults 0 and `Latest()`. `max-round > Latest()` or `min-round > max-round`, also once `next` is applied: 400. Names follow the indexer. |
| `order` | `asc` (default) or `desc`. Both are one iterator direction in Pebble. Entries carry new full values, not diffs, so `desc` needs nothing extra ("last 10 changes of X"). |
| `limit` | versions (rounds) per page; default 100, max 10,000, or 1,000 with `include=values`. |
| `next` | the previous page's `next-token`: the round to resume from, opaque to clients. |
| `include` | `values`: include new values. |
| `exclude` | account versions only; same values as `/v2/accounts/{address}`: `all` (base record only), `none`, `created-apps-params`, `created-assets-params` (created ids still version on create/destroy, but params changes don't). |

A page ends at `limit` versions, or when the stored (msgpack) values reach the 1 MB budget box pages use (`maxBoxFetchBytes`; the JSON is larger), whichever is first; `next-token` is present iff more remain. A round is never split across pages. The top-level `round` is the effective `max-round`: on a descending page resumed from `next`, the token's round. Responses are JSON only, like the other paginated endpoints (`/v2/accounts/{address}/assets`).

## Response shapes

Leaf (`/v2/applications/1002/versions?min-round=100&limit=3`):

```json
{
  "round": 4500000,
  "versions": [
    { "round": 120, "timestamp": 1700000123, "deleted": false },
    { "round": 131, "timestamp": 1700000170, "deleted": false },
    { "round": 140, "timestamp": 1700000201, "deleted": true }
  ],
  "next-token": "141"
}
```

With `include=values`, each non-deleted entry adds `"application": {...}` (the `Application` model, as `GET /v2/applications/1002?round=120` returns it). `AssetVersion` adds `asset`, `BoxVersion` adds `value` (base64).

App boxes (`/v2/applications/1002/boxes/versions?include=values`): each entry is a round in which any box of the app changed, with the boxes that did, in name order.

```json
{
  "round": 4500000,
  "versions": [
    { "round": 120, "timestamp": 1700000123, "boxes": [ { "name": "Yg==", "deleted": false, "value": "djE=" }, { "name": "Yw==", "deleted": true } ] }
  ],
  "next-token": "121"
}
```

Account (`/v2/accounts/ABC.../versions?include=values`): each entry lists only what changed at that round.

```json
{
  "round": 4500000,
  "versions": [
    {
      "round": 120,
      "timestamp": 1700000123,
      "base": { "deleted": false, "account": { "address": "ABC...", "amount": 5000000, "round": 120, "...": "Account fields as with exclude=all" } },
      "assets": [ { "asset-id": 31566704, "deleted": false, "asset-holding": { "amount": 10, "is-frozen": false } } ],
      "apps-local-state": [ { "app-id": 1002, "deleted": true } ],
      "created-apps": [ { "app-id": 1003, "deleted": false, "application": { "...": "Application model" } } ]
    }
  ],
  "next-token": "121"
}
```

Without `include=values` the same entries carry ids and `deleted` only (`"base": {"deleted": false}` says the base record changed). Lists that didn't change are omitted. Reconstructing the account at any R in [`min-round`, `max-round`]: fetch `?round=min-round`, apply every entry with `min-round < round ≤ R`, replacing sub-objects by id.

## Implementation

1. **Store** (`ledger/timetravel/store.go`): `Store.Versions(obj, lo, hi, desc, fn)`: one iterator over `c obj [0, hi]`, positioned at the record before `lo` (ascending) or walking back past `lo` (descending) so the first in-range record can be compared with its predecessor; emits only value changes. `Store.Objects(prefix, fn)` lists every object that ever had a record under a prefix, deleted ones included (the object loop of `Scan`, shared). `TestStoreModel` checks both against the in-memory model, with no-op writes in the history.
2. **Ledger** (`ledger/timetravel_versions.go`): `histView.Versions`/`Objects` merge the rounds still in memory; `AccountVersions`, `AccountAssetVersions`, `AccountAppVersions`, `AssetVersions`, `AppVersions`, `KvVersions`. A page is built in two passes: the first scans each object's versions, keeping only the first `Limit + 1` rounds in page order (a heap evicts the last one, and an object's scan stops once it passes them), recording which objects changed in each; the second reads the values of only the page's versions, applying the byte budget, and decodes them. Memory stays bounded by the page whatever the history. `TestTimeTravelVersions` checks every lookup against an oracle built from the per-round lookups (`LookupAsset` etc. at every round), with the last rounds only in memory and again after they reach the store, for full ranges, sub-ranges, page sizes 1-3 both ways, and a 1-byte budget.
3. **REST** (`daemon/algod/api/server/v2/timetravel_versions.go`): the six endpoints, tag `timetravel`, generated package `generated/timetravel` registered in `router.go` iff `EnableTimeTravel`. `basicAccountInformation`'s conversion is now `baseAccount`, shared with the account versions. `algod.oas3.yml` was converted with a local `swaggerapi/swagger-converter` container (it reproduces the committed file byte for byte from the old spec) instead of the Makefile's public converter. Handler tests: `test/timetravel_versions_test.go`.
4. **End to end**: `tools/ttlab demo` checks the endpoints on a dev-mode node against the live state it recorded at every round (app, box and account versions with values, paging both ways, the creator's composite versions).

5. **App boxes**: merging every box an app ever had (option A below) costs two seeks per box per page, and the busiest mainnet box apps churn through a different name on nearly every write (~10–20 M boxes for app 1264162043, with 2 live). So boxes get a secondary index, round-major per app (`b` app round name, see the [README](README.md#box-index)), and `Store.BoxChanges` reads a range of it, checking each entry against the box's records to drop no-ops. `histView.BoxChanges` merges the rounds in memory; `Ledger.AppBoxVersions` groups the changes by round, never splitting one, under the same `Limit` and byte budget. `TestStoreModel` and `TestStoreIndexBoxes` check the index (with no-op writes, a backfill, a store extended without the index), `TestTimeTravelAppBoxVersions` against `LookupKv` at every round with box churn (created and deleted in adjacent rounds and within one).

   | option | read cost per page | extra storage (mainnet, est.) | verdict |
   |---|---|---|---|
   | A. merge the app's boxes (`Objects` + `versions`) | O(boxes ever) seeks | 0 | unusable for churning apps |
   | B. **index entry per box write** | 1 seek + the page's entries + 1 seek each | ~1–3 GB | implemented |
   | C. index entry per (app, round) with packed names | same as B | ~0.8–2 GB | small saving, the backfill must group names |
   | D. re-key boxes round-major | good for this endpoint | 0 | breaks one-seek `?round=` box reads and listings |
   | E. values in the index | saves a ~5 µs seek per entry | +~1.8 GB | not worth it |

Not done: the `tools/ttlab versions` measurements (hot app params, accounts with 1k and 10k holdings, warm and cold), and the box index's size and page latency on fnet after `ttimport -index-boxes`.

## Later, if needed

- `include=state-delta` on app and local-state versions: global/local state changes against the previous version in the `EvalDelta` format clients already decode (action 1/2/3), with a flag when anything else (programs, schema) changed. The server already holds both adjacent values. Plus `key=` to keep only versions where a given global key changed.
- A per-account change index, if composite pages for accounts with very many resources prove too slow: the box index's pattern (`L` addr round → changed constituent keys, written blind in `AddRound`, entries checked against the records on read, rebuilt by one pass over the store, a meta record saying from which round it is complete). Costs an extra record per touched account per round.

## Non-goals

- "What changed in round R" across all objects: the store is object-major, so this needs the round's delta; `/v2/deltas/{R}` covers rounds whose block is stored.
- Supply/totals versions, and history before the store's first round (it starts at genesis).
