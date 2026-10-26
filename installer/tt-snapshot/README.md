# tt-snapshot

A systemd setup for a [ttsnap](../../tools/ttsnap/README.md#producer) producer: it runs [scripts/dt_snapshot.sh](../../scripts/dt_snapshot.sh) once per catchpoint and mirrors the snapshot tree to a Cloudflare R2 bucket served on a custom domain. Mainnet's snapshots, at `https://tt-snapshots-mainnet.d13.co`, come from it.

It expects the producer laid out as the ttsnap README's [Producer](../../tools/ttsnap/README.md#producer) section describes (a time travel node whose genesis directory is a btrfs `@live` subvolume, from [scripts/btrfs_hot_ledger.sh](../../scripts/btrfs_hot_ledger.sh)), a Falcon producer key that consumers trust, and a ttsnap that signs with it: the dtalgorand package's from 5.0.4-dt3 on, or one built from this branch.

| file | installed as | does |
|---|---|---|
| `setup.sh` | | installs or updates everything below, and rclone 1.59 or later from rclone.org when needed (Ubuntu 22.04's rclone has no R2 support) |
| `tt-snapshot.env` | `/etc/tt-snapshot.env` (0600, kept when it exists) | `dt_snapshot.sh` settings, the bucket and the R2 credentials |
| `tt-snapshot.service` | `/etc/systemd/system/` | one run of `dt_snapshot.sh`: btrfs snapshot, `ttsnap prepare`, `publish` and `gc`, then the upload. Runs at low CPU and IO priority, and starts at most twice an hour, so a failing run isn't retried in a loop |
| `tt-snapshot-trigger.timer`, `.service`, `trigger.sh` | `/etc/systemd/system/`, `/usr/local/lib/tt-snapshot/` | every 2 minutes: starts `tt-snapshot.service` when a snapshot is due ([when](#when-it-runs)) |
| `upload.sh` | `/usr/local/lib/tt-snapshot/` | `dt_snapshot.sh`'s `UPLOAD`: mirrors `OUT` into the bucket with rclone |
| `r2-check.sh` | `/usr/local/lib/tt-snapshot/` | checks that the credentials list and write the bucket, and that the domain serves what was written, with range requests |

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

The first run copies the whole history into `OUT` and uploads it: on mainnet, 70 GB, about 12 minutes to publish and 10 to upload on a gigabit link. Later runs copy and upload only the tables that changed since the run before, usually a few MB. Rerun `setup.sh` to update ttsnap or the scripts; it keeps `/etc/tt-snapshot.env`.

## When it runs

A node that installs a snapshot catches up to a catchpoint C only if C's balances round (C − 320) is at most the snapshot's last round. Catchpoints come every 10,000 rounds, about 7.7 hours on mainnet, which no clock schedule follows. The trigger reads the node's round from its REST API (the address in the data directory's `algod.net`) and starts a run once the round is 30 rounds past the next catchpoint's balances round, unless the latest published snapshot already reaches that balances round. The snapshot then reaches C and the catchpoint before it, so `ttsnap catchup` with the network's latest catchpoint works for a node that installs it until the catchpoint after C is published.

`trigger.sh` takes `ALGOD_DATA`, `ALGOD_URL`, `INTERVAL`, `LOOKBACK` and `MARGIN` from the environment or `/etc/tt-snapshot.env`; `DRY_RUN=1` prints what it would do. It logs only when it starts a run or cannot reach the node.

## Operating it

- Logs: `journalctl -u tt-snapshot -u tt-snapshot-trigger`.
- A snapshot now, whatever the round: `sudo systemctl start --no-block tt-snapshot.service`.
- Only the upload: `sudo bash -c 'set -a; . /etc/tt-snapshot.env; /usr/local/lib/tt-snapshot/upload.sh'`.
- A failed run keeps its btrfs snapshot in `ROOT/@snap` for a look; the next successful run deletes it. After two failed starts in an hour, systemd refuses more until `sudo systemctl reset-failed tt-snapshot`.
- The upload mirrors `OUT`: it deletes from the bucket what `OUT` lacks, and refuses an `OUT` without a `latest-*` pointer. Put attesters' signatures in `OUT/snapshots/<genesis id>/statements/<round>/`, not in the bucket.
- Consumers refuse a latest snapshot older than 7 days (`ttsnap fetch -max-age`), so runs that keep failing break new installs after a week.
