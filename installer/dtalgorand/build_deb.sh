#!/usr/bin/env bash

# build_deb.sh - Build the dtalgorand .deb (this fork, for apt.d13.co) for this host's architecture.
#
# dtalgorand installs alongside the algorand package. goal and algoh run algod and kmd from their
# own directory, so the binaries keep their stock names in /usr/lib/dtalgorand/bin and /usr/bin
# links to them with a dt prefix (dtalgod, dtgoal, ...). The node lives in /var/lib/dtalgorand.
#
# Syntax:   installer/dtalgorand/build_deb.sh [output directory]
#
# Versions follow the upstream release the fork is based on, the nearest v<x.y.z>-stable tag in
# git history, plus a fork revision: <x.y.z>-dt<n>. The binaries report the upstream version
# (algod -v says x.y.z), and the package version comes from the v<x.y.z>-dt<n> tag at HEAD. A
# build without one is <x.y.z>-dt<n>+dev<buildnumber.py>, where n is the last release on that base
# (0 if none), so it sorts after that release and before the next.
#
# Set DEB_VERSION to pin the package version, e.g. to give every architecture the same dev
# version, and NO_BUILD=1 to package the binaries of a previous build again. BUILDNUMBER
# overrides the binaries' build number (the upstream patch version by default).
#
# Examples: installer/dtalgorand/build_deb.sh tmp/dtalgorand
#           DEB_VERSION=5.0.2-dt1+dev306622 installer/dtalgorand/build_deb.sh tmp/dtalgorand

set -eo pipefail
umask 022

OUTDIR=$(mkdir -p "${1:-.}" && cd "${1:-.}" && pwd -P)
REPO_DIR=$(cd "$(dirname "$0")/../.." && pwd -P)
SRC_DIR="${REPO_DIR}/installer/dtalgorand"
cd "${REPO_DIR}"

PKG_NAME=dtalgorand
ARCH=$(dpkg --print-architecture)
if [ "${ARCH}" != "$(go env GOARCH)" ]; then
    echo "go builds for $(go env GOARCH), not ${ARCH}: build on a ${ARCH} host"
    exit 1
fi

# version.sh works out the package version, PKG_VERSION, and the binaries' BUILDNUMBER.
PKG_VERSION=${DEB_VERSION}
. "${SRC_DIR}/version.sh"
VER=${PKG_VERSION}

# A separate GOBIN, so that the build leaves $GOPATH/bin alone.
export GOBIN="${REPO_DIR}/tmp/dtalgorand/${ARCH}/bin"
if [ -z "${NO_BUILD}" ]; then
    make buildsrc
fi

PKG_ROOT=$(mktemp -d)
trap 'rm -rf "${PKG_ROOT}"' EXIT
chmod 755 "${PKG_ROOT}"

mkdir -p "${PKG_ROOT}/usr/lib/dtalgorand/bin" "${PKG_ROOT}/usr/bin"
for bin in algocfg algod algoh algokey diagcfg goal kmd node_exporter; do
    install -m 755 "${GOBIN}/${bin}" "${PKG_ROOT}/usr/lib/dtalgorand/bin/${bin}"
done
for bin in algocfg algod algoh algokey diagcfg goal kmd; do
    ln -s "../lib/dtalgorand/bin/${bin}" "${PKG_ROOT}/usr/bin/dt${bin}"
done
install -m 755 "${GOBIN}/ttimport" "${PKG_ROOT}/usr/bin/ttimport"

DATA_DIR="${PKG_ROOT}/var/lib/dtalgorand"
mkdir -p "${DATA_DIR}"
cp installer/config.json.example installer/system.json "${DATA_DIR}"
for dir in installer/genesis/*/; do
    net=$(basename "${dir}")
    mkdir -p "${DATA_DIR}/genesis/${net}"
    cp "${dir}/genesis.json" "${DATA_DIR}/genesis/${net}/genesis.json"
done
cp installer/genesis/mainnet/genesis.json "${DATA_DIR}/genesis.json"
# files should not be group writable but directories should be
chmod -R g-w "${DATA_DIR}"
find "${DATA_DIR}" -type d -exec chmod g+w {} +

install -D -m 644 -t "${PKG_ROOT}/lib/systemd/system" "${SRC_DIR}/dtalgorand.service" "${SRC_DIR}/dtalgorand@.service"
install -D -m 644 -t "${PKG_ROOT}/etc/apt/apt.conf.d" "${SRC_DIR}/51dtalgorand-upgrades"

mkdir -p "${PKG_ROOT}/usr/share/doc/${PKG_NAME}"
cat <<EOF > "${PKG_ROOT}/usr/share/doc/${PKG_NAME}/copyright"
Format: https://www.debian.org/doc/packaging-manuals/copyright-format/1.0/
Upstream-Name: Algorand
Upstream-Contact: Algorand developers <dev@algorand.com>
Source: https://github.com/d13co/dt-algorand
Comment: A fork of https://github.com/algorand/go-algorand

Files: *
Copyright: Algorand developers <dev@algorand.com>
License: AGPL-3+
EOF
sed 's/^$/./g' < COPYING | sed 's/^/ /g' >> "${PKG_ROOT}/usr/share/doc/${PKG_NAME}/copyright"

INSTALLED_SIZE=$(du -sk "${PKG_ROOT}" | cut -f1)
mkdir -p "${PKG_ROOT}/DEBIAN"
sed -e "s,@VER@,${VER}," \
    -e "s,@ARCH@,${ARCH}," \
    -e "s,@INSTALLED_SIZE@,${INSTALLED_SIZE}," \
    < "${SRC_DIR}/debian/control" > "${PKG_ROOT}/DEBIAN/control"
install -m 644 "${SRC_DIR}/debian/conffiles" "${PKG_ROOT}/DEBIAN/conffiles"
install -m 755 "${SRC_DIR}/debian/postinst" "${SRC_DIR}/debian/prerm" "${SRC_DIR}/debian/postrm" "${PKG_ROOT}/DEBIAN"

OUTPUT="${OUTDIR}/${PKG_NAME}_${VER}_${ARCH}.deb"
# xz, which every dpkg reads (Ubuntu's dpkg-deb defaults to zstd)
dpkg-deb --root-owner-group -Zxz --build "${PKG_ROOT}" "${OUTPUT}"
