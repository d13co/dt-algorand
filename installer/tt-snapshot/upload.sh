#!/usr/bin/env bash
# Mirror the ttsnap tree OUT into the R2 bucket R2_BUCKET (rclone remote "r2", defined by the
# RCLONE_CONFIG_R2_* variables), in the order that keeps the bucket consistent for readers:
#
#   1. blobs (named by their SHA-256, so one already in the bucket is never sent again)
#   2. manifests, their signatures and the statements
#   3. the latest-* pointers, so a reader never finds a manifest whose blobs aren't there yet
#   4. deletions: whatever ttsnap gc removed from OUT, and *.tmp files of interrupted publishes
#
# It holds dt_snapshot.sh's lock, ROOT/@snap/.lock, so that no publish runs during an upload (step 3
# would send a pointer whose new blobs step 1 missed). When all four steps succeed, it records a
# digest of what it mirrored in /var/lib/tt-snapshot/uploaded (lib.sh's mirror_digest), so that the
# trigger can tell an upload that failed at any step. dt_snapshot.sh runs it as UPLOAD after each
# publish, and tt-snapshot-upload.service runs it when the bucket lags OUT. By hand:
#   sudo systemctl start tt-snapshot-upload.service && journalctl -fu tt-snapshot-upload
set -euo pipefail

# shellcheck source=lib.sh
. "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/lib.sh"
out=${OUT:?set OUT to the snapshot tree}
out=${out%/}
bucket=${R2_BUCKET:?set R2_BUCKET}
root=${ROOT:-/mnt/dtalgod-mainnet-hot-root}
root=${root%/}
dst=r2:$bucket

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $(date -u +%FT%TZ) upload: $*"; }

command -v rclone >/dev/null || die "rclone not found"
# the final sync deletes what OUT lacks: refuse an OUT that isn't a published tree
[[ -n $(pointers "$out") ]] || die "$out holds no latest-* pointer; not uploading"
[[ -d $out/blobs ]] || die "$out/blobs does not exist"

if [[ -d $root/@snap ]]; then
  # run by dt_snapshot.sh, this inherits the lock it holds as fd 9
  [[ /proc/self/fd/9 -ef $root/@snap/.lock ]] || exec 9>"$root/@snap/.lock"
  flock -n 9 || die "dt_snapshot.sh or another upload is running"
fi
# before the copies: OUT may still change during them (statements added by hand), and the receipt
# must then differ from it
digest=$(mirror_digest "$out" "$bucket")

# about transfers x upload-concurrency x chunk-size of RAM at most (1 GiB)
common=(--fast-list --transfers 4 --checkers 16 --s3-upload-concurrency 8 --s3-chunk-size 32M
  --retries 5 --low-level-retries 20 --stats 1m --stats-one-line --stats-log-level NOTICE
  --exclude '*.tmp' --exclude '*.tmp.sig')

log "blobs"
rclone copy "${common[@]}" --ignore-existing "$out/blobs" "$dst/blobs"
log "manifests and statements"
rclone copy "${common[@]}" --exclude 'latest-*' "$out/snapshots" "$dst/snapshots"
log "latest pointers"
rclone copy "${common[@]}" "$out/snapshots" "$dst/snapshots"
log "deleting what gc removed"
rclone sync "${common[@]}" --delete-excluded "$out" "$dst"
mkdir -p "$state_dir"
echo "$digest" >"$uploaded.tmp"
mv "$uploaded.tmp" "$uploaded"
log "done"
