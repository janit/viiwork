#!/usr/bin/env python3
"""Single-cluster soak driver. Hammers ONE viiwork cluster with N concurrent
in-flight requests, polls per-pid RSS and per-GPU VRAM, writes a CSV time
series. Designed for the sequential A/B overnight test where prod and fork
clusters are exercised one at a time.

Companion to soak.py (which drives both clusters in parallel). The dual
driver is fine for a single-GPU smoke check, but for the real overnight
A/B we want:
  * one cluster up at a time (no shared PSU / thermal coupling)
  * configurable concurrency (5 in flight, not 1)
  * 5 GPUs per cluster, not 3

Usage:
  python3 soak_one.py \\
      --label smoke-prod --duration 5m \\
      --base-url http://localhost:8091 \\
      --backend-ports 9101 9102 9103 9104 9105 \\
      --gpus 5 6 7 8 9 \\
      --concurrency 5 --interval 30s \\
      --out /home/janit/gfx906-work/results/soak
"""
import argparse
import csv
import json
import os
import re
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from datetime import datetime, timezone
from pathlib import Path

# Same Helsinki/Tampere/Turku rotation as soak.py - real production prompt
# from the driving-directions site. Long structured input, free-form 200-300
# word generation, the exact shape that triggers the malloc drift the parent
# spec calls out. Imported as a module to avoid duplicating ~120 lines of
# prompt text.
from soak import HELSINKI_PROMPT, TAMPERE_PROMPT, TURKU_PROMPT, PROMPTS


@dataclass
class ClusterStats:
    name: str
    base_url: str
    backend_ports: list  # llama-server child ports inside the container
    gpus: list           # physical GPU indices for VRAM polling
    req_total: int = 0
    req_ok: int = 0
    req_fail: int = 0
    tokens_total: int = 0
    last_window_tokens: int = 0
    lock: threading.Lock = field(default_factory=threading.Lock)
    stop_event: threading.Event = field(default_factory=threading.Event)


def parse_duration(s: str) -> float:
    m = re.fullmatch(r"\s*(\d+(?:\.\d+)?)\s*([smhd]?)\s*", s)
    if not m:
        raise ValueError(f"bad duration: {s!r}")
    n = float(m.group(1))
    unit = m.group(2) or "s"
    return n * {"s": 1, "m": 60, "h": 3600, "d": 86400}[unit]


def find_pids_for_ports(ports):
    """Return {port: host_pid} for llama-server processes on the given ports."""
    out = {}
    try:
        pgrep = subprocess.check_output(
            ["pgrep", "-x", "llama-server"], text=True, timeout=2
        )
    except (subprocess.SubprocessError, FileNotFoundError):
        return out
    for line in pgrep.split():
        try:
            pid = int(line.strip())
        except ValueError:
            continue
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as fh:
                cmdline = fh.read().decode("utf-8", errors="replace")
        except OSError:
            continue
        for p in ports:
            if f"--port\x00{p}\x00" in cmdline:
                out[p] = pid
                break
    return out


def query_rss_mb(pid: int) -> int:
    if pid <= 0:
        return 0
    try:
        with open(f"/proc/{pid}/status") as fh:
            for line in fh:
                if line.startswith("VmRSS:"):
                    parts = line.split()
                    return int(parts[1]) // 1024
    except (OSError, ValueError):
        pass
    return 0


def query_in_flight(base_url: str) -> int:
    """Hit /v1/status and return total_in_flight. Used to verify the
    cluster is saturated end-to-end (concurrency 10 vs 5 GPUs * cap 2
    should hold this near 10 the entire run). Returns -1 on error."""
    try:
        with urllib.request.urlopen(f"{base_url}/v1/status", timeout=2) as resp:
            d = json.loads(resp.read().decode("utf-8"))
        return int(d.get("total_in_flight", -1))
    except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
        return -1


def query_vram_mb(gpu_index: int) -> int:
    try:
        out = subprocess.check_output(
            ["/opt/rocm/bin/rocm-smi", "--showmeminfo", "vram",
             "-d", str(gpu_index), "--json"],
            stderr=subprocess.DEVNULL, text=True, timeout=5,
        )
        data = json.loads(out)
        for card_id, fields in data.items():
            if not card_id.startswith("card"):
                continue
            for k, v in fields.items():
                kl = k.lower()
                if "vram" in kl and "used" in kl and "total" in kl:
                    return int(v) // (1024 * 1024)
    except (subprocess.SubprocessError, json.JSONDecodeError, FileNotFoundError, OSError):
        pass
    return 0


