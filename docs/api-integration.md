# Integrating the mesh API into another application

How to surface a viiwork fleet — models, live jobs, per-GPU load, and the
prompt/output history — inside an application you already run, rather than
sending people to viiwork's own `/mesh` dashboard.

Written for an implementor who has not worked on viiwork. It covers the
endpoints, the two ways to reach them from a browser application, and the
handful of semantics that are not visible in the payloads and will otherwise
cost a day each to discover.

Applies to viiwork 1.1.0 and later. Where a behaviour is newer than 1.0.0 it
says so.

---

## 1. What you are building against

viiwork is an LLM inference load balancer across a fleet of GPU hosts. Any node
in the mesh is a complete entry point: **ask one node and it reports the whole
cluster.** You do not need to reach every host, and you do not need to know the
topology in advance.

That single property is what makes this integration small. One HTTP call to one
reachable node gives you the fleet; one held-open stream gives you live
activity for all of it.

Everything the API exposes is read-only state held in memory. Nothing is
persisted, and a node restart clears its history.

## 2. Access and trust

**viiwork has no authentication.** There are no API keys, no tokens, no sessions.
Every endpoint is open to anything that can open a TCP connection to the node.
This is deliberate — viiwork is built to run on a private network where
reachability *is* the authorization model — but it has consequences you must
design around:

- **Whatever you build must not become a path from an untrusted network to a
  node.** If your application is reachable more widely than viiwork is, then
  your application's own authentication is the only thing in between. Put the
  integration behind it.
- **Prompt and output text are readable over the API**, unauthenticated like
  everything else. If the traffic on your fleet is sensitive, that is a reason
  to keep the integration server-side.

How your application reaches the fleet — VPN, overlay network, routed LAN,
service discovery — is deployment-specific and out of scope here. This document
assumes only that some host running your code can make HTTP requests to a node.

## 3. Two integration modes

```
PATH A — SERVER-SIDE PROXY (RECOMMENDED)

  Browser  ──same origin──▶  Your server  ──private network──▶  viiwork node

  No CORS involved · your existing auth applies · works against any version

PATH B — DIRECT FROM THE BROWSER (FALLBACK)

  Browser  ───────────private network, needs CORS───────────▶  viiwork node

  Viewer's browser must reach the node itself · needs 1.1.0+ · no auth gate
```

The asymmetry that decides it: **your server can almost certainly reach the
fleet; your users' browsers may not.**

### Path A — server-side proxy

Add endpoints in your own application that fetch from a node and return the
response. Two properties make this the default choice:

1. **Your application's authentication becomes the gate.** viiwork provides
   none. A proxy endpoint inside your authenticated area is the only
   arrangement where fleet access is actually controlled.
2. **The viewer does not have to be able to reach the fleet.** With Path B, a
   user outside the private network sees an empty page and no useful error.

Streaming passes through unchanged in any runtime with a streaming HTTP client —
forward the upstream response body rather than buffering it, or the live view
will arrive all at once when the connection closes.

Put the node's base URL in configuration (an environment variable), not in
source. Nodes move.

### Path B — direct from the browser

Available from 1.1.0, which added CORS. Take it only when Path A genuinely
cannot do the job — for example an event stream you would rather not hold open
through your own server — and accept that you are giving up the auth gate.

Configure the allowlist under `server.cors` in `viiwork.yaml`:

```yaml
server:
  cors:
    allow_origins: ["*.ts.net", "localhost", "127.0.0.1", "*.your-app.example"]
    allow_tailnet_ips: true
```

`*.example.com` matches subdomains only, never the bare apex; every other entry
must match the host exactly. What ships by default covers Tailscale MagicDNS
names, localhost, and literal tailnet IP ranges — **your application's own
origin is deployment-specific and you must add it.** `allow_origins: []` sends
no CORS header at all.

Preflights are answered, and a refused origin gets `403` rather than a silent
failure, so a misconfiguration is visible in devtools. Cross-origin responses
expose `X-GPU-Backend`, `X-Queue-Depth` and `X-Viiwork-Origin`.

