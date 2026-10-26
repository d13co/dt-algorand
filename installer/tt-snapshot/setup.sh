#!/usr/bin/env bash
# Install or update the ttsnap producer service on this host (see README.md):
#
#   - the given ttsnap, scripts/dt_snapshot.sh and this directory's upload, check and trigger
#     scripts into /usr/local/lib/tt-snapshot
#   - /etc/tt-snapshot.env from tt-snapshot.env; an existing one is kept, and gets the settings it
#     lacks
#   - tt-snapshot.service, tt-snapshot-upload.service and tt-snapshot-trigger.service/.timer, which
#     start a snapshot once per catchpoint and an upload when the bucket lags (the timer is
#     installed, not enabled; an earlier daily tt-snapshot.timer is removed), with mount
#     dependencies for the env file's ROOT and OUT: rerun it after changing them
#   - rclone from rclone.org, signature checked, when it is missing or older than 1.59 (Ubuntu
#     22.04's has no R2 support), and jq
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
units=/etc/systemd/system
# rclone's release key (https://rclone.org/KEYS), which signs each release's SHA256SUMS
rclone_key=FBF737ECE9F8AB18604BD2AC93935E02FF3B54FA

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }
warn() { echo "warning: $*" >&2; }

case ${1:-} in -h | --help | '') sed -n '2,18s/^# \{0,1\}//p' "$0"; exit 2 ;; esac
ttsnap=$1
[[ $EUID -eq 0 ]] || die "run as root: sudo $0 $ttsnap"
[[ -x $ttsnap ]] || die "$ttsnap is not an executable"
usage=$("$ttsnap" 2>&1 || true)
grep -q 'Falcon-1024' <<<"$usage" || die "$ttsnap does not sign with Falcon-1024: use dtalgorand 5.0.4-dt3 or later, or build it from this branch"
for f in "$repo/scripts/dt_snapshot.sh" "$here"/{upload.sh,r2-check.sh,trigger.sh,tt-snapshot.env} \
  "$here"/{tt-snapshot.service,tt-snapshot-upload.service,tt-snapshot-trigger.service,tt-snapshot-trigger.timer}; do
  [[ -f $f ]] || die "$f is missing"
done

