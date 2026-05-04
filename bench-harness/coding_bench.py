#!/usr/bin/env python3
"""Coding bench for the viiwork autonomous evaluation.

Two prompts (short / long-form React todo app), 3 phases:
  P1: warmup + single-stream quality run on each prompt (saves full output).
  P2: single-stream throughput sample (5 reqs of prompt A).
  P3: sustained concurrent at the layout's natural ceiling.

Saves throughput numbers to <out_dir>/summary.json and full first-response
outputs (P1) to <out_dir>/quality_*.txt for side-by-side eval.

Args:  --label <id> --url <url> --model <model> [--conc <int>] [--duration <sec>]
       [--max-tokens <int>] [--system <str>]
"""
from __future__ import annotations
import argparse
import json
import os
import statistics
import sys
import threading
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor


def _print(*args, **kwargs):
    kwargs.setdefault("flush", True)
    print(*args, **kwargs)

PROMPT_SHORT = (
    "Build a React functional component for a todo list with: add task, "
    "remove task, toggle complete, and localStorage persistence. "
    "Use modern React (hooks). Provide the full self-contained component code."
)

PROMPT_LONG = (
    "Build a complete React todo app suitable for a small team. Requirements:\n"
    "- useReducer for state management (actions: ADD, REMOVE, TOGGLE, EDIT, REORDER, CLEAR_COMPLETED)\n"
    "- Dark / light theme toggle persisted to localStorage\n"
    "- Drag-and-drop reordering (use @dnd-kit/sortable)\n"
    "- Due-date input with overdue highlighting and a 'today / this week / later' filter\n"
    "- Inline edit on double-click, with escape to cancel\n"
    "- Keyboard shortcuts (n: new, /: focus search, d: toggle dark)\n"
    "- Unit tests for the reducer using Vitest\n"
    "Provide ALL files: App.jsx, the reducer module, the components, the test file, and a brief README explaining the file layout. "
    "Use modern React (hooks), Vite-style structure, and JS (not TypeScript)."
)


