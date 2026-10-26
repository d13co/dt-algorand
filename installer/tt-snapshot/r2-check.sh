#!/usr/bin/env bash
# Check the R2 side before the first upload: the credentials can list and write the bucket, and
# R2_PUBLIC_URL serves what was written, with range requests (ttsnap fetch resumes with them).
#
#   sudo /usr/local/lib/tt-snapshot/r2-check.sh      (reads /etc/tt-snapshot.env)
set -euo pipefail

# shellcheck source=lib.sh
. "$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")/lib.sh"
env=${ENV_FILE:-/etc/tt-snapshot.env}
load_env "$env"
bucket=${R2_BUCKET:?set R2_BUCKET}
url=${R2_PUBLIC_URL:?set R2_PUBLIC_URL}
url=${url%/}

die() { echo "FAIL: $*" >&2; exit 1; }
ok() { echo "ok: $*"; }

grep -q FILL_IN <<<"${RCLONE_CONFIG_R2_ENDPOINT:-}${RCLONE_CONFIG_R2_ACCESS_KEY_ID:-}${RCLONE_CONFIG_R2_SECRET_ACCESS_KEY:-}" &&
  die "fill in the RCLONE_CONFIG_R2_* placeholders in $env"

rclone lsf --max-depth 1 "r2:$bucket" >/dev/null || die "cannot list r2:$bucket (account ID in the endpoint, key, bucket name?)"
ok "listed r2:$bucket"

obj=.r2-check-$(date +%s)
body="ttsnap producer check $(date -u +%FT%TZ) 0123456789"
rclone rcat "r2:$bucket/$obj" <<<"$body" || die "cannot write to r2:$bucket (does the token have Object Read & Write on it?)"
trap 'rclone deletefile "r2:$bucket/$obj" || true' EXIT
ok "wrote $obj"

got=$(curl -fsS --max-time 30 "$url/$obj") || die "GET $url/$obj failed (custom domain connected and active?)"
[[ $got == "$body" ]] || die "GET $url/$obj returned something else"
ok "GET $url/$obj"

code=$(curl -sS -o /dev/null -w '%{http_code}' -r 5-14 --max-time 30 "$url/$obj") || die "range request to $url/$obj failed"
[[ $code == 206 ]] || die "a range request returned HTTP $code, not 206"
ok "range request (206)"

code=$(curl -sS -o /dev/null -w '%{http_code}' --max-time 30 "$url/snapshots/no-such-file") || die "GET $url/snapshots/no-such-file failed"
[[ $code == 404 ]] || echo "warning: a missing object returned HTTP $code, not 404" >&2
echo "R2 is ready"