# --- rclone and jq --------------------------------------------------------------------------------
rclone_ok() {
  local v
  v=$(rclone version 2>/dev/null | sed -n '1s/^rclone v\([0-9]*\.[0-9]*\).*/\1/p') || return 1
  [[ -n $v ]] && printf '1.59\n%s\n' "$v" | sort -V -C
}
if ! rclone_ok; then
  case $(dpkg --print-architecture) in
    amd64) arch=amd64 ;;
    arm64) arch=arm64 ;;
    armhf) arch=arm-v7 ;;
    i386) arch=386 ;;
    *) die "rclone.org has no .deb for $(dpkg --print-architecture): install rclone 1.59 or later yourself" ;;
  esac
  command -v gpg >/dev/null && command -v gpgv >/dev/null || die "gpg or gpgv not found (apt-get install gnupg): they check the rclone download"
  ver=$(curl -fsS --max-time 30 https://downloads.rclone.org/version.txt | awk '{print $2}')
  [[ $ver == v[0-9]* ]] || die "could not look up the current rclone version"
  deb=rclone-$ver-linux-$arch.deb
  tmp=$(mktemp -d)
  trap 'rm -rf "$tmp"' EXIT
  log "installing rclone $ver from downloads.rclone.org"
  curl -fsSL --max-time 300 -o "$tmp/$deb" "https://downloads.rclone.org/$ver/$deb"
  curl -fsSL --max-time 30 -o "$tmp/SHA256SUMS" "https://downloads.rclone.org/$ver/SHA256SUMS"
  curl -fsSL --max-time 30 -o "$tmp/KEYS" https://rclone.org/KEYS
  # gpgv needs no keyring setup or agent; -o writes out only the signed text, so lines added around
  # it can't name a checksum
  install -d -m 700 "$tmp/gnupg"
  export GNUPGHOME=$tmp/gnupg
  gpg --batch --dearmor <"$tmp/KEYS" >"$tmp/keys.gpg" || die "could not read rclone's KEYS"
  status=$(gpgv --keyring "$tmp/keys.gpg" --status-fd 1 -o "$tmp/sums" "$tmp/SHA256SUMS" 2>/dev/null) ||
    die "SHA256SUMS has no good signature"
  grep -q "^\[GNUPG:\] VALIDSIG .* $rclone_key\$" <<<"$status" || die "SHA256SUMS is not signed by rclone's release key $rclone_key"
  (cd "$tmp" && grep " $deb\$" sums | sha256sum -c -) || die "$deb does not match the signed SHA256SUMS"
  dpkg -i "$tmp/$deb" >/dev/null
fi
log "$(rclone version | head -1)"
if ! command -v jq >/dev/null; then
  log "installing jq"
  apt-get install -y -qq jq >/dev/null || die "could not install jq: apt-get install jq"
fi

# --- scripts and ttsnap ---------------------------------------------------------------------------
log "installing $lib"
install -d -m 755 "$lib"
# the installed one may be passed, to keep it
[[ $ttsnap -ef $lib/ttsnap ]] || install -m 755 "$ttsnap" "$lib/ttsnap"
install -m 755 "$repo/scripts/dt_snapshot.sh" "$here/upload.sh" "$here/r2-check.sh" "$here/trigger.sh" "$lib/"

# --- env file -------------------------------------------------------------------------------------
names() { sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$1" | sort -u; }
if [[ -e $env ]]; then
  # compare names only: the file holds the R2 credentials
  log "keeping $env"
  added=
  for name in $(comm -13 <(names "$env") <(names "$here/tt-snapshot.env")); do
    [[ -n $added ]] || printf '\n# added by tt-snapshot setup.sh on %s\n' "$(date -u +%F)" >>"$env"
    line=$(grep -m1 "^$name=" "$here/tt-snapshot.env")
    echo "$line" >>"$env"
    log "added to $env: $line"
    added=1
  done
  extra=$(comm -23 <(names "$env") <(names "$here/tt-snapshot.env") | xargs)
  [[ -z $extra ]] || log "$env also sets $extra"
else
  log "installing $env (fill in the R2 credentials)"
  install -m 600 -o root -g root "$here/tt-snapshot.env" "$env"
fi
set -a
# shellcheck source=tt-snapshot.env
. "$env"
set +a
root=${ROOT:-/mnt/dtalgod-mainnet-hot-root}
out=${OUT:-}
key=${KEY:-}

# --- units ----------------------------------------------------------------------------------------
log "installing tt-snapshot.service, tt-snapshot-upload.service and tt-snapshot-trigger.service/.timer"
install -m 644 "$here"/{tt-snapshot.service,tt-snapshot-upload.service,tt-snapshot-trigger.service,tt-snapshot-trigger.timer} "$units/"
# systemd can't read paths from the env file
for u in tt-snapshot tt-snapshot-upload; do
  install -d -m 755 "$units/$u.service.d"
  printf '# written by tt-snapshot setup.sh from %s: rerun it after changing ROOT or OUT\n[Unit]\nRequiresMountsFor=%s\n' \
    "$env" "$root${out:+ $out}" >"$units/$u.service.d/paths.conf"
done
# the first version ran daily from tt-snapshot.timer: the trigger replaces it
if [[ -e $units/tt-snapshot.timer ]]; then
  log "removing the daily tt-snapshot.timer (tt-snapshot-trigger.timer replaces it)"
  systemctl disable --now tt-snapshot.timer 2>/dev/null || true
  rm -f "$units/tt-snapshot.timer"
fi
systemctl daemon-reload

# --- checks ---------------------------------------------------------------------------------------
if [[ -z $key ]]; then
  warn "KEY is not set in $env: set it to the producer key (ttsnap keygen makes one)"
elif [[ ! -f $key ]]; then
  warn "no signing key at $key: set KEY in $env to the producer key (ttsnap keygen makes one)"
elif ! head -c 23 "$key" | grep -qx 'ttsnap-falcon1024-seed '; then
  warn "$key is not a ttsnap Falcon key (ttsnap keygen makes one)"
else
  chmod 600 "$key"
  log "signing key $key found"
fi
btrfs subvolume show "$root/@live" >/dev/null 2>&1 || warn "$root/@live is not a btrfs subvolume (scripts/btrfs_hot_ledger.sh lays it out)"
if [[ -z $out ]]; then
  warn "OUT is not set in $env"
else
  [[ -d $out ]] || warn "$out does not exist"
  at=$out
  until [[ -d $at ]]; do at=$(dirname "$at"); done
  avail=$(df --output=avail -BG "$at" | tail -1 | tr -dc 0-9)
  ((avail >= 80)) || warn "only ${avail}G free for $out: the first publish copies the whole history (~70G on mainnet) into it"
fi

if systemctl is-enabled -q tt-snapshot-trigger.timer 2>/dev/null; then
  log "updated; tt-snapshot-trigger.timer is enabled"
  exit 0
fi
cat <<EOF

Installed. Next:
  1. sudoedit $env
       the R2 account ID, access key ID and secret
  2. sudo $lib/r2-check.sh
       checks that the credentials, the custom domain and range requests work
  3. sudo systemctl start --no-block tt-snapshot.service && journalctl -fu tt-snapshot
       the first snapshot and upload (the whole history)
  4. sudo systemctl enable --now tt-snapshot-trigger.timer
       a snapshot after each catchpoint's balances round, and uploads the bucket lacks
EOF