def request(url: str, model: str, system: str, prompt: str, max_tokens: int,
            timeout: float = 1800.0) -> dict:
    payload = {
        "model": model,
        "messages": (
            [{"role": "system", "content": system}] if system else []
        ) + [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "temperature": 0.3,
    }
    body = json.dumps(payload).encode()
    req = urllib.request.Request(
        url, data=body, headers={"Content-Type": "application/json"}
    )
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            j = json.loads(resp.read())
        elapsed = time.time() - t0
        usage = j.get("usage", {})
        timings = j.get("timings", {})
        msg = j.get("choices", [{}])[0].get("message", {})
        return {
            "ok": True,
            "elapsed": elapsed,
            "completion_tokens": usage.get("completion_tokens", 0),
            "prompt_tokens": usage.get("prompt_tokens", 0),
            "tps": timings.get("predicted_per_second", 0.0),
            "prompt_tps": timings.get("prompt_per_second", 0.0),
            "content": msg.get("content", ""),
            "reasoning": msg.get("reasoning_content") or msg.get("reasoning", ""),
        }
    except Exception as e:
        return {"ok": False, "elapsed": time.time() - t0, "error": str(e)}


def pct(xs: list[float], p: float) -> float:
    if not xs:
        return 0.0
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round(p / 100 * (len(xs) - 1)))))
    return xs[k]


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--label", required=True, help="L1, L2, ...")
    ap.add_argument("--url", default="http://localhost:8094/v1/chat/completions")
    ap.add_argument("--model", required=True)
    ap.add_argument("--system", default="")
    ap.add_argument("--conc", type=int, default=4)
    ap.add_argument("--duration", type=int, default=240)
    ap.add_argument("--max-tokens", type=int, default=8192)
    ap.add_argument("--out", default="bench-harness/results")
    args = ap.parse_args()

    out_dir = os.path.join(args.out, f"{args.label}-{time.strftime('%Y%m%dT%H%M%S')}")
    os.makedirs(out_dir, exist_ok=True)
    _print(f"[{args.label}] writing results to {out_dir}")
    _print(f"[{args.label}] model={args.model} max_tokens={args.max_tokens} "
          f"conc={args.conc} duration={args.duration}s")

    summary = {
        "label": args.label,
        "model": args.model,
        "system": args.system,
        "max_tokens": args.max_tokens,
        "conc": args.conc,
        "duration": args.duration,
    }

    # --- P1: warmup + quality sample on each prompt ---
    _print(f"\n[{args.label}] === P1: quality sample (single-stream, both prompts) ===")
    p1_results = {}
    for name, prompt in [("short", PROMPT_SHORT), ("long", PROMPT_LONG)]:
        _print(f"  [{name}] sending...")
        r = request(args.url, args.model, args.system, prompt, args.max_tokens)
        if r["ok"]:
            _print(f"  [{name}] {r['completion_tokens']} tok in {r['elapsed']:.1f}s "
                  f"-> {r['tps']:.1f} tok/s (prompt {r['prompt_tps']:.1f} tok/s)")
            with open(os.path.join(out_dir, f"quality_{name}.txt"), "w") as f:
                f.write(f"# {args.label} / {name}\n")
                f.write(f"# model: {args.model}\n")
                f.write(f"# max_tokens: {args.max_tokens}, system: {args.system!r}\n")
                f.write(f"# completion_tokens: {r['completion_tokens']}, "
                        f"elapsed: {r['elapsed']:.2f}s, tps: {r['tps']:.2f}\n\n")
                if r.get("reasoning"):
                    f.write("=== REASONING ===\n")
                    f.write(r["reasoning"])
                    f.write("\n\n=== ANSWER ===\n")
                f.write(r["content"])
            p1_results[name] = {
                "completion_tokens": r["completion_tokens"],
                "elapsed": r["elapsed"],
                "tps": r["tps"],
                "prompt_tps": r["prompt_tps"],
            }
        else:
            _print(f"  [{name}] FAIL: {r['error']}")
            p1_results[name] = {"error": r["error"]}
    summary["p1_quality"] = p1_results

    # --- P2: single-stream throughput sample (5x short prompt) ---
    _print(f"\n[{args.label}] === P2: single-stream throughput (3x short prompt) ===")
    p2_results = []
    for i in range(3):
        r = request(args.url, args.model, args.system, PROMPT_SHORT, args.max_tokens)
        if r["ok"]:
            _print(f"  [{i+1}/5] {r['completion_tokens']} tok in {r['elapsed']:.1f}s "
                  f"-> {r['tps']:.1f} tok/s")
            p2_results.append({
                "completion_tokens": r["completion_tokens"],
                "elapsed": r["elapsed"],
                "tps": r["tps"],
            })
        else:
            _print(f"  [{i+1}/5] FAIL: {r['error']}")
    if p2_results:
        tpss = [r["tps"] for r in p2_results]
        lats = [r["elapsed"] for r in p2_results]
        toks = [r["completion_tokens"] for r in p2_results]
        summary["p2_single_stream"] = {
            "n": len(p2_results),
            "tps_mean": statistics.mean(tpss),
            "tps_min": min(tpss),
            "tps_max": max(tpss),
            "lat_mean": statistics.mean(lats),
            "lat_p50": pct(lats, 50),
            "lat_p95": pct(lats, 95),
            "tok_mean": statistics.mean(toks),
        }
        _print(f"  -> mean {summary['p2_single_stream']['tps_mean']:.1f} tok/s, "
              f"lat p50 {summary['p2_single_stream']['lat_p50']:.1f}s")

    # --- P3: sustained concurrent (short prompts only, capped output) ---
    p3_max_tokens = min(args.max_tokens, 2048)
    _print(f"\n[{args.label}] === P3: sustained concurrent (conc={args.conc}, "
          f"duration={args.duration}s, short-only, max_tokens={p3_max_tokens}) ===")
    deadline = time.time() + args.duration
    t_start = time.time()
    results: list[dict] = []
    lock = threading.Lock()
    counter = {"i": 0}

    def worker():
        while time.time() < deadline:
            r = request(args.url, args.model, args.system,
                        PROMPT_SHORT, p3_max_tokens)
            with lock:
                results.append(r)
                if len(results) % 3 == 0:
                    elapsed = time.time() - t_start
                    ok = sum(1 for x in results if x["ok"])
                    _print(f"  t+{elapsed:>5.1f}s  reqs={len(results)}  ok={ok}")

    with ThreadPoolExecutor(max_workers=args.conc) as pool:
        for _ in range(args.conc):
            pool.submit(worker)

    wall = time.time() - t_start
    ok = [r for r in results if r["ok"]]
    fail = [r for r in results if not r["ok"]]
    total_tok = sum(r["completion_tokens"] for r in ok)
    agg_tps = total_tok / wall if wall > 0 else 0.0
    p3 = {
        "wall_seconds": wall,
        "requests_total": len(results),
        "requests_ok": len(ok),
        "requests_fail": len(fail),
        "completion_tokens_total": total_tok,
        "agg_tps": agg_tps,
    }
    if ok:
        lats = [r["elapsed"] for r in ok]
        tpss = [r["tps"] for r in ok]
        p3["lat_mean"] = statistics.mean(lats)
        p3["lat_p50"] = pct(lats, 50)
        p3["lat_p95"] = pct(lats, 95)
        p3["lat_max"] = max(lats)
        p3["per_req_tps_mean"] = statistics.mean(tpss)
    summary["p3_concurrent"] = p3
    _print(f"\n  duration: {wall:.1f}s")
    _print(f"  requests: total {len(results)}, ok {len(ok)}, fail {len(fail)}")
    _print(f"  AGGREGATE: {agg_tps:.1f} tok/s (total {total_tok} tokens)")
    if ok:
        _print(f"  latency: mean {p3['lat_mean']:.1f}s, p50 {p3['lat_p50']:.1f}s, "
              f"p95 {p3['lat_p95']:.1f}s, max {p3['lat_max']:.1f}s")

    with open(os.path.join(out_dir, "summary.json"), "w") as f:
        json.dump(summary, f, indent=2)
    _print(f"\n[{args.label}] summary written to {out_dir}/summary.json")


if __name__ == "__main__":
    main()
