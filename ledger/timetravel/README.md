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
- `tools/ttdump` explores the store of a stopped node: any object at any round, its versions with the changes between them, listings at past rounds, headers, and the raw records and their statistics. See [tools/ttdump/README.md](../../tools/ttdump/README.md).
- `TimeTravelCheckpointInterval` (N) is fixed when the store is created. 1 (the default) is the recommended setting; see [Patches and N](#patches-and-n).

### REST

Read endpoints take `?round=R` (the `at-round` parameter in the API spec) and answer as of the end of round R (responses that report a round say R):

| endpoint | at round R |
|---|---|
| `GET /v2/accounts/{addr}` (incl. `exclude=`), `/assets`, `/applications`, `/assets/{id}`, `/applications/{id}` | account, resources, pending rewards at R |
| `GET /v2/assets/{id}`, `GET /v2/applications/{id}` | params (and global state) at R |
| `GET /v2/applications/{id}/box?name=`, `/boxes`¹ | box value / box listing at R |
| `GET /v2/ledger/supply` | totals at R |
| `GET /v2/blocks/{R}?header-only=true` | header of any round (no `?round=` needed) |
| `POST /v2/transactions/simulate` with `"round": R` in the request | evaluates the group as if it were in block R+1, projecting every object it touches, including unnamed resources (`allow-unnamed-resources`), from the history. Block headers after R can't be read, as on chain, where block R+1 isn't there yet: a heartbeat with `first-valid` R+1 fails on its seed |

¹ `/boxes` takes stock algod's own `round` parameter, not `at-round`: a node without time travel serves it for the rounds it still holds in memory (`MaxAcctLookback`) instead of rejecting it, and a time travel node for every round.

Not served for pruned rounds (the price of `Archival: false`): full blocks (`/v2/blocks/{R}` without `header-only`), `/v2/blocks/{R}/hash`, `/txids`, `/logs`, `/transactions/{txid}/proof`, `/lightheader/proof`, and `/v2/stateproofs/{R}`, which read the full blocks after R. `goal clerk simulate --round R` works unchanged.

Object histories (`timetravel` tag in the API spec, registered only with `EnableTimeTravel`): `GET .../versions` lists the rounds at which an object's stored value changed, paginated, with the new values on `include=values`. A write of an identical value is not a version; a deletion is (`deleted: true`). Pending rewards are not versions: each version's account is the account at that round, as `?round=` returns it. On the app state endpoints (the account, its local state of an app, the app's params), `include=keys` lists on each app params or local state version the global or local state keys that changed against the round before, net over the round: a key set and deleted again within the round isn't listed. See [VERSIONS.md](VERSIONS.md) for the design.

| endpoint | versions of |
|---|---|
| `GET /v2/accounts/{addr}/versions` | everything `/v2/accounts/{addr}` returns: base record, holdings, local states, created assets' and apps' params (`exclude=all`, `created-apps-params`, `created-assets-params` narrow it as they do there). Each version lists only the objects that changed in that round, so `?round=min-round` plus the versions gives the account at any round of the range |
| `GET /v2/accounts/{addr}/assets/{id}/versions`, `/applications/{id}/versions` | the holding or local state, plus the params if the account created the asset or app |
| `GET /v2/assets/{id}/versions`, `GET /v2/applications/{id}/versions` | the params (global state included) |
| `GET /v2/applications/{id}/box/versions?name=` | the box |
| `GET /v2/applications/{id}/boxes/versions` | every box of the app: each version is a round in which any of them changed, listing the boxes that did (name, `deleted`, and the value with `include=values`), in name order. Served from the [box index](#box-index); 503 for ranges that start before the index is complete |

They take `min-round`, `max-round` (inclusive, default 0 and the latest round), `order=asc|desc`, `limit` (default 100, at most 10,000, or 1,000 with values or keys) and `next` (the previous page's `next-token`, present if and only if more versions remain); pages with values also stop before the version that would take their stored values past 1 MB, unless it is the first (the JSON is larger: base64, decoded state); with keys, the previous values the keys are found from count too. History never changes, so pages stay consistent while blocks arrive. A leaf object's versions are one range scan (its versions are adjacent in the store); an account merges one scan per object it has ever had, capped by `MaxAPIResourcesPerAccount`, counting every resource the account ever had, deleted ones included (400 beyond it). An app's boxes are not merged that way, since busy apps churn through millions of box names; their changes come from the box index, so a page costs what it returns. With `rounds=a,b,c` instead of a range, each entry is the object as of a requested round, with the round it last changed (`changed-round`): one seek per object per round, and a page ends before a round that would take it past 100,000 seeks. For an app's boxes that merges every box the app ever had, bounded by `MaxAPIBoxPerApplication` (400 beyond it), and a round with more boxes or values than a page holds fails with 400. See [VERSIONS.md](VERSIONS.md#as-of-rounds).

`GET /v2/deltas/{R}` works for any round whose block is still stored (every round with `Archival: true`, otherwise the last ~1,000, more with `MaxBlockHistoryLookback`): rounds older than the trackers' in-memory deltas are re-evaluated (see [Node integration](#node-integration)).

`Archival: true` also works: the node keeps every block and certificate and serves every stock archival endpoint unchanged, with `?round=` and historical simulate on top. The cost is the archival block DB (~3.5 TB) plus the history (~65 GB, of which ~12 GB are headers that duplicate the block DB's; historical lookups read them from the history all the same).

### Unnamed-resource limits in simulation

With `allow-unnamed-resources`, simulate caps the unnamed resources a group may touch at what the group could legally reference under the consensus rules: each app call's account, asset, app, box and total reference limits (plus those of the empty slots of a group shorter than 16), the limit on holdings and local states, and the box I/O budget those box refs bring. That aims to keep the reported `unnamed-resources-accessed` sized for a real group (a txn that uses an `access` list can still get more than fits, an existing upstream limitation), but it blocks read-only uses such as reading many boxes or accounts in one call.

`SimulateResourceLimitMultiplier` (node config, default 1) multiplies all of those limits, and with them the box I/O budget. Values below 1 count as 1; values above 1024 count as 1024, and the node logs a warning at startup. Clients cannot set it; simulate responses do not report it, but [`GET /v2/transactions/simulate/limits`](#simulate-limits) does. Above 1, the reported resources may no longer fit in a submittable group. The limits on the submitted transactions themselves (foreign arrays, `access` list, app args, group size) and the I/O budget of the box refs they list do not change. The setting is independent of time travel and works on any node.

High values have a cost. The opcode budget (including `extra-opcode-budget`) bounds the work a call can do, but the multiplier bounds how many resources simulate records, and so the memory it uses and the size of the response: near the ceiling, one response can list hundreds of thousands of `unnamed-resources-accessed` entries (a full group at 1024× allows about 131,000 boxes or accounts). Raise it only as far as the reads you need.

### Extra opcode budget limit in simulation

Upstream, `extra-opcode-budget` may be at most 320,000 per group (16 × the LogicSig budget), on top of the group's own app budget (700 per app call) and any op-ups. `MaxAPISimulateExtraOpcodeBudget` (node config, default 320000) sets that limit instead, per top-level group; inner groups share their top-level group's budget and don't get the extra again. 0 counts as 320,000; values above 320,000,000 (1000×) are clamped to 320,000,000 when used, and the node logs a warning at startup. A negative `extra-opcode-budget` is rejected (400). Clients cannot change it; [`GET /v2/transactions/simulate/limits`](#simulate-limits) reports it. The setting is independent of time travel and works on any node. `goal clerk simulate --allow-more-opcode-budget` still asks for exactly 320,000; use `--extra-opcode-budget N` to ask for more. The limit can also be set lower than 320,000, but then clients that ask for the upstream maximum, such as that goal flag, get a 400.

This is a change for apps that make inner app calls. Upstream 5.0.x and earlier dtalgorand builds give every inner group the extra budget again, so with `extra-opcode-budget` a call with inner app calls could run on far more budget than it asked for (up to 257× with 256 inner groups). This build has upstream's later fix (go-algorand #6708): the extra budget is added once per top-level group, and `app-budget-added` counts 700 for each inner app call. A simulation that only passed because of the inflated budget now fails with `dynamic cost budget exceeded`; ask for a larger `extra-opcode-budget` if the call needs it.

The budget is time on the node's CPU. As a rough guide (one core of a desktop CPU), cheap opcodes and hashes take about 60–90 ns per budget unit, so 320,000 is about 20–30 ms; opcodes that build 4 KB byte arrays (`bzero`, `concat`) take about 350 ns per unit, and box writes into 32 KB boxes (`box_replace`, `box_splice`) about 2 µs per unit, so 320,000 of those is over half a second and the 320,000,000 ceiling about ten minutes per group. Multiply by the groups and rounds a request may hold (`MaxAPISimulateGroups`, `MaxAPISimulateRounds`) for the most one request may ask for; the node's time limit and `SimulateParallelism` bound what it spends (see "Overload limits" below).

A group stops when the client goes away or the request reaches the node's time limit (`RestWriteTimeoutSeconds`, default 120, less a tenth: 108 s), so raising the limit beyond what runs in that time also needs a longer write timeout.

Execution traces (`exec-trace-config`, only with `EnableDeveloperAPI`) record every opcode run, at about 112 bytes each before any stack, scratch or state data, and stack tracing copies every byte value pushed. At 320,000,000 that is tens of GB for one group, and even at 320,000 a stack trace of 4 KB values can reach a GB. A traced request therefore keeps upstream's limits: it may not list `rounds`, and may ask for at most 320,000 `extra-opcode-budget` whatever `MaxAPISimulateExtraOpcodeBudget` allows; either is a 400. It may hold up to `MaxAPISimulateGroups` groups and use one `round`, a past round from the time travel history included. The worst case is the groups times upstream's largest trace of one group. A group can run about 830,000 opcodes (the pooled app budget with the extra budget, op-ups by inner app calls, and its LogicSigs, which are traced too), about 90 MB of trace before any recorded values, and GBs with stack tracing of large byte values. At the default 16 groups that is over 1.4 GB, and `MaxAPISimulateGroups` can be raised to 256. Don't enable `EnableDeveloperAPI` on a node others can reach.

### Several transaction groups in one simulation

`txn-groups` may hold up to `MaxAPISimulateGroups` groups (node config, default 16; values below 1 count as 1, values above 256 count as 256, with a startup warning). By default they run in order as consecutive groups of one simulated block, so a later group sees the effects of the earlier ones, a transaction or lease seen in an earlier group is a duplicate, and the groups share the block's byte limit. Each group reports its own `failure-message` and `failed-at`: a failed group is rolled back and the later groups still run. The exceptions are the placeholder PQ signature check with `fix-signers`, which runs after its group is applied, and a group that leaves the evaluator state corrupted (a panic while it is committed): then every later group is reported as not simulated, with the index of the group that stopped them, and no block is made. Group indexes in these messages and `failed-at` paths are 0-based. A request with a StateProof transaction is rejected as a whole (400). A verification failure that can't be blamed on one transaction (a bad signature found by the batch verifier, a LogicSig args pool overflow) fails only its group, with no `failed-at`, but stays a 400 when the request has only that group.

With `parallel-groups: true`, every group is simulated alone against the state at the start round, on up to one evaluator per CPU shared by all simulate requests of the node. Groups don't see each other's effects, duplicates and leases are not detected across groups, and several groups may create the same app or asset ID. `initial-states` merges the groups' states, lowest group first.

`extra-opcode-budget` applies to each group, as it does to a lone group. `goal clerk simulate -t` splits its file into groups on changes of the Group field, as `goal clerk rawsend` does, and `--parallel-groups` sets the flag.

### One simulation at several rounds

`rounds` lists the rounds to simulate the same request at, each meaning what `round` means. A 0 entry is resolved once, to the node's last round when the request starts, so it can differ from the latest round a later single-round request uses. The response then has `round-results`, one entry per listed round in request order, each with its `last-round` and either the `txn-groups` and `initial-states` of that round's simulation or an `error`. The top-level `last-round` is 0 and `txn-groups` is empty; `version`, `eval-overrides` and `exec-trace-config` come from the first entry that was simulated, or from the request when none was. A round listed several times is simulated once, but still counts against the limit below. Within each round, the groups run as described above, sequentially or with `parallel-groups`. With time travel any round since genesis can be listed; otherwise only the last `MaxAcctLookback` rounds are available, and older ones get an `error` entry.

Problems with the request as a whole are a 400 before anything runs: a non-zero `round` with `rounds` (`round: 0` is the same as no `round`), more entries than `MaxAPISimulateRounds`, duplicates included (node config, default 16; 0 disables `rounds`; values above 256 count as 256, with a startup warning), a `format` other than json or msgpack, an enabled `exec-trace-config`, and everything a single-round request rejects whatever its round (`fix-signers` without `allow-empty-signatures`, too many groups, too much extra budget, exec traces without `EnableDeveloperAPI`, a StateProof transaction, both validity window modes, `rewrite-validity-window` without `allow-empty-signatures`). `skip-validity-window` or `rewrite-validity-window` without `rounds` is a 400 too. What depends on the round is reported in that round's entry instead, including the cases where a single-round request answers 400 or 500: a round the node doesn't have, or a group that is invalid under that round's protocol.

A request runs its rounds one after another, in the one slot of `SimulateParallelism` it was admitted with (see "Overload limits" below); `SimulateRoundsParallelism` has no effect. Parallel groups inside a round also take the node's per-CPU group slots. A request can do up to rounds × groups simulations, each group with up to `MaxAPISimulateExtraOpcodeBudget` of extra opcode budget, all within the node's time limit. `goal clerk simulate` has no flag for `rounds` yet.

#### Overload limits

Every simulate request, with or without `rounds`, is bounded in time and stops when nobody waits for it:

- **Time limit.** A request may run for `RestWriteTimeoutSeconds` less a margin to encode and write the answer before the server's write deadline: a tenth of the timeout and at least a second, so 108 s at the default 120 s, or half the timeout when it is under 2 s; no limit at 0. The deadline counts from the request's arrival, so a slow request body uses up the time too. A request without `rounds` that reaches it gets a 504. With `rounds`, the rounds that finished keep their results and the others get the `error` `not simulated: the request reached the node's time limit (RestWriteTimeoutSeconds)`.
- **Client gone.** When the client disconnects (or a load balancer closes the connection), the simulation stops and no response is written.
- Both stop the simulation between groups, between rounds, and inside a running program every 16,384 units of budget, so a large extra opcode budget can't keep the node busy after either. A group that finished keeps its result even when the limit passes right after. Ledger reads between two checks are not stopped: a cold read costs a unit or two of budget, and at a past round it can be a disk read of the history. They are bounded by the consensus limits on the resources a group may touch, times `SimulateResourceLimitMultiplier`, so keep the multiplier at 1 on a node with time travel open to others.
- **Node-wide concurrency.** `SimulateParallelism` (default 0 = GOMAXPROCS) is the most simulations that run at once across the node: one per request, a `rounds` request included. A request that can't get a slot within 250 ms gets a 503 with `Retry-After: 1`, so a load balancer can send it to another node. A `rounds` request releases its slot once its last round ends, before the answer is written.
- **Time travel reads.** The versions endpoints stop at the same time limit (504) and when the client goes away. A `/v2/deltas/{round}` older than the in-memory deltas is rebuilt by re-evaluating its block in one of two node-wide slots: it waits at most 250 ms for one, then gets a 503 with `Retry-After: 1`, and it stops at the time limit or when the client goes away.

#### Effort header

With `EnableEffortHeader` (default off), simulate requests, the versions endpoints, `/v2/deltas/{round}` and reads at a past round (`?round=`) answer with an `X-Algod-Effort` header: the work the request did, so that a load balancer can bill on it instead of on wall time. It is an RFC 9651 dictionary of integers, every key always present, e.g. `X-Algod-Effort: opcodes=184220, lookups=912, history=640, scanned=0, response-bytes=48213, eval-us=5310`. Readers should ignore keys they don't know.

| Key | Counts |
|---|---|
| `opcodes` | App and logicsig budget consumed, over every group and distinct round (a round listed twice in `rounds` runs once): the sum of `app-budget-consumed` and `logic-sig-budget-consumed`, plus programs that ran before the time limit or an error stopped them, which the response doesn't report. 0 for a rebuilt delta. |
| `lookups` | Ledger objects read: accounts, holdings, apps, boxes, headers, totals, creators. An evaluator reads an object once per simulated block; with `parallel-groups` each group has its own evaluator, so an object shared by groups counts once per group. A delta counts one, plus the reads of a rebuild. |
| `history` | The lookups the time travel history answered: a point read (an account, a holding, a box) or a box listing when the trackers no longer hold its round, and every read of an account's full state or its asset or app listings at a past round, which only the history serves. At most `lookups`. |
| `scanned` | Entries read by scans: versions read by the versions endpoints, and history entries iterated by listings the history answered (assets, apps, boxes). |
| `response-bytes` | The answer's encoded size before compression (algod gzips for clients that accept it, so `Content-Length` doesn't tell): traces, logs and inner transactions make it grow. Simulate, `/v2/deltas/{round}` and the versions endpoints report it. Reads at a past round report 0, since they return single objects billed on `lookups`; error answers (400, 504) report 0 too, since they aren't the work's output. |
| `eval-us` | Microseconds from the start of the work (after admission) to the answer. For diagnostics. |

Every answer written after the work started carries it: 200 (also a `rounds` answer at the time limit), 504 at the time limit, and the errors found during the work (a simulate request the simulator rejects, a delta that isn't stored, a past-round read that fails), with what was counted so far. 503s never carry it (the node was too busy to start), nor do requests refused before the work started (most invalid parameters), nor reads at the latest round. A request whose client went away writes nothing. Enable it only behind a proxy that removes it before answering clients.

#### Validity window modes

A transaction is valid for at most `MaxTxnLife` (1000) rounds, from its `first-valid` to its `last-valid`, and is rejected (`txn dead`) at any other round. So without help, the rounds of one request can't be more than about 1000 apart. Two request options lift that, only with `rounds`, and not both at once. With either, the normal checks still apply otherwise: genesis ID and hash, well-formedness (`last-valid` ≥ `first-valid`, a window of at most `MaxTxnLife`, keyreg `VoteFirst` ≤ `last-valid` + 1), fees (PQ fee contributions included), and the rest. The response's `eval-overrides` shows which mode was set.

`skip-validity-window` evaluates the transactions as sent at every round. The round range check is skipped, and so is the check for a transaction ID the ledger already confirmed: at a round long after its `last-valid`, the node no longer keeps its transaction IDs. Leases are still checked, confirmed ones and ones used earlier in the simulated block, as are duplicates within the block; a transaction that was confirmed with a lease still fails on its own lease at rounds where that lease is live. Every lease still live at the simulated round is checked, under every protocol. The node keeps the leases of its last `MaxTxnLife` rounds only, so leases confirmed before those are not seen, the same gap as for any simulation at a past round. Programs see the original `first-valid` and `last-valid`, so `txn FirstValid`, the rounds `block` can read and `txn FirstValidTime`, and the windows of inner transactions all stay tied to the original window, not the simulated round. When that window lies after the start round, reading a block header after the start round fails, even on a node that has it: the simulation must not see the future.

`rewrite-validity-window` moves every top-level transaction's window to the simulated block at every round, keeping its width: `first-valid` becomes the start round + 1 and `last-valid` that plus the original `last-valid` − `first-valid` (a malformed window is left as is, so it is reported as usual). Group IDs that were valid are recomputed over the rewritten transactions; an invalid one is left, so the group still fails. All the normal checks then run unchanged, and programs, `block`, `FirstValidTime` and inner transactions see a window at the simulated round. Because transaction IDs change, signatures over the transaction can't verify: the mode requires `allow-empty-signatures`, and drops `sig`, `msig` and the bytes of a PQ signature of every transaction it changes, which then counts as a placeholder (its fee contribution and envelope are still checked). A transaction whose window already starts at the simulated block, in a group whose ID doesn't change, is kept as it is. Logic signatures sign the program, not the transaction, so they are kept. Notes:

- The results echo the rewritten transactions: a different `first-valid`, `last-valid`, group and transaction ID at every round, no `sig`. Don't match results by transaction ID; the echoed transactions can't be submitted as they are.
- Transactions that differ only by their window (e.g. the same call built a few rounds apart with the suggested window) become identical, so a later one fails as a duplicate of the earlier one in the same simulated block. Give them distinct notes.
- Heartbeat transactions keep their window: their proof is bound to `last-valid` and their seed to `first-valid`. They take their group's new ID, and lose their signature if that changes it.
- A keyreg going online fails when its `VoteFirst` is after the rewritten `last-valid` + 1, e.g. when it is moved to an earlier round. That is reported as a normal group failure.
- With `fix-signers`, every transaction whose signature was dropped needs a synthetic signature, so its `auth-addr` is replaced with the signer the ledger (or an earlier group) has, as for any unsigned transaction. Logic signature transactions keep theirs.
- Reading a block header after the start round fails here too, which only a heartbeat can try (its seed is that of its own `first-valid`).

### A later protocol at a past round

`protocol` simulates the round under a later consensus protocol, as if an upgrade to it had taken effect right after the start round: run an app that needs AVM 13 against 2022 state, or see what an old app does under today's rules. It takes a consensus version string, `v24` to `v43` for the mainline ones, `latest` (the protocol of the block after the node's latest one: the latest block's, or the one an upgrade switches to right after it) or `future` (vFuture, only with `EnableDeveloperAPI`). The target must be a later protocol of the chain than the one the round's next block really ran, following the approved upgrades (plus FNet's v41 → fnet5); the same protocol simulates the round as it was, reported as not applied; anything else is a 400, or that round's `error` with `rounds`. An unknown name, or `future` without the developer API, is a 400 for the whole request. `goal clerk simulate --protocol` sets it. It works on any node, at any round the node can simulate; with time travel that is every round since genesis.

How it works: simulate patches the start round's header so that it carries an approved upgrade to the target, switching at the next round. The simulated block is then built and checked exactly as the first block after a real upgrade: the target's programs, limits, fees and minimum balances apply, to programs deployed earlier too, its `Bonus` is the target's base amount when the upgrade turns incentives on, and its `Branch` hashes the patched header, so it does not match the real chain. Programs can't see the patch: `block` reads every header as it was, `BlkProtocol` included.

The response's `protocol-override` (in each `round-results` entry instead, with `rounds`) gives `from` (the protocol the next block really ran), `to`, `applied` and `notes`. Where the new protocol reads data the chain never produced, the simulation sees what a real upgrade would have seen at that round, usually zero, and a note says so, listing up to 16 transactions (group index and `failed-at`-style path) that read it:

| Note | When | What the simulation sees |
|---|---|---|
| `semantics-changed` | always, when a parameter that affects transactions differs | the target's rules; the message lists each changed parameter, old → new (e.g. `LogicSigMsig: true → false` rejects legacy multisig delegations, `PerByteTxnSurcharge` raises fees, `MaxTxnBytesPerBlock` changes what fits) |
| `no-heartbeat-challenges` | the target has heartbeats and other payout rules | no challenge, since earlier challenge rounds ran other rules: only full-fee heartbeats are valid, and no account is absent |
| `no-load-history` | the target tracks load and the real protocol did not | a congestion tax computed from a load of 0 (header only today) |
| `block-incentive-fields` | `block` reads `BlkProposer`, `BlkFeesCollected`, `BlkBonus` or `BlkProposerPayout` of a round without incentives | zero |
| `block-hash-fields` | `block` reads `BlkBranch512`, `BlkSha512TxnCommitment` or `BlkSha256TxnCommitment` of a round that did not compute it | zero bytes |
| `online-stake-includes-expired` | `online_stake` when the balance round (320 back) did not exclude expired keys and the target does | the online money including keys that expire before the simulated round, as in the first 320 rounds after a real upgrade (an upper bound on the target's steady-state value) |
| `account-incentive-fields` | `acct_params_get` `AcctIncentiveEligible`, `AcctLastProposed` or `AcctLastHeartbeat`, or `voter_params_get` `VoterIncentiveEligible`, before incentives | false or 0, as for every account right after the upgrade |
| `app-version-starts-at-upgrade` | `app_params_get AppVersion` before app versions | 0 for apps updated earlier: versions count from the upgrade on |

Notes are about the real protocol, not version numbers (FNet's fnet1 already had incentives, for instance). Values that come from the protocol itself, such as `global PayoutsEnabled` or `global MinTxnFee`, are the target's. The limits of simulating at an old round (below) still apply; in particular `online_stake` fails where the history has no record of the online stake.

### Simulate limits

`GET /v2/transactions/simulate/limits` (`dtalgorand` tag in the API spec, served by every node of this fork, time travel or not) tells clients what this node's simulate accepts, with each value as the handlers clamp it:

```json
{ "max-txn-groups": 16, "max-rounds": 16, "max-extra-opcode-budget": 320000, "resource-limit-multiplier": 1, "time-limit-seconds": 108, "time-travel": true, "developer-api": false }
```

| field | node config | meaning |
|---|---|---|
| `max-txn-groups` | `MaxAPISimulateGroups` | most groups in `txn-groups`; a config of 0 counts as 1 |
| `max-rounds` | `MaxAPISimulateRounds` | most entries in `rounds`, duplicates included; a config of 0 disables `rounds` |
| `max-extra-opcode-budget` | `MaxAPISimulateExtraOpcodeBudget` | most `extra-opcode-budget`; a config of 0 counts as 320,000 |
| `resource-limit-multiplier` | `SimulateResourceLimitMultiplier` | factor on the unnamed-resource limits; 1: the consensus limits; a config of 0 counts as 1 |
| `time-limit-seconds` | `RestWriteTimeoutSeconds` | longest a request may run: the write timeout less a tenth, at least a second and at most half (1 for a limit under a second); 0: no limit |
| `time-travel` | `EnableTimeTravel` | `round` and `rounds` reach back to genesis, not only `MaxAcctLookback` rounds |
| `developer-api` | `EnableDeveloperAPI` | `exec-trace-config` with `enable: true`, and `protocol: "future"`, are allowed; a traced request may not list `rounds` and may ask for at most 320,000 extra budget; a disabled `exec-trace-config` is accepted either way |

The response holds the values after these rules, so `max-rounds` and `time-limit-seconds` are the only fields that can be 0. The extra budget is added once to each top-level group that has an app call, to the group's pooled app budget: a group of only LogicSigs and payments gets nothing from it, since it never raises a LogicSig's budget.

Two limits are not in the response, because they don't come from the node's config:

- A request body is at most 1,000,000 bytes, whatever `max-txn-groups` allows; a larger one gets a 400 `http: request body too large`. A full group of 16 transactions takes several KB, so a request near the 256-group ceiling can reach it first.
- Groups simulated in order share one block, so together they may take at most the protocol's `MaxTxnBytesPerBlock`, counting what they produce (logs, inner transactions) as well as what was sent. A group that doesn't fit fails with `block does not have space for transaction`, without a `failed-at`, and the later groups still run; a smaller one may still fit. With `parallel-groups` every group gets a block of its own.

A stock algod answers 404, which tells a client it can't use any of the fork's simulate features. `SimulateParallelism` is left out: it limits how many requests run at once, not what one may ask.

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
- `ttimport -dedup -d <data dir>` removes them from the store of a stopped node, including those written before the cache existed, and drops the tombstones of objects that never existed (created and deleted in one round). On fnet it removed 50.3 M records (3.21 → 2.94 GiB) in 1.5 minutes, and every read compared answered the same. It rebuilds the [box index](#box-index) in the same pass.

### Box index

`/v2/applications/{id}/boxes/versions` asks "which boxes of app X changed, and to what, in rounds [lo, hi]". The object-major layout answers it only by visiting every box the app ever had (`Objects` over `k bx:app`, then one `Versions` scan each): two seeks per box, whatever the page size or range. The busiest mainnet box apps write a different box name on nearly every write, so that is millions of boxes per app.

So the store keeps a secondary index that is round-major per app: `b` app (8 bytes) round (8 bytes) name → `F` (written) or `D` (deleted). The name is the last component and needs no escaping. A range of an app's rounds is one bounded iterator, either direction.

- `AddRound` writes one entry per box change it stores (blind, like the records). An entry can be a no-op its cache missed, or the tombstone of a box created and deleted within the round, so `BoxChanges` checks each one against the box's record and the one before it (N = 1: a seek and a step back on a shared iterator), skipping those that change nothing; that read also yields the value. A page costs a seek per entry in it.
- `IndexBoxes` (`ttimport -index-boxes -d <data dir>`, node stopped) rebuilds the index from the box records in one pass, writing entries for versions only; `RemoveNoops` does the same in its pass. Both are idempotent, and `CompactBoxIndex` / `CompactCheckpoints` reclaim what they replace.
- Readers check that no rebuild started or finished while they opened their iterators (a generation counter), and fail with `ErrNoBoxIndex` otherwise.
- Completeness: the meta record `mb` holds the round the index is complete from and the next round `AddRound` indexes. A store created by a build with the index, or holding no box when such a build first opens it, is complete from round 0 (boxes arrived with v36, around mainnet round 25 M, so an import that has not reached it yet needs nothing). Otherwise the index starts at the store's next round, and until `-index-boxes` runs `BoxChanges` fails with `ErrNoBoxIndex` (REST 503) when it needs an earlier round: at once ascending, and once it reaches that round descending, so "the latest changes" still work. If a build without the index adds rounds, the next open moves the start past them.
- Size: one entry per box write, mostly 17 bytes of key prefix plus the name (24 bytes on average on mainnet) and 1 byte of value, before prefix compression. A sample of 1,637 mainnet rounds (`/v2/deltas`) suggests ~117 M box writes since boxes arrived, so an estimated 1–3 GB on disk, 2–5% of the store. The rebuild scans the box records once (~117 M before dedup); writes arrive name-major, so it compacts the index afterwards.

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
