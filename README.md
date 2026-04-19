# viiwork

LLM inference load balancer for AMD Radeon VII GPUs. Runs multiple llama-server instances and exposes a single OpenAI-compatible API with adaptive load balancing. Multiple nodes can form a mesh cluster where any node is an entry point and requests route by model.

![viiwork dashboard](viiwork-260405.webp)

## Background

I had 50 Radeon VII cards sitting in servers in my mother-in-law's garage (who doesn't?) and wanted to do something useful with them. viiwork was born out of that — a way to turn a pile of aging-but-capable GPUs into a practical LLM inference cluster.

The Radeon VII, Instinct MI50/MI60 are all gfx906 cards with 16GB HBM2 (32GB for MI60) and a 1 TB/s memory bus — legacy hardware that punches well above its weight for LLM inference where memory bandwidth is the bottleneck. These cards are cheap secondhand and still very capable.

viiwork is designed to be useful at any scale: a single old gaming GPU on your desktop, a few Radeon Pro VII cards in a workstation, or racks of Instinct MI50s in your mother-in-law's garage. Use it standalone as an OpenAI-compatible API, or connect it to any MCP-compatible AI assistant via the built-in MCP server.

## Quick Start

```bash
# 1. Interactive setup (recommended) — detects GPUs, picks models, downloads, generates configs
./scripts/setup-node.sh

# 2. Build and run
docker compose up -d

# 3. Test
curl http://localhost:8080/v1/models
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model-name","messages":[{"role":"user","content":"Hello"}]}'
```

Or manual setup:

```bash
cp viiwork.yaml.example viiwork.yaml
# Edit viiwork.yaml: set model path, GPU count, etc.
mkdir -p models
huggingface-cli download unsloth/gemma-4-26B-A4B-it-GGUF \
  gemma-4-26B-A4B-it-UD-Q3_K_M.gguf --local-dir models
docker compose up -d
```

## Multi-Model Setup

Run multiple models on one host using `./scripts/setup-node.sh`. It detects GPUs, lets you assign models to GPU groups, downloads models, and generates configs with mesh peering between instances. Supports both **replica mode** (one backend per GPU, N-way concurrency) and **tensor-split mode** (one backend spanning multiple GPUs for models too large for a single card).

Example: 10 GPUs split across 3 models:
- 4 GPUs on port 8080: Gemma-4-26B-A4B-IT (replica mode, 4-way concurrency)
- 4 GPUs on port 8081: Qwen3-32B (replica mode, aggressive quant to fit 16GB)
- 2 GPUs on port 8082: Gemma-4-31B-IT (tensor-split, full quality Q4_K_M across 2 GPUs)

All models visible from any port via mesh routing.

### "I'm Feeling Lucky" Mode

The setup script can auto-discover trending models that fit your hardware:

```bash
./scripts/setup-node.sh
# At the model prompt, enter:
#   0   — any category (surprise me)
#   0c  — coding models
#   0r  — reasoning models
#   0v  — vision/multimodal
#   0w  — writing/chat
#   0l  — multilingual
#   0a  — agentic models
```

