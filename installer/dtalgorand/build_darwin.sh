#!/usr/bin/env bash

# build_darwin.sh - Build the dtalgorand macOS tarball (this fork, for the d13co/tap Homebrew
# formula) for this Mac's architecture.
#
# The tarball holds what the .deb does, for the formula to lay out: the binaries under their stock
# names in bin/ (goal and algoh run algod and kmd from their own directory), and in share/ the
# files the .deb puts in the data directory (config.json.example, system.json, and the networks'
# genesis files under genesis/). The formula links the binaries with a dt prefix (dtalgod,
# dtgoal, ...) and seeds the data directory from share/.
#
# Syntax:   installer/dtalgorand/build_darwin.sh [output directory]
#
# The version comes from installer/dtalgorand/version.sh, as for the .deb. Set PKG_VERSION to pin
# it, and NO_BUILD=1 to package the binaries of a previous build again. BUILDNUMBER overrides the
# binaries' build number (the upstream patch version by default).
#
# The binaries run on MACOSX_DEPLOYMENT_TARGET (default 14.0, the formula's minimum) and later.
#
# Example: installer/dtalgorand/build_darwin.sh tmp/dtalgorand

set -eo pipefail
umask 022

if [ "$(uname -s)" != Darwin ]; then
    echo "build on macOS: the binaries need cgo for macOS"
    exit 1
fi

OUTDIR=$(mkdir -p "${1:-.}" && cd "${1:-.}" && pwd -P)
REPO_DIR=$(cd "$(dirname "$0")/../.." && pwd -P)
SRC_DIR="${REPO_DIR}/installer/dtalgorand"
cd "${REPO_DIR}"

PKG_NAME=dtalgorand
ARCH=$(go env GOARCH)
HOST_ARCH=$(uname -m)
[ "${HOST_ARCH}" = x86_64 ] && HOST_ARCH=amd64
if [ "${ARCH}" != "${HOST_ARCH}" ]; then
    echo "go builds for ${ARCH}, but this Mac is ${HOST_ARCH}"
    exit 1
fi

. "${SRC_DIR}/version.sh"
VER=${PKG_VERSION}

# The oldest macOS the binaries run on; else clang targets the build host's release.
export MACOSX_DEPLOYMENT_TARGET=${MACOSX_DEPLOYMENT_TARGET:-14.0}

# A separate GOBIN, so that the build leaves $GOPATH/bin alone.
export GOBIN="${REPO_DIR}/tmp/dtalgorand/darwin-${ARCH}/bin"
if [ -z "${NO_BUILD}" ]; then
    make buildsrc
fi

PKG_ROOT=$(mktemp -d)
trap 'rm -rf "${PKG_ROOT}"' EXIT
TOP="${PKG_NAME}-${VER}"
DIR="${PKG_ROOT}/${TOP}"

mkdir -p "${DIR}/bin" "${DIR}/share"
for bin in algocfg algod algoh algokey diagcfg goal kmd node_exporter ttimport; do
    install -m 755 "${GOBIN}/${bin}" "${DIR}/bin/${bin}"
done
cp installer/config.json.example installer/system.json "${DIR}/share"
for dir in installer/genesis/*/; do
    net=$(basename "${dir}")
    mkdir -p "${DIR}/share/genesis/${net}"
    cp "${dir}/genesis.json" "${DIR}/share/genesis/${net}/genesis.json"
done
cp COPYING "${DIR}/COPYING"
echo "${VER}" > "${DIR}/VERSION"

for f in "${DIR}"/bin/*; do
    # Apple silicon runs only signed code. The linker signs ad hoc; sign whatever it didn't.
    codesign --verify "${f}" 2>/dev/null || codesign --sign - --force "${f}"
    codesign --verify --strict "${f}"
    # Only the system's libraries: the Makefile puts Homebrew's on the library path.
    if otool -L "${f}" | tail -n +2 | grep -Ev '^[[:space:]]+/(usr/lib|System/Library)/'; then
        echo "$(basename "${f}") links the libraries above, which a Mac may not have"
        exit 1
    fi
done
# what the binaries say is the oldest macOS they run on
vtool -show-build "${DIR}/bin/algod" | grep -E 'minos|sdk' || true

OUTPUT="${OUTDIR}/${PKG_NAME}_${VER}_darwin-${ARCH}.tar.gz"
# no AppleDouble (._*) files or extended attributes in the archive
COPYFILE_DISABLE=1 tar -C "${PKG_ROOT}" --no-xattrs -czf "${OUTPUT}" "${TOP}"
echo "${OUTPUT}"