> **Do not mix modes.** Pick one per data source and stay there. An integration
> that proxies its snapshots but opens its event stream directly will silently
> show live data only to viewers whose browsers happen to reach the fleet, and
> the difference is invisible until someone reports "the job list is always
> empty" from a machine you cannot see.

## 4. API reference

Every endpoint below is `GET`, returns JSON, and requires no headers.

### `GET /v1/cluster` — the primary call

Whole-mesh snapshot: the node you asked plus every peer it polls. This is the
one endpoint a fleet overview needs.

```json
{
  "node_id": "viiwork-50f3ac6cf39a2160",
  "version": "v1.1.0",
  "hostname": "node0",
  "single_host": false,
  "models": ["some-model-27B-Q4_K_XL", "another-model-8b"],
  "local": {
    "model": "some-model-27B-Q4_K_XL",
    "listen_addr": "node0:8080",
    "power_watts": 549, "power_available": true, "power_source": "dcmi",
    "energy_kwh_24h": 12.4, "energy_kwh_30d": 301.8,
    "host_mem_total_mb": 64196, "host_mem_used_mb": 31426,
    "gpus": [ { "gpu_id": 0, "util": 0, "vram_used_mb": 12488.1, "vram_total_mb": 16368 } ],
    "backends": [ {
      "gpu_id": -1, "gpu_ids": [0, 1],
      "model": "some-model-27B-Q4_K_XL", "status": "healthy",
      "in_flight": 2, "rss_mb": 4934,
      "slot_ctx": 49152, "slot_count": 2, "slot_active": 2,
      "tok_decoded": 86, "tok_remain": 2914
    } ]
  },
  "peers": [ {
    "addr": "node1:8080", "hostname": "node1",
    "status": "reachable", "node_id": "viiwork-52ebd96f1c30321c",
    "models": ["another-model-8b"],
    "healthy_backends": 1,
    "power_watts": 210, "power_available": true, "power_source": "sdr:Power Supply",
    "host_mem_total_mb": 128395, "host_mem_used_mb": 41220,
    "backends": [ ], "gpus": [ ]
  } ]
}
```

| Field | Meaning |
|---|---|
| `version` | Build string of the node answering. Absent on older builds — treat absence as "pre-1.1.0". |
| `hostname`, `peers[].hostname` | The hostname each node reports for itself, which is how you tell co-located instances apart from separate machines. It is the **grouping key for anything measured per host** — see the wattage row below. Older nodes may not report one, in which case the peer's entry falls back to the host part of its configured address; a consumer should not assume the two forms never mix. |
| `single_host` | True when every peer is co-located on this machine — several viiwork instances on one host, meshed via localhost. Render those as instances of one host, not as separate hosts. |
| `peers[].status` | `reachable` or `unreachable`. An unreachable peer still appears, with its other fields blank. Show it as unreachable rather than hiding it. |
| `backends[].gpu_id` | `-1` means a tensor-split group; read `gpu_ids` instead. A plain replica has `gpu_id >= 0` and no `gpu_ids`. |
| `backends[].status` | `starting` · `healthy` · `unhealthy` · `dead`. Cold-loading a large model can sit in `starting` for tens of minutes. |
| `slot_ctx`, `slot_count`, `slot_active` | Context window per slot, slots configured, slots busy. Context *use* is `slot_active / slot_count`. |
| `tok_decoded`, `tok_remain` | Tokens produced and budget left across active slots. |
| `power_available`, `cost_available` | False where power or price data is not configured. When false, ignore the accompanying numbers rather than rendering zeros. |
| `power_watts` | **A whole-host measurement, not a per-instance one.** A host running one viiwork instance per model reports the same BMC reading from every one of them, so summing this field across `local` + `peers` counts such a host once per instance. Group by `hostname` and take one reading per host. |
| `power_source` | Which IPMI reading the node settled on: `dcmi`, `sdr:Power Supply`, or `sensor:<NAME>`. Probed at startup because it is board-specific. Diagnostic only — surface it where an operator is asking why a host reads what it does. Absent on older nodes and whenever `power_available` is false. |
| `energy_kwh_24h`, `energy_kwh_30d` | Whole-node energy over the **rolling last 24 hours** and **last 30 days**, in kWh, from the durable store. Present only on the one instance per host that records it, so these are **not** per-instance figures — group by `hostname`, as with `power_watts`. Absent where the store is not enabled, and a young store reports the same value for both. |
| `host_mem_total_mb`, `host_mem_used_mb` | Host RAM, `used` being `MemTotal - MemAvailable` so reclaimable page cache is not counted as pressure. Also whole-host, so group by `hostname` as with wattage. Absent on nodes older than this field; read a zero total as "unknown", not as "no memory". On the pushed stream these are coarsened — see §6. |

