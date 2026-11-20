# shellcheck shell=bash
# version.sh - Work out the dtalgorand package version, for build_deb.sh and build_darwin.sh.
#
# Source it from the repository root. It reads PKG_VERSION and BUILDNUMBER if set, and sets
# them otherwise; it also exports BUILDNUMBER, BUILD_NUMBER and CHANNEL for make.
#
# The version is the upstream release the fork is based on, the nearest v<x.y.z>-stable (or
# v<x.y.z>-beta) tag in git history, plus a fork revision: <x.y.z>-dt<n>. The binaries report the
# upstream version (algod -v says x.y.z), and the package version comes from the v<x.y.z>-dt<n>
# tag at HEAD. A build without one is <x.y.z>-dt<n>+dev<buildnumber.py>, where n is the last
# release on that base (0 if none), so it sorts after that release and before the next.
#
# Betas: a v<x.y.z>-dt<n>-beta<m> tag at HEAD gives <x.y.z>-dt<n>~beta<m> (git refs can't hold a
# ~), which sorts before <x.y.z>-dt<n>. A beta may sit on an upstream beta (v5.1.0-beta); a
# release may not. Betas, and any build on an upstream beta, get the beta channel.

CHANNEL=stable
if [ -z "${BUILDNUMBER}" ] || [ -z "${PKG_VERSION}" ]; then
    # The upstream release this is based on. d13co/dt-algorand doesn't carry upstream's tags.
    BASE=$(git describe --tags --abbrev=0 --match 'v[0-9]*.[0-9]*.[0-9]*-stable' --match 'v[0-9]*.[0-9]*.[0-9]*-beta' 2>/dev/null) || {
        echo "no upstream v*-stable or v*-beta tag in this checkout's history; fetch them with"
        echo "  git fetch --no-tags https://github.com/algorand/go-algorand.git 'refs/tags/v*-stable:refs/tags/v*-stable' 'refs/tags/v*-beta:refs/tags/v*-beta'"
        exit 1
    }
    BASE_TAG=${BASE}
    BASE=${BASE#v}
    BASE=${BASE%-stable}
    BASE=${BASE%-beta}
    # On an upstream beta, unless that release's stable tag is in the history too.
    BASE_IS_BETA=
    case ${BASE_TAG} in
    *-beta) git merge-base --is-ancestor "v${BASE}-stable" HEAD 2>/dev/null || BASE_IS_BETA=1 ;;
    esac
    MAJOR_MINOR=$(BUILD_NUMBER=0 ./scripts/compute_build_number.sh -f)
    MAJOR_MINOR=${MAJOR_MINOR%.0}
    if [ "${BASE%.*}" != "${MAJOR_MINOR}" ]; then
        echo "the nearest upstream release is ${BASE}, but config/version.go is ${MAJOR_MINOR}"
        exit 1
    fi
    [ -z "${BASE_IS_BETA}" ] || CHANNEL=beta
fi
if [ -z "${PKG_VERSION}" ]; then
    BASE_RE="^v${BASE//./\\.}-dt[0-9]+\$"
    BETA_RE="^v${BASE//./\\.}-dt[0-9]+-beta[0-9]+\$"
    TAG=$(git tag --points-at HEAD --list "v${BASE}-dt*" | grep -E "${BASE_RE}" | sort -V | tail -n 1 || true)
    BETA_TAG=$(git tag --points-at HEAD --list "v${BASE}-dt*-beta*" | grep -E "${BETA_RE}" | sort -V | tail -n 1 || true)
    if [ -n "${TAG}" ]; then
        if [ -n "${BASE_IS_BETA}" ]; then
            echo "${TAG} is on upstream's v${BASE}-beta: a release needs v${BASE}-stable (tag a beta, ${TAG}-beta<m>)"
            exit 1
        fi
        PKG_VERSION=${TAG#v}
    elif [ -n "${BETA_TAG}" ]; then
        PKG_VERSION=${BETA_TAG#v}
        PKG_VERSION=${PKG_VERSION%-beta*}~beta${PKG_VERSION##*-beta}
    else
        LAST=$(git describe --tags --abbrev=0 --match "v${BASE}-dt*" --exclude "v${BASE}-dt*-beta*" 2>/dev/null | grep -E "${BASE_RE}" || echo "v${BASE}-dt0")
        PKG_VERSION=${LAST#v}+dev$(./scripts/buildnumber.py)
    fi
fi
case ${PKG_VERSION} in *~beta*) CHANNEL=beta ;; esac
# The Makefile reads BUILDNUMBER, compute_build_number.sh reads BUILD_NUMBER.
export BUILDNUMBER=${BUILDNUMBER:-${BASE##*.}}
export BUILD_NUMBER=${BUILDNUMBER}
export CHANNEL
