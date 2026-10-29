#!/usr/bin/env bash
# Start tt-snapshot.service once per catchpoint, just after the catchpoint's balances round, and
# tt-snapshot-upload.service when the bucket lags the snapshot tree.
#
# A node that installs a snapshot catches up to a catchpoint C only if C's balances round
# (C - LOOKBACK) is at most the snapshot's last round. A snapshot taken between C's balances round
# and C fits both C and the catchpoint before it, so `ttsnap catchup` with the network's latest
# catchpoint works until the one after C is published, by which time the next snapshot is out.
# Catchpoints come every INTERVAL rounds (about 7.7 hours on mainnet), which no clock schedule
# follows, so tt-snapshot-trigger.timer runs this every 2 minutes. It takes the latest balances
# round that the node's round is at least MARGIN rounds past, and starts:
#   - tt-snapshot.service when OUT's latest history ends before that round: MARGIN rounds after
#     each balances round, or as soon as it can when that run was missed. Not while the node
#     catches up (a nonzero catchup-time in /v2/status): its state lags the network's then, and
#     each catchpoint it passed would start a run;
#   - otherwise, when UPLOAD runs this directory's upload.sh, tt-snapshot-upload.service when the
#     bucket lags OUT: OUT changed since upload.sh last completed (a failed or interrupted upload,
#     or statements added by hand), or R2_PUBLIC_URL serves an older latest-* pointer than OUT
#     holds. This part needs no node, and goes on while algod is down.
# It starts neither while one runs. It starts a unit again only RETRY seconds after its last run
# ended when that run was started for the same round or the same OUT, and failed or fell short
# (a failure for an earlier catchpoint doesn't delay the next one's run). A run that fell short it
# logs as a warning, as another run may do no better (for an upload: R2_PUBLIC_URL serves another
# bucket, or a cache holds latest-*). In /run/tt-snapshot it keeps what it started each unit for,
# and the units record when each run ended. Otherwise it logs only when it starts a unit or cannot
# read the node or the bucket.
#
# Env (all optional): ALGOD_DATA (default /var/lib/dtalgorand, for algod.net and algod.token),
# ALGOD_URL and ALGOD_TOKEN (default: from ALGOD_DATA), OUT, UPLOAD, R2_BUCKET and R2_PUBLIC_URL
# (from the env file), INTERVAL (10000), LOOKBACK (320), MARGIN (30), RETRY (1800), ENV_FILE
# (/etc/tt-snapshot.env, whose settings apply unless the environment sets them), DRY_RUN=1 to print
# the decision instead.
set -euo pipefail

here=$(dirname "$(readlink -f "${BASH_SOURCE[0]}")")
# shellcheck source=lib.sh
. "$here/lib.sh"
load_env "${ENV_FILE:-/etc/tt-snapshot.env}"

data=${ALGOD_DATA:-/var/lib/dtalgorand}
out=${OUT:?set OUT}
out=${out%/}
bucket=${R2_BUCKET:-}
public=${R2_PUBLIC_URL:-}
public=${public%/}
interval=${INTERVAL:-10000}
lookback=${LOOKBACK:-320}
margin=${MARGIN:-30}
retry=${RETRY:-1800}
snapshot=tt-snapshot.service
upload=tt-snapshot-upload.service
# per unit: what the trigger last started it for (a round, or OUT's digest), and when; cleared at
# boot, as unit states are
ran=/run/tt-snapshot

# journald reads a <N> prefix as the syslog level: the unit's LogLevelMax=notice keeps 5 and 4
say() { echo "<5>$*"; }
warn() { echo "<4>$*" >&2; }

state() { systemctl show --property=ActiveState --value "$1"; }

# when a unit's last run ended, in seconds since the epoch; 0 if it hasn't run since boot. Each run
# touches the file (ExecStopPost=): systemd forgets when a run that succeeded ended
ended() { stat -c %Y "$ran/$1.ended" 2>/dev/null || echo 0; }

# Start unit $1 for goal $2 (the round OUT should reach, or OUT's digest for the bucket), logging
# reason $3, and exit. If the trigger last started it for goal $2 too, return instead until RETRY
# seconds after that run ended, or after the trigger started it if that is later (a start whose job
# never ran). Then say which: a run that failed, a start that never ran, or a run for goal $2 that
# fell short, as $4 tells how; the last two as warnings. A run for another goal that failed delays
# nothing: a new catchpoint's run starts on time.
start() {
  local unit=$1 goal=$2 why=$3 last='' at=0 end since now level=5 what=$1
  if [[ -r $ran/$unit ]]; then read -r last at <"$ran/$unit" || true; fi
  if [[ $last == "$goal" ]]; then
    end=$(ended "$unit")
    since=$((end > at ? end : at))
    now=$(date +%s)
    if ((now - since < retry)); then
      [[ -z ${DRY_RUN:-} ]] || say "$unit's last run failed or fell short; not starting it for another $((since + retry - now))s: $why"
      return 0
    fi
    if [[ $(state "$unit") == failed ]]; then
      what="$unit, whose last run failed"
    elif ((end < at)); then
      level=4 what="$unit again"
      why="the trigger started it $((now - at))s ago for the same, and it did not run (a failed dependency? see systemctl status $unit): $why"
    else
      level=4 what="$unit again" why=$4
    fi
  elif [[ $(state "$unit") == failed ]]; then
    what="$unit, whose last run, for another goal, failed"
  fi
  if [[ -n ${DRY_RUN:-} ]]; then
    say "would start $what: $why"
    exit 0
  fi
  echo "<$level>starting $what: $why"
  mkdir -p "$ran"
  echo "$goal $(date +%s)" >"$ran/$unit"
  systemctl start --no-block "$unit"
  exit 0
}

