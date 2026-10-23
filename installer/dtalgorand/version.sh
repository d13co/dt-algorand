# shellcheck shell=bash
# version.sh - Work out the dtalgorand package version, for build_deb.sh and build_darwin.sh.
#
# Source it from the repository root. It reads PKG_VERSION and BUILDNUMBER if set, and sets
# them otherwise; it also exports BUILDNUMBER, BUILD_NUMBER and CHANNEL for make.
#
# The version is the upstream release the fork is based on, the nearest v<x.y.z>-stable tag in
# git history, plus a fork revision: <x.y.z>-dt<n>. The binaries report the upstream version
# (algod -v says x.y.z), and the package version comes from the v<x.y.z>-dt<n> tag at HEAD. A
# build without one is <x.y.z>-dt<n>+dev<buildnumber.py>, where n is the last release on that base
# (0 if none), so it sorts after that release and before the next.

if [ -z "${BUILDNUMBER}" ] || [ -z "${PKG_VERSION}" ]; then
    # The upstream release this is based on. d13co/dt-algorand doesn't carry upstream's tags.
    BASE=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*-stable' 2>/dev/null) || {
        echo "no upstream v*-stable tag in this checkout's history; fetch them with"
        echo "  git fetch --no-tags https://github.com/algorand/go-algorand.git 'refs/tags/v*-stable:refs/tags/v*-stable'"
        exit 1
    }
    BASE=${BASE#v}
    BASE=${BASE%-stable}
    MAJOR_MINOR=$(BUILD_NUMBER=0 ./scripts/compute_build_number.sh -f)
    MAJOR_MINOR=${MAJOR_MINOR%.0}
    if [ "${BASE%.*}" != "${MAJOR_MINOR}" ]; then
        echo "the nearest upstream release is ${BASE}, but config/version.go is ${MAJOR_MINOR}"
        exit 1
    fi
fi
if [ -z "${PKG_VERSION}" ]; then
    BASE_RE="^v${BASE//./\\.}-dt[0-9]+\$"
    TAG=$(git tag --points-at HEAD --list "v${BASE}-dt*" | grep -E "${BASE_RE}" | sort -V | tail -n 1 || true)
    if [ -n "${TAG}" ]; then
        PKG_VERSION=${TAG#v}
    else
        LAST=$(git describe --tags --abbrev=0 --match "v${BASE}-dt*" 2>/dev/null | grep -E "${BASE_RE}" || echo "v${BASE}-dt0")
        PKG_VERSION=${LAST#v}+dev$(./scripts/buildnumber.py)
    fi
fi
# The Makefile reads BUILDNUMBER, compute_build_number.sh reads BUILD_NUMBER.
export BUILDNUMBER=${BUILDNUMBER:-${BASE##*.}}
export BUILD_NUMBER=${BUILDNUMBER}
export CHANNEL=stable
