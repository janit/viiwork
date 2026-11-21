# Fleet autodiscovery

Point a coding client at any node and the fleet describes itself: which models
it serves, and how large a prompt each one will actually take. No model list
written down on the client, no context window typed in by hand, and nothing to
regenerate when the fleet changes.

**The claim this makes**: a metadata-aware client pointed at any node discovers
models, safe context windows and capabilities from the live fleet. **The claim
it does not make**: universal zero-config for every client. Cline, and Roo Code
configured as a generic OpenAI-compatible provider, still require manual entry
and will not read the enriched fields.

## Why this needs doing at all

The market standardised the *transport* and never standardised *discovery*.
OpenAI's `GET /v1/models` returns `{id, object, created, owned_by}` — no
window, no output ceiling, no capabilities — and has not been extended despite
years of requests. So every client either ships a hardcoded table of the models
it has heard of, or makes the user type the numbers in.

Four separate projects are closing that gap right now, in three incompatible
spellings of the same integer. viiwork is unusually well placed to answer,
because every one of those formats assumes one host with one model loaded,
while a viiwork node already computes the minimum context across the hosts its
router would route to. That number is the thing a client can safely size a
prompt against.

## The number

Everything below publishes one figure, the **served context**: the total
prompt+completion budget per slot that the fleet guarantees.

- It is the **minimum** across hosts serving the model. Nothing stops an
  operator loading one model at 32K on one machine and 98K on another, and the
  router will route to either — so the floor is the only number a client can
  rely on.
- Only a host that is **fresh and has slots** contributes it. A host whose
  report has gone stale asserts nothing, and a draining host takes no work, so
  neither is offering a window. It is the smallest windows that drain first, so
  this is not a hypothetical.
- It is **not the checkpoint's architectural maximum**. No node knows that
  number, and it is not what the fleet will serve.
- When no host can say, the figure is **absent, not zero** — in every format
  that can express the difference.

One canonical record in `internal/discovery` holds it, and every format below
is a thin serializer over that record. Three formats that each assembled their
own view would be three things to keep true, and the first one written would
quietly become the source of truth for the rest.

## Path 1 — any client that reads `/v1/models`

Nothing to configure. The list every OpenAI-compatible client already fetches
now carries the window in both spellings the ecosystem converged on:

```json
{
  "id": "granite-4.2-8b",
  "object": "model",
  "owned_by": "local",
  "max_model_len": 32768,
  "context_length": 32768
}
```

`max_model_len` is vLLM's name for it, `context_length` is OpenRouter's, and
both formats define their field as a prompt+completion total — which is what
makes it correct to emit the same figure as both. Widest reach for the least
code: any client that reads either one gets a true window.

Both are additive and `omitempty`, the permitted form of a C4 contract change.
A node one version behind simply omits them, and a reader sees "this node
cannot say" rather than a measured zero.

## Path 2 — OpenCode, on `/api.json`

