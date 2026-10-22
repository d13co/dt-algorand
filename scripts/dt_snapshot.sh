#!/usr/bin/env bash
# Snapshot a running time travel node's ledger and publish its history with tools/ttsnap.
#
#   1. btrfs subvolume snapshot of ROOT/@live (the layout scripts/btrfs_hot_ledger.sh makes) into
#      ROOT/@snap/<UTC time>, while algod runs: the snapshot is atomic, and a ledger copied at any
#      instant opens like one after a crash (blocks are written before the trackers commit them,
#      the history is synced before the tracker DB commits)
#   2. ttsnap prepare on the snapshot: the history's WAL is flushed into tables and the SQLite WALs
#      folded into their DBs, so the published files are self-contained
#   3. ttsnap publish into OUT, signed with KEY: the history (FULL=1: also the whole ledger, for
#      operators who trust this producer's state)
#   4. make the snapshot read-only, keep the newest KEEP snapshots, ttsnap gc OUT (keeping GC_KEEP
#      snapshots of each kind), then run UPLOAD if set (e.g. an rclone sync of OUT to a bucket)
#
# Run it as root from a systemd timer, e.g. hourly; runs don't overlap (flock). Snapshots pin the
# blocks the node has rewritten since they were taken, so keep KEEP small.
#
# Usage: dt_snapshot.sh
# Env: OUT and KEY (required), ROOT (default /mnt/dtalgod-mainnet-hot-root), TTSNAP (default
# ttsnap on PATH, else /usr/bin/ttsnap), FULL, KEEP (default 2), GC_KEEP (default 3),
# UPLOAD (a shell command run after a successful publish, with OUT exported), LEDGER (the genesis
# dir's path inside @live, when @live holds a whole data dir; found by itself if there is just one)
set -euo pipefail

case ${1:-} in -h | --help) sed -n '2,23s/^# \{0,1\}//p' "$0"; exit 2 ;; esac

root=${ROOT:-/mnt/dtalgod-mainnet-hot-root}
root=${root%/}
out=${OUT:?set OUT to the snapshot tree}
key=${KEY:?set KEY to the ssh key that signs manifests}
keep=${KEEP:-2}
gc_keep=${GC_KEEP:-3}
full=${FULL:-}
ttsnap=${TTSNAP:-$(command -v ttsnap || echo /usr/bin/ttsnap)}

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $(date -u +%FT%TZ) $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
command -v btrfs >/dev/null || die "btrfs not found"
[[ -x $ttsnap ]] || die "ttsnap not found (set TTSNAP)"
btrfs subvolume show "$root/@live" >/dev/null 2>&1 || die "$root/@live is not a btrfs subvolume: lay the ledger out with scripts/btrfs_hot_ledger.sh"
[[ -d $root/@snap ]] || die "$root/@snap does not exist"
# @live is the genesis dir, or a data dir holding it (an image laid out on the data dir)
sub=${LEDGER:-}
sub=${sub%/}
if [[ -z $sub && ! -d $root/@live/ledger.timetravel ]]; then
  mapfile -t found < <(find "$root/@live" -mindepth 2 -maxdepth 2 -type d -name ledger.timetravel -printf '%h\n')
  ((${#found[@]} <= 1)) || die "$root/@live holds several genesis dirs (${found[*]}); set LEDGER to one"
  ((${#found[@]} == 0)) || sub=${found[0]#"$root/@live/"}
fi
[[ -d $root/@live/${sub:+$sub/}ledger.timetravel ]] || die "$root/@live${sub:+/$sub} holds no time travel store: is TimeTravelDBDir set elsewhere? Keep the store in the genesis dir so one snapshot covers it"
((keep >= 1)) || die "KEEP must be at least 1"

exec 9>"$root/@snap/.lock"
flock -n 9 || die "another dt_snapshot.sh is running"

snap=$root/@snap/$(date -u +%Y%m%dT%H%M%SZ)
ledger=$snap${sub:+/$sub}
log "snapshotting $root/@live to $snap"
btrfs subvolume snapshot "$root/@live" "$snap" >/dev/null

# a snapshot whose prepare or publish failed is kept for a look; later runs prune it with the rest
trap 'echo "error: failed; the snapshot is kept at $snap" >&2' ERR

log "preparing $snap"
"$ttsnap" prepare ${full:+-full} "$ledger"
log "publishing the history to $out"
"$ttsnap" publish -o "$out" -key "$key" "$ledger"
if [[ -n $full ]]; then
  log "publishing the whole ledger to $out"
  "$ttsnap" publish -o "$out" -key "$key" -full "$ledger"
fi
btrfs property set -ts "$snap" ro true
trap - ERR

# the newest KEEP snapshots stay; names sort by time
mapfile -t snaps < <(find "$root/@snap" -mindepth 1 -maxdepth 1 -type d -name '2*Z' -printf '%f\n' | sort)
for ((i = 0; i < ${#snaps[@]} - keep; i++)); do
  log "deleting old snapshot ${snaps[i]}"
  btrfs subvolume delete "$root/@snap/${snaps[i]}" >/dev/null
done

"$ttsnap" gc -o "$out" -keep "$gc_keep"
if [[ -n ${UPLOAD:-} ]]; then
  log "uploading"
  OUT=$out bash -c "$UPLOAD"
fi
log "done"