### `GET /v1/status`

The asked node only — the same backend and GPU detail as `local` above, plus
`total_in_flight`, `healthy_backends` and `total_backends`. This is what peers
poll each other with. Use it only when addressing a specific node; otherwise
`/v1/cluster` already contains it.

### `POST /v1/power`, `POST /v1/mesh/power` — chassis power

Both take `{"host": "...", "action": "status|on|off|cycle"}` and return
`{"host", "action", "result", "via"}`, or `{"error"}`. Disabled by default;
a node without it configured answers **503**, not 404, so you can tell "will
not" from "does not know how".

`/v1/mesh/power` is the one to call. It forwards to the node living on the
target host, and reaches the host's BMC over the network only when nobody
answers there — which is the only way to act on a host that is powered off.
`/v1/power` is the executor and acts solely on the node you address it to.

| Refusal | Meaning |
|---|---|
| 403, "not listed in power.control.hosts" | The host is not in the allowlist. There is no wildcard; being a peer is not enough. |
| 403, "is serving this dashboard" | A node will not power off its own host — that would destroy the answer to the request. Ask a different node. `status` is still allowed. |
| 400 | Unknown action, or `/v1/power` addressed at some other host. |
| 502 | The host is unreachable and has no out-of-band BMC configured, so it cannot be woken. |

**There is no authentication on this**, as with the rest of the API. Anything
that can reach a node can power off any host in that node's allowlist. Keep the
allowlist to the hosts you actually want reachable this way, and prefer a
server-side proxy (Path A) if a browser needs to drive it.

`power_control` on `/v1/cluster` lists what a node will accept — `hosts`, and
`out_of_band` for the subset reachable while off. Hosts in that list may be
absent from `peers` entirely: that is what a powered-off host looks like, and
it is why the list is published at all.

### `GET /v1/models`

OpenAI-shaped model list, aggregated across the mesh:
`{"object": "list", "data": [{"id": "..."}]}`. Includes pipeline virtual models
where configured.

### `GET /health`

Liveness. Use it for a reachability indicator, **not** for fleet health — a node
answers `/health` while every backend under it is dead or still loading.

### `GET /v1/mesh/stream` — `text/event-stream`

One held-open connection carrying **two named event types**. Nothing polls:
cluster snapshots are diffed server-side and pushed only when they change, so an
idle mesh is silent. Peer fan-out happens on the server, so one reachable node
is enough to see the whole mesh.

```
event: activity
data: {"t":1756360000,"type":"request","message":"some-model → ts-0,1","rid":41,
       "task_id":"","gpu_id":-1,"node_id":"viiwork-50f3…","hostname":"node0","addr":""}

event: cluster
data: { …a full /v1/cluster payload… }
```

Handle them **by name** — `es.addEventListener("activity", …)` and
`("cluster", …)`. A bare `onmessage` handler receives nothing, because named
events never dispatch to it.

| Activity field | Meaning |
|---|---|
| `t` | Unix seconds. |
| `type` | `request` · `backend` · `system`. Only `request` events carry a `rid`. |
| `message` | Human text, shaped `"<model> → <destination>"`, with `" done (1.2s)"` or `" aborted by client"` appended on completion. Split on `→` rather than re-deriving the parts. |
| `rid` | Request id. **Per-process counter, not cluster-wide** — see §6. |
| `node_id`, `hostname`, `addr` | Which node the event came from. `addr` is empty for the node you are connected to and set for peers; you need it for prompt lookups. |

