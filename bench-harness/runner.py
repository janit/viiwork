"""Benchmark runner: spawns llama-server, drives requests, captures metrics.

The runner is binary-agnostic — it just needs a path to a `llama-server`
that speaks the standard /completion API. Used both for the in-docker
upstream baseline (the binary at /usr/local/bin/llama-server inside the
viiwork image) and for the host-built fork binary later.

Memory metering: a background sampler thread polls VRAM (rocm-smi on the
pinned GPU) and the llama-server process's RSS at 250 ms intervals during
each /completion request, then records the peak. Both numbers go into the
BenchmarkResult so every commit on the gfx906 branch carries an explicit
memory footprint stamp - this is the "<=5% RSS drift over a 24h soak"
success criterion the parent spec calls out.
"""
import json
import os
import re
import subprocess
import threading
import time
import urllib.error
import urllib.request
from dataclasses import dataclass, field
from pathlib import Path

from workloads import Workload


# Matches lines like:
#   prompt eval time =   123.45 ms /    50 tokens (    2.47 ms per token,   405.00 tokens per second)
#           eval time =  1234.56 ms /   200 runs   (    6.17 ms per run,   162.05 tokens per second)
TIMING_RE = re.compile(
    r"^\s*(prompt eval|eval) time\s*=\s*([\d.]+)\s*ms\s*/\s*(\d+)\s*(tokens|runs).*?([\d.]+)\s*tokens per second"
)


@dataclass
class TimingLine:
    kind: str  # "prompt_eval" or "eval"
    duration_ms: float
    tokens: int
    tokens_per_second: float


@dataclass
class BenchmarkResult:
    tokens_per_second: float
    latency_ms: float
    peak_vram_mb: int
    peak_rss_mb: int = 0
    rss_baseline_mb: int = 0   # RSS just after model load, before request
    rss_growth_mb: int = 0     # peak - baseline; what to watch for leak drift
    first_token_ms: float = 0.0
    raw_timings: list = field(default_factory=list)


def parse_server_timings(line: str):
    m = TIMING_RE.match(line)
    if not m:
        return None
    kind_raw, duration_ms, tokens, _unit, tps = m.groups()
    kind = "prompt_eval" if kind_raw == "prompt eval" else "eval"
    return TimingLine(
        kind=kind,
        duration_ms=float(duration_ms),
        tokens=int(tokens),
        tokens_per_second=float(tps),
    )


def _dev_gpu_index() -> str:
    """Which physical GPU index does the bench treat as 'the dev GPU'?

    rocm-smi (unlike the HIP runtime) does NOT honor ROCR_VISIBLE_DEVICES,
    so we need an explicit -d <index> when querying VRAM. The convention
    for this fork is that the parent shell exports ROCR_VISIBLE_DEVICES
    to a single physical index; we read it back and pass it through.
    Falls back to "3" which is the documented default in env.sh.
    """
    return os.environ.get("ROCR_VISIBLE_DEVICES", "3").split(",")[0].strip() or "3"


def query_vram_mb() -> int:
    """Return absolute VRAM use in MiB on the dev GPU. Returns 0 on error."""
    try:
        out = subprocess.check_output(
            ["/opt/rocm/bin/rocm-smi", "--showmeminfo", "vram",
             "-d", _dev_gpu_index(), "--json"],
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=5,
        )
        data = json.loads(out)
        for card_id, fields in data.items():
            if not card_id.startswith("card"):
                continue
            for k, v in fields.items():
                kl = k.lower()
                if "vram" in kl and "used" in kl and "total" in kl:
                    # "VRAM Total Used Memory (B)" -> bytes
                    try:
                        return int(v) // (1024 * 1024)
                    except (TypeError, ValueError):
                        return 0
    except (subprocess.SubprocessError, json.JSONDecodeError, FileNotFoundError, OSError):
        pass
    return 0


