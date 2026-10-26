#!/usr/bin/env bash
# Start tt-snapshot.service once per catchpoint, just after the catchpoint's balances round.
#
# A node that installs a snapshot catches up to a catchpoint C only if C's balances round
# (C - LOOKBACK) is at most the snapshot's last round. A snapshot taken in the LOOKBACK rounds
# before C fits both C and the catchpoint before it, so `ttsnap catchup` with the network's latest
# catchpoint works until the one after C is published, by which time the next snapshot is out.
# Catchpoints come every INTERVAL rounds (about 7.7 hours on mainnet), which no clock schedule
# follows, so tt-snapshot-trigger.timer runs this every 2 minutes and it starts a snapshot when:
#   - the node's round is at least MARGIN rounds past the next catchpoint's balances round, and
#   - the latest published snapshot ends before that balances round.
#
# Env (all optional): ALGOD_DATA (default /var/lib/dtalgorand, for algod.net), ALGOD_URL (default
# from algod.net), OUT (from /etc/tt-snapshot.env), INTERVAL (10000), LOOKBACK (320), MARGIN (30),
# UNIT (tt-snapshot.service), DRY_RUN=1 to print the decision instead of starting the unit.
set -euo pipefail

env=${ENV_FILE:-/etc/tt-snapshot.env}
if [[ -r $env ]]; then set -a; . "$env"; set +a; fi
data=${ALGOD_DATA:-/var/lib/dtalgorand}
url=${ALGOD_URL:-http://$(cat "$data/algod.net")}
out=${OUT:?set OUT}
interval=${INTERVAL:-10000}
lookback=${LOOKBACK:-320}
margin=${MARGIN:-30}
unit=${UNIT:-tt-snapshot.service}

# journald reads a <N> prefix as the syslog level: 5 is notice, which the unit's LogLevelMax keeps
say() { echo "<5>$*"; }

round=$(curl -fsS --max-time 10 "$url/v2/status" | jq -er '."last-round"') || { echo "<4>cannot read the node's round from $url"; exit 0; }

# the next catchpoint after this round, and its balances round
cp=$(((round / interval + 1) * interval))
balances=$((cp - lookback))
((round >= balances + margin)) || exit 0

# the latest published snapshot's last round (0 when there is none)
published=0
for p in "$out"/snapshots/*/latest-history; do
  [[ -r $p ]] || continue
  name=$(tr -d '[:space:]' <"$p")
  [[ $name =~ ^history-([0-9]+)\.json$ ]] && ((BASH_REMATCH[1] > published)) && published=${BASH_REMATCH[1]}
done
((published < balances)) || exit 0

if systemctl is-active -q "$unit"; then
  exit 0
fi
if [[ -n ${DRY_RUN:-} ]]; then
  say "would start $unit: round $round, catchpoint $cp, balances round $balances, latest snapshot $published"
  exit 0
fi
say "starting $unit: round $round is past catchpoint $cp's balances round $balances; latest snapshot ends at $published"
systemctl start --no-block "$unit"
