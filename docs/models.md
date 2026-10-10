# Models and Hardware

What the reference fleet runs, what has been measured on it, and the hardware
rules those measurements imply. Numbers here are measured throughput, not
estimates.

Most of this document is about the gfx906 lane: 10x Radeon VII (16 GB HBM2) on a
mining-rig topology, running `llamacpp`. There, one hard constraint shapes every
recommendation: **a dense model whose weights plus KV cache do not fit in a
single 16 GB card pays a ~3x throughput tax** (validated on Qwen3.5-A3B Q4_K_M vs
Q3_K_M on the same GPU). Above that line, tensor-split across 2+ GPUs avoids the
tax at the cost of single-stream parallelism — see
[configuration.md](configuration.md#tensor-split-mode).

**That rule is a property of the engine and the card, not of viiwork.** The
fleet's `freetoken` lane breaks it deliberately: a sparse MoE far larger than one
card's VRAM runs on a *single* GPU by keeping only the hot experts resident and
streaming the rest from host memory. See
[Large models on one card](#large-models-on-one-card-freetoken-and-moe-offload).
The `strata` engine does the same for one model family on gfx906 itself, with
the layers split across a backend's cards: see
[Strata](#strata-one-moe-family-across-several-cards).

- [What the reference fleet runs today](#what-the-reference-fleet-runs-today)
- [Large models on one card: FreeToken and MoE offload](#large-models-on-one-card-freetoken-and-moe-offload)
- [Strata: one MoE family across several cards](#strata-one-moe-family-across-several-cards)
- [Models from viiwork-parrot](#models-from-viiwork-parrot)
- [Tuning rules measured on gfx906](#tuning-rules-measured-on-gfx906)
- [Validated production deployments](#validated-production-deployments)
- [Bring-ups in progress](#bring-ups-in-progress)
- [Single-GPU picks](#single-gpu-picks-16-gb)
- [Tensor-split picks](#tensor-split-picks-multi-gpu)

## What the reference fleet runs today

Five models across a five-host ROCm mesh, read from `/v1/capacity` on
2026-09-13. The table below this one is the evaluation catalogue — measurements
for models that were benchmarked, whether or not they are deployed — so start
here for what is actually in service.

| Model | Spread | Context per slot | Role |
|---|---|---|---|
| `Qwen3.8-27B` | 2 hosts | 49152 | general coder and prose; behind `stable-coder` and `stable-prose` |
| `granite-4.2-8b` | 2 hosts | 16384 | fast utility model; behind `stable-granite` and `stable-factcheck` |
| `translategemma-27b-it` | 3 hosts, 3 backends each | 4096 | the translation lane; behind `stable-translate` |
| `gemma-4-31B-it` | 3 hosts | 6144 | prose |
| `Ornith-1.5-35B-A3B` | 1 host | 262144 | long context |

Two things this shows that the catalogue does not. **A model is usually served
by more than one host**, which is what lets a node forward a request rather than
queue it — spread is a routing property, not a redundancy nicety. And **context
per slot varies by an order of magnitude between models on the same hardware**,
because it is traded against slot count and VRAM per backend, not fixed by the
checkpoint.

Aliases are mesh-wide and resolve on whichever node a request reaches; see
[mesh.md](mesh.md#aliases). `viiwork alias list` against any node prints the live table.

Those five are the ROCm hosts. The fleet also runs `vllm` on one host and
`freetoken` on two — all three engines in one mesh behind one API, which is what
v2.2.0 is. `strata` is the fourth engine, added later; it has run on one
ten-card Radeon VII host (below).

## Large models on one card: FreeToken and MoE offload

FreeToken's unit of deployment is **one card per process** — it has a
`--tensor-parallel-size` flag but rejects more than one GPU, and viiwork fails
the config rather than letting it die at load with a Python traceback. So it
cannot reach a large model the way llama.cpp does, by spreading layers across
cards.

It reaches them the other way: **the weights do not all have to be resident.**
For a sparse MoE, only the hot experts stay in VRAM and the rest stream from host
memory, so a checkpoint far larger than the card runs on one GPU. The strategy is
the `moe_backend` key:

```yaml
  - name: DeepSeek-V4-Flash
    engine: freetoken
    path: /models/DSV4-Flash-NVFP4
    gpus: [0, 1, 2]
    gpus_per_backend: 1      # three single-card backends
    context: 32768           # -> --max-seq-len-override
    parallel: 4              # -> --max-running-requests
    startup_timeout: 45m
    freetoken:
      memory_ratio: 0.90
      moe_backend: auto      # auto | fused | offload | cpu | hybrid
```

**Proven, not theoretical.** v2.2.0's release criterion was that a real model run
on the hardware it is meant for, and DeepSeek-V4-Flash serving from NVFP4
checkpoints on RTX 5090s is what met it. That lane took part in the 60-minute
fleet soak alongside the gfx906 and vLLM hosts — 104 slots, 7 models, all three
engines, 0.1x to 4x of capacity.

Three things to know before you plan one:

- **Loading is slow, and that is the offload working.** FreeToken loads weights,
  sizes its cache pools and captures CUDA graphs before it serves, and on an
  offload MoE backend it then fills the GPU expert cache from host memory. On the
  models this engine exists to run that is minutes to tens of minutes, which is
  why viiwork's default `startup_timeout` for `freetoken` is **30 minutes** — ten
  times the llama.cpp default. Size it from your observed cold-load time.

- **The advertised context is not the servable context, and viiwork does not
  correct it.** On a live RTX 5090 capture, DeepSeek-V4-Flash advertises
  **1,048,576** tokens while the KV pool the engine actually built holds
  **64,128** — a factor of 16. FreeToken sizes that pool from the VRAM left after
  weights *and* the expert cache, so the ceiling is a property of the card and
  the checkpoint, not of the checkpoint alone. The node publishes the `context:`
  you wrote, so writing the advertised number gets you a mesh advertising it and
  a backend answering `400 context_length_exceeded`. **Set `context:` to what the
  card can actually serve**, and check `/v1/cache/status` on the backend to find
  out what that is.

- **Set `kv_reserve_tokens` on every offload model.** viiwork never generates
  `--moe-cache-auto`, but the engine turns it on itself whenever the strategy is
  offload, cpu or hybrid and no cache-sizing flag was given — which is what
  `moe_backend: auto` resolves to on a MoE model. The expert cache is
  MoE-priority and takes VRAM the KV cache needs, and `0` is not "no reserve":
  the flag is simply not generated and the engine keeps its own floor of 8192
  tokens, so the backend serves 8192 tokens however much context the node
  publishes. Set it to at least `context`, so one slot can always use the
  window the node advertises; `context × parallel` is the ceiling, every slot
  full at full context.

Everything else — dtype, attention backend, MoE cache size, KV capacity, page
size, CUDA-graph sizes — FreeToken resolves from the checkpoint and the card, and
does it better than a config file can. That is why the `freetoken:` block is four
keys; anything else belongs in `models[].args`.

## Strata: one MoE family across several cards

[Strata](https://github.com/Niko1221/Strata) (MIT) serves one model family,
Qwen3.8-Flash-Next (a 125B-parameter sparse MoE) in several sizes and
fine-tunes. Like FreeToken it keeps the experts in host memory and the hot ones
on the card; unlike FreeToken a backend spans **several cards through a layer
split**, and it has a gfx906 build, so it runs on the Radeon VII hosts.

Each backend is a Python server that starts the C++ engine from a JSON config.
viiwork launches and probes the Python server; **`path` is that JSON file, not
the weights**:

```yaml
models:
  - name: qwen3.8-flash-next-coder
    engine: strata
    path: /s/run/strata-coder.json   # Strata's own JSON config
    gpus: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]
    gpus_per_backend: 5              # two backends of five cards
    context: 131072                  # per slot
    parallel: 1
    startup_timeout: 45m
    env: {STRATA_ARENA_MMAP: "1"}    # needs --pcie-frac 0 in the JSON's args
    strata:
      dir: /opt/strata               # the Strata checkout; required
      python: python3                # default
      warmup: 30s                    # default
```

- **One JSON serves every backend of the model.** The node passes host, port
  and cards as flags; the file's `host`, `port` and `gpu` are ignored. Leave
  `layer_split` out and Strata splits the layers itself; the measurements below
  used explicit split points.
- **The file must agree with the entry**, or the backend refuses to start and
  names the key: `model_name` (or an alias) with `name`, `--max-context` with
  `context`, and `lazy_load`, `idle_unload_s` and `api_key` off. See
  [configuration.md](configuration.md#models).
- **`parallel` is 1 to 8, and the JSON's `"parallel"` must say the same.**
  Each slot is a conversation of the full `context`, and every slot takes VRAM
  that the expert cache would otherwise hold, so a second slot can cost more
  speed than it gains; upstream's `docs/BATCHING.md` has the numbers. The
  engine runs fewer slots than asked when they do not fit and says so only in
  its log; the node publishes the count `/slots` lists, so look at
  `/v1/status` after a load. A request that is alone decodes on the engine's
  faster single path, and a second one moves both into batch slots. Needs
  Strata v0.1.41: an older build lists one slot whatever the file says.
- **Preparing the model is upstream's job.** The pack, the GGUF shards and the
  MTP layer come from Strata's own `setup.py`; viiwork does not fetch or build
  them. Images and their traps are in [BUILDS.md](../BUILDS.md).
- **Host RAM is part of the budget.** With `STRATA_ARENA_MMAP=1` the backends
  of one model share one 24 GB mapped expert file; two backends loaded used
  36 GB. Without it one instance held about 27 GB resident.
- **`--trim-stage-weights` is what fits the Coder on two cards.** Without it
  90% of the experts were resident and decode fell from 56 to 21 tok/s.
- **With three cards per backend, leave `--kv-resident` out.** The whole KV
  cache of a 262,144-token context is about 1 GiB at `--kv int8`, and three
  Radeon VIIs have the room. On gb3 (2026-10-05, three backends of three
  cards, three coding sessions for 25 minutes) total output rose from 37 to 60
  tok/s and decode at 40 to 80 thousand tokens of context from 31 to 57 tok/s;
  a 250,000-token cold prompt was read at 676 tok/s and left about 850 MiB
  free on the fullest card. `--prefill auto` was slower than `--prefill 4096`
  there: it borrowed expert-cache slots for its larger chunks and prompt
  reading fell to between a quarter and a half.
- **A backend waits 30 seconds after loading** (`strata: {warmup: 30s}`, phase
  `warming up`). The wait was added for `saving a checkpoint part failed` on
  Radeon VIIs and does not prevent it; `warmup: 0s` turns it off.
- **`saving a checkpoint part failed` on Radeon VIIs is fixed in the
  `viiwork-strata` image from Strata v0.1.40.1** (the gfx906 patch,
  [BUILDS.md](../BUILDS.md); upstream has the fix itself since v0.1.42). It was the first long prompt after a load, on a
  backend of more than one card: the engine saves a checkpoint every 16,384
  prompt tokens while another thread is still capturing its prompt graphs,
  and the ROCm runtime refuses that copy. An image built before that still
  fails that way, and the backend reloads.

### Measured on Radeon VII

The Coder (IQ1_M quant, 54 GB), 131,072 context, one slot per backend, on a
ten-card Radeon VII host with a 4-core EPYC 3151. Upstream's `benchmark.py`,
fresh prompts, 256-token outputs, Strata v0.1.39.

| Cards per backend | Prompt read tok/s (4K / 32K / 128K) | Decode tok/s (4K / 32K / 128K) | Time to first token, s |
|---|---|---|---|
| 2 | 124 / 265 / 254 | 48 / 42 / 37 | 33 / 124 / 505 |
| 4 | 282 / 747 / not run | 44 / 37 / not run | 15 / 44 / not run |
| 8 | 192 / 750 / 896 | 36 / 32 / 30 | 21 / 44 / 143 |
| 5, with a second five-card backend loaded | 212 / ~710 / 640–694 | 34 / 28–30 / 33 | 19 / 46 / 185–200 |

The two-card row is three runs per length, the four-card row two, the others
one. More cards read a long prompt faster and decode slower. **A cold 128K
prompt takes minutes to the first token** on any of these layouts, so size
`routing.queue_timeout` and client timeouts for it.

Under viiwork, on the same host as two backends of five cards:

- Both backends were healthy about 10 minutes after start; loads are serial,
  3.5 minutes each, about 6 on the first start that writes the mapped expert
  file.
- Pinning holds: each backend's engine processes sat on its own five cards.
- `/health` and `/slots` answered in 1 to 2 ms throughout a 41,409-token cold
  prompt, so a long prefill does not trip the health ladder.
- A repeated prompt reports `cached_tokens` in `usage`.
- Thinking: a stream carries the reasoning in `reasoning_content`, apart from
  the answer; see [thinking-models.md](thinking-models.md#strata).
- The CUDA image served under viiwork on three RTX A4000 (2026-10-04) at the
  model's full 262,144-token context: a 258,663-token cold prompt answered
  correctly in 88.5 s (about 2,900 tok/s prompt reading, about 60 tok/s
  decode), `/v1/status` answering within 15 ms throughout, and a prompt over
  the limit refused with a 400.

Known gaps, as of 2026-10-05:

- **Untested:** `layer_split` left to auto.
- **A stream cut short reads as complete.** Strata's server ends a streamed
  reply by closing the connection, so when a backend dies mid-reply the node
  cannot tell the early close from the end: the client's stream ends cleanly
  without a `[DONE]` line. A client that needs to know checks for `[DONE]`.

## Models from viiwork-parrot

A model can name a viiwork-parrot catalog id instead of a file:

```yaml
models:
  - name: Qwen3.8-27B
    engine: llamacpp
    source: viiwork-parrot:qwen3.8-27b-q4kxl
    gpus: [0, 1]
    context: 49152
```

At start the node asks the host's viiwork-parrot (`POST /ensure` on
`viiwork_parrot.api`, default `127.0.0.1:7950`) for the model and runs the
engine on the path it returns. viiwork-parrot downloads it if needed, checks
every sha256 and seeds it; viiwork keeps no copy of its own.

- Set `path` or `source`, never both.
- While viiwork-parrot fetches, the model's backends are `starting` with phase
  `fetching`, and the node logs progress every 10%. A download does not count
  against `startup_timeout`, and it does not hold up any other model's load.
- If viiwork-parrot is not running yet, the node waits for it (retrying up to
  once a minute) — start order does not matter.
- If viiwork-parrot refuses (unknown id, not available on this host, a failed
  verification, no disk space), the model's backends are dead with its message.
  Fix the cause and restart the node or change the model and send SIGHUP.
- **Paths must match.** viiwork-parrot answers with host paths. In a container,
  mount its data directory at the same path (for example
  `/srv/viiwork-parrot:/srv/viiwork-parrot:ro`); the node checks the path
  exists and says so if it does not.
- A folder model (a safetensors directory) needs an engine that takes a
  directory — vllm or freetoken. llamacpp refuses a directory.
- `viiwork-accept config` checks a sourced model read-only through
  viiwork-parrot's `GET /status`.

## Tuning rules measured on gfx906

Each of these cost a bring-up to learn. They are specific to gfx906 where
they say so; the reasoning generalises further than the numbers do.

> **Build note.** Hybrid-attention models (Qwen3.5-A3B, Qwen3.6/3.8, Laguna, anything using DeltaNet / linear attention) need an upstream-current `llama.cpp` — build a fresh image from `docker/Dockerfile.rocm`.
>
> Same-week architectures can need more than "current master" — they can need an *unmerged* one. Check `general.architecture` in the GGUF before planning a bring-up: if `llama-server` answers `unknown model architecture: 'X'`, no flag will fix it and the only path is a build from whichever PR adds `X` (Qwen3.8-Flash-Next needed PR #27742 from `unslothai/llama.cpp`; master rejected it outright). Pin the PR head in a dedicated `docker/test/Dockerfile.<model>-test` rather than tracking `master`, and re-pin to a release tag once it merges.

> **Quant choice on gfx906: take the highest quant that fits.** Measured on
> Qwen3.8-27B, Q6_K costs **0 tok/s** against Q4 despite reading 28% more bytes
> per token. This hardware is kernel-bound, not bandwidth-bound, and higher
> quants trade bytes for dequant ALU work — the two cancel. The usual
> "drop a quant level for speed" instinct is wrong here; drop one only to fit
> VRAM or buy context. One exception: archs whose tensor columns are not
> divisible by 256 (e.g. `nemotron_h_moe`) silently fall back to non-K types,
> where Q6_K becomes q8_0 and is pure loss — check before assuming.

> **Split mode must stay `layer`.** Measured 2026-08-15: `--split-mode row` is
> refused outright by the ROCm backend ("does not support split buffers"), and
> `--split-mode tensor` loads but runs ~9× slower on prefill while using *more*
> VRAM. Layer split runs a group's cards strictly sequentially — one card
> computes at a time — so **extra GPUs in a group buy VRAM and context, never
> throughput**. For throughput, run more backends, not wider ones.

> **Size `startup_timeout` from your cold-load time.** `llama-server` answers
> `/health` with 503 *"Loading model"* for the whole time it is reading tensors.
> A starting backend waits for that patiently, but only for the model's
> `startup_timeout` (10 minutes by default for llama.cpp), after which the node
> gives up and respawns it. Measured 2026-08-27: a 104 GB model on USB loaded at
> ~30 MB/s (~60 min), and a restart minutes from ready discarded 78 GB already
> placed in VRAM. Rule of thumb: `model_bytes / observed_read_bytes_per_sec`,
> then double it.

> **A single tensor larger than one card is a hard wall, not a tuning problem.**
> Layer split assigns whole *layers* to cards and `-ot` assigns a whole *tensor*
> to one device — neither can spread one oversized tensor, and `--split-mode row`
> (which would) is refused by this ROCm backend. So any model carrying a
> monolithic tensor above **16.37 GB** must keep it in host RAM here, no matter
> how many GPUs you own. When that tensor is on the per-token path the model
> becomes single-core CPU-bound and the GPUs idle. Qwen3.8-Flash-Next is the
> worked example below: a 26.82 GB n-gram embedding, unchanged across every
> published quant. Check the largest tensor before assuming VRAM total is what
> matters — total capacity is necessary, not sufficient.

> **Gemma 4 quant: prefer QAT.** Gemma 4 ships [quantization-aware-trained Q4 checkpoints](https://blog.google/innovation-and-ai/technology/developers-tools/quantization-aware-training-gemma-4/) — int4 weights at near-bf16 quality and ~3× less memory than fp16. `scripts/download-gemma4-31b.sh` defaults to these. Two gotchas, both verified on gfx906: (1) Google's own day-one GGUFs are broken (garbage detokenization / leaked special tokens) — use Unsloth's clean requants (`unsloth/gemma-4-*-it-qat-GGUF`) and a current `llama.cpp` (`viiwork:latest`, b10437+); (2) Gemma 4 is a *thinking* model — for prose/direct output, disable thinking server-side with `args: ["--jinja", "--chat-template-kwargs", "{\"enable_thinking\": false}"]` (`--reasoning-budget 0` does not take on this template).

## Validated production deployments

These layouts have stress-test data behind them. The numbers were measured
under viiwork 1.x, one instance per model; the snippets below are the same
layouts as `models:` entries for the machine's one `viiwork.yaml`
([migrating-to-v2.md](migrating-to-v2.md) has the rules they follow — above
all, `context` is **per slot**).

**General all-rounder pick: `gpt-oss-120b` on 2× TS=5.** If you have 10 GPUs and want a single deploy that's both fast (≥40 tok/s single-stream) and high quality across coding, prose, translation, and reasoning, run the entry below. The 117B / 5.1B-active MoE is large enough to be smart and sparse enough to be quick on this hardware. Set `Reasoning: low` for snappy chat, `high` for harder problems.

```yaml
models:
  - name: gpt-oss-120b
    engine: llamacpp
    path: /models/openai_gpt-oss-120b-MXFP4_MOE-00001-of-00002.gguf
    gpus: [0, 1, 2, 3, 4, 5, 6, 7, 8, 9]
    gpus_per_backend: 5      # two backends, each layer-split across five cards
    context: 16384           # per slot
    parallel: 1
```

| Model | Quant | Mode | Measured |
|---|---|---|---|
| **gpt-oss-120b (MoE, 5.1B active)** — *all-rounder* | MXFP4_MOE (native) | 2× TS=5 (10 GPUs) | **41 tok/s** single-stream, **73 tok/s** aggregate at conc=4 (5-min sustained, 120/120 success). Per-request decode held flat under load (40.9 → 40.3 tok/s). Latency p50/p95: 4.9 / 6.7 s single, 10.2 / 12.2 s at conc=4. Reasoning-enabled (harmony format) — set `Reasoning: low/medium/high`. |
| Gemma-4-26B-A4B-IT (MoE, 4B active) | UD-Q3_K_XL + KV-q4 | replica × 5 | **142 tok/s** aggregate at conc=10 (5.5h KV bench, 0 fail). KV-q4 vs fp16 is +9.2% throughput, -2 GB VRAM, 7/7 functional eval matches baseline. Highest aggregate throughput on this hardware. *Quant note: the QAT Q4 checkpoint (`unsloth/gemma-4-26B-A4B-it-qat-GGUF`, UD-Q4_K_XL, ~14.2 GB) is the quality-first choice but is tight for replica×5 on 16 GB — Q3_K_XL remains the measured throughput config until QAT is benched on this fleet.* |
| Qwen3.6-27B (dense hybrid) | Q4_K_M | 5× pair tensor-split (`gpus_per_backend: 2`) | **76 tok/s** aggregate at conc=10 across all 10 GPUs (15-min stress, 0 fail). Single-pair single-stream: 16.9 tok/s. |
| Qwen3.8-27B (dense hybrid) — *supersedes 3.6* | Q6_K | TS=2 pair | ~15 tok/s single-stream, **the same as 3.6 at Q4** — this is a quality and VRAM upgrade, not a speed one. 1.4 GB lighter than 3.6 and ships MTP weights embedded. Context ceiling is **98304, not 131072**: MTP allocates a *second* KV cache that also scales with context, and 131072 OOMs at `common_speculative_init_result`. Prefill 176 tok/s at `-ub 512`. |
| Qwen3.5-35B-A3B (MoE hybrid, 3B active) | Q3_K_M + KV-q4 | replica per GPU | **40.7 tok/s** sustained at conc=9 (15-min stress, 0 fail). 2.8× faster than Q4_K_M because weights fit fully in VRAM. |
| Gemma-4-31B-IT (33B dense) | QAT UD-Q4_K_XL | TS=2 single backend | ~17.3 GB across 2 GPUs (down from ~21.5 GB at the old Q5_K_S, same prose quality); used as the prose generator in the localization pipeline. Run with `--jinja --chat-template-kwargs '{"enable_thinking": false}'` for direct output; the entry is below the table. |
| EuroLLM-22B-Instruct-2512 | Q5_K_M | TS=2 single backend | ~16 GB across 2 GPUs; purpose-trained on 24 EU languages + Norwegian / Icelandic / Russian — the translator step in the localization pipeline. |
| Laguna-XS-2.1 (MoE, 33B / ~2.8B active) — *evaluated, since displaced* | Q4_K_M | 5× TS=2 (10 GPUs) | 36.3 tok/s single-stream per backend; **113 tok/s aggregate** at conc=5. That is 3.1× single-stream, not 5× — the reference host has 4 CPU cores for 5 backends and viiwork warns about the oversubscription at startup. VRAM 14.6/13.2 GB per pair at 128K. TS=2 is mandatory: 20.3 GB does not fit one 16 GB card. |
| Laguna-S-2.1 (MoE, 118B / 8.1B active) — *evaluated, not retained* | unsloth UD-Q6_K (97.9 GB) | TS=10 (whole host) | 20.8 tok/s decode, 176 tok/s prefill. Beat its own projection on decode but **prefill is the weak side**: ~3.4 TFLOPS effective, 2.9× less FLOP-efficient per token than a dense 27B, because top-10-of-256 routing at `-ub 512` leaves each expert ~20 tokens of work. A cold 256K fill costs ~72 min, so the advertised context is real in VRAM and unaffordable in wall-clock. Replaced by the XS fleet above after use. |
| Granite-4.2-8B | Q4_K_M | single-GPU replica × N | ~5 GB weights, generous KV headroom for 16k context. Run with `-fa on`. IBM's enterprise/utility model — strong instruction following, function/tool calling, RAG / structured-output workflows, multilingual; well-suited to back-office automation, doc Q&A, and embedding into agentic loops where you want a small, predictable, English-leaning helper next to a heavier reasoning model on the mesh. |

Gemma-4-31B-IT as the pipeline's prose generator, with thinking off:

```yaml
models:
  - name: gemma-4-31b-it
    engine: llamacpp
    path: /models/gemma-4-31B-it-qat-UD-Q4_K_XL.gguf
    gpus: [0, 1]
    gpus_per_backend: 2      # one backend across both cards
    context: 16384           # per slot
    parallel: 1
    args: ["--jinja", "--chat-template-kwargs", '{"enable_thinking": false}']
```

## Bring-ups in progress

Not production rows yet — recorded so the next attempt does not rediscover the
same walls.

| Model | State | What is known |
|---|---|---|
| Soofi-S-30B-A3B (hybrid Mamba-2 / MoE) | **Blocked** on HuggingFace manual approval | A TS=2 layout on two cards was drafted but never checked in. The GGUF declares `general.architecture = nemotron_h_moe`, **not** "soofi" — it reuses an existing arch, so no llama.cpp bump is needed; do not grep binaries for "soofi". Quant choice inverts the rule above: columns (2688/1856/3712) are not divisible by 256, so every K-quant falls back — Q6_K becomes q8_0 (~32 GB, no quality gain) and Q5_K_M becomes q5_1 (~25 GB, the pick). No community requant exists to route around the gate, and self-converting is blocked because the base repo is gated too. |
| Qwen3.8-Flash-Next (125B total / 6B active, GDN + QSA hybrid) | **Runs, but loses to the 27B** — not retained | Loads and serves correctly across all 10 GPUs, and is *slower than Qwen3.8-27B on two*: 8.7 / 16.0 / 20.4 tok/s at conc 1 / 2 / 4 against the 27B's 10.2 / 18.6 / 27.0. The cause is structural, not tuning. `general.architecture = qwen4exp`, which upstream master rejects outright (`unknown model architecture`); support is only in the still-open PR #27742 from `unslothai/llama.cpp` (branch `qwen4exp/qwen3.8-flash-next`). The blocker is `per_layer_token_embd.weight`: **26.82 GB as one indivisible IQ4_NL tensor** (51.2B elements — the n-gram table). A Radeon VII holds 16.37 GB and `-ot` assigns a whole tensor to one device, so it can never be GPU-resident here and stays in host RAM. Decode is then pinned to a **single CPU core** (measured 0.91 of 4 cores busy with GPUs at 0%), which is the real ceiling: 48.5/160 GB VRAM is in use while 30.9 GB sits in RSS. Needs cards ≥27 GB to be worth revisiting. Re-tested at UD-Q4_K_XL (103.7 GB) to rule out the quant: decode was **unchanged at 12.6 tok/s**, confirming the ceiling is CPU, not quantization — on gfx906 the higher quant rides free. Quality at Q4_K_XL was *better* than the 27B on Finnish (correct terminology throughout vs four terminology errors and a case error) and equal on strict-JSON extraction, and it was more token-efficient (5 of 8 eval prompts completed in budget vs the 27B's 2 of 8). But **neither model solves hard reasoning through this stack**: on one bridge-crossing problem the 27B burned 6,000 tokens / 6.4 min and Flash-Next 10,000 tokens / 17.1 min, both still mid-deliberation, and Flash-Next's decode degraded 12.6 -> 9.7 tok/s as context grew (QSA attention cost). Both emit raw chain-of-thought into `content` with no `<think>` delimiters, which looks like a template/integration gap rather than a reasoning limit — worth retrying under vLLM/SGLang before concluding anything about the models. Verdict: not retained; the 27B gives comparable quality at 1.6x the speed on 2 GPUs instead of 10. |
| Muse-Glimmer-30B (meta-models) | Ran on GPUs 4+7, since displaced | Needs llama.cpp **b10369+** (`muse_glimmer` landed in PR #26841); the older b9222 pin could not load it. kquant-dynamic is 19.65 GB so TS=2 is required, not preferred. Output needs `reasoning_strength: low` — at the template default the model self-talks and that text leaks into `content`. `--mmproj` and the DFlash drafter are deliberately not wired in (upstream #26873, #26894). |

## Single-GPU picks (≤16 GB)

For lightweight / multi-replica setups. Q3_K_M is the practical ceiling on a Radeon VII for the 30B class — anything heavier triggers the VRAM-fit tax.

| Model | Quant | Approx VRAM | Notes |
|---|---|---|---|
| Gemma-4-26B-A4B-IT | QAT UD-Q4_K_XL | ~14.2 GB | Best general-purpose pick on 16 GB; QAT Q4 = near-bf16 quality. Tight on a single card — run with KV-q4 + short context (`-fa on --cache-type-k q4_0 --cache-type-v q4_0`). For replica×N throughput or more KV headroom, drop to non-QAT UD-Q3_K_XL (~12.5 GB). |
| Gemma-4-E4B-IT | QAT UD-Q4_K_XL | ~4.2 GB | 8B multimodal; QAT Q4 = near-bf16 quality at half the VRAM of the old Q8_0 (~8.2 GB). |
| Granite-4.2-8B | Q4_K_M | ~5 GB | Strong instruction following, tool calling, RAG / structured-output workflows. Use as a fast utility model alongside a heavier reasoner. |

## Tensor-split picks (multi-GPU)

For models above the single-GPU ceiling. Layer-mode tensor split costs roughly 2-13% per extra GPU on the reference fleet's PCIe-gen1-x1 mining-rig topology (measured on the gfx906 fork 6h stress). On modern PCIe gen3/4/5 the penalty is smaller.

| Model | Quant | Min GPUs | Why tensor-split |
|---|---|---|---|
| Gemma-4-31B-IT | QAT UD-Q4_K_XL | 2 | 33B dense at near-bf16 QAT Q4 (~17.3 GB); higher prose quality than the 26B MoE. |
| EuroLLM-22B | Q5_K_M | 2 | 22B dense translator; doesn't fit comfortably at Q5 on one card. |
| Qwen3.6-27B | Q4_K_M | 2 (per pair, `gpus_per_backend: 2`) | Hybrid dense at single-stream tensor-parallel speed; 5-pair layout gives both per-request latency and aggregate throughput. |
| Laguna-XS-2.1 | Q4_K_M | 2 (per pair, `gpus_per_backend: 2`) | 20.3 GB will not fit a 16 GB card, so TS=2 is the floor rather than a tuning choice. Five pairs is the throughput layout on a 10-GPU node. |
| gpt-oss-120b | MXFP4_MOE | 5 (per group, `gpus_per_backend: 5`) | 117B / 5.1B-active MoE; the all-rounder pick on a 10-GPU node — see the validated row above for measured throughput. |

> Other 30-32B models (Qwen3-32B, DeepSeek-R1-Distill, Qwen2.5-Coder, etc.) load on this hardware but aren't currently part of the reference fleet — add a `models:` entry to a node's `viiwork.yaml` and run `scripts/bench-sustained.sh` to add measured numbers.