OpenCode has **no** `/v1/models` discovery for openai-compatible providers.
That is an [open feature request](https://github.com/anomalyco/opencode/issues/6231),
not a feature, and there is no vLLM provider either — a config naming one does
nothing. What OpenCode does have is a catalogue source: `OPENCODE_MODELS_URL`,
from which it fetches `<url>/api.json`, caches it for five minutes and
refreshes it in the background. That is the seam `internal/catalog` fills.

Two things, once:

```bash
export OPENCODE_MODELS_URL=http://gb1:8086     # any node; every node serves the whole mesh
```

```json
{
  "$schema": "https://opencode.ai/config.json",
  "provider": { "viiwork": {} }
}
```

That is the entire client configuration. `opencode models` then lists the
fleet:

```
viiwork/DeepSeek-V4-Flash
viiwork/big
viiwork/granite-4.2-8b
viiwork/summarise
```

`scripts/setup-opencode.sh` writes both for you.

The document's shape is models.dev's, because that is what OpenCode parses. It
is deliberately **not** in `meshapi`: nothing here crosses a boundary between
viiwork nodes, and the fields belong to a third party.

### What the document says

- **Every model `/v1/models` lists**, including aliases and pipelines. The two
  endpoints render one list, so a model can never be callable but invisible.
- **`limit.context` is the served context.** An alias inherits its target's. A
  pipeline has no capacity row anywhere, so it gets a deliberately small
  fallback rather than a zero — see [what viiwork does not
  claim](#what-viiwork-does-not-claim).
- **`limit.output` is a quarter of the window**, capped at 32768. This is a
  guess, and it is documented as one in the code. It stays because models.dev
  requires the field and omitting it is what made OpenCode ask for 32000 output
  tokens and have every request rejected by a smaller backend.
- **`api` is the node the client reached**, not a name baked in at build time.
  Any node is an entry point and every node serves the whole mesh, so the
  address that fetched the catalogue is the address that will serve inference.
- **`modalities.input` is `["text"]`, or `["text", "image"]`** for a model
  that some host with slots says it reads images with (v2.10.0). A client
  that reads `image` there attaches pictures, so the word appears only when a
  request with one has somewhere to go.
- **`tool_call` is true** and `npm` is `@ai-sdk/openai-compatible`. Without the
  latter OpenCode treats the provider as one of its own native APIs and never
  reaches the node at all.

### Where the fleet appears in the picker

OpenCode orders its model picker by provider **name**, compared byte by byte,
after its own entry. `api.catalog.provider_name` is therefore the only thing
that decides the position, and the default — `" viiwork"`, with a leading space
— is what puts the fleet at the top. A plain `viiwork` sorts below every
capitalised hosted provider. Rename it to whatever you like; the leading
character is the whole mechanism.

Note that `opencode models`, the CLI, sorts by provider **id** instead. The two
orderings differ, so the CLI is not a test of the picker.

### Chaining a hosted catalogue

Setting `OPENCODE_MODELS_URL` **replaces** the client's whole catalogue, so on
its own it costs you Anthropic, OpenAI and the rest. `api.catalog.upstream`
chains one back in:

```yaml
api:
  catalog:
    upstream: https://models.opencode.ai
    upstream_ttl: 1h
```

The node then serves its own provider plus every provider upstream publishes,
with the fleet's entry winning on a collision. The upstream is fetched once per
TTL, never per request, and it is an enrichment rather than a dependency: if it
cannot be reached the node logs it and serves the fleet alone. A stale copy is
kept rather than dropped, so providers do not vanish from a client's list
because one refresh failed.

This is off by default. A node on a tailnet may have no route off it, and it
should not acquire one because a config file said nothing.

### Operational notes

- The endpoint is read-only and holds no state beyond the upstream cache. It
  costs a node nothing when nobody fetches it.
- It is not under `/v1`: the path is OpenCode's to choose, since it appends
  `/api.json` to whatever URL it is given.
- `api.catalog.enabled: false` turns it off; `/api.json` is then a 404.
- Behind a proxy that rewrites the host, set `api.catalog.base_url` to the
  address clients actually use.
- The client caches for five minutes, so discovery is up to five minutes stale
  during churn whatever viiwork does. That is the client's cache and there is
  nothing useful to do about it from this side.

## Path 3 — Roo Code and other LiteLLM readers, on `/v1/model/info`

Roo Code's generic OpenAI-compatible provider makes you hand-type a context
window for every model. Its **LiteLLM** provider reads one from
`/v1/model/info`. Same fleet, same transport — the only difference is whether
the numbers are discovered or typed. So configure Roo Code as a LiteLLM
provider pointed at a node:

```
Base URL:  http://gb1:8086
API key:   (anything; viiwork authenticates nothing)
```

The document:

```json
{
  "data": [
    {
      "model_name": "granite-4.2-8b",
      "litellm_params": { "model": "granite-4.2-8b" },
      "model_info": {
        "max_input_tokens": 32768,
        "mode": "chat",
        "supports_function_calling": true
      }
    }
  ]
}
```

`model_info` also carries `"supports_vision": true` for a model that some
host with slots says it reads images with (v2.10.0). The field is left out
otherwise, which this format reads as unknown.

Serving this shape does **not** make a node a LiteLLM proxy. Only the discovery
document is LiteLLM's; inference stays on the OpenAI-compatible endpoints the
node already serves, which is the transport these clients speak anyway. That is
what keeps it safe — a node must never advertise a dialect it will not then
accept requests in, and here it advertises none.

The endpoint is unconditional, unlike `/api.json`. It renders exactly what
`/v1/models` already publishes, in another client's spelling, so there is no
exposure to opt out of.

## What viiwork does not claim

Three deliberate silences, and two deliberate approximations. They are marked
in the code so that the next serializer does not copy them by accident.

**No output ceiling.** viiwork has no output limit distinct from the shared
window: a slot's context is one prompt+completion budget and the split between
them is the client's to choose. Any output figure could only be a guess, so
`/v1/models` and `/v1/model/info` emit none. The one exception is the models.dev
document's `limit.output`, which is required by that format — and it is
documented there as a client-side budgeting hint, not a server capability.

**No cost, and no modality beyond text and images.** Images are listed only
on a host's own word (see [models.md](models.md#models-that-read-images));
audio and video are never claimed. Local inference is free at the point
of use; the electricity is in the energy store, not in a per-token price.

**A model of another modality is not listed at all** (v2.13). A mesh member
that is not a viiwork node (viiwork-voice) publishes each of its models with
`modality` on `/v1/capacity`: `"voice"` for a full-duplex audio session
model, `"image"` for an image generator. Absent is text, which is accurate,
because no node before v2.13 serves anything else. A model the fleet serves
only with another modality is dropped from the discovery record, and so from
`/v1/models`, `/api.json` and `/v1/model/info`, along with any alias that
targets it: a chat client could name it but never use it, since text
inference refuses it with 400 (`code: model_modality`). When some host serves
the same name as text, the model stays listed and only its text hosts count
toward the served context. `/v1/fleet/capacity` lists every model, with
`modality` on the model and on each host, so a consumer that does speak the
other protocol finds its hosts there.

**No architectural maximum.** No node knows the largest context a checkpoint
could support, only the context it was loaded at.

**`max_input_tokens` is knowingly generous.** LiteLLM defines it as a
prompt-only ceiling, while viiwork's number is a shared prompt+completion
budget, so a client budgeting `input + output` against it will overshoot
slightly. It is emitted anyway, because it is still a true hard bound — the
server really will accept a prompt of that size, it just leaves no room to
answer — and because the alternative is not "no claim": Roo Code defaults its
context window to **200000** when the field is absent, which is wrong by an
order of magnitude on every model on this fleet. A slightly generous true bound
beats an invented one.

**A pipeline gets a fallback window in the catalogue.** A pipeline is
node-local and appears in no capacity report, so nothing can size it. The
record says so, and every format that can express "unknown" passes it through —
but models.dev cannot, and OpenCode reads a zero as a window rather than as
silence. So the catalogue substitutes a deliberately small figure. The two
errors are not symmetric: too large a window makes every request fail at the
backend, too small a one only shortens the conversation.

## Not served, and why

**Ollama's `/api/tags` and `/api/show`.** The largest client ecosystem by a
distance, and the temptation is real. But serving Ollama *discovery* without
Ollama's `/api/chat` dialect would advertise a fleet that then refuses the
requests it invited. That makes it a C8 client-API-dialect decision rather than
a discovery one, and it deserves its own spec. Unexamined rather than rejected.

**LM Studio's `/api/v0/models`.** Its `max_context_length` versus
`loaded_context_length` split is exactly viiwork's architectural-versus-served
distinction, and is the right mental model to carry through every serializer.
The client ecosystem is small enough that the shape itself can wait.

## Checking it by hand

```bash
curl -s http://gb1:8086/v1/models     | jq '.data[] | {id, max_model_len}'
curl -s http://gb1:8086/api.json      | jq '.viiwork.models | keys'
curl -s http://gb1:8086/v1/model/info | jq '.data[] | {model_name, ctx: .model_info.max_input_tokens}'
```

All three must agree on both the model list and the number; a node test pins
that, because no single client would ever notice if they drifted.

Against the real OpenCode client, with no node and no GPU:

```bash
go test -tags catalogfixture -run TestCatalogFixture ./internal/node/ &
OPENCODE_MODELS_URL=http://127.0.0.1:18087 opencode models
```

The fixture serves canned fleet data through the real handler.
`CATALOGFIXTURE_UPSTREAM=https://models.opencode.ai` exercises the chained
path, and `CATALOGFIXTURE_FOR=150s` bounds the run. Go tests prove the document
says what viiwork means; only the client proves the client agrees — and an
integration test caught a real bug this way already, advertising a 512-token
model a 1024-token output limit.
