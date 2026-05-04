#!/usr/bin/env python3
"""Phase 2 mixed bench: hit gpt-oss + coding-side simultaneously,
measure per-side throughput while both are under load.
"""
from __future__ import annotations
import json, statistics, sys, threading, time, urllib.request

CODING_URL = "http://localhost:8094/v1/chat/completions"
CODING_MODEL = "Qwen_Qwen3.5-122B-A10B-Q4_K_M-00001-of-00002"
GENERAL_URL = "http://localhost:8095/v1/chat/completions"
GENERAL_MODEL = "openai_gpt-oss-120b-MXFP4_MOE-00001-of-00002"

CODING_PROMPT = (
    "Build a React functional component for a todo list with: add task, "
    "remove task, toggle complete, and localStorage persistence. "
    "Use modern React (hooks). Provide the full self-contained component code."
)
GENERAL_PROMPT = (
    "Explain in 3 short paragraphs how Raft consensus achieves fault tolerance "
    "with a leader, followers, and log replication. Avoid bullet points."
)

DURATION = 240  # seconds
MAX_TOKENS_CODING = 4096
MAX_TOKENS_GENERAL = 1024


def request(url, model, prompt, max_tokens, system=""):
    payload = {
        "model": model,
        "messages": ([{"role": "system", "content": system}] if system else []) +
                    [{"role": "user", "content": prompt}],
        "max_tokens": max_tokens,
        "temperature": 0.3,
    }
    body = json.dumps(payload).encode()
    req = urllib.request.Request(url, data=body, headers={"Content-Type": "application/json"})
    t0 = time.time()
    try:
        with urllib.request.urlopen(req, timeout=600) as resp:
            j = json.loads(resp.read())
        return {
            "ok": True,
            "elapsed": time.time() - t0,
            "tokens": j.get("usage", {}).get("completion_tokens", 0),
            "tps": j.get("timings", {}).get("predicted_per_second", 0.0),
        }
    except Exception as e:
        return {"ok": False, "elapsed": time.time() - t0, "error": str(e)}


def runner(label, url, model, prompt, max_tokens, system, deadline, results, lock):
    while time.time() < deadline:
        r = request(url, model, prompt, max_tokens, system)
        with lock:
            results[label].append(r)
            n = len(results[label])
            if n % 2 == 0 or not r["ok"]:
                elapsed = time.time() - results["t_start"]
                tag = "ok" if r["ok"] else f"FAIL: {r.get('error','')[:50]}"
                print(f"  [{label}] t+{elapsed:>5.1f}s  req#{n}  {r['tokens']} tok in "
                      f"{r['elapsed']:.1f}s ({r['tps']:.1f} tok/s)  [{tag}]", flush=True)


def stats(rs):
    ok = [r for r in rs if r["ok"]]
    if not ok:
        return {"n": 0, "fail": len(rs)}
    tps = [r["tps"] for r in ok]
    lat = [r["elapsed"] for r in ok]
    toks = [r["tokens"] for r in ok]
    return {
        "n_ok": len(ok),
        "n_fail": len(rs) - len(ok),
        "tps_mean_per_req": statistics.mean(tps),
        "tps_min": min(tps),
        "tps_max": max(tps),
        "lat_mean": statistics.mean(lat),
        "lat_p50": sorted(lat)[len(lat)//2],
        "lat_p95": sorted(lat)[int(len(lat)*0.95)],
        "tokens_total": sum(toks),
    }


def main():
    print(f"=== Phase 2 mixed bench, duration={DURATION}s ===", flush=True)
    print(f"  coding:  {CODING_MODEL} on {CODING_URL}  max_tokens={MAX_TOKENS_CODING}", flush=True)
    print(f"  general: {GENERAL_MODEL} on {GENERAL_URL}  max_tokens={MAX_TOKENS_GENERAL}", flush=True)
    print()

    deadline = time.time() + DURATION
    results = {"coding": [], "general": [], "t_start": time.time()}
    lock = threading.Lock()
    threads = [
        threading.Thread(target=runner, args=("coding", CODING_URL, CODING_MODEL,
                                              CODING_PROMPT, MAX_TOKENS_CODING, "",
                                              deadline, results, lock)),
        threading.Thread(target=runner, args=("general", GENERAL_URL, GENERAL_MODEL,
                                              GENERAL_PROMPT, MAX_TOKENS_GENERAL,
                                              "Reasoning: low",
                                              deadline, results, lock)),
    ]
    for t in threads: t.start()
    for t in threads: t.join()

    wall = time.time() - results["t_start"]
    print(f"\n--- mixed-bench results ({wall:.1f}s wall) ---", flush=True)
    for label in ("coding", "general"):
        s = stats(results[label])
        print(f"\n[{label}] {s.get('n_ok',0)} ok / {s.get('n_fail',0)} fail")
        if s.get("n_ok"):
            agg = s["tokens_total"] / wall
            print(f"  per-req tps: mean {s['tps_mean_per_req']:.1f}  range {s['tps_min']:.1f}-{s['tps_max']:.1f}")
            print(f"  latency: p50 {s['lat_p50']:.1f}s  p95 {s['lat_p95']:.1f}s  mean {s['lat_mean']:.1f}s")
            print(f"  tokens: total {s['tokens_total']}  agg tps: {agg:.1f}")

    out = {
        "duration": DURATION,
        "wall_seconds": wall,
        "coding": stats(results["coding"]),
        "general": stats(results["general"]),
    }
    out_path = "bench-harness/results/coding-eval-2026-05-04/phase2-mixed.json"
    with open(out_path, "w") as f:
        json.dump(out, f, indent=2)
    print(f"\nsaved to {out_path}", flush=True)


if __name__ == "__main__":
    main()
