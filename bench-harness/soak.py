#!/usr/bin/env python3
"""Long-running memory soak: drive sustained load against both viiwork
clusters in parallel and record per-cluster RSS + VRAM as a CSV time
series. Designed to answer the parent spec's primary success criterion:
"<= 5 % RSS drift over a 24 h production-load soak".

Setup assumed:
  cluster A: viiwork (production binary, port 8080, GPUs 0,1,2)
  cluster B: viiwork-gfx906 (stripped fork, port 8090, GPUs 4,5,6)

Output CSV columns:
  timestamp_utc, elapsed_s,
  prod_req_total, prod_req_ok, prod_req_fail, prod_tps_window,
  fork_req_total, fork_req_ok, fork_req_fail, fork_tps_window,
  prod_rss_mb, prod_rss_per_pid, prod_vram_mb,
  fork_rss_mb, fork_rss_per_pid, fork_vram_mb

The per-pid columns are JSON arrays; total columns are sums across the
3 backend processes / 3 GPUs of each cluster.

Usage:
  python3 soak.py --duration 6h --interval 30s --label overnight-1 --out ./soak-results
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

# Real production prompt from the driving-directions site that the
# viiwork cluster serves. This prompt is known to trigger the host RSS
# drift the parent spec calls out: long structured input, free-form
# generation in the 200-300 word range, repeated thousands of times per
# day. The 24h soak's primary purpose is to characterize that drift.
#
# Two minor location variants are mixed in (~10 % share each) so the
# slot KV cache can't lock onto a single warm-prompt path - it has to
# do real prompt-eval on each request, which is what production sees.
HELSINKI_PROMPT = """Write a short description of a location for a driving directions website. Write like a local who knows this place well.

## Location Data

- Name: Helsinki
- Country: Finland
- Coordinates: 60.1712N, 24.9327E
- Type: city
- Region: Uusimaa
- Population: 23,000
- Founded: 1550
- Elevation: 17m
- Languages: Finnish, Swedish
- Coastal: yes
- Airport: yes
- Rail connections: yes
- Metro/subway: yes

## Known For

Finnish capital, design district, Helsinki Cathedral, Suomenlinna fortress, sauna culture, Nordic architecture

## Geographic Features

Gulf of Finland, Baltic Sea, archipelago

## Climate (monthly avg C)

Jan: -4.5, Feb: -5, Mar: -1.5, Apr: 4, May: 10.5, Jun: 15, Jul: 18, Aug: 16.5, Sep: 11.5, Oct: 6, Nov: 1, Dec: -2.5

## Background (reference material, do not copy verbatim)

Helsinki is the capital and most populous city of Finland, situated on the shore of the Gulf of Finland in the Uusimaa region of southern Finland. The city municipality has a population of around 690,000, with 1.3 million in the capital region and 1.6 million in the wider metropolitan area. Founded in 1550 by King Gustav I of Sweden as a trading town to rival the Hanseatic port of Tallinn, Helsinki became the capital of the Grand Duchy of Finland under Russian rule in 1812 and has remained the capital through Finnish independence in 1917 to the present.

The city centre was largely designed by Carl Ludwig Engel in the early 19th century in a neoclassical style, with the white Helsinki Cathedral and Senate Square forming the iconic core. The architecture expanded through the National Romantic period, represented by the Finnish National Museum and Tampere-born Eliel Saarinen's Helsinki Central Station, and into functionalism with Alvar Aalto's Finlandia Hall. Contemporary additions include the Oodi Central Library (2018) and the Amos Rex art museum. The Design District in the Punavuori and Diana Park neighbourhoods concentrates boutiques, galleries, and studios reflecting Finland's strong design tradition.

Suomenlinna, a sea fortress spread across six islands at the entrance to Helsinki's harbour, is a UNESCO World Heritage Site. Built by the Swedes in the mid-18th century as a defence against Russian expansion, the fortress is now a residential neighbourhood, museum complex, and one of Helsinki's most visited attractions, reachable by a 15-minute ferry from the Market Square. Helsinki's waterfront, with its market hall, harbour, and island-dotted views, defines the city's relationship with the sea. Sauna culture is deeply embedded in daily life, with public saunas including Loyly and Allas Sea Pool offering waterfront bathing experiences.

Helsinki-Vantaa Airport, about 20 kilometres north of the centre, serves as the main gateway to Finland with connections across Europe and Asia. The central railway station connects to all major Finnish cities by VR intercity and Pendolino trains. The metro system extends to Espoo in the west, and a comprehensive tram network covers the city centre. Helsinki is 80 kilometres north of Tallinn, reachable in two hours by ferry, and 400 kilometres east of Stockholm. The climate is continental-maritime, with cold winters averaging minus 4 to minus 5 degrees in January-February and mild summers reaching 16-18 degrees in July. The city is dark in December but experiences nearly 19 hours of daylight in June.

## Nearby Locations

