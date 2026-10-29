#!/bin/sh
# render.sh - Print the d13co/tap formula for a dtalgorand macOS tarball.
#
# Syntax:   installer/dtalgorand/homebrew/render.sh <version> <tarball url> <sha256>
# Example:  installer/dtalgorand/homebrew/render.sh 5.0.4-dt2 \
#             https://github.com/d13co/dt-algorand/releases/download/v5.0.4-dt2/dtalgorand_5.0.4-dt2_darwin-arm64.tar.gz \
#             <sha256> > Formula/dtalgorand.rb
set -eu
[ $# -eq 3 ] || { echo "usage: $0 <version> <tarball url> <sha256>" >&2; exit 2; }
ver=$1 url=$2 sha=$3
case $ver in *[!A-Za-z0-9.+~-]*|'') echo "bad version: $ver" >&2; exit 2 ;; esac
case $sha in *[!0-9a-f]*) echo "bad sha256: $sha" >&2; exit 2 ;; esac
[ ${#sha} -eq 64 ] || { echo "bad sha256: $sha" >&2; exit 2; }
case $url in *[!A-Za-z0-9:/._~+-]*|'') echo "bad url: $url" >&2; exit 2 ;; esac
[ "${url##*/}" = "dtalgorand_${ver}_darwin-arm64.tar.gz" ] || { echo "the url isn't version $ver's tarball: $url" >&2; exit 2; }
sed -e "s,@VER@,$ver," -e "s,@URL@,$url," -e "s,@SHA256@,$sha," "$(dirname "$0")/dtalgorand.rb.in"
