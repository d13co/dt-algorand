#!/bin/sh
# Add .debs to the apt repo at https://apt.d13.co, then rebuild and sign its index.
# Usage: apt-publish.sh [pkg.deb ...]    (no args: just re-index and re-sign)
#
# The release workflow runs it on every tag; ~/code/infra/apt/publish.sh runs it by hand
# (the algorand .debs). Either way it first pulls the bucket's pools, so the index lists
# every package in the bucket, not only the ones on this machine.
#
# Two suites: stable lists pool/, and beta lists pool/ and pool-beta/, so that beta users
# also get each release (a beta, x.y.z-dt<n>~beta<m>, sorts before x.y.z-dt<n>). The .debs
# given here go to APT_SUITE's pool, and every run rebuilds and signs both suites. (An older
# copy of this script that knows only stable leaves dists/beta as it was: beta then lacks the
# .debs that copy added until the next run of this one.)
#
#   APT_SUITE            stable (default) or beta: where the given .debs go
#   APT_REPO             local copy of the repo tree (default: ./apt-repo)
#   R2_ACCOUNT_ID        and aws credentials for the "apt" bucket (AWS_PROFILE=r2 by hand,
#                        AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY in CI)
#   GPG_PASSPHRASE_FILE  signing subkey passphrase, for gpg without a pinentry (CI)
set -eu
KEY=5B708A29E1D79C75016BBCE4B4C52CB3D4B85B22!   # CI signing subkey
PRIMARY=F66E77133063650C159F405AC4D2171FB5B60471 # its primary key, as install.sh pins it
REPO=${APT_REPO:-apt-repo}
ARCHES="amd64 arm64"
SUITES="stable beta"
BUCKET=s3://apt   # R2 bucket, served at https://apt.d13.co
R2="--endpoint-url https://${R2_ACCOUNT_ID:?set R2_ACCOUNT_ID}.r2.cloudflarestorage.com --only-show-errors"
export AWS_DEFAULT_REGION=auto

SUITE=${APT_SUITE:-stable}
case $SUITE in
stable|beta) ;;
*) echo "apt-publish: APT_SUITE is stable or beta, not $SUITE" >&2; exit 2 ;;
esac
# the pools a suite lists, and the one its given .debs go to (and whose files its index vouches for)
pools() { case $1 in stable) echo pool ;; beta) echo pool pool-beta ;; esac; }
own() { case $1 in stable) echo pool ;; beta) echo pool-beta ;; esac; }

mkdir -p "$REPO/pool" "$REPO/pool-beta" "$REPO/old"
# ponytail: pulls the whole pool (algorand .debs are ~160MB each); merge Packages stanzas
# instead once that gets slow.
aws s3 sync $BUCKET/pool "$REPO/pool" $R2
aws s3 sync $BUCKET/pool-beta "$REPO/pool-beta" $R2

