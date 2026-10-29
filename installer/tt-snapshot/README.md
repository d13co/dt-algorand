# tt-snapshot

A systemd setup for a [ttsnap](../../tools/ttsnap/README.md#producer) producer: it runs [scripts/dt_snapshot.sh](../../scripts/dt_snapshot.sh) once per catchpoint and mirrors the snapshot tree to a Cloudflare R2 bucket served on a custom domain. Mainnet's snapshots, at `https://tt-snapshots-mainnet.d13.co`, come from it.

It expects the producer laid out as the ttsnap README's [Producer](../../tools/ttsnap/README.md#producer) section describes (a time travel node whose genesis directory is a btrfs `@live` subvolume, from [scripts/btrfs_hot_ledger.sh](../../scripts/btrfs_hot_ledger.sh)), a Falcon producer key that consumers trust, and a ttsnap that signs with it: the dtalgorand package's from 5.0.4-dt3 on, or one built from this branch.

| file | installed as | does |
|---|---|---|
| `setup.sh` | | installs or updates everything below, rclone 1.59 or later from rclone.org when needed (Ubuntu 22.04's rclone has no R2 support; the download's signature is checked against rclone's release key), and jq |
| `tt-snapshot.env` | `/etc/tt-snapshot.env` (0600, kept when it exists; `setup.sh` adds settings new to it, not ones removed by hand) | `dt_snapshot.sh` settings, the bucket and the R2 credentials |
| `tt-snapshot.service` | `/etc/systemd/system/` | one run of `dt_snapshot.sh`: btrfs snapshot, `ttsnap prepare`, `publish` and `gc`, then the upload. Runs at low CPU and IO priority |
| `tt-snapshot-upload.service` | `/etc/systemd/system/` | `upload.sh` alone, when the bucket lags `OUT` (a failed upload) |
| `tt-snapshot-trigger.timer`, `.service`, `trigger.sh` | `/etc/systemd/system/`, `/usr/local/lib/tt-snapshot/` | every 2 minutes: starts `tt-snapshot.service` when a snapshot is due ([when](#when-it-runs)), or `tt-snapshot-upload.service` when the bucket lags |
| `upload.sh` | `/usr/local/lib/tt-snapshot/` | `dt_snapshot.sh`'s `UPLOAD`: mirrors `OUT` into the bucket with rclone, holding `dt_snapshot.sh`'s lock, and records what it mirrored in `/var/lib/tt-snapshot/uploaded` when it completes |
| `r2-check.sh` | `/usr/local/lib/tt-snapshot/` | checks that the credentials list and write the bucket, and that the domain serves what was written, with range requests |
| `lib.sh` | `/usr/local/lib/tt-snapshot/` | the env file reading and `OUT` digest the scripts share |

## Cloudflare

The domain's zone must be on Cloudflare, in the account that holds the bucket.

1. R2, Create bucket: Standard storage class (Infrequent Access charges for every GB read).
2. The bucket's Settings, Custom Domains: add the domain, and wait until it is Active. Leave the r2.dev URL off.
3. R2, Manage API tokens: a token with Object Read & Write on this bucket only. Note its Access Key ID and Secret Access Key, and the account ID from the S3 endpoint `https://<account id>.r2.cloudflarestorage.com`.
4. Caching: keep the defaults. Cloudflare caches no file without an extension at the edge, so `latest-history` is never stale. A Cache Everything rule on the domain must leave out `latest-*`.

Mainnet's bucket costs about $1 a month: about 70 GB stored, and the operations stay in the free tier. R2 charges nothing for downloads.

## Install

```sh
sudo installer/tt-snapshot/setup.sh /usr/bin/ttsnap  # or "$(go env GOPATH)/bin/ttsnap" after make install
sudoedit /etc/tt-snapshot.env                        # bucket, domain, R2 account ID and keys; KEY, OUT, ROOT
sudo /usr/local/lib/tt-snapshot/r2-check.sh
sudo systemctl start --no-block tt-snapshot.service && journalctl -fu tt-snapshot
sudo systemctl enable --now tt-snapshot-trigger.timer
```

The first run copies the whole history into `OUT` and uploads it: on mainnet, 70 GB, about 12 minutes to publish and 10 to upload on a gigabit link. Later runs copy and upload only the tables that changed since the run before, usually a few MB. Rerun `setup.sh` to update ttsnap or the scripts; it keeps `/etc/tt-snapshot.env` and adds the settings new since its last run. A setting removed from the file by hand stays out (`/var/lib/tt-snapshot/env-names` lists those it has offered): to turn one off, set it empty or delete it. Also rerun it after changing `ROOT` or `OUT` there: the units' mount dependencies (`RequiresMountsFor=`, in `tt-snapshot.service.d/paths.conf` and `tt-snapshot-upload.service.d/paths.conf`) come from them.

## When it runs

A node that installs a snapshot catches up to a catchpoint C only if C's balances round (C − 320) is at most the snapshot's last round. Catchpoints come every 10,000 rounds, about 7.7 hours on mainnet, which no clock schedule follows. Every 2 minutes, the trigger reads the node's round from its REST API (the address and token in the data directory's `algod.net` and `algod.token`) and takes the latest balances round the node is 30 rounds past. It starts a run when the snapshot tree's latest history ends before that round: normally 30 rounds after each balances round, which leaves about 13 minutes before C, or as soon as it can when that run was missed (algod was down, or the run failed). It starts none while the node catches up (a nonzero `catchup-time` in `/v2/status`): the node's state lags the network's then, and each catchpoint it passed would start a run; once it is in sync, one run catches up. An on-time snapshot reaches C and the catchpoint before it, so `ttsnap catchup` with the network's latest catchpoint works for a node that installs it until the catchpoint after C is published. A late one reaches C only.

When it starts no run and `UPLOAD` runs `upload.sh` (the retries run it, and only it records what it mirrored; another `UPLOAD` gets no retries), the trigger starts `tt-snapshot-upload.service` when the bucket lags the tree: `OUT` changed since `upload.sh` last completed (an upload that failed at any step, or statements added by hand), or a `latest-*` pointer the domain serves names an older snapshot than the tree's. This needs no node, and goes on while algod is down. An empty `UPLOAD=` turns uploads off, and these retries with them. It starts nothing while either unit runs. It starts a unit again only 30 minutes (`RETRY`) after its last run ended when that run failed, or was started for the same round or the same `OUT` and fell short. It logs the latter as a warning: an upload that completes while the domain keeps serving an older pointer means `R2_PUBLIC_URL` is not the bucket's domain, or a cache rule holds `latest-*`. In `/run/tt-snapshot` it keeps what it started each unit for, and the units record when each run ended (systemd forgets that once a run succeeds).

`trigger.sh` takes `ALGOD_DATA`, `ALGOD_URL`, `ALGOD_TOKEN`, `INTERVAL`, `LOOKBACK`, `MARGIN` and `RETRY` from the environment, then from `/etc/tt-snapshot.env`; `DRY_RUN=1` prints what it would do. It logs only when it starts a unit, or cannot read the node or the bucket.

## Operating it

- Logs: `journalctl -u tt-snapshot -u tt-snapshot-upload -u tt-snapshot-trigger`.
- A snapshot now, whatever the round: `sudo systemctl start --no-block tt-snapshot.service`.
- Only the upload: `sudo systemctl start --no-block tt-snapshot-upload.service`. It refuses to run during a snapshot run.
- A failed run keeps its btrfs snapshot in `ROOT/@snap` for a look; the next run deletes it. The trigger retries a failed unit 30 minutes after the run ended; to retry sooner, start the unit by hand.
- The upload mirrors `OUT`: it deletes from the bucket what `OUT` lacks, and refuses an `OUT` without a `latest-*` pointer. It leaves out the `*.tmp` files of an interrupted publish, and deletes them from the bucket. Put attesters' signatures in `OUT/snapshots/<genesis id>/statements/<round>/`, not in the bucket.
- Consumers refuse a latest snapshot older than 7 days (`ttsnap fetch -max-age`), so runs that keep failing break new installs after a week.
