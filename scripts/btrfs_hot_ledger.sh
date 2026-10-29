#!/usr/bin/env bash
# Move a node's ledger directory onto a btrfs filesystem in a loopback image file, laid out so that
# scripts/dt_snapshot.sh can snapshot it while the node runs.
#
#   1. create the sparse image (SIZE caps how large it can grow, default 100G; suffixes K/M/G/T)
#      and mkfs.btrfs it, unless it already exists
#   2. move TARGET aside to TARGET-temp
#   3. mount the image's top level on ROOT (fstab entry with loop,nofail), with the ledger in its
#      @live subvolume and room for snapshots in @snap, and bind-mount @live on TARGET (fstab too;
#      the bare mountpoint is made immutable so a node started without the mount fails instead of
#      filling the parent disk). Snapshots stay out of TARGET.
#   4. move the files from TARGET-temp into TARGET, then remove TARGET-temp. @live is chattr +C and
#      the image is mounted nodatacow, so the files are not copy-on-write, except that a block
#      shared with a snapshot is copied once when the node next writes it.
#
# An image from an earlier version of this script, with the ledger in its top level mounted on
# TARGET, is converted: its top level is snapshotted into @live (no data is copied), the originals
# are removed from the top level, and fstab is rewritten. A conversion that stopped resumes.
#
# Safe to re-run after a failure: each step is skipped when already done, and rsync resumes.
# Refuses to run while any process has files open under TARGET; set SERVICE to have the script
# stop the node first, start it at the end, and order the unit after the mount (RequiresMountsFor).
#
# Keep the time travel store in TARGET (no TimeTravelDBDir elsewhere): one snapshot then covers the
# tracker DB, the blocks and the history at the same instant.
#
# Usage: btrfs_hot_ledger.sh [SIZE]
# Env overrides: IMG (default /mnt/dtalgod-mainnet-hot.img), TARGET (default
# /var/lib/algorand/mainnet-v1.0), ROOT (default /mnt/<IMG name>-root), MOUNT_OPTS (default
# noatime,nodatacow), SERVICE (systemd unit)
set -euo pipefail

img=${IMG:-/mnt/dtalgod-mainnet-hot.img}
target=${TARGET:-/var/lib/algorand/mainnet-v1.0}
target=${target%/}
root=${ROOT:-/mnt/$(basename "$img" .img)-root}
root=${root%/}
temp=$target-temp
opts=${MOUNT_OPTS:-noatime,nodatacow}
service=${SERVICE:-}
[[ -n $service && $service != *.* ]] && service=$service.service
size=${1:-100G}

case $size in -h | --help) sed -n '2,32s/^# \{0,1\}//p' "$0"; exit 2 ;; esac

die() { echo "error: $*" >&2; exit 1; }
log() { echo "==> $*"; }

[[ $EUID -eq 0 ]] || die "must run as root"
for cmd in mkfs.btrfs btrfs blkid truncate rsync mountpoint numfmt chattr findmnt losetup; do
  command -v "$cmd" >/dev/null || die "$cmd not found"
