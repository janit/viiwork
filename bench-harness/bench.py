#!/usr/bin/env python3
"""Run the gfx906 benchmark suite and write results to a timestamped directory.

Usage:
    python3 bench.py --binary /path/to/llama-server --models /path/to/models --out ./results

Or with a docker-wrapped binary (so the runner shells out via `docker run`
into the viiwork image, which already contains an upstream-b8660 build):

    python3 bench.py --binary-cmd "docker run --rm -i ..." --models ... --out ...
"""
import argparse
import json
import shlex
import statistics
import sys
from datetime import datetime, timezone
from pathlib import Path

from runner import run_workload
from workloads import default_workloads


def main() -> int:
    p = argparse.ArgumentParser()
    p.add_argument("--binary", type=str, default=None,
                   help="Path to a llama-server binary")
    p.add_argument("--binary-cmd", type=str, default=None,
                   help="Shell-quoted command prefix that, when llama-server "
                        "args are appended, runs a llama-server (e.g. for "
                        "wrapping in `docker run`)")
    p.add_argument("--models", type=Path, required=True, help="Directory containing GGUF files")
    p.add_argument("--out", type=Path, required=True, help="Output dir for results")
    p.add_argument("--label", type=str, default="run", help="Run label for the output dir")
    p.add_argument("--only", type=str, default=None,
                   help="Run only the workload(s) whose name matches this substring")
    args = p.parse_args()

    if args.binary and args.binary_cmd:
        print("error: pass exactly one of --binary or --binary-cmd", file=sys.stderr)
        return 2
    if not args.binary and not args.binary_cmd:
        print("error: pass --binary or --binary-cmd", file=sys.stderr)
        return 2

    if args.binary:
        binary = Path(args.binary)
        if not binary.exists():
            print(f"binary not found: {binary}", file=sys.stderr)
            return 2
    else:
        binary = shlex.split(args.binary_cmd)

    if not args.models.is_dir():
        print(f"models dir not found: {args.models}", file=sys.stderr)
        return 2

    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    out_dir = args.out / f"{timestamp}-{args.label}"
    out_dir.mkdir(parents=True, exist_ok=True)

    workloads = default_workloads(args.models)
    if args.only:
        workloads = [w for w in workloads if args.only in w.name]
        if not workloads:
            print(f"--only={args.only} matched zero workloads", file=sys.stderr)
            return 2

    summary = {
        "timestamp": timestamp,
        "label": args.label,
        "binary": str(binary),
        "results": [],
    }

    for w in workloads:
        if not w.model_path.exists():
            print(f"SKIP {w.name}: model missing at {w.model_path}", file=sys.stderr)
            continue
        print(f"RUN  {w.name} (n_runs={w.n_runs})", flush=True)
        results = run_workload(binary, w)
        tps_values = [r.tokens_per_second for r in results]
        latencies = [r.latency_ms for r in results]
        peak_vram = max((r.peak_vram_mb for r in results), default=0)
        peak_rss = max((r.peak_rss_mb for r in results), default=0)
        baseline_rss = max((r.rss_baseline_mb for r in results), default=0)
        rss_growth = max((r.rss_growth_mb for r in results), default=0)

        entry = {
            "workload": w.name,
            "model": w.model_path.name,
            "n_predict": w.n_predict,
            "n_ctx": w.n_ctx,
            "n_runs": w.n_runs,
            "tokens_per_second_median": statistics.median(tps_values),
            "tokens_per_second_min": min(tps_values),
            "tokens_per_second_max": max(tps_values),
            "tokens_per_second_stdev": statistics.stdev(tps_values) if len(tps_values) > 1 else 0.0,
            "latency_ms_median": statistics.median(latencies),
            "peak_vram_mb": peak_vram,
            "peak_rss_mb": peak_rss,
            "rss_baseline_mb": baseline_rss,
            "rss_growth_mb": rss_growth,
            "raw_runs": [
                {"tokens_per_second": r.tokens_per_second, "latency_ms": r.latency_ms,
                 "peak_vram_mb": r.peak_vram_mb, "peak_rss_mb": r.peak_rss_mb,
                 "rss_baseline_mb": r.rss_baseline_mb, "rss_growth_mb": r.rss_growth_mb}
                for r in results
            ],
        }
        summary["results"].append(entry)
        print(
            f"OK   {w.name}: median {entry['tokens_per_second_median']:.2f} tok/s, "
            f"peak vram {peak_vram} MiB, peak rss {peak_rss} MiB "
            f"(growth +{rss_growth} MiB over baseline {baseline_rss})",
            flush=True,
        )

    summary_path = out_dir / "summary.json"
    summary_path.write_text(json.dumps(summary, indent=2))
    print(f"\nWrote {summary_path}")

    md_path = out_dir / "summary.md"
    md_path.write_text(format_markdown(summary))
    print(f"Wrote {md_path}")
    return 0


def format_markdown(summary: dict) -> str:
    lines = [
        f"# Benchmark run {summary['timestamp']} ({summary['label']})",
        "",
        f"Binary: `{summary['binary']}`",
        "",
        "| Workload | tok/s median | stdev | latency ms | VRAM MiB | RSS base MiB | RSS peak MiB | RSS growth MiB |",
        "|---|---|---|---|---|---|---|---|",
    ]
    for r in summary["results"]:
        lines.append(
            f"| {r['workload']} | {r['tokens_per_second_median']:.2f} | "
            f"{r['tokens_per_second_stdev']:.2f} | {r['latency_ms_median']:.1f} | "
            f"{r['peak_vram_mb']} | {r.get('rss_baseline_mb', 0)} | "
            f"{r.get('peak_rss_mb', 0)} | +{r.get('rss_growth_mb', 0)} |"
        )
    return "\n".join(lines) + "\n"


if __name__ == "__main__":
    sys.exit(main())
