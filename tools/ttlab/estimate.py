#!/usr/bin/env python3
"""Extrapolate mainnet time-travel storage from ttlab measurements.

usage: estimate.py <stats.csv> <era>=<ingest.jsonl> ...  e.g. 2022=m2022.jsonl

stats.csv (ttlab stats) samples mainnet every 20k rounds: object changes and
full value bytes per kind, header/block/cert sizes. Each era's ingest results
(contiguous runs) give, per configuration, disk bytes per round relative to the
logical size of storing every version in full (N=1, no compression). That
ratio is applied to the sampled history of the rounds the era stands for.
"""
import csv
import json
import sys

SAMPLE = 20000
# record key bytes (keyspace prefix + object key + round) plus the record tag
OVERHEAD = {'a': 43, 'h': 51, 'l': 51, 'A': 19, 'P': 19, 'k': 47}
TOTALS_PER_ROUND = 120 + 11
# rounds each era run stands for
ERAS = [(0, 35_000_000, '2022'), (35_000_000, 55_000_000, '2024'), (55_000_000, 1 << 62, '2026')]


def cfg_name(r):
    # runs from before the checkpoint interval's removal carry n and inc
    n = '' if r.get('n', 1) == 1 else f"N={r['n']}{' inc' if r.get('inc') else ''} "
    return f"{n}{r['comp']} {r['block'] // 1024}K"


def main():
    rows = list(csv.DictReader(open(sys.argv[1])))
    runs = {}
    for arg in sys.argv[2:]:
        era, path = arg.split('=')
        runs[era] = [json.loads(line) for line in open(path)]

    f = lambda x: float(x) if x else 0.0
    logical = {e[2]: 0.0 for e in ERAS}
    rounds = {e[2]: 0 for e in ERAS}
    hdr = blk = cert = 0.0
    for r in rows:
        rnd = int(r['round'])
        era = next(e for lo, hi, e in ERAS if lo <= rnd < hi)
        rounds[era] += SAMPLE
        logical[era] += SAMPLE * (TOTALS_PER_ROUND + sum(f(r['b_' + k]) + f(r['n_' + k]) * o for k, o in OVERHEAD.items()))
        hdr += SAMPLE * f(r['hdr_bytes'])
        blk += SAMPLE * f(r['block_bytes'])
        cert += SAMPLE * f(r['cert_bytes'])

    gb = lambda b: b / 1e9
    print(f"history: {sum(rounds.values()) / 1e6:.1f}M rounds; full-version logical state history {gb(sum(logical.values())):.0f} GB")
    for e in logical:
        print(f"  era {e}: {rounds[e] / 1e6:.1f}M rounds, {logical[e] / max(rounds[e], 1):.0f} B/round logical")
    print(f"archival block DB (blocks+certs+duplicated headers, raw): {gb(blk + hdr):.0f} GB, of which certs {gb(cert):.0f} GB, headers {gb(hdr):.1f} GB")

    # per-config ratio to the era's N=1 uncompressed logical bytes per round
    configs = {}
    for era, rs in runs.items():
        base = next(r for r in rs if r.get('n', 1) == 1 and r['comp'] == 'none')['steady_logical_bytes_per_round']
        for r in rs:
            configs.setdefault(cfg_name(r), {})[era] = (r['steady_disk_bytes_per_round'] / base, r)
    print(f"\n{'config':28} {'state history':>14} {'write h':>8}   per-era disk B/round")
    for name, eras in configs.items():
        if set(eras) != set(runs):
            continue
        total = sum(logical[e] * eras[e][0] for e in eras)
        write_h = sum(rounds[e] * eras[e][1]['write_us_per_round'] for e in eras) / 1e6 / 3600
        per = ' '.join(f"{e}:{eras[e][1]['steady_disk_bytes_per_round']:.0f}" for e in sorted(eras))
        print(f"{name:28} {gb(total):11.0f} GB {write_h:7.1f}h   {per}")


if __name__ == '__main__':
    main()
