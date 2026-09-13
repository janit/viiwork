#!/usr/bin/env python3
"""Fleet soak: drive every model under, at and over its measured capacity.

    ./soak.py --url http://teddy:8086 --minutes 60

What makes this different from bench.py and bench-sustained.sh: concurrency is
not a number you pick. It is read from /v1/fleet/capacity per model and scaled
per phase, so "over capacity" means over the capacity the fleet actually has
right now rather than over a guess made when the script was written.

Every model is driven at once, each at its own ratio. A fleet serves all its
models simultaneously, and cross-model effects — two models on one host, the
node-wide load gate, a busy peer refusing a forward — only appear that way.

What it is looking for, phase by phase:

  under     queue stays empty, TTFB flat. The baseline.
  at        the queue engages. TTFB rises while tokens/sec stays flat; that
            split is the signature of queueing rather than slow backends,
            which is why TTFB and total are recorded separately.
  over      backpressure, and WHICH KIND. 503 with Retry-After (nothing served
            it in queue_timeout) and 429 (a forward hit a full node) are the
            designed answers. Connection errors, timeouts, or a bare 5xx are
            not, and are the finding if they appear.
  recover   capacity comes back promptly. This phase exists for the refusal
            marks: a refused forward marks that (member, model) full until a
            REPORT RECEIVED AFTER the refusal arrives, and a bug there would
            strand capacity after the burst rather than during it.

Stdlib only, so it runs anywhere the fleet does.
"""

import argparse
import json
import statistics
import sys
import threading
import time
import urllib.error
import urllib.request
from collections import defaultdict
from datetime import datetime, timezone

# A slot is held for the LIFETIME of a request, so prompt and reply size decide
# whether capacity is actually scarce. A calibration run on granite at 2x with
# 48-token replies produced zero refusals and higher throughput than at 1x: the
# queue drained faster than it filled, so the backpressure path was never
# reached. Production-shaped work is what makes slots contended.
PROMPT = (
    "You are writing for a driving-directions website. Write two paragraphs, "
    "200-300 words, describing Helsinki for someone planning to drive there. "
    "Emphasise what matters to a driver: which roads connect it, what the drive "
    "is like, and what makes the place worth the trip. Work the geography and "
    "climate in naturally rather than as a list. Plain, direct sentences, varied "
    "length. No headings, no bullets, no em dashes. End with one concrete, "
    "specific detail about the place rather than a generic flourish."
)

# ratio of fleet capacity, and share of the run. Ratios are per model: a model
# with 36 slots and one with 3 are each driven at their own multiple, so no
# model is incidentally saturated while another idles.
def build_phases(over_ratios):
    """warm, under, at, then one phase per over-ratio, then recover.

    The over-ratios are a list because 2x is not guaranteed to reach the limit
    — see the PROMPT note. Escalating until refusals appear is what locates the
    saturation point, rather than assuming slots is it.
    """
    over = [(f"over{r:g}x", r) for r in over_ratios]
    fixed = 0.05 + 0.20 + 0.20 + 0.20          # warm, under, at, recover
    each = (1.0 - fixed) / max(1, len(over))
    return ([("warm", 0.10, 0.05), ("under", 0.50, 0.20), ("at", 1.00, 0.20)]
            + [(n, r, each) for n, r in over]
            + [("recover", 0.50, 0.20)])


PHASES = build_phases([2.0])


def get_json(url, timeout=10):
    req = urllib.request.Request(url, headers={"Accept": "application/json"})
    with urllib.request.urlopen(req, timeout=timeout) as r:
        return json.loads(r.read().decode())


def aliases_for(base):
    """real model name -> an alias pointing at it, for alias-addressed traffic.

    Alias resolution happens on the ORIGIN node, once per request, before
    routing. beta3 made ?model= resolve aliases too, so a soak that only ever
    sends real names never exercises either path under load.
    """
    out = {}
    try:
        d = get_json(f"{base}/v1/aliases")
    except Exception:
        return out
    for a in d.get("aliases", []):
        if a.get("state") == "ok" and a.get("resolved"):
            out.setdefault(a["resolved"], a["name"])
    return out


