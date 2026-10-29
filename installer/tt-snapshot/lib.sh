# Shared by setup.sh, trigger.sh, upload.sh and r2-check.sh, which source it from their directory.
# shellcheck shell=bash

# what setup.sh and upload.sh record (see below)
state_dir=/var/lib/tt-snapshot
# shellcheck disable=SC2034 # used by the scripts that source this
uploaded=$state_dir/uploaded

# the names an env file sets, on its plain NAME=value lines
env_names() { sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$1"; }

# the env file's settings, except those the environment sets
load_env() {
  local f=$1 name kv keep=()
  [[ -r $f ]] || return 0
  for name in $(env_names "$f"); do
    if [[ -v $name ]]; then keep+=("$name=${!name}"); fi
  done
  set -a
  # shellcheck source=tt-snapshot.env
  . "$f"
  set +a
  # shellcheck disable=SC2163 # kv is NAME=value
  for kv in "${keep[@]}"; do export "$kv"; done
}

# OUT's ($1) latest-<kind> pointers, but those an interrupted publish left (latest-<kind>.tmp)
pointers() {
  local p
  for p in "$1"/snapshots/*/latest-*; do
    if [[ -f $p && $p != *.tmp ]]; then echo "$p"; fi
  done
}

# A digest of what upload.sh mirrors: the bucket ($2), and the path, size and mtime of each file in
# OUT ($1) but the *.tmp files of interrupted publishes. upload.sh writes it to $uploaded when it
# completes, and the trigger starts an upload when OUT's differs. -H: OUT may be a symlink, which
# rclone follows.
mirror_digest() {
  {
    echo "$2"
    find -H "$1" -type f ! -name '*.tmp' ! -name '*.tmp.sig' -printf '%P %s %T@\n' | LC_ALL=C sort
  } | sha256sum | cut -c1-16
}
