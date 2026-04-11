#!/usr/bin/env python3
"""Compare two soak_one.py CSVs (prod vs fork) and write a markdown summary.

Inputs: two CSVs produced by soak_one.py.
Output: markdown table with start/end RSS, RSS drift (absolute and %),
        VRAM drift, throughput (tok/s mean), request counts, fail count.

Usage:
  soak_compare.py prod.csv fork.csv > summary.md
"""
import csv
import json
import statistics
import sys
from pathlib import Path


def load(path: Path):
    rows = []
    with open(path) as f:
        for r in csv.DictReader(f):
            rows.append(r)
    return rows


def metrics(rows):
    if not rows:
        return None
    first = rows[0]
    last = rows[-1]
    rss0 = int(first["rss_mb"])
    rss1 = int(last["rss_mb"])
    vram0 = int(first["vram_mb"])
    vram1 = int(last["vram_mb"])
    tps_vals = [float(r["tps_window"]) for r in rows if float(r["tps_window"]) > 0]
    tps_mean = statistics.mean(tps_vals) if tps_vals else 0.0
    tps_median = statistics.median(tps_vals) if tps_vals else 0.0
    # in_flight is a recent addition; tolerate older CSVs without the column
    if_vals = []
    for r in rows:
        v = r.get("in_flight")
        if v is None or v == "":
            continue
        try:
            iv = int(v)
        except ValueError:
            continue
        if iv >= 0:
            if_vals.append(iv)
    in_flight_mean = statistics.mean(if_vals) if if_vals else float("nan")
    in_flight_min = min(if_vals) if if_vals else float("nan")
    in_flight_max = max(if_vals) if if_vals else float("nan")

    # in_flight column may be absent on older CSVs
    if_vals = []
    for r in rows:
        v = r.get("in_flight")
        if v is None or v == "":
            continue
        try:
            iv = int(v)
            if iv >= 0:
                if_vals.append(iv)
        except ValueError:
            pass
    in_flight_mean = statistics.mean(if_vals) if if_vals else None
    req_ok = int(last["req_ok"])
    req_fail = int(last["req_fail"])
    elapsed = float(last["elapsed_s"])
    rss_drift_pct = ((rss1 - rss0) / rss0 * 100.0) if rss0 else 0.0

    # Per-pid drift
    pid0 = json.loads(first["rss_per_pid_json"])
    pid1 = json.loads(last["rss_per_pid_json"])
    per_pid_drift = {}
    for k in pid1:
        if k in pid0:
            per_pid_drift[k] = pid1[k] - pid0[k]
    return {
        "elapsed_s": elapsed,
        "samples": len(rows),
        "rss0": rss0, "rss1": rss1, "rss_drift_mb": rss1 - rss0,
        "rss_drift_pct": rss_drift_pct,
        "vram0": vram0, "vram1": vram1, "vram_drift_mb": vram1 - vram0,
        "req_ok": req_ok, "req_fail": req_fail,
        "tps_mean": tps_mean, "tps_median": tps_median,
        "in_flight_mean": in_flight_mean,
        "in_flight_min": in_flight_min,
        "in_flight_max": in_flight_max,
        "per_pid_drift_mb": per_pid_drift,
    }


def main():
    if len(sys.argv) != 3:
        print("usage: soak_compare.py prod.csv fork.csv", file=sys.stderr)
        return 1
    prod_path, fork_path = Path(sys.argv[1]), Path(sys.argv[2])
    prod = metrics(load(prod_path))
    fork = metrics(load(fork_path))
    if not prod or not fork:
        print("error: empty CSV", file=sys.stderr)
        return 1

    def fmt_pct(x):
        return f"{x:+.2f}%"

    print(f"# Soak A/B comparison\n")
    print(f"- prod CSV: `{prod_path}`")
    print(f"- fork CSV: `{fork_path}`\n")
    print("| metric | prod (upstream) | fork (gfx906 stripped) | delta |")
    print("|---|---|---|---|")
    rows = [
        ("elapsed (s)", f"{prod['elapsed_s']:.0f}", f"{fork['elapsed_s']:.0f}", "-"),
        ("samples", str(prod["samples"]), str(fork["samples"]), "-"),
        ("requests ok", str(prod["req_ok"]), str(fork["req_ok"]),
         f"{fork['req_ok'] - prod['req_ok']:+d}"),
        ("requests failed", str(prod["req_fail"]), str(fork["req_fail"]),
         f"{fork['req_fail'] - prod['req_fail']:+d}"),
        ("tok/s (mean)", f"{prod['tps_mean']:.2f}", f"{fork['tps_mean']:.2f}",
         f"{fork['tps_mean'] - prod['tps_mean']:+.2f}"),
        ("tok/s (median)", f"{prod['tps_median']:.2f}", f"{fork['tps_median']:.2f}",
         f"{fork['tps_median'] - prod['tps_median']:+.2f}"),
        ("in_flight mean", f"{prod['in_flight_mean']:.2f}",
         f"{fork['in_flight_mean']:.2f}", "-"),
        ("in_flight min/max", f"{prod['in_flight_min']}/{prod['in_flight_max']}",
         f"{fork['in_flight_min']}/{fork['in_flight_max']}", "-"),
        ("RSS start (MiB)", str(prod["rss0"]), str(fork["rss0"]), "-"),
        ("RSS end (MiB)", str(prod["rss1"]), str(fork["rss1"]), "-"),
        ("RSS drift (MiB)", f"{prod['rss_drift_mb']:+d}",
         f"{fork['rss_drift_mb']:+d}",
         f"{fork['rss_drift_mb'] - prod['rss_drift_mb']:+d}"),
        ("RSS drift (%)", fmt_pct(prod["rss_drift_pct"]),
         fmt_pct(fork["rss_drift_pct"]),
         fmt_pct(fork["rss_drift_pct"] - prod["rss_drift_pct"])),
        ("VRAM start (MiB)", str(prod["vram0"]), str(fork["vram0"]), "-"),
        ("VRAM end (MiB)", str(prod["vram1"]), str(fork["vram1"]), "-"),
        ("VRAM drift (MiB)", f"{prod['vram_drift_mb']:+d}",
         f"{fork['vram_drift_mb']:+d}", "-"),
    ]
    for r in rows:
        print("| " + " | ".join(r) + " |")

    print("\n## Per-pid RSS drift (MiB)\n")
    print("| port | prod | fork |")
    print("|---|---|---|")
    all_keys = sorted(set(prod["per_pid_drift_mb"]) | set(fork["per_pid_drift_mb"]))
    for k in all_keys:
        p = prod["per_pid_drift_mb"].get(k, "-")
        f = fork["per_pid_drift_mb"].get(k, "-")
        print(f"| {k} | {p} | {f} |")
    return 0


if __name__ == "__main__":
    sys.exit(main())
