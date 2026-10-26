#!/usr/bin/env bash
# Mirror the ttsnap tree OUT into the R2 bucket R2_BUCKET (rclone remote "r2", defined by the
# RCLONE_CONFIG_R2_* variables), in the order that keeps the bucket consistent for readers:
#
#   1. blobs (named by their SHA-256, so one already in the bucket is never sent again)
#   2. manifests, their signatures and the statements
#   3. the latest-* pointers, so a reader never finds a manifest whose blobs aren't there yet
#   4. deletions: whatever ttsnap gc removed from OUT
#
# dt_snapshot.sh runs it as UPLOAD after each publish. By hand:
#   sudo bash -c 'set -a; . /etc/tt-snapshot.env; /usr/local/lib/tt-snapshot/upload.sh'
set -euo pipefail

out=${OUT:?set OUT to the snapshot tree}
out=${out%/}
bucket=${R2_BUCKET:?set R2_BUCKET}
dst=r2:$bucket

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $(date -u +%FT%TZ) upload: $*"; }

command -v rclone >/dev/null || die "rclone not found"
# the final sync deletes what OUT lacks: refuse an OUT that isn't a published tree
compgen -G "$out/snapshots/*/latest-*" >/dev/null || die "$out holds no latest-* pointer; not uploading"
[[ -d $out/blobs ]] || die "$out/blobs does not exist"

# about transfers x upload-concurrency x chunk-size of RAM at most (1 GiB)
common=(--fast-list --transfers 4 --checkers 16 --s3-upload-concurrency 8 --s3-chunk-size 32M
  --retries 5 --low-level-retries 20 --stats 1m --stats-one-line --stats-log-level NOTICE)

log "blobs"
rclone copy "${common[@]}" --ignore-existing "$out/blobs" "$dst/blobs"
log "manifests and statements"
rclone copy "${common[@]}" --exclude 'latest-*' "$out/snapshots" "$dst/snapshots"
log "latest pointers"
rclone copy "${common[@]}" "$out/snapshots" "$dst/snapshots"
log "deleting what gc removed"
rclone sync "${common[@]}" "$out" "$dst"
log "done"