def fleet_capacity(base):
    """Per-model fleet slots and the hosts serving them."""
    d = get_json(f"{base}/v1/fleet/capacity")
    out = {}
    for m in d.get("models", []):
        hosts = [h for h in m.get("hosts", []) if not h.get("stale")]
        if m["slots"] > 0 and hosts:
            out[m["name"]] = {
                "slots": m["slots"],
                "ctx": m.get("ctx"),
                "urls": [f"http://{h['api']}" for h in hosts if h.get("api")] or [base],
            }
    return out


class Recorder:
    """Per-request outcomes and the sampled fleet timeline."""

    def __init__(self):
        self.lock = threading.Lock()
        self.reqs = []       # dicts, one per request
        self.timeline = []   # capacity + power samples

    def add(self, **kw):
        with self.lock:
            self.reqs.append(kw)

    def sample(self, row):
        with self.lock:
            self.timeline.append(row)


def one_request(url, model, max_tokens, timeout):
    """Send one streaming completion. Returns (status, ttfb_ms, total_ms,
    tokens, retry_after).

    Streaming is not incidental: TTFB is only meaningful with it, and TTFB is
    the number that separates "queued" from "slow".
    """
    body = json.dumps({
        "model": model,
        "messages": [{"role": "user", "content": PROMPT}],
        "max_tokens": max_tokens,
        "stream": True,
    }).encode()
    req = urllib.request.Request(
        f"{url}/v1/chat/completions", data=body,
        headers={"Content-Type": "application/json"}, method="POST")

    t0 = time.monotonic()
    ttfb = None
    tokens = 0
    try:
        with urllib.request.urlopen(req, timeout=timeout) as r:
            for raw in r:
                if ttfb is None:
                    ttfb = (time.monotonic() - t0) * 1000
                if raw.startswith(b"data: ") and b'"content"' in raw:
                    tokens += 1
            return r.status, ttfb, (time.monotonic() - t0) * 1000, tokens, None
    except urllib.error.HTTPError as e:
        # The designed backpressure answers arrive here. Retry-After is the
        # field that distinguishes "full, come back" from an unexplained 5xx.
        ra = e.headers.get("Retry-After")
        e.read()
        return e.code, ttfb, (time.monotonic() - t0) * 1000, 0, ra
    except Exception as e:                      # timeout, refused, reset
        return type(e).__name__, ttfb, (time.monotonic() - t0) * 1000, 0, None


def worker(stop, rec, model, urls, idx, phase, max_tokens, timeout, send_as=None):
    """One in-flight request at a time, looping until the phase ends.

    Each worker pins a URL round-robin across the model's hosts, which is the
    configuration docs/consuming-fleet-capacity.md tells a consumer to use.
    """
    url = urls[idx % len(urls)]
    name = send_as or model
    while not stop.is_set():
        status, ttfb, total, tokens, ra = one_request(url, name, max_tokens, timeout)
        rec.add(phase=phase, model=model, via=("alias" if send_as else "direct"), url=url, status=status,
                ttfb_ms=ttfb, total_ms=total, tokens=tokens, retry_after=ra,
                t=time.time())


def sampler(stop, rec, base, interval=5.0):
    """Fleet capacity and per-node power, every interval."""
    while not stop.is_set():
        row = {"t": time.time()}
        try:
            cap = get_json(f"{base}/v1/fleet/capacity", timeout=5)
            row["models"] = {
                m["name"]: {"slots": m["slots"], "busy": m["busy"],
                            "free": m["free"], "queued": m["queued"]}
                for m in cap.get("models", [])
            }
        except Exception as e:
            row["capacity_error"] = type(e).__name__
        try:
            cl = get_json(f"{base}/v1/cluster", timeout=5)
            row["power_w"] = {
                m["node"]: ((m.get("status") or {}).get("power") or {}).get("watts")
                for m in cl.get("members", [])
            }
        except Exception as e:
            row["power_error"] = type(e).__name__
        rec.sample(row)
        stop.wait(interval)


def pct(xs, p):
    xs = [x for x in xs if x is not None]
    if not xs:
        return None
    xs = sorted(xs)
    k = max(0, min(len(xs) - 1, int(round((p / 100) * (len(xs) - 1)))))
    return xs[k]