Uses [llmfit](https://www.llmfit.org/) for hardware-aware scoring when installed, with HuggingFace API as fallback. Auto-picks a diverse assortment and assigns GPUs.

## Tensor-Split Mode

For models that don't fit in a single GPU's VRAM, tensor-split mode runs one llama-server process spanning multiple GPUs. The model's layers are distributed across GPUs, with cross-GPU traffic at layer boundaries.

```yaml
gpus:
  devices: [0, 1]
  base_port: 9001
  tensor_split:
    enabled: true
    mode: layer    # "layer" recommended; "row" is broken on the gfx906 fork
model:
  parallel: 1      # forced to 1 in tensor-split mode
```

Trade-offs vs replica mode:

| | Replica mode | Tensor-split mode |
|---|---|---|
| Concurrency | N backends = N-way parallel | 1 backend = serial requests |
| Model size cap | Must fit in 1 GPU | Can span N GPUs |
| Throughput | Higher (parallel) | Lower (serial) |
| Use case | Models ≤13GB on 16GB cards | Models >13GB that need 2+ cards |

On the gfx906 mining-rig topology (PCIe gen1 x1 risers), measured tensor-split penalty is -2 to -13% for 2-GPU and -7 to -20% for 4-GPU splits. On PCIe gen3/4/5 the penalty is smaller.

The setup script offers tensor-split models (17-20) and custom tensor-split (91) for any model. See `configs/viiwork.tensor-split.yaml.example` for all options.

## Configuration

Copy `viiwork.yaml.example` to `viiwork.yaml` and edit. Override any setting via CLI:

```bash
./viiwork --config viiwork.yaml --gpus.count 4 --model.path /models/other.gguf
```

See `viiwork.yaml.example` for all options.

## Mesh Mode

Multiple viiwork nodes form a cluster. Any node is an entry point, `/v1/models` shows all models across nodes, and requests route transparently to the correct node.

```yaml
peers:
  hosts:
    - 192.168.1.10:8080
    - 192.168.1.11:8080
  poll_interval: 10s
  timeout: 3s
```

Peers that go down are skipped and automatically re-added when they recover. Without the `peers` section, viiwork runs standalone.

## GPU Power Limits

Optionally limit power draw per Radeon VII card:

```yaml
gpus:
  count: 10
  power_limit_watts: 180  # applied via rocm-smi at startup
```

## Cost Tracking

Track real-time electricity cost per node using Nord Pool spot prices.

1. Get an API key from [ENTSO-E Transparency Platform](https://transparency.entsoe.eu/)
2. Create a `.env` file: `ENTSOE_API_KEY=your-key-here`
3. Add a `cost` section to `viiwork.yaml` (see example config)

The dashboard shows per-node cost rate (EUR/h), daily accumulated cost, and cluster totals.

## Pipelines

Pipelines chain multiple LLM steps into virtual models. A consumer calls a virtual model name (e.g. `localize-fi` or `improve-en`) and viiwork executes a sequence of prompts across one or more real backend models.

Two pipeline types are included:

- **Localization** — translate, culturally adapt, and QC text in a single request. Supports locale aliases and per-locale glossaries.
- **Text improvement** — generate text then rewrite it to remove AI writing patterns (de-slop).

Each step specifies a model, a Go template prompt, and temperature. Steps execute sequentially, with each step's output feeding the next. Configure pipelines in `viiwork.yaml` — see the example config for both pipeline types.

## Dashboard

Available at `http://localhost:8080/`. Shows:
- Local backends table with per-GPU status, in-flight count, context usage, and RSS memory
- Live in-flight request timers with token progress, context, and RAM usage
- Activity log (newest first) with model name, completion time, and token counts
- Host memory graph
- Live GPU utilization and VRAM graphs (1 hour history, SSE updates)
- Peer mesh connectivity
- Power consumption and electricity cost

A lightweight chat UI is available at `/chat` for quick model interaction.

## Security

viiwork is designed for trusted local networks and has no built-in authentication. All API endpoints are open to any client that can reach the server. If you expose viiwork to an untrusted network, use a reverse proxy (Caddy, nginx) or firewall rules to restrict access.

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Status dashboard |
| `/chat` | GET | Lightweight chat UI |
| `/health` | GET | System health (JSON) |
| `/v1/models` | GET | List all models (local + mesh peers) |
| `/v1/chat/completions` | POST | Chat completion (routes by model) |
| `/v1/completions` | POST | Text completion (routes by model) |
| `/v1/embeddings` | POST | Embeddings (routes by model) |
| `/v1/status` | GET | Node state (JSON) |
| `/v1/cluster` | GET | Cluster state with all peers (JSON) |
| `/v1/metrics` | GET | GPU metrics history (JSON) |
| `/v1/metrics/stream` | GET | Live GPU metrics (SSE) |

## Host Requirements

- Linux with `amdgpu` kernel driver loaded (standard on modern kernels)
- Docker with GPU device access (`/dev/kfd`, `/dev/dri`)
- No ROCm installation needed on the host
- `huggingface-cli` for model downloads (`pip install huggingface-hub`)
- Optional: `jq` for "I'm feeling lucky" model discovery
- Optional: [llmfit](https://www.llmfit.org/) for hardware-aware model recommendations

## Recommended Models

**Single-GPU models** fit in 16GB VRAM (Radeon VII) with full GPU offload. The safe VRAM ceiling is ~13GB after accounting for KV cache and ROCm runtime overhead. **Large models** use tensor-split mode across 2+ GPUs — higher quality quants at the cost of serial-only inference.

### Coding

| Model | Quant | VRAM | Best For |
|-------|-------|------|----------|
| Qwen2.5-Coder-14B | Q6_K | ~12.1GB | Best quality coding model for 16GB |
| Devstral-Small-24B | Q3_K_M | ~11.5GB | Multi-file frontend tasks, agent workflows |
| DeepSeek-R1-Distill-Qwen-14B | Q4_K_M | ~9GB | Algorithmic reasoning |
| Qwen2.5-Coder-32B | Q2_K | ~12.3GB | Largest coder, aggressive quant |

### Text Generation & Reasoning

| Model | Quant | VRAM | Best For |
|-------|-------|------|----------|
| Qwen3-32B | UD-Q2_K_XL | ~12.8GB | General reasoning, thinking mode |
| Gemma-3-27B-IT | Q3_K_S | ~12.2GB | Factual summarization, structured-to-prose |
| Mistral-Small-3.1-24B | IQ4_XS | ~12.8GB | Multilingual text generation, instruction following |

### Gemma 4

| Model | Quant | VRAM | Best For |
|-------|-------|------|----------|
| Gemma-4-26B-A4B-IT | UD-Q3_K_M | ~12.5GB | MoE with only 4B active params, best quality that fits |
| Gemma-4-26B-A4B-IT | UD-IQ3_S | ~11.2GB | MoE, extra KV cache headroom |
| Gemma-4-E4B-IT | Q8_0 | ~8.2GB | 8B multimodal, near-lossless quant |
| Gemma-4-E2B-IT | Q8_0 | ~5GB | 5B multimodal, ultra-lightweight |

### Data Science & Analytics

| Model | Quant | VRAM | Best For |
|-------|-------|------|----------|
| DeepSeek-R1-Distill-Qwen-32B | Q2_K | ~12.3GB | Chain-of-thought reasoning, math, complex analysis |
| DeepSeek-R1-Distill-Qwen-14B | Q4_K_M | ~9GB | Reasoning at higher quant quality |

### Large Models (tensor-split, 2+ GPUs)

These models are too large for a single 16GB GPU at reasonable quant levels. Use tensor-split mode to split them across 2 or more GPUs.

| Model | Quant | Size | Min GPUs | Best For |
|-------|-------|------|----------|----------|
| Gemma-4-31B-IT | Q4_K_M | ~18GB | 2 | Full 31B dense model, higher quality than the 26B MoE |
| Qwen3-32B | Q4_K_M | ~19GB | 2 | General reasoning at full quality (vs Q2_K single-GPU) |
| DeepSeek-R1-Distill-Qwen-32B | Q4_K_M | ~19GB | 2 | Reasoning at full quality (vs Q2_K single-GPU) |
| Qwen2.5-Coder-32B | Q4_K_M | ~19GB | 2 | Largest coder at full quality (vs Q2_K single-GPU) |

## Builds

viiwork ships in two parallel builds in this same repo. They share the Go server, balancer, dashboard, and API — they differ only in the llama.cpp binary the server spawns.

| | Stable foundation | Experimental track |
|---|---|---|
| Image | `viiwork:latest` | `viiwork:gfx906` |
| Dockerfile | `Dockerfile` | `Dockerfile.gfx906` |
| Make target | `make docker` (alias `make docker-stable`) | `make docker-gfx906` (alias `make docker-experimental`) |
| llama.cpp | Pinned upstream `ggml-org/llama.cpp` release | Local `llama.cpp-gfx906` fork tree (stripped, gfx906-specialized) |
| Status | Default. Production-stable, runs everywhere. | Bake-in track, opt-in per node. +3.0% sustained tok/s vs upstream and identical memory profile in the 4 h A/B soak (`milestone/gfx906-fork-4h-soak-2026-04-09`). |

`scripts/setup-node.sh` asks which build to use as its very first prompt — option 1 (stable) is the default. To switch a running node between tracks in place without re-running setup, use `scripts/switch-node-build.sh`.

See **[BUILDS.md](BUILDS.md)** for the full comparison, when to use which, image distribution between nodes, rollback procedure, and the specific design rationale for the experimental track.

## Docker Build

Both builds pin llama.cpp to a specific release tag and patch the HIP FP8 header for gfx906 compatibility. To bump the upstream version on the stable build:

```bash
docker compose build --build-arg LLAMA_CPP_VERSION=b8700
```

The experimental build is pinned to a specific commit on the `llama.cpp-gfx906` fork — bump it by updating the fork tree at `$GFX906_FORK` (default `~/gfx906-work/llama.cpp-gfx906`) and re-running `make docker-gfx906`.

The FP8 patch is required because ROCm 6.2+ includes `<hip/hip_fp8.h>` for all architectures, but gfx906 has no FP8 hardware and the header fails to compile.

## Scripts

| Script | Description |
|--------|-------------|
| `scripts/setup-node.sh` | Interactive setup: pick build (stable/experimental), detect GPUs, select models (replica or tensor-split), download, generate configs, optionally run the power/perf benchmark |
| `scripts/switch-node-build.sh` | Flip a running node between the stable foundation and the experimental gfx906 track in place |
| `scripts/power-perf-sweep.sh` | Sweep one GPU through power-cap settings (150/180/210/250W), measure tok/s + watts + temperature, recommend the best `power_limit_watts`. ~15-20 min, power-cap-only, fully reversible |
| `scripts/power-perf-sweep-phase2.sh` | Advanced sweep: voltage curve + memory clock tuning. Riskier than Phase 1 — requires explicit user go-ahead. Has correctness gate (compares outputs against baseline) |
| `scripts/setup-opencode.sh` | Configure OpenCode client with auto-detected models |
| `scripts/update.sh` | Pull latest, rebuild Docker image, restart |
| `scripts/rebuild.sh` | Full clean rebuild: stop, remove images, rebuild, start |
| `scripts/bench.sh` | Stress benchmark: ramp concurrency from 1 to N, measure throughput and latency |
| `scripts/bench-sustained.sh` | Sustained load benchmark: hold N concurrent requests for a duration |

## MCP Server

`viiwork-mcp` is an MCP server that exposes the viiwork cluster as tools for any MCP-compatible AI assistant. This lets AI coding tools delegate inference to your locally hosted models.

### Build

```bash
make mcp    # builds bin/viiwork-mcp
```

### Tools

| Tool | Description |
|------|-------------|
| `query` | Send a prompt to a local model. Params: `prompt` (required), `system`, `model`, `max_tokens`, `temperature` |
| `models` | List available models on the cluster |
| `status` | Get cluster health, per-GPU backend status, in-flight counts |

### Configuration

The MCP server connects to a viiwork instance via `--url` flag or `VIIWORK_URL` environment variable:

```bash
viiwork-mcp --url http://your-viiwork-host:8080
```

Add it to your MCP client's configuration as a stdio transport server pointing at the `viiwork-mcp` binary.

## Development

```bash
make build         # build binary (with git version embedded)
make mcp           # build MCP server
make test          # run unit tests
make docker        # build stable Docker image (viiwork:latest)
make docker-gfx906 # build experimental Docker image (viiwork:gfx906)
make up            # docker compose up -d
make down          # docker compose down

go test -v -tags=integration  # integration tests (mock backends, no GPU needed)
go test -v -run TestName ./internal/package  # single test
```