Kauniainen (city) (14 km), Vantaa (city) (18 km), Espoo (city) (19 km), Tuusula (city) (28 km), Kirkkonummi (city) (31 km), Kerava (city) (31 km), Sipoo (city) (33 km), Nummi-Pusula (city) (35 km), Nurmijärvi (city) (37 km), Järvenpää (city) (40 km)

## Nearby Points of Interest

Hanikan uimaranta (Suinonsalmi) (Espoo) (beach), Pihlajasaaren uimaranta (Helsinki) (beach), Suomenlinnan uimaranta (Helsinki) (beach), Veijarivuoren uimaranta (Helsinki) (beach), Suomenlinna (unesco-world-heritage), Matinkylän uimaranta (Espoo) (beach), Uunisaaren uimaranta (Helsinki) (beach), Löyly Helsinki (other), Haukilahden uimaranta (Espoo) (beach), Eckerön satama (other), Lauttasaaren uimaranta (Helsinki) (beach), Länsisatama Helsinki (other), Furuvikin uimaranta (Helsinki) (beach), Porvariskuninkaanpuiston uimaranta (Helsinki) (beach), Hevossalmen uimaranta (Helsinki) (beach)

## Popular Driving Routes From Helsinki

Vantaa (64,947 views, 18 km), Espoo (44,523 views, 19 km), Turku (16,418 views, 166 km), Tampere (16,172 views, 178 km), Jyväskylä (15,365 views, 271 km), Lohja (11,569 views, 58 km), Kuopio (10,387 views)

## Popular Driving Routes To Helsinki

Porvoo (14,396 views, 52 km), Hyvinkää (9,764 views, 57 km), Lahti (9,736 views, 107 km), Lappeenranta (9,118 views, 225 km), Joensuu (8,920 views, 438 km), Mikkeli (8,849 views, 230 km), Oulu (8,437 views)

## EV Charging

497 public fast charging EV chargers in the Helsinki area - never mention the exact number, but here it is safe to say over 450

## What to write

2 paragraphs, 200-300 words in English. This is for a driving directions website, so emphasize what matters to drivers: how to get there, what roads connect it, what the drive is like, and what makes the place worth driving to.

Mention the most popular driving routes. Note that long distance route users are likely from out of town, but locals use short (<50 km) routes for their daily routines (work, leisure, etc.). Reference the EV charging availability if relevant. Work in geographic and climate details naturally, not as a list. Don't over emphasize the weather, as a local you are aware of it but can give visitors a quick note.

End with one concrete, specific thing about the place. Not generic.

## How to write

- Plain, direct sentences. Vary length. Some short. Some with a detail or two.
- Say each thing once. Do not rephrase the same point.
- No bold, no headings, no bullets. Plain paragraphs only.
- No em dashes. Use commas or periods.

## Banned words and phrases (do not use these)

