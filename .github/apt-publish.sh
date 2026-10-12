#!/bin/sh
# Add .debs to the apt repo at https://apt.d13.co, then rebuild and sign its index.
# Usage: apt-publish.sh [pkg.deb ...]    (no args: just re-index and re-sign)
#
# The release workflow runs it on every tag; ~/code/infra/apt/publish.sh runs it by hand
# (the algorand .debs). Either way it first pulls the bucket's pool, so the index lists
# every package in the bucket, not only the ones on this machine.
#
#   APT_REPO             local copy of the repo tree (default: ./apt-repo)
#   R2_ACCOUNT_ID        and aws credentials for the "apt" bucket (AWS_PROFILE=r2 by hand,
#                        AWS_ACCESS_KEY_ID/AWS_SECRET_ACCESS_KEY in CI)
#   GPG_PASSPHRASE_FILE  signing subkey passphrase, for gpg without a pinentry (CI)
set -eu
KEY=5B708A29E1D79C75016BBCE4B4C52CB3D4B85B22!   # CI signing subkey
PRIMARY=F66E77133063650C159F405AC4D2171FB5B60471 # its primary key, as install.sh pins it
REPO=${APT_REPO:-apt-repo}
ARCHES="amd64 arm64"
BUCKET=s3://apt   # R2 bucket, served at https://apt.d13.co
R2="--endpoint-url https://${R2_ACCOUNT_ID:?set R2_ACCOUNT_ID}.r2.cloudflarestorage.com --only-show-errors"
export AWS_DEFAULT_REGION=auto

mkdir -p "$REPO/pool" "$REPO/old"
# ponytail: pulls the whole pool (algorand .debs are ~160MB each); merge Packages stanzas
# instead once that gets slow.
aws s3 sync $BUCKET/pool "$REPO/pool" $R2

# Everything in the pool gets signed below, and the R2 token alone can write the pool. So
# only sign what the current signed index already lists, plus the .debs given here.
: >"$REPO/old/allowed"
if [ $# -gt 0 ]; then sha256sum "$@" | cut -d' ' -f1 >>"$REPO/old/allowed"; fi
if [ -n "$(find "$REPO/pool" -type f)" ]; then
  # Don't carry on without it: deleting InRelease would then get anything in the pool signed.
  aws s3 cp $BUCKET/dists/stable/InRelease "$REPO/old/InRelease" $R2 ||
    { echo "apt-publish: the bucket has a pool but no signed index; if a first publish stopped half-way, empty the bucket's pool and run again" >&2; exit 1; }
  # The VALIDSIG check below reports a bad signature, so don't stop on gpg's exit status.
  gpg --yes --status-fd 3 -o "$REPO/old/Release" -d "$REPO/old/InRelease" 3>"$REPO/old/status" 2>/dev/null || true
  grep -q "^\[GNUPG:\] VALIDSIG .* $PRIMARY\$" "$REPO/old/status" ||
    { echo "apt-publish: the bucket's InRelease is not signed by $PRIMARY" >&2; exit 1; }
  for a in $ARCHES; do
    p=main/binary-$a/Packages
    aws s3 cp $BUCKET/dists/stable/$p "$REPO/old/Packages" $R2
    grep -Eq "^ $(sha256sum "$REPO/old/Packages" | cut -d' ' -f1) +[0-9]+ $p\$" "$REPO/old/Release" ||
      { echo "apt-publish: the bucket's $p does not match its signed Release" >&2; exit 1; }
    sed -n 's/^SHA256: //p' "$REPO/old/Packages" >>"$REPO/old/allowed"
  done
fi

# apt-ftparchive --arch filters on the filename, so name each .deb from its own fields.
for f in "$@"; do
  cp "$f" "$REPO/pool/$(dpkg-deb -f "$f" Package)_$(dpkg-deb -f "$f" Version)_$(dpkg-deb -f "$f" Architecture).deb"
done
find "$REPO/pool" -type f | while read -r f; do
  grep -qx "$(sha256sum "$f" | cut -d' ' -f1)" "$REPO/old/allowed" ||
    { echo "apt-publish: $f is in the pool but not in the signed index; if the bucket has it, remove it there and check who holds the R2 token" >&2; exit 1; }
done

cd "$REPO"
d=dists/stable
for a in $ARCHES; do   # --arch also takes Architecture: all packages
  mkdir -p $d/main/binary-$a
  apt-ftparchive --arch $a packages pool > $d/main/binary-$a/Packages
  gzip -9kf $d/main/binary-$a/Packages
done
rm -f $d/Release $d/InRelease   # else the new Release hashes the old one
# by-hash: apt then fetches each Packages by its hash, so a client never pairs a new InRelease
# with an old Packages (or the other way round) while the upload or the CDN cache catches up.
apt-ftparchive \
  -o APT::FTPArchive::DoByHash=true \
  -o APT::FTPArchive::Release::Acquire-By-Hash=yes \
  -o APT::FTPArchive::Release::Suite=stable \
  -o APT::FTPArchive::Release::Codename=stable \
  -o APT::FTPArchive::Release::Components=main \
  -o APT::FTPArchive::Release::Architectures="$ARCHES" \
  release $d > Release.tmp
mv Release.tmp $d/Release
gpg --yes ${GPG_PASSPHRASE_FILE:+--batch --pinentry-mode loopback --passphrase-file "$GPG_PASSPHRASE_FILE"} \
  -u "$KEY" --clearsign -o $d/InRelease $d/Release

# Pool first, then the Packages files, then Release and InRelease, so that everything an index lists
# is up before the index. sync uploads in parallel, so the last two go up on their own.
# The index gets a short cache so Cloudflare serves a new release within a minute.
# ponytail: never deletes old .debs or by-hash files from the bucket; prune there by hand if it grows.
aws s3 sync pool $BUCKET/pool $R2
aws s3 sync dists $BUCKET/dists $R2 --cache-control max-age=60 --exclude '*Release'
for f in Release InRelease; do aws s3 cp $d/$f $BUCKET/$d/$f $R2 --cache-control max-age=60; done
# key.asc changes only when the subkey is renewed, and that's done by hand.
if [ -f key.asc ]; then aws s3 cp key.asc $BUCKET/key.asc $R2; fi