### `GET /v1/activity`, `GET /v1/activity/stream`

Single-node event log — the last 200 events, and an SSE stream of the same. No
peer merging and no server-side fan-out, so for a mesh view prefer
`/v1/mesh/stream`. Useful when you are deliberately looking at one node.

### `GET /v1/metrics`, `GET /v1/metrics/stream`

Per-GPU utilisation and VRAM history — one hour at 5-second resolution (720
samples per GPU), plus a live stream. Returns `{"available": false}` where GPU
metrics tooling is missing; handle that rather than assuming samples exist.

### `POST /v1/chat/completions`

Standard OpenAI chat completions, streaming or not, routed to whichever node
serves the requested model. Relevant only if your integration includes a prompt
box. Response headers name the backend that served it: `X-GPU-Backend`
(`gpu-4`, or `ts-4,5` for a tensor-split group) and `X-Queue-Depth`. `429` means
every backend is at capacity — respect `Retry-After`. `503` means no healthy
backend at all.

**Pinning a host.** `?host=<hostname>` on any of the three inference endpoints
narrows routing to one machine — `POST /v1/chat/completions?host=gb2`. Absent
or `mesh` routes as usual. The name is a hostname (`gb2`), not `host:port`: a
machine running several viiwork instances is one host, and the request is
still balanced across its GPUs. A host that is not serving the model answers
`404` with a message naming both, never a silent fallback to the mesh, and a
malformed value is `400`. `X-Viiwork-Origin` on the response names the peer
the request was proxied to; its absence means the answering node ran it. The
value is only compared against hostnames the node already knows and is never
dialled, so it cannot reach anything normal routing could not. Through the
gateway a pinned request may take two hops (gateway → a node serving the
model → the pinned host); that is expected and invisible to the caller.

## 5. Prompt and output history

The part of the API with real sharp edges. Read this before designing a request
list.

Every proxied request has its prompt captured when it starts and its output
captured when it finishes, keyed by request id. **Text is never pushed on the
event stream** — the stream carries only that a request happened — so the list
of requests and the content of any one request are two separate fetches. That is
deliberate: full bodies on every event would put per-request payload on a path
most consumers never look at.

### `GET /v1/mesh/prompt?rid=<id>&addr=<host:port>` — use this one

Fetches one stored request. `addr` is the `addr` from the activity event that
produced the `rid`; pass it **empty** for a request the asked node ran itself.
The node forwards to the named peer server-side and returns the result, which is
what makes lookups work when the caller cannot reach that peer directly.

```json
{
  "rid": 41,
  "t": 1756360000,
  "model": "some-model-27B-Q4_K_XL",
  "prompt": "what is 2+2?",
  "output": "2 + 2 is 4.",
  "elapsed_ms": 1240
}
```

| Field | Notes |
|---|---|
| `prompt` | Last user message, or the raw `prompt` field for legacy completions. Can be absent — a multimodal request with array content parts yields none. |
| `output` | **Absent while the request is still running**, and absent entirely before 1.1.0. Present once a request finishes. For a failed request this is the error body, which is usually the most useful thing to see. |
| `elapsed_ms` | Wall time, recorded with the output. Absent until then. |

**Reasoning models.** With thinking enabled a model puts its answer in a
separate reasoning channel. `output` then arrives labelled —
`"[reasoning]\n…\n\n[answer]\n…"`, or `"[reasoning]\n…"` alone when there was no
separate answer. Render those sections distinctly rather than as one blob; the
labels exist precisely so a consumer can split them.

### `GET /v1/prompts?rid=<id>`

Same payload, the asked node's own store only, no forwarding. This is what
`/v1/mesh/prompt` calls on the owning peer. Use it only when you are certain
which node minted the id; otherwise it 404s.