def report(rec, models, out_prefix):
    by = defaultdict(list)
    for r in rec.reqs:
        by[(r["phase"], r["model"])].append(r)

    print("\n" + "=" * 100)
    print("PHASE RESULTS  (ttfb separates queueing from slow backends)")
    print("=" * 100)
    hdr = f'{"phase":8} {"model":24} {"n":>5} {"ok":>5} {"429":>4} {"503":>4} {"err":>4} {"ttfb p50":>9} {"p95":>7} {"p99":>7} {"tok/s":>7}'
    print(hdr)
    print("-" * 100)
    for phase, _, _ in PHASES:
        for model in models:
            rs = by.get((phase, model))
            if not rs:
                continue
            ok = [r for r in rs if r["status"] == 200]
            n429 = sum(1 for r in rs if r["status"] == 429)
            n503 = sum(1 for r in rs if r["status"] == 503)
            err = sum(1 for r in rs if not isinstance(r["status"], int))
            span = max(r["t"] for r in rs) - min(r["t"] for r in rs) or 1
            toks = sum(r["tokens"] for r in ok) / span
            print(f'{phase:8} {model:24} {len(rs):5} {len(ok):5} {n429:4} {n503:4} {err:4} '
                  f'{_f(pct([r["ttfb_ms"] for r in ok], 50)):>9} '
                  f'{_f(pct([r["ttfb_ms"] for r in ok], 95)):>7} '
                  f'{_f(pct([r["ttfb_ms"] for r in ok], 99)):>7} '
                  f'{toks:7.1f}')

    # The judgements the phases exist to make.
    print("\n" + "=" * 100)
    print("FINDINGS")
    print("=" * 100)
    # Transport failures are checked across the WHOLE run, not just the over
    # phases. The first version of this looked only at over-capacity and
    # printed "OK, 0" while five timeouts sat in warm and under — a soak that
    # only inspects the phase it expects trouble in will miss trouble
    # everywhere else.
    allbad = [r for r in rec.reqs if not isinstance(r["status"], int)]
    if allbad:
        byp = defaultdict(int)
        for r in allbad:
            byp[(r["phase"], r["model"], r["status"])] += 1
        print(f"  FINDING: {len(allbad)} transport failures across the run:")
        for (ph, m, s), n in sorted(byp.items()):
            print(f"      {n:5} x {s:18} {m:24} in {ph}")
    else:
        print("  OK  transport failures across the run: 0")

    over = [r for r in rec.reqs if r["phase"].startswith("over")]
    if over:
        bad = [r for r in over if not isinstance(r["status"], int)]
        n503 = [r for r in over if r["status"] == 503]
        no_ra = [r for r in n503 if not r["retry_after"]]
        print(f"  over-capacity: {len(over)} requests, "
              f"{sum(1 for r in over if r['status']==200)} served, "
              f"{sum(1 for r in over if r['status']==429)} x 429, {len(n503)} x 503")
        print(f"  {'OK ' if not bad else 'FINDING: '}over-capacity transport failures: {len(bad)}"
              + ("" if not bad else "  <-- backpressure should be a STATUS, not a dropped connection"))
        print(f"  {'OK ' if not no_ra else 'FINDING: '}503s without Retry-After: {len(no_ra)}/{len(n503)}"
              + ("" if not no_ra else "  <-- a 503 must tell a client when to come back"))
    al = [r for r in rec.reqs if r.get("via") == "alias"]
    if al:
        bad = [r for r in al if not isinstance(r["status"], int)]
        ok = sum(1 for r in al if r["status"] == 200)
        print(f"  alias-addressed: {len(al)} requests, {ok} served, {len(bad)} transport failures")

    u = [r["ttfb_ms"] for r in rec.reqs if r["phase"] == "under" and r["status"] == 200]
    v = [r["ttfb_ms"] for r in rec.reqs if r["phase"] == "recover" and r["status"] == 200]
    if u and v:
        a, b = pct(u, 50), pct(v, 50)
        if a and b:
            ratio = b / a
            print(f"  {'OK ' if ratio < 1.5 else 'FINDING: '}recovery TTFB p50 {b:.0f}ms vs under {a:.0f}ms "
                  f"({ratio:.2f}x)" + ("" if ratio < 1.5 else "  <-- capacity did not come back; check refusal marks"))

    raw = f"{out_prefix}-requests.json"
    tl = f"{out_prefix}-timeline.json"
    with open(raw, "w") as f:
        json.dump(rec.reqs, f)
    with open(tl, "w") as f:
        json.dump(rec.timeline, f)
    print(f"\n  raw: {raw}\n  timeline: {tl}")