def driver_loop(stats: ClusterStats, worker_id: int, max_tokens: int, model: str):
    """One worker thread: tight loop firing /v1/chat/completions requests.
    With N workers all sharing the same ClusterStats, the cluster sees N
    in-flight requests at all times."""
    i = worker_id  # phase the prompt rotation per worker so they're not lockstep
    while not stats.stop_event.is_set():
        prompt = PROMPTS[i % len(PROMPTS)]
        i += len(PROMPTS) // 2 + 1
        body = json.dumps({
            "model": model,
            "messages": [{"role": "user", "content": prompt}],
            "temperature": 0.0,
            "top_k": 1,
            "max_tokens": max_tokens,
            "stream": False,
        }).encode("utf-8")
        req = urllib.request.Request(
            f"{stats.base_url}/v1/chat/completions",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        with stats.lock:
            stats.req_total += 1
        try:
            with urllib.request.urlopen(req, timeout=180) as resp:
                payload = json.loads(resp.read().decode("utf-8"))
            usage = payload.get("usage", {}) or {}
            ct = int(usage.get("completion_tokens", 0))
            with stats.lock:
                stats.req_ok += 1
                stats.tokens_total += ct
                stats.last_window_tokens += ct
        except (urllib.error.URLError, OSError, json.JSONDecodeError, ValueError):
            with stats.lock:
                stats.req_fail += 1
            time.sleep(0.5)


def sample_loop(stats: ClusterStats, interval: float, deadline: float, csv_path: Path):
    started = time.monotonic()
    fields = [
        "timestamp_utc", "elapsed_s",
        "req_total", "req_ok", "req_fail", "tps_window",
        "rss_mb", "rss_per_pid_json", "vram_mb", "vram_per_gpu_json",
        "in_flight",
    ]
    with open(csv_path, "w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow(fields)
        f.flush()

        while time.monotonic() < deadline:
            time.sleep(interval)
            now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            elapsed = time.monotonic() - started

            with stats.lock:
                req_total = stats.req_total
                req_ok = stats.req_ok
                req_fail = stats.req_fail
                win_tokens = stats.last_window_tokens
                stats.last_window_tokens = 0
            tps = win_tokens / interval if interval > 0 else 0.0

            pids = find_pids_for_ports(stats.backend_ports)
            rss_per_pid = {p: query_rss_mb(pid) for p, pid in pids.items()}
            rss_total = sum(rss_per_pid.values())
            vram_per_gpu = {g: query_vram_mb(g) for g in stats.gpus}
            vram_total = sum(vram_per_gpu.values())
            in_flight = query_in_flight(stats.base_url)

            writer.writerow([
                now, f"{elapsed:.1f}",
                req_total, req_ok, req_fail, f"{tps:.2f}",
                rss_total, json.dumps(rss_per_pid),
                vram_total, json.dumps(vram_per_gpu),
                in_flight,
            ])
            f.flush()
            print(
                f"[{now}] +{elapsed:6.0f}s "
                f"{stats.name}: rss {rss_total:6d} MiB vram {vram_total:6d} MiB "
                f"in_flight {in_flight:2d} "
                f"req {req_ok:6d} ({req_fail} fail) {tps:6.1f} tok/s",
                flush=True,
            )


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--label", required=True, help="run label, e.g. smoke-prod")
    p.add_argument("--duration", default="5m", help="e.g. 5m, 4h")
    p.add_argument("--interval", default="30s", help="sampling interval")
    p.add_argument("--base-url", required=True, help="e.g. http://localhost:8091")
    p.add_argument("--backend-ports", type=int, nargs="+", required=True,
                   help="llama-server child ports, e.g. 9101 9102 9103 9104 9105")
    p.add_argument("--gpus", type=int, nargs="+", required=True,
                   help="physical GPU indices for VRAM polling, e.g. 5 6 7 8 9")
    p.add_argument("--concurrency", type=int, default=5,
                   help="number of in-flight requests held against the cluster")
    p.add_argument("--model", default=None,
                   help="model name for requests (default: auto-detect from /v1/models)")
    p.add_argument("--max-tokens", type=int, default=400)
    p.add_argument("--out", type=Path, default=Path("/home/janit/gfx906-work/results/soak"))
    args = p.parse_args()

    duration_s = parse_duration(args.duration)
    interval_s = parse_duration(args.interval)
    args.out.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    csv_path = args.out / f"{timestamp}-{args.label}.csv"

    base_url = args.base_url.rstrip("/")
    model = args.model
    if model is None:
        try:
            with urllib.request.urlopen(f"{base_url}/v1/models", timeout=10) as resp:
                data = json.loads(resp.read().decode("utf-8"))
            models = [m["id"] for m in data.get("data", [])]
            if len(models) == 1:
                model = models[0]
            elif models:
                model = models[0]
                print(f"warning: multiple models available {models}, using {model}", flush=True)
            else:
                print("error: no models found at /v1/models, use --model", file=sys.stderr)
                return 1
        except (urllib.error.URLError, OSError, json.JSONDecodeError) as e:
            print(f"error: cannot auto-detect model from /v1/models: {e}", file=sys.stderr)
            print("       pass --model explicitly", file=sys.stderr)
            return 1
    print(f"model: {model}", flush=True)

    stats = ClusterStats(
        name=args.label,
        base_url=base_url,
        backend_ports=list(args.backend_ports),
        gpus=list(args.gpus),
    )

    drivers = []
    for w in range(args.concurrency):
        t = threading.Thread(
            target=driver_loop, args=(stats, w, args.max_tokens, model), daemon=True
        )
        t.start()
        drivers.append(t)

    deadline = time.monotonic() + duration_s
    print(
        f"soak start: label={args.label} duration={duration_s:.0f}s "
        f"interval={interval_s:.0f}s concurrency={args.concurrency} csv={csv_path}",
        flush=True,
    )
    try:
        sample_loop(stats, interval_s, deadline, csv_path)
    except KeyboardInterrupt:
        print("interrupted", flush=True)
    finally:
        stats.stop_event.set()
        for t in drivers:
            t.join(timeout=5)

    print(
        f"\nfinal: {stats.req_ok} ok / {stats.req_fail} fail / "
        f"{stats.tokens_total} tokens"
    )
    print(f"csv: {csv_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