def find_llama_server_pid(port: int = 18080) -> int:
    """Return the host-side PID of the llama-server child process, or 0.

    Works whether llama-server is invoked directly or through `docker run`
    (docker uses host PID namespace by default, so the in-container PID is
    visible from the host).

    The dev box already has multiple long-running llama-server processes
    serving the production viiwork stack on ports 9001..9003, so we need
    to disambiguate: filter by `comm == "llama-server"` (which excludes
    the docker CLI wrapper - whose comm is "docker") and then by the
    presence of `--port <our port>` in the argv (which excludes prod).
    """
    try:
        out = subprocess.check_output(
            ["pgrep", "-x", "llama-server"],
            stderr=subprocess.DEVNULL,
            text=True,
            timeout=2,
        )
    except (subprocess.SubprocessError, FileNotFoundError):
        return 0
    needle = f"--port\x00{port}\x00"
    for line in out.split():
        try:
            pid = int(line.strip())
        except ValueError:
            continue
        if pid <= 0:
            continue
        try:
            with open(f"/proc/{pid}/cmdline", "rb") as fh:
                # /proc/.../cmdline is NUL-separated.
                cmdline = fh.read().decode("utf-8", errors="replace")
        except OSError:
            continue
        if needle in cmdline:
            return pid
    return 0


def query_rss_mb(pid: int) -> int:
    """Return RSS of /proc/<pid> in MiB, or 0 if it can't be read."""
    if pid <= 0:
        return 0
    try:
        with open(f"/proc/{pid}/status") as fh:
            for line in fh:
                if line.startswith("VmRSS:"):
                    parts = line.split()
                    return int(parts[1]) // 1024 if parts[2] == "kB" else int(parts[1]) * 0
    except (OSError, ValueError, IndexError):
        pass
    return 0


class MemorySampler(threading.Thread):
    """Background sampler that records peak VRAM and peak RSS while a request
    is in flight. Run as a daemon thread so it dies if the parent does.
    """

    def __init__(self, interval_s: float = 0.25):
        super().__init__(daemon=True)
        self.interval = interval_s
        self.stop_event = threading.Event()
        self.peak_vram_mb = 0
        self.peak_rss_mb = 0
        self.baseline_rss_mb = 0
        self.samples = 0

    def stop(self):
        self.stop_event.set()

    def run(self):
        # First sample is the baseline (just after model load, before request).
        pid = find_llama_server_pid()
        self.baseline_rss_mb = query_rss_mb(pid)
        self.peak_rss_mb = self.baseline_rss_mb
        self.peak_vram_mb = query_vram_mb()
        self.samples = 1
        while not self.stop_event.is_set():
            # Re-resolve PID each tick: if the server got restarted out from
            # under us we want the new one, not stale data from the old PID.
            pid = find_llama_server_pid() or pid
            v = query_vram_mb()
            r = query_rss_mb(pid)
            if v > self.peak_vram_mb:
                self.peak_vram_mb = v
            if r > self.peak_rss_mb:
                self.peak_rss_mb = r
            self.samples += 1
            self.stop_event.wait(self.interval)


def start_llama_server(binary, model_path: Path, n_ctx: int, port: int = 18080,
                       extra_args=None) -> subprocess.Popen:
    """Start llama-server with the given model. Returns the Popen handle.

    `binary` may be a path string OR a list of argv prefix tokens — passing a
    list lets the caller wrap llama-server in `docker run` / `docker exec`.
    Caller is responsible for terminating the returned process.
    """
    env = os.environ.copy()
    env.setdefault("HSA_OVERRIDE_GFX_VERSION", "9.0.6")

    if isinstance(binary, (list, tuple)):
        cmd = list(binary)
    else:
        cmd = [str(binary)]

    cmd += [
        "-m", str(model_path),
        "-c", str(n_ctx),
        "--host", "127.0.0.1",
        "--port", str(port),
        "--n-gpu-layers", "999",
        # Disable the in-server prompt cache so every run measures cold-prompt
        # eval, never a warm replay of a previous request.
        "--cache-ram", "0",
        # Single slot — we are not measuring batched serving.
        "--parallel", "1",
        "--no-warmup",
    ]
    if extra_args:
        cmd += list(extra_args)

    proc = subprocess.Popen(
        cmd,
        env=env,
        stdout=subprocess.PIPE,
        stderr=subprocess.STDOUT,
        text=True,
        bufsize=1,
    )
    deadline = time.monotonic() + 300  # large models on cold start can be slow
    while time.monotonic() < deadline:
        if proc.poll() is not None:
            raise RuntimeError(
                f"llama-server exited early with code {proc.returncode}. "
                f"cmd={' '.join(cmd)}"
            )
        try:
            with urllib.request.urlopen(f"http://127.0.0.1:{port}/health", timeout=2) as r:
                if r.status == 200:
                    return proc
        except (OSError, urllib.error.URLError):
            time.sleep(1)
    proc.terminate()
    raise TimeoutError("llama-server did not become ready within 300s")