def _f(v):
    return "-" if v is None else f"{v:.0f}"


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--url", default="http://127.0.0.1:8086", help="any node; used for discovery and sampling")
    ap.add_argument("--minutes", type=float, default=60)
    ap.add_argument("--max-tokens", type=int, default=300,
                    help="production-shaped replies; short ones free slots too fast to contend")
    ap.add_argument("--over", default="2,4",
                    help="comma-separated over-capacity multiples, escalating until refusals appear")
    ap.add_argument("--timeout", type=float, default=180)
    ap.add_argument("--models", default="", help="comma-separated subset (default: every model in the fleet)")
    ap.add_argument("--ingress", choices=["spread", "single"], default="spread",
                    help="spread: one URL per host, as a consumer should. "
                         "single: everything through --url, which tests forwarding instead")
    ap.add_argument("--alias-share", type=float, default=0.25,
                    help="fraction of workers addressing a model by its alias, where one exists; "
                         "alias resolution is per-request on the origin node and deserves load")
    ap.add_argument("--dry-run", action="store_true", help="print the plan and exit")
    a = ap.parse_args()

    global PHASES
    PHASES = build_phases([float(x) for x in a.over.split(",") if x.strip()])

    cap = fleet_capacity(a.url)
    alias = aliases_for(a.url) if a.alias_share > 0 else {}
    if a.models:
        want = {m.strip() for m in a.models.split(",")}
        cap = {k: v for k, v in cap.items() if k in want}
    if not cap:
        sys.exit("no models with capacity found")

    total = a.minutes * 60
    print(f"fleet soak  {a.minutes:g} min  via {a.url}  ingress={a.ingress}")
    print(f"{'model':24} {'slots':>5} " + " ".join(f"{p:>7}" for p, _, _ in PHASES))
    for name, info in sorted(cap.items(), key=lambda kv: -kv[1]["slots"]):
        conc = [max(1, round(info["slots"] * r)) for _, r, _ in PHASES]
        print(f"{name:24} {info['slots']:5} " + " ".join(f"{c:>7}" for c in conc))
    if alias:
        print("alias-addressed traffic at %.0f%%: %s" % (
            a.alias_share * 100, ", ".join(f"{v}->{k}" for k, v in sorted(alias.items()))))
    peak = sum(max(1, round(i["slots"] * 2.0)) for i in cap.values())
    print(f"\npeak concurrency at 2x: {peak} in flight across {len(cap)} models")
    for p, r, s in PHASES:
        print(f"  {p:8} {r:>5.2f}x  {total*s/60:5.1f} min")
    if a.dry_run:
        return

    rec = Recorder()
    stop_all = threading.Event()
    st = threading.Thread(target=sampler, args=(stop_all, rec, a.url), daemon=True)
    st.start()

    try:
        for phase, ratio, share in PHASES:
            dur = total * share
            stop = threading.Event()
            threads = []
            for name, info in cap.items():
                urls = info["urls"] if a.ingress == "spread" else [a.url]
                n = max(1, round(info["slots"] * ratio))
                n_alias = int(round(n * a.alias_share)) if name in alias else 0
                for i in range(n):
                    send_as = alias[name] if i < n_alias else None
                    t = threading.Thread(target=worker,
                                         args=(stop, rec, name, urls, i, phase, a.max_tokens, a.timeout, send_as),
                                         daemon=True)
                    t.start()
                    threads.append(t)
            print(f"\n[{datetime.now(timezone.utc).strftime('%H:%M:%S')}] {phase} "
                  f"({ratio:g}x, {len(threads)} in flight) for {dur/60:.1f} min")
            time.sleep(dur)
            stop.set()
            for t in threads:
                t.join(timeout=a.timeout + 10)
    except KeyboardInterrupt:
        print("\ninterrupted; reporting what was collected")
    finally:
        stop_all.set()

    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    report(rec, list(cap), f"results/soak-{stamp}")


if __name__ == "__main__":
    main()