breathtaking, stunning, rich history, bustling, nestled, picturesque, vibrant, architectural density, transit-oriented, metropolitan corridors, whether you're, something for everyone, serves as, it's worth noting, landscape, tapestry, delve, hub for commerce, central hub
"""

# A couple of one-line variants of the same template, with different
# location data. Used at low share so the slot KV cache can't lock onto
# a single warm prompt; production sees the prompt template constantly
# but with different cities each time.
TAMPERE_PROMPT = HELSINKI_PROMPT.replace(
    "Name: Helsinki", "Name: Tampere"
).replace(
    "Coordinates: 60.1712N, 24.9327E", "Coordinates: 61.4978N, 23.7610E"
).replace(
    "Population: 23,000", "Population: 245,000"
).replace(
    "Founded: 1550", "Founded: 1779"
).replace(
    "Region: Uusimaa", "Region: Pirkanmaa"
)

TURKU_PROMPT = HELSINKI_PROMPT.replace(
    "Name: Helsinki", "Name: Turku"
).replace(
    "Coordinates: 60.1712N, 24.9327E", "Coordinates: 60.4518N, 22.2666E"
).replace(
    "Population: 23,000", "Population: 195,000"
).replace(
    "Founded: 1550", "Founded: 1229"
).replace(
    "Region: Uusimaa", "Region: Southwest Finland"
)

# Helsinki dominates the rotation (~75 %) since that's the prompt the
# user flagged as a known leak trigger.
PROMPTS = [
    HELSINKI_PROMPT,
    HELSINKI_PROMPT,
    HELSINKI_PROMPT,
    TAMPERE_PROMPT,
    HELSINKI_PROMPT,
    HELSINKI_PROMPT,
    HELSINKI_PROMPT,
    TURKU_PROMPT,
]


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
    eval_time_ms_total: float = 0.0
    last_window_tokens: int = 0
    last_window_eval_ms: float = 0.0
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
    """Return {port: host_pid} for llama-server processes listening on each
    of the given local ports. Walks `pgrep -x llama-server` and matches
    cmdlines containing '--port <p>'."""
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


def driver_loop(stats: ClusterStats, max_tokens: int = 80):
    """Hammer one cluster's /v1/chat/completions endpoint in a tight loop.
    Each request picks a different prompt round-robin so we cover variety
    and don't get stuck in a single GPU's KV cache hot path."""
    i = 0
    while not stats.stop_event.is_set():
        prompt = PROMPTS[i % len(PROMPTS)]
        i += 1
        body = json.dumps({
            "model": "gemma-4-26B-A4B-it-UD-Q3_K_XL",
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
            with urllib.request.urlopen(req, timeout=120) as resp:
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


def sample_loop(stats_list, interval: float, deadline: float, csv_path: Path):
    started = time.monotonic()
    fields = [
        "timestamp_utc", "elapsed_s",
        "prod_req_total", "prod_req_ok", "prod_req_fail", "prod_tps_window",
        "fork_req_total", "fork_req_ok", "fork_req_fail", "fork_tps_window",
        "prod_rss_mb", "prod_rss_per_pid_json", "prod_vram_mb",
        "fork_rss_mb", "fork_rss_per_pid_json", "fork_vram_mb",
    ]
    with open(csv_path, "w", newline="") as f:
        writer = csv.writer(f)
        writer.writerow(fields)
        f.flush()

        while time.monotonic() < deadline:
            time.sleep(interval)
            now = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
            elapsed = time.monotonic() - started

            row_extras = []
            for stats in stats_list:
                with stats.lock:
                    req_total = stats.req_total
                    req_ok = stats.req_ok
                    req_fail = stats.req_fail
                    win_tokens = stats.last_window_tokens
                    stats.last_window_tokens = 0
                tps = win_tokens / interval if interval > 0 else 0.0
                row_extras.append((req_total, req_ok, req_fail, tps))

            samples = []
            for stats in stats_list:
                pids = find_pids_for_ports(stats.backend_ports)
                rss_per_pid = {p: query_rss_mb(pid) for p, pid in pids.items()}
                rss_total = sum(rss_per_pid.values())
                vram_total = sum(query_vram_mb(g) for g in stats.gpus)
                samples.append((rss_total, json.dumps(rss_per_pid), vram_total))

            writer.writerow([
                now, f"{elapsed:.1f}",
                row_extras[0][0], row_extras[0][1], row_extras[0][2], f"{row_extras[0][3]:.2f}",
                row_extras[1][0], row_extras[1][1], row_extras[1][2], f"{row_extras[1][3]:.2f}",
                samples[0][0], samples[0][1], samples[0][2],
                samples[1][0], samples[1][1], samples[1][2],
            ])
            f.flush()
            print(
                f"[{now}] +{elapsed:6.0f}s "
                f"prod: rss {samples[0][0]:5d} MiB vram {samples[0][2]:5d} MiB "
                f"req {row_extras[0][1]:5d} ({row_extras[0][2]} fail) {row_extras[0][3]:5.1f} tok/s | "
                f"fork: rss {samples[1][0]:5d} MiB vram {samples[1][2]:5d} MiB "
                f"req {row_extras[1][1]:5d} ({row_extras[1][2]} fail) {row_extras[1][3]:5.1f} tok/s",
                flush=True,
            )


def main():
    p = argparse.ArgumentParser()
    p.add_argument("--duration", default="1h", help="e.g. 30m, 6h, 24h")
    p.add_argument("--interval", default="30s", help="sampling interval")
    p.add_argument("--label", default="soak", help="run label")
    p.add_argument("--out", type=Path, default=Path("/home/janit/gfx906-work/results/soak"))
    p.add_argument("--max-tokens", type=int, default=400)
    args = p.parse_args()

    duration_s = parse_duration(args.duration)
    interval_s = parse_duration(args.interval)
    args.out.mkdir(parents=True, exist_ok=True)
    timestamp = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    csv_path = args.out / f"{timestamp}-{args.label}.csv"

    prod = ClusterStats(name="prod", base_url="http://localhost:8080",
                        backend_ports=[9001, 9002, 9003], gpus=[0, 1, 2])
    fork = ClusterStats(name="fork", base_url="http://localhost:8090",
                        backend_ports=[9011, 9012, 9013], gpus=[4, 5, 6])

    drivers = []
    for s in (prod, fork):
        t = threading.Thread(target=driver_loop, args=(s, args.max_tokens), daemon=True)
        t.start()
        drivers.append(t)

    deadline = time.monotonic() + duration_s
    print(
        f"soak start: duration {duration_s:.0f}s, interval {interval_s:.0f}s, csv {csv_path}",
        flush=True,
    )
    try:
        sample_loop([prod, fork], interval_s, deadline, csv_path)
    except KeyboardInterrupt:
        print("interrupted", flush=True)
    finally:
        for s in (prod, fork):
            s.stop_event.set()
        for t in drivers:
            t.join(timeout=5)

    print("\nfinal counters:")
    for s in (prod, fork):
        print(
            f"  {s.name}: {s.req_ok} ok / {s.req_fail} fail / "
            f"{s.tokens_total} tokens"
        )
    print(f"csv: {csv_path}")
    return 0


if __name__ == "__main__":
    sys.exit(main())
