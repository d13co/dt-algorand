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
#     each balances round, or as soon as it can when that run was missed;
#   - otherwise tt-snapshot-upload.service when the bucket's latest-history (at R2_PUBLIC_URL) is
#     older than OUT's, as after a failed upload.
# It starts neither while one runs, and starts a unit that failed only RETRY seconds after its last
# start. It logs only when it starts a unit or cannot read the node or the bucket.
#
# Env (all optional): ALGOD_DATA (default /var/lib/dtalgorand, for algod.net and algod.token),
# ALGOD_URL and ALGOD_TOKEN (default: from ALGOD_DATA), OUT and R2_PUBLIC_URL (from the env file),
# INTERVAL (10000), LOOKBACK (320), MARGIN (30), RETRY (1800), ENV_FILE (/etc/tt-snapshot.env, whose
# settings apply unless the environment sets them), DRY_RUN=1 to print the decision instead.
set -euo pipefail

# the env file's settings, except those the environment sets
load_env() {
  local f=$1 name kv keep=()
  [[ -r $f ]] || return 0
  for name in $(sed -n 's/^\([A-Za-z_][A-Za-z0-9_]*\)=.*/\1/p' "$f"); do
    if [[ -v $name ]]; then keep+=("$name=${!name}"); fi
  done
  set -a
  # shellcheck source=tt-snapshot.env
  . "$f"
  set +a
  for kv in "${keep[@]}"; do export "$kv"; done
}
load_env "${ENV_FILE:-/etc/tt-snapshot.env}"

data=${ALGOD_DATA:-/var/lib/dtalgorand}
out=${OUT:?set OUT}
out=${out%/}
public=${R2_PUBLIC_URL:-}
public=${public%/}
interval=${INTERVAL:-10000}
lookback=${LOOKBACK:-320}
margin=${MARGIN:-30}
retry=${RETRY:-1800}
snapshot=tt-snapshot.service
upload=tt-snapshot-upload.service

# journald reads a <N> prefix as the syslog level: the unit's LogLevelMax=notice keeps 5 and 4
say() { echo "<5>$*"; }
give_up() {
  echo "<4>$*"
  exit 0
}

state() { systemctl show --property=ActiveState --value "$1"; }

# a unit that failed, and started less than RETRY seconds ago
waiting() {
  local started up
  [[ $(state "$1") == failed ]] || return 1
  started=$(systemctl show --property=InactiveExitTimestampMonotonic --value "$1") # µs since boot
  read -r up _ </proc/uptime
  ((${up%.*} - started / 1000000 < retry))
}

start() { # unit, reason
  if waiting "$1"; then
    [[ -z ${DRY_RUN:-} ]] || say "$1 failed less than ${retry}s after its last start; not starting it: $2"
    exit 0
  fi
  if [[ -n ${DRY_RUN:-} ]]; then
    say "would start $1: $2"
    exit 0
  fi
  say "starting $1: $2"
  systemctl start --no-block "$1"
  exit 0
}

for u in "$snapshot" "$upload"; do
  case $(state "$u") in activating | active | deactivating | reloading) exit 0 ;; esac
done

url=${ALGOD_URL:-}
if [[ -z $url ]]; then
  net=$(cat "$data/algod.net" 2>/dev/null) || give_up "cannot reach algod: $data/algod.net is missing (algod is not running?)"
  url=http://$net
fi
token=${ALGOD_TOKEN:-$(cat "$data/algod.token" 2>/dev/null || true)}

# the token goes in a header file, off curl's command line, which every user can read
api() {
  if [[ -n $token ]]; then
    curl -fsS --max-time 10 -H @<(printf 'X-Algo-API-Token: %s\n' "$token") "$url$1"
  else
    curl -fsS --max-time 10 "$url$1"
  fi
}

round=$(api /v2/status | jq -er '."last-round"') || give_up "cannot read the node's round from $url"
genesis=$(api /versions | jq -er .genesis_id) || give_up "cannot read the node's genesis ID from $url"

# the latest catchpoint whose balances round the node is MARGIN rounds past
cp=$(((round - margin + lookback) / interval * interval))
balances=$((cp - lookback))

# a latest-history pointer holds history-<last round>.json
pointer_round() { sed -n '1s/^history-\([0-9]\{1,\}\)\.json[[:space:]]*$/\1/p'; }

made=0
p=$out/snapshots/$genesis/latest-history
if [[ -r $p ]]; then made=$(pointer_round <"$p"); fi
made=${made:-0}
if ((made < balances)); then
  start "$snapshot" "round $round is past catchpoint $cp's balances round $balances; the latest snapshot ends at $made"
fi

[[ -n $public ]] || exit 0
resp=$(curl -sS --max-time 10 -H 'Cache-Control: no-cache' -w '\n%{http_code}' "$public/snapshots/$genesis/latest-history") ||
  give_up "cannot read $public/snapshots/$genesis/latest-history"
code=${resp##*$'\n'}
case $code in
  200) served=$(pointer_round <<<"${resp%$'\n'*}") ;;
  404) served=0 ;;
  *) give_up "$public/snapshots/$genesis/latest-history returned HTTP $code" ;;
esac
served=${served:-0}
if ((served < made)); then
  start "$upload" "the bucket's latest snapshot ends at $served, $out's at $made"
fi
