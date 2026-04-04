# viiwork

LLM inference load balancer for AMD Radeon VII GPUs. Runs multiple llama-server instances and exposes a single OpenAI-compatible API with adaptive load balancing. Multiple nodes can form a mesh cluster where any node is an entry point and requests route by model.

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
  gemma-4-26B-A4B-it-UD-Q3_K_XL.gguf --local-dir models
docker compose up -d
```

## Multi-Model Setup

Run multiple models on one host using `./scripts/setup-node.sh`. It detects GPUs, lets you assign models to GPU groups, downloads models, and generates configs with mesh peering between instances.

Example: 10 GPUs split across 3 models:
- 4 GPUs on port 8080: Gemma-4-31B-IT (flagship text generation)
- 4 GPUs on port 8081: Gemma-4-26B-A4B-IT (fast MoE inference)
- 2 GPUs on port 8082: Gemma-4-E4B-IT (lightweight tasks)

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

## Dashboard

Available at `http://localhost:8080/`. Shows:
- Node status and GPU health
- Peer mesh connectivity
- Live GPU utilization and VRAM graphs (1 hour history, SSE updates)
- Power consumption and electricity cost
- Build version in footer

## API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/` | GET | Status dashboard |
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

All models fit in 16GB VRAM (Radeon VII) with full GPU offload. The safe VRAM ceiling is ~13GB after accounting for KV cache and ROCm runtime overhead.

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

## Docker Build

The Docker image pins llama.cpp to a specific release tag and patches the HIP FP8 header for gfx906 compatibility. To bump the llama.cpp version:

```bash
docker compose build --build-arg LLAMA_CPP_VERSION=b8700
```

The FP8 patch is required because ROCm 6.2+ includes `<hip/hip_fp8.h>` for all architectures, but gfx906 has no FP8 hardware and the header fails to compile.

## Scripts

| Script | Description |
|--------|-------------|
| `scripts/setup-node.sh` | Interactive setup: detect GPUs, select models, download, generate configs |
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
make build    # build binary (with git version embedded)
make mcp      # build MCP server
make test     # run unit tests
make docker   # build Docker image
make up       # docker compose up -d
make down     # docker compose down

go test -v -tags=integration  # integration tests (mock backends, no GPU needed)
go test -v -run TestName ./internal/package  # single test
```