# a latest-<kind> pointer holds <kind>-<last round>.json
pointer_round() { sed -n '1s/^[a-z]\{1,\}-\([0-9]\{1,\}\)\.json[[:space:]]*$/\1/p'; }

for u in "$snapshot" "$upload"; do
  case $(state "$u") in activating | active | deactivating | reloading) exit 0 ;; esac
done

# --- the snapshot, from the node's round ---------------------------------------------------------

# the token goes in a header file, off curl's command line, which every user can read
api() {
  if [[ -n $token ]]; then
    curl -fsS --max-time 10 -H @<(printf 'X-Algo-API-Token: %s\n' "$token") "$url$1"
  else
    curl -fsS --max-time 10 "$url$1"
  fi
}

check_snapshot() {
  local net status round catchup genesis cp balances made='' p
  url=${ALGOD_URL:-}
  if [[ -z $url ]]; then
    net=$(cat "$data/algod.net" 2>/dev/null) || { warn "cannot reach algod: $data/algod.net is missing (algod is not running?)"; return 0; }
    url=http://$net
  fi
  token=${ALGOD_TOKEN:-$(cat "$data/algod.token" 2>/dev/null || true)}
  status=$(api /v2/status) || { warn "cannot read the node's status from $url"; return 0; }
  round=$(jq -er '."last-round"' <<<"$status") || { warn "$url/v2/status has no last-round"; return 0; }
  # nanoseconds the node has been catching up for, 0 when it is in sync
  catchup=$(jq -er '."catchup-time"' <<<"$status") || { warn "$url/v2/status has no catchup-time"; return 0; }
  genesis=$(api /versions | jq -er .genesis_id) || { warn "cannot read the node's genesis ID from $url"; return 0; }

  # the latest catchpoint whose balances round the node is MARGIN rounds past
  cp=$(((round - margin + lookback) / interval * interval))
  balances=$((cp - lookback))
  p=$out/snapshots/$genesis/latest-history
  if [[ -r $p ]]; then made=$(pointer_round <"$p"); fi
  made=${made:-0}
  ((made < balances)) || return 0
  if ((catchup > 0)); then
    [[ -z ${DRY_RUN:-} ]] || say "not starting $snapshot while the node catches up (at round $round, for $((catchup / 1000000000))s)"
    return 0
  fi
  start "$snapshot" "$balances" \
    "round $round is past catchpoint $cp's balances round $balances; the latest snapshot ends at $made" \
    "its last run, for catchpoint $cp, ended with the latest snapshot at $made, before the balances round $balances"
}

# --- the upload, from OUT and the bucket ---------------------------------------------------------

# the round of the snapshot that the bucket's pointer $1 (a path under R2_PUBLIC_URL) names; 0 if
# there is none
served() {
  local resp code r=
  resp=$(curl -sS --max-time 10 -H 'Cache-Control: no-cache' -w '\n%{http_code}' "$public/$1") ||
    { warn "cannot read $public/$1"; return 1; }
  code=${resp##*$'\n'}
  case $code in
    200) r=$(pointer_round <<<"${resp%$'\n'*}") ;;
    404) ;;
    *) warn "$public/$1 returned HTTP $code"; return 1 ;;
  esac
  echo "${r:-0}"
}

check_upload() {
  local cmd ptrs digest p rel made got
  # an empty UPLOAD turns uploads off, retries too
  [[ -n ${UPLOAD:-} ]] || return 0
  # the retries run upload.sh, which records what it mirrored: another UPLOAD gets none
  read -r cmd _ <<<"$UPLOAD"
  if [[ ! $cmd -ef $here/upload.sh ]]; then
    [[ -z ${DRY_RUN:-} ]] || say "not checking the bucket: UPLOAD runs $cmd, not $here/upload.sh"
    return 0
  fi
  # upload.sh refuses an OUT with no pointer
  mapfile -t ptrs < <(pointers "$out")
  ((${#ptrs[@]})) || return 0
  # a file may vanish while find lists OUT (dt_snapshot.sh run by hand): try on the next tick
  digest=$(mirror_digest "$out" "$bucket") || { warn "cannot list $out"; return 0; }
  if [[ $(cat "$uploaded" 2>/dev/null) != "$digest" ]]; then
    start "$upload" "$digest" "$out changed since upload.sh last completed" \
      "its last run, of the same $out, did not complete"
    return 0
  fi
  [[ -n $public ]] || return 0
  for p in "${ptrs[@]}"; do
    rel=${p#"$out"/}
    made=$(pointer_round <"$p")
    made=${made:-0}
    got=$(served "$rel") || exit 0
    if ((got < made)); then
      start "$upload" "$digest" "$public/$rel names round $got, $p round $made" \
        "its last run, of the same $out, completed, yet $public/$rel names round $got, not $made: is R2_PUBLIC_URL the domain of R2_BUCKET, with latest-* left out of any cache rule?"
      return 0
    fi
  done
}

check_snapshot
check_upload
