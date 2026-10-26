#!/usr/bin/env bash
# Install or update the ttsnap producer service on this host (see README.md):
#
#   - the given ttsnap, scripts/dt_snapshot.sh and this directory's upload, check and trigger scripts
#     into /usr/local/lib/tt-snapshot
#   - /etc/tt-snapshot.env from tt-snapshot.env, unless it exists (an existing one is kept)
#   - tt-snapshot.service, and tt-snapshot-trigger.service/.timer, which start a snapshot once per
#     catchpoint (the timer is installed, not enabled; an earlier daily tt-snapshot.timer is removed)
#   - rclone from rclone.org, when it is missing or older than 1.59 (Ubuntu 22.04's has no R2 support)
#
# Pass a ttsnap that signs with Falcon-1024: the dtalgorand package's /usr/bin/ttsnap from 5.0.4-dt3
# on, or one built from this branch (make install puts it in $(go env GOPATH)/bin):
#
#   sudo installer/tt-snapshot/setup.sh <path to ttsnap>
set -euo pipefail

here=$(cd "$(dirname "$0")" && pwd)
repo=$(cd "$here/../.." && pwd)
lib=/usr/local/lib/tt-snapshot
env=/etc/tt-snapshot.env

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }
warn() { echo "warning: $*" >&2; }

case ${1:-} in -h | --help | '') sed -n '2,14s/^# \{0,1\}//p' "$0"; exit 2 ;; esac
ttsnap=$1
[[ $EUID -eq 0 ]] || die "run as root: sudo $0 $ttsnap"
[[ -x $ttsnap ]] || die "$ttsnap is not an executable"
usage=$("$ttsnap" 2>&1 || true)
grep -q 'Falcon-1024' <<<"$usage" || die "$ttsnap does not sign with Falcon-1024: use dtalgorand 5.0.4-dt3 or later, or build it from this branch"
for f in "$repo/scripts/dt_snapshot.sh" "$here/upload.sh" "$here/r2-check.sh" "$here/trigger.sh" \
  "$here/tt-snapshot.env" "$here/tt-snapshot.service" "$here/tt-snapshot-trigger.service" "$here/tt-snapshot-trigger.timer"; do
  [[ -f $f ]] || die "$f is missing"
done

# --- rclone ---------------------------------------------------------------------------------------
rclone_ok() {
  local v
  v=$(rclone version 2>/dev/null | sed -n '1s/^rclone v\([0-9]*\.[0-9]*\).*/\1/p') || return 1
  [[ -n $v ]] && printf '1.59\n%s\n' "$v" | sort -V -C
}
if ! rclone_ok; then
  ver=$(curl -fsS --max-time 30 https://downloads.rclone.org/version.txt | awk '{print $2}')
  [[ $ver == v[0-9]* ]] || die "could not look up the current rclone version"
  deb=rclone-$ver-linux-amd64.deb
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  log "installing rclone $ver from downloads.rclone.org"
  curl -fsSL --max-time 300 -o "$tmp/$deb" "https://downloads.rclone.org/$ver/$deb"
  curl -fsSL --max-time 30 -o "$tmp/SHA256SUMS" "https://downloads.rclone.org/$ver/SHA256SUMS"
  (cd "$tmp" && grep " $deb\$" SHA256SUMS | sha256sum -c -) || die "$deb does not match its SHA256SUMS"
  dpkg -i "$tmp/$deb" >/dev/null
fi
log "$(rclone version | head -1)"

# --- scripts and ttsnap ---------------------------------------------------------------------------
log "installing $lib"
install -d -m 755 "$lib"
install -m 755 "$ttsnap" "$lib/ttsnap"
install -m 755 "$repo/scripts/dt_snapshot.sh" "$here/upload.sh" "$here/r2-check.sh" "$here/trigger.sh" "$lib/"

if [[ -e $env ]]; then
  log "keeping the existing $env; the shipped one differs:"
  diff "$env" "$here/tt-snapshot.env" || true
else
  log "installing $env (fill in the R2 credentials)"
  install -m 600 -o root -g root "$here/tt-snapshot.env" "$env"
fi

log "installing tt-snapshot.service and tt-snapshot-trigger.service/.timer"
install -m 644 "$here/tt-snapshot.service" "$here/tt-snapshot-trigger.service" "$here/tt-snapshot-trigger.timer" /etc/systemd/system/
# the first version ran daily from tt-snapshot.timer: the trigger replaces it
if [[ -e /etc/systemd/system/tt-snapshot.timer ]]; then
  log "removing the daily tt-snapshot.timer (tt-snapshot-trigger.timer replaces it)"
  systemctl disable --now tt-snapshot.timer 2>/dev/null || true
  rm -f /etc/systemd/system/tt-snapshot.timer
fi
systemctl daemon-reload

# --- checks ---------------------------------------------------------------------------------------
set -a
# shellcheck source=tt-snapshot.env
. "$env"
set +a
if [[ ! -f $KEY ]]; then
  warn "no signing key at $KEY: set KEY in $env to the producer key (ttsnap keygen makes one)"
elif ! head -c 23 "$KEY" | grep -qx 'ttsnap-falcon1024-seed '; then
  warn "$KEY is not a ttsnap Falcon key (ttsnap keygen makes one)"
else
  chmod 600 "$KEY"
  log "signing key $KEY found"
fi
btrfs subvolume show "$ROOT/@live" >/dev/null 2>&1 || warn "$ROOT/@live is not a btrfs subvolume (scripts/btrfs_hot_ledger.sh lays it out)"
[[ -d $OUT ]] || warn "$OUT does not exist"
avail=$(df --output=avail -BG / | tail -1 | tr -dc 0-9)
((avail >= 80)) || warn "only ${avail}G free on /: the first publish copies the whole history (~70G on mainnet) into $OUT"

if systemctl is-enabled -q tt-snapshot-trigger.timer 2>/dev/null; then
  log "updated; tt-snapshot-trigger.timer is enabled"
  exit 0
fi
cat <<EOF

Installed. Next:
  1. sudoedit $env                        R2 account ID, access key ID and secret
  2. sudo $lib/r2-check.sh                credentials, custom domain and range requests work
  3. sudo systemctl start --no-block tt-snapshot.service && journalctl -fu tt-snapshot
                                          first snapshot and upload (the whole history)
  4. sudo systemctl enable --now tt-snapshot-trigger.timer
                                          a snapshot after each catchpoint's balances round
EOF