done
case $root in "$target" | "$target"/*) die "ROOT ($root) must not be inside TARGET ($target)" ;; esac
case $img in "$root"/*) die "IMG ($img) must not be inside ROOT ($root)" ;; esac

# PIDs of processes with an open file or cwd under $target or $temp.
holders() {
  local p l
  for p in /proc/[0-9]*/fd/* /proc/[0-9]*/cwd; do
    l=$(readlink "$p" 2>/dev/null) || continue
    case $l in "$target" | "$target"/* | "$temp" | "$temp"/*) p=${p#/proc/}; echo "${p%%/*}" ;; esac
  done | sort -un
}

# fstab_has MOUNTPOINT: whether an uncommented fstab entry mounts something on it.
fstab_has() { awk -v t="$1" '$1 !~ /^#/ && $2 == t { f = 1 } END { exit !f }' /etc/fstab; }

# make_mountpoint DIR: an empty, immutable directory to mount on.
make_mountpoint() {
  mkdir -p "$1"
  chattr +i "$1" 2>/dev/null || echo "warning: could not make bare mountpoint $1 immutable" >&2
}

# --- 1. image ---------------------------------------------------------------------------------
if [[ -e $img ]]; then
  [[ $(blkid -o value -s TYPE "$img" 2>/dev/null) == btrfs ]] || die "$img exists and is not a btrfs image"
  log "reusing existing btrfs image $img"
else
  bytes=$(numfmt --from=iec "${size%B}") || die "bad SIZE: $size"
  log "creating sparse $size image $img"
  touch "$img"
  chattr +C "$img" 2>/dev/null || true # no CoW for the image if /mnt is itself btrfs
  truncate -s "$bytes" "$img"
  mkfs.btrfs -q -L "$(basename "$img" .img)" "$img"
fi

# --- 2. stop the node, move the existing directory aside ----------------------------------------
if [[ -n $service ]] && systemctl is-active -q "$service"; then
  log "stopping $service"
  systemctl stop "$service"
fi
pids=$(holders)
if [[ -n $pids ]]; then
  ps -o pid,user,cmd -p "${pids//$'\n'/,}" >&2
  die "processes above have files open under $target; stop the node (or set SERVICE)"
fi

# from_img MOUNTPOINT: whether it is mounted from a loop device of $img.
from_img() {
  local s
  s=$(findmnt -no SOURCE "$1") || return 1
  s=${s%%\[*}
  losetup -j "$img" -O NAME -n | grep -qx "$s"
}

# The image's top level on $target is the earlier layout, or a conversion that stopped; it is
# converted below. Everything here is decided from what is mounted and what the image holds, so a
# run that stopped anywhere resumes.
top_on_target=
if mountpoint -q "$target"; then
  if [[ $(findmnt -no FSROOT "$target") == /@live ]]; then
    log "$target is already mounted from @live"
  elif from_img "$target" && [[ $(findmnt -no FSROOT "$target") == / ]]; then
    log "$target holds the image's top level: converting it to the @live subvolume"
    top_on_target=1
  else
    die "$target is mounted from something other than $img"
  fi
elif [[ -e $temp ]]; then
  # A previous run moved the directory aside; $target must be the bare mountpoint it created.
  [[ ! -e $target || -z $(ls -A "$target") ]] || die "both $target and $temp exist and $target is not empty"
elif [[ -d $target && -z $(ls -A "$target") ]]; then
  # Bare mountpoint left by a completed run whose image is not mounted (e.g. it failed at boot).
  log "$target is an empty mountpoint"
else
  [[ -d $target ]] || die "$target does not exist"
  log "moving $target to $temp"
  mv -T "$target" "$temp"
fi

# --- 3. fstab and mounts ------------------------------------------------------------------------
# One rewrite: the image's top level on $root, @live bind-mounted on $target, and no other entry
# for $target (the earlier layout's).
root_line=$(printf '%s %s btrfs loop,nofail,subvolid=5,%s 0 0' "$img" "$root" "$opts")
bind_line=$(printf '%s %s none bind,nofail,x-systemd.requires-mounts-for=%s 0 0' "$root/@live" "$target" "$root")
awk -v t="$target" -v r="$root" -v live="$root/@live" \
  '$1 ~ /^#/ || !(($2 == t && $1 != live) ) { print }' /etc/fstab >/etc/fstab.new
fstab_has "$root" || echo "$root_line" >>/etc/fstab.new
awk -v t="$target" '$1 !~ /^#/ && $2 == t { f = 1 } END { exit !f }' /etc/fstab.new || echo "$bind_line" >>/etc/fstab.new
if ! cmp -s /etc/fstab /etc/fstab.new; then
  log "updating /etc/fstab"
  diff /etc/fstab /etc/fstab.new || true
  mv /etc/fstab.new /etc/fstab
  systemctl daemon-reload
else
  rm /etc/fstab.new
fi

if ! mountpoint -q "$root"; then
  make_mountpoint "$root"
  if [[ -n $top_on_target ]]; then
    # the image is mounted already: mount the same device, not a second loop of the file
    dev=$(findmnt -no SOURCE "$target")
    dev=${dev%%\[*}
    log "mounting $dev (top level) on $root"
    mount -o "subvolid=5,$opts" "$dev" "$root"
  else
    log "mounting $img on $root"
    mount "$root"
  fi
fi
mountpoint -q "$root" || die "$root did not mount"
[[ $(findmnt -no FSTYPE "$root") == btrfs ]] || die "$root is mounted but is not btrfs"
# ROOT must be this image's top level: the conversion below deletes files there
from_img "$root" || die "$root is mounted from $(findmnt -no SOURCE "$root"), not from $img"
[[ $(findmnt -no FSROOT "$root") == / ]] || die "$root is not the top level of $img"

content=$(find "$root" -mindepth 1 -maxdepth 1 ! -name @live ! -name @snap -printf '%f\n' | sort)
if ! btrfs subvolume show "$root/@live" >/dev/null 2>&1; then
  if [[ -n $content ]]; then
    # the earlier layout: the ledger in the top level
    # a snapshot holds nested subvolumes as empty directories, and mounts below ROOT not at all:
    # their originals must not be removed
    nested=$(btrfs subvolume list -o "$root" | awk '{print $NF}' | grep -vx -e @live -e @snap || true)
    [[ -z $nested ]] || die "$root holds other subvolumes ($nested); convert it by hand"
    mounts=$(findmnt -R -n -o TARGET "$root" | grep -vx "$root" || true)
    [[ -z $mounts ]] || die "filesystems are mounted below $root ($mounts); unmount them first"
    log "snapshotting the top level into @live"
    btrfs subvolume snapshot "$root" "$root/@live" >/dev/null
    [[ $(find "$root/@live" -mindepth 1 -maxdepth 1 -printf '%f\n' | sort) == "$content" ]] ||
      die "$root/@live does not hold what the top level does; nothing was removed"
    content=
    log "removing the originals from the top level (@live shares their data)"
    find "$root" -mindepth 1 -maxdepth 1 ! -name @live ! -name @snap -exec rm -rf --one-file-system -- {} +
  else
    log "creating subvolume $root/@live"
    btrfs subvolume create "$root/@live" >/dev/null
  fi
fi
[[ -z $content ]] || echo "warning: $root holds files outside @live and @snap (an interrupted conversion's originals?): $content" >&2
mkdir -p "$root/@snap"
chattr +C "$root/@live" # new files inherit no-CoW, also if mounted without nodatacow

if mountpoint -q "$target" && [[ $(findmnt -no FSROOT "$target") != /@live ]]; then
  log "unmounting the image's top level from $target"
  umount "$target"
fi
if ! mountpoint -q "$target"; then
  [[ -d $target ]] || make_mountpoint "$target"
  log "bind-mounting $root/@live on $target"
  mount "$target"
fi
mountpoint -q "$target" || die "$target did not mount"
[[ $(findmnt -no FSROOT "$target") == /@live ]] || die "$target is not mounted from @live"

# --- 4. move the files in -----------------------------------------------------------------------
if [[ -e $temp ]]; then
  chown --reference="$temp" "$target"
  chmod --reference="$temp" "$target"

  # The image grows as files land in it. When it shares a filesystem with $temp, each file's source
  # is removed once copied, so the copy needs room for the largest file; otherwise for all of it.
  margin=$((2 << 30))
  need=$(du -sb "$temp" | cut -f1)
  fs_avail=$(df --output=avail -B1 "$target" | tail -1)
  ((need + margin < fs_avail)) ||
    die "$temp holds $(numfmt --to=iec "$need"), image has $(numfmt --to=iec "$fs_avail") free"
  if [[ $(stat -c %d "$(dirname "$img")") == $(stat -c %d "$temp") ]]; then
    need=$(find "$temp" -type f -printf '%s\n' | sort -n | tail -1)
  fi
  host_avail=$(df --output=avail -B1 "$(dirname "$img")" | tail -1)
  ((${need:-0} + margin < host_avail)) ||
    die "$(dirname "$img") has $(numfmt --to=iec "$host_avail") free, the move needs $(numfmt --to=iec "${need:-0}") + 2G"

  log "moving files from $temp into $target"
  rsync -aHAX --numeric-ids --remove-source-files --info=progress2 "$temp"/ "$target"/
  find "$temp" -depth -type d -empty -delete
  [[ ! -e $temp ]] || die "$temp still has entries after the move: $(find "$temp" | head)"
fi

if [[ -n $service ]]; then
  dropin=/etc/systemd/system/$service.d/requires-hot-mount.conf
  if [[ ! -e $dropin ]]; then
    log "adding $dropin"
    mkdir -p "$(dirname "$dropin")"
    printf '[Unit]\nRequiresMountsFor=%s\n' "$target" >"$dropin"
    systemctl daemon-reload
  fi
  log "starting $service"
  systemctl start "$service"
fi

df -h "$target"
log "done; snapshots go in $root/@snap (scripts/dt_snapshot.sh)"