# Everything in the pools gets signed below, and the R2 token alone can write them. So only
# sign what the suites' current signed indexes list, plus the .debs given here: a file in
# pool/ must be in stable's index, one in pool-beta/ in beta's. (Beta's index lists pool/
# too, but only stable's vouches for pool/, so a beta .deb moved there doesn't reach stable.)
: >"$REPO/old/given"
if [ $# -gt 0 ]; then sha256sum "$@" | cut -d' ' -f1 >>"$REPO/old/given"; fi
for s in $SUITES; do
  o=$REPO/old/$s
  mkdir -p "$o"
  : >"$o/allowed"
  if [ "$s" = "$SUITE" ]; then cat "$REPO/old/given" >>"$o/allowed"; fi
  if [ -z "$(find "$REPO/$(own $s)" -type f)" ] && ! aws s3 ls $BUCKET/dists/$s/InRelease $R2 >/dev/null 2>&1; then
    continue # the suite's first publish
  fi
  # Don't carry on without it: deleting InRelease would then get anything in the pool signed.
  aws s3 cp $BUCKET/dists/$s/InRelease "$o/InRelease" $R2 ||
    { echo "apt-publish: the bucket has $(own $s) but no signed $s index; if a first publish stopped half-way, empty the bucket's $(own $s) and run again" >&2; exit 1; }
  # The VALIDSIG check below reports a bad signature, so don't stop on gpg's exit status.
  gpg --yes --status-fd 3 -o "$o/Release" -d "$o/InRelease" 3>"$o/status" 2>/dev/null || true
  grep -q "^\[GNUPG:\] VALIDSIG .* $PRIMARY\$" "$o/status" ||
    { echo "apt-publish: the bucket's $s InRelease is not signed by $PRIMARY" >&2; exit 1; }
  # The key signs both suites, so check it is this suite's: else beta's index, copied over
  # stable's, would vouch for a beta .deb copied into pool/.
  grep -qx "Suite: $s" "$o/Release" ||
    { echo "apt-publish: the bucket's $s InRelease is another suite's" >&2; exit 1; }
  for a in $ARCHES; do
    p=main/binary-$a/Packages
    aws s3 cp $BUCKET/dists/$s/$p "$o/Packages" $R2
    grep -Eq "^ $(sha256sum "$o/Packages" | cut -d' ' -f1) +[0-9]+ $p\$" "$o/Release" ||
      { echo "apt-publish: the bucket's $s $p does not match its signed Release" >&2; exit 1; }
    sed -n 's/^SHA256: //p' "$o/Packages" >>"$o/allowed"
  done
done

# apt-ftparchive --arch filters on the filename, so name each .deb from its own fields.
for f in "$@"; do
  cp "$f" "$REPO/$(own "$SUITE")/$(dpkg-deb -f "$f" Package)_$(dpkg-deb -f "$f" Version)_$(dpkg-deb -f "$f" Architecture).deb"
done
for s in $SUITES; do
  find "$REPO/$(own $s)" -type f | while read -r f; do
    grep -qx "$(sha256sum "$f" | cut -d' ' -f1)" "$REPO/old/$s/allowed" ||
      { echo "apt-publish: $f is in $(own $s) but not in the signed $s index; if the bucket has it, remove it there and check who holds the R2 token" >&2; exit 1; }
  done
done

cd "$REPO"
for s in $SUITES; do
  d=dists/$s
  for a in $ARCHES; do   # --arch also takes Architecture: all packages
    mkdir -p $d/main/binary-$a
    for p in $(pools $s); do apt-ftparchive --arch $a packages $p; done > $d/main/binary-$a/Packages
    gzip -9kf $d/main/binary-$a/Packages
  done
  rm -f $d/Release $d/InRelease   # else the new Release hashes the old one
  # by-hash: apt then fetches each Packages by its hash, so a client never pairs a new InRelease
  # with an old Packages (or the other way round) while the upload or the CDN cache catches up.
  apt-ftparchive \
    -o APT::FTPArchive::DoByHash=true \
    -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
    -o APT::FTPArchive::Release::Suite=$s \
    -o APT::FTPArchive::Release::Codename=$s \
    -o APT::FTPArchive::Release::Components=main \
    -o APT::FTPArchive::Release::Architectures="$ARCHES" \
    release $d > Release.tmp
  mv Release.tmp $d/Release
  gpg --yes ${GPG_PASSPHRASE_FILE:+--batch --pinentry-mode loopback --passphrase-file "$GPG_PASSPHRASE_FILE"} \
    -u "$KEY" --clearsign -o $d/InRelease $d/Release
done

# Pools first, then the Packages files, then Release and InRelease, so that everything an index
# lists is up before the index. sync uploads in parallel, so the last two go up on their own.
# The index gets a short cache so Cloudflare serves a new release within a minute.
# ponytail: never deletes old .debs or by-hash files from the bucket; prune there by hand if it grows.
aws s3 sync pool $BUCKET/pool $R2
aws s3 sync pool-beta $BUCKET/pool-beta $R2
aws s3 sync dists $BUCKET/dists $R2 --cache-control max-age=60 --exclude '*Release'
for s in $SUITES; do
  for f in Release InRelease; do aws s3 cp dists/$s/$f $BUCKET/dists/$s/$f $R2 --cache-control max-age=60; done
done
# key.asc changes only when the subkey is renewed, and that's done by hand.
if [ -f key.asc ]; then aws s3 cp key.asc $BUCKET/key.asc $R2; fi