> **Security constraint — do not work around.** `/v1/mesh/prompt` forwards
> **only** to addresses already in the answering node's configured peer list.
> That check is load-bearing: without it the endpoint fetches whatever address
> it is handed and echoes the response back, which is a server-side request
> forgery primitive on a private network. If a lookup returns
> `400 unknown peer`, the fix is the peer configuration on the viiwork side —
> never a proxy in your application that fetches arbitrary addresses on a
> caller's behalf.

### Storage limits — design the UI around these

- **Capacity** — the last **`activity.prompt_history` requests per node**,
  1000 by default, evicted oldest-first. It is per-node configuration, so
  different nodes in one mesh may keep different depths. Each node reports its
  own value as `prompt_history` on `/v1/status` and in `/v1/cluster` (both
  `local` and each entry of `peers`); it is absent on nodes older than v1.1.1,
  where you should assume 100. **Size any client-side list from the largest
  value you see rather than hardcoding one**, or you will evict rows the owning
  node could still answer a lookup for.
- **Durability** — memory only. A node restart loses everything, with no signal
  other than lookups starting to 404.
- **Truncation** — prompt and output are each capped at 50 000 characters,
  suffixed `"... [truncated]"`.
- **Failure mode** — a lookup for an evicted or restarted-away id returns `404`.
  Treat it as expected, not as an error: "this request has aged out of the
  node's history".

## 6. Semantics that bite

Five properties that are not visible in the payloads.

### Request ids are per-node, not cluster-wide

`rid` is a process-local counter. Two nodes will both mint `rid 41` for
unrelated requests within seconds of each other. **Key every request by
`(node_id, rid)`** and carry the event's `addr` alongside, or your list will
merge unrelated work and open the wrong prompt.

### There is no server-side registry of running jobs

No endpoint returns "what is running now". In-flight requests are reconstructed
by the consumer from the event stream: a `request` event adds an entry, a
matching event whose message contains *done* or *aborted* removes it, keyed as
above.

Both streams replay their event ring when the connection opens, which repairs
most of what that costs. Replayed events carry `"replay": true` and arrive
before live ones, so a consumer that connects mid-flight sees the jobs already
running, and one that reconnects gets the *done* events it missed while away.

Two obligations come with it:

- **Rebuild on reconnect, do not carry state across.** Anything that finished
  during a gap leaves a start with no done — a row that ages forever. Clear the
  reconstructed set when the stream opens and let the replay repopulate it. A
  request older than the node's ring is then dropped rather than re-added,
  which under-reports rather than inventing work; the aggregate counts stay
  correct either way.
- **Deduplicate anything you display.** Reconstructed state is keyed and so
  replays for free, but an append-only event log will show every replayed line
  a second time. Key on node, request id, timestamp and message, and hold the
  set deeper than the list you render.

The ring bounds it: a gap longer than `maxEvents` on a given node cannot be
repaired from the replay. The aggregate counts in `/v1/cluster` (`in_flight`
per backend) remain correct regardless and are what to trust for totals.

### Freshness differs by column

| Data | Freshness | Source |
|---|---|---|
| Peer jobs / activity | live | merged event streams |
| Local backend + GPU state | ~5 s | health-check tick |
| Peer backend counts, peer GPU load | ~10 s | `peers.poll_interval` |

Do not present all three as equally current. A job appearing instantly next to a
GPU utilisation figure that is ten seconds stale reads as a contradiction.

### Host memory is coarsened on pushed snapshots

Snapshots on `/v1/mesh/stream` are diffed server-side and sent only when they
change. Host memory ticks every second on a live host — measured at ~86 MB/s,
spiking past 600 MB — and an exact figure defeats that entirely, so it used to
be removed from the pushed copy altogether.

It is now included but coarsened: quantized to 64 buckets of the host's total,
and held at the published level until the reading drifts a full step away from
it. In practice the value moves only when memory moves visibly, and a host that
is merely idling republishes nothing. Two consequences for a consumer:

- **Treat `host_mem_used_mb` from the stream as approximate**, within about one
  bucket (~1 GB on a 64 GB host). Render it as a level or a percentage, not as
  an exact figure, and never diff two stream values to infer an allocation.
- **`/v1/cluster` remains exact.** Call it directly if you need the real number
  — for an alert threshold, a capacity report, or anything a human will quote.

### SSE, not WebSockets

The feed is one-way and viiwork carries no WebSocket dependency. Use
`EventSource` (Path B) or forward the stream through your own server (Path A).
Note that `EventSource` sends no preflight — under Path B it relies on the allow
header being present on the `GET` response itself, which it is.

## 7. Interface expectations

This document specifies no visual design: build with your application's existing
components and conventions so the result is indistinguishable from the pages
around it. Do not port viiwork's dashboard styling across.

Two behavioural requirements, because both are load-bearing rather than
cosmetic:

- **A freeze control on any live list.** Rows in a live-updating request list
  move under the pointer, which makes them effectively unclickable during busy
  periods. Freezing must *queue* incoming events and apply them on resume, not
  merely stop re-rendering — a frozen render over a still-mutating store leaves
  rows on screen whose entries have already been evicted underneath.
- **Request detail opens as a real link.** The working pattern is triaging many
  requests at once, so a request row should be a link to its own URL rather than
  a click handler opening a dialog. Cmd-click, middle-click and "open in new
  tab" all need to work, and each tab needs a title identifying which request it
  is.

Everything else — layout, grouping, which columns to show, whether GPU load gets
a meter or a number — is yours. viiwork's own `/mesh` page is a reference for
*what information matters*, not a design to copy.

## 8. Before you start

**Check what the node is actually running.** `GET /v1/cluster` reports the build
in its `version` field. Against pre-1.1.0 nodes: Path A works unchanged,
`output` and `elapsed_ms` are simply absent from prompt lookups, and every
direct browser fetch fails with an opaque CORS error that no amount of
client-side debugging will explain. Build against Path A and a fleet upgrade is
a non-event.

**Expect a mixed fleet during a rolling upgrade.** Nodes at different versions
mesh fine in both directions, so your integration will see some peers reporting
`output` and others not. Handle the field's absence rather than its emptiness.

**Cold loads are long.** A large model loading from network storage can hold a
backend in `starting` for twenty minutes or more, during which the node answers
`/health` but serves no traffic. Any "is the fleet up?" indicator must
distinguish *node reachable* from *backends healthy*, or it will report a green
fleet that cannot take a request.

**Stale peer entries are normal.** A peer list is static configuration. Nodes
that moved, changed port, or were retired stay in `/v1/cluster` as
`unreachable`, and prompt lookups against them fail with `unknown peer`. Render
that honestly; it is a fleet configuration matter, not something your
integration should paper over.

## 9. Acceptance checklist

Verifiable against a fleet under load.

- [ ] The whole mesh renders — every node, its models, healthy backend counts,
      per-GPU utilisation and VRAM — from a single call to one node.
- [ ] Unreachable peers appear as unreachable rather than being omitted or shown
      as healthy.
- [ ] A cold-loading backend reads as `starting`, not as up.
- [ ] In-flight requests appear and clear live, with model, destination and
      elapsed time.
- [ ] Recent requests list correctly across nodes, with no id collisions.
- [ ] Opening a request shows its prompt and, once finished, its output;
      reasoning and answer are visually distinct where both are present.
- [ ] Opening several requests at once puts each in its own identifiable tab.
- [ ] Freezing a live list stops all movement, and resuming applies everything
      that arrived meanwhile, in order.
- [ ] A lookup for an aged-out request reads as history expiry, not as an error.
- [ ] Every call is behind your application's own authentication.
- [ ] A user whose browser cannot reach the fleet directly sees the same thing
      as one whose browser can.

---

Reference implementation of everything above: viiwork's own `/mesh` and
`/prompt` pages, plus the "Mesh Dashboard", "Prompt and output history",
"Security" and "API Endpoints" sections of the README.