def stop_llama_server(proc: subprocess.Popen) -> str:
    """Terminate the server and return its captured stderr/stdout."""
    proc.terminate()
    try:
        out, _ = proc.communicate(timeout=15)
    except subprocess.TimeoutExpired:
        proc.kill()
        out, _ = proc.communicate()
    return out or ""


def run_one(binary, workload: Workload, port: int = 18080, extra_args=None) -> BenchmarkResult:
    """Run a single workload one time, return measured metrics."""
    proc = start_llama_server(binary, workload.model_path, workload.n_ctx, port, extra_args)
    sampler = MemorySampler(interval_s=0.25)
    sampler.start()
    try:
        body = json.dumps({
            "prompt": workload.prompt,
            "n_predict": workload.n_predict,
            "stream": False,
            "cache_prompt": False,
            # Force the model to actually generate n_predict tokens. Without
            # this, Gemma-3 emits EOS after a few tokens and the eval-line
            # tps math degenerates to a meaningless ratio (e.g. 1e6 tok/s
            # for "1 token in ~0 ms"). We want a steady-state generation
            # measurement, not the time-to-first-eos.
            "ignore_eos": True,
            "temperature": 0.0,
            "top_k": 1,
        }).encode("utf-8")
        req = urllib.request.Request(
            f"http://127.0.0.1:{port}/completion",
            data=body,
            headers={"Content-Type": "application/json"},
            method="POST",
        )
        t0 = time.monotonic()
        with urllib.request.urlopen(req, timeout=900) as resp:
            json.loads(resp.read().decode("utf-8"))
        latency_ms = (time.monotonic() - t0) * 1000.0
    finally:
        sampler.stop()
        sampler.join(timeout=2.0)
        stderr = stop_llama_server(proc)

    peak_vram = sampler.peak_vram_mb
    peak_rss = sampler.peak_rss_mb
    baseline_rss = sampler.baseline_rss_mb
    rss_growth = max(0, peak_rss - baseline_rss)

    timings = []
    for line in stderr.splitlines():
        t = parse_server_timings(line)
        if t:
            timings.append(t)

    # Use total eval-tokens / total eval-time across whatever eval blocks
    # the server printed. With ignore_eos this is one block of n_predict
    # tokens. Recompute from raw rather than trusting the printed tps line,
    # so that single-token eval blocks don't produce nonsensical infinities.
    eval_timings = [t for t in timings if t.kind == "eval"]
    total_eval_tokens = sum(t.tokens for t in eval_timings)
    total_eval_ms = sum(t.duration_ms for t in eval_timings)
    if total_eval_tokens >= 8 and total_eval_ms > 0:
        eval_tps = total_eval_tokens * 1000.0 / total_eval_ms
    elif eval_timings:
        # Too few tokens to trust — record the printed value but flag it
        # by leaving the result low-confidence. Caller can spot it via
        # latency_ms vs tokens_per_second sanity check.
        eval_tps = eval_timings[-1].tokens_per_second
    else:
        eval_tps = 0.0

    return BenchmarkResult(
        tokens_per_second=eval_tps,
        latency_ms=latency_ms,
        peak_vram_mb=peak_vram,
        peak_rss_mb=peak_rss,
        rss_baseline_mb=baseline_rss,
        rss_growth_mb=rss_growth,
        raw_timings=timings,
    )


def run_workload(binary, workload: Workload, extra_args=None) -> list:
    """Run a workload n_runs times. Returns all results (caller aggregates)."""
    return [run_one(binary, workload, extra_args=extra_args) for _ in range(workload.n_runs)]
