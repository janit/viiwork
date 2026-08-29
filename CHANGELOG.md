# Changelog

## v1.2.0

### Power probing, per-GPU wattage, and a durable energy store

Node power had been silently reporting 0 W across the whole gfx906 fleet, which
also kept cost tracking switched off — `cost.Tracker` gives up when power is
unavailable. Every Gigabyte board here answers `sdr type "Power Supply"` with
presence flags and no wattage, so the previous hardcoded command summed nothing
and reported a confident zero. The sampler now probes for a reading that is
actually above zero: DCMI first (the standardised whole-node reading), then the
`Power Supply` sensor class, then any Watts-valued sensor. `power.source` pins
it to `dcmi`, `sdr`, `sensor:<NAME>` or `none` if you would rather not probe,
and whichever source was adopted is logged and published as `power_source`.

`rocm-smi` is now also asked for per-GPU package power, with a fallback to the
original flags if that arg set is rejected — wattage is a bonus, and losing
utilisation and VRAM to gain it would be a bad trade.

On top of those, `internal/energy` keeps a durable per-host, per-model kWh
history: node draw, per-GPU draw, and a marginal-power split between the models
causing the load and the baseline a host draws just by being switched on. Off by
default; see *Energy History* in the README. Enable it on exactly one instance
per host — node wattage is a whole-host measurement.

### The mesh dashboard shows fleet power

`/mesh` gains a **Fleet Power** panel: the mesh total as a headline number, a
stacked graph with one band per host, and a table naming each host's draw and
which IPMI reading it came from. Grouped by host, the backends table also carries
each host's wattage on its group header.

It needs no configuration and adds no polling — the samples come off the cluster
snapshots the dashboard already receives. The window is since page load, capped
at 720 readings; it is a live view, not history.

Under it is a film strip of per-host RAM: one small sparkline per host, sharing
one time window, each scaled 0 to that host's total so height reads as memory
pressure. Host memory now travels on `/v1/status`, so the strip covers every
host rather than only the one serving the page.

Host memory used to be stripped from the pushed mesh snapshot, because an exact
figure moves every second on a live host and made the stream push a full
snapshot that often. It is now coarsened to 64 buckets with a deadband instead —
a bucket is under a pixel on the strip, and `/v1/cluster` still carries the exact
figures.

### Fixed

- **Cluster wattage counted multi-instance hosts several times.** A host running
  one viiwork instance per model reports the same whole-host BMC reading from
  each of them, and both dashboards summed the payload as it arrived — three
  times over on a three-tenant host. Both now key by hostname and count each
  host once.
- **Co-located instances appeared as two hosts.** `/v1/cluster` derived a peer's
  hostname from the address it is dialled on, but co-located peers are
  configured by IP, so one machine showed up under both its hostname and its
  address.
  The hostname the peer actually reports is now preferred, with the
  address-derived one kept as the fallback for peers too old to report one.
- **A single energy recorder left most of its host unattributed.** Recording
  runs on one instance per host, and that instance labelled only the GPUs it
  owns — one card in ten on a 10-GPU host. Model labels for a co-tenant's cards
  now come from the peer poll, which already carries `gpu_ids`, a model and a
  hostname.

## v1.1.1

### Prompt history depth is configurable, and defaults to 1000

```yaml
activity:
  prompt_history: 1000   # was a hardcoded 100
```

Memory scales with it — roughly the count times up to 100 KB, since prompt and
output are each truncated at 50 000 characters — so the default is about 100 MB
of worst-case headroom and realistically far less. A value below 1 falls back to
the default rather than producing a store that silently drops everything.

The number is no longer kept in two places. Each node publishes its capacity as
`prompt_history` on `/v1/status` and in `/v1/cluster` (under `local`, and per
entry in `peers`), and the mesh dashboard sizes its own list from the largest
value any node reports. Raise the config and the view follows. Nodes older than
v1.1.1 omit the field; consumers should read its absence as "unknown", not as
"keeps nothing".

### Fixed

- **The prompt page claimed a request had aged out when it had barely started.**
  A request is not necessarily in the store the instant its activity event
  reaches a browser, and the page is routinely opened from a row that is still
  running, but the first 404 was reported as permanent loss. A miss is now
  retried briefly before it is believed, and the three cases that were collapsed
  into one message — not yet recorded, genuinely evicted, node unreachable — say
  different things.
- **The page no longer strands a running request.** Output is written once, when
  the response finishes; the page now follows the request to completion instead
  of showing a half-empty page that a manual reload would have fixed.
- **The "aged out" message quoted a hardcoded 100** rather than the node's
  configured depth.

## v1.1.0

Mesh dashboard work, and the first release that lets a browser on another origin
talk to a node.

### Prompt history now records the output too

`/v1/prompts` and `/v1/mesh/prompt` return `output` and `elapsed_ms` alongside
the prompt. Same store as before: last 100 requests per node, in memory,
oldest-first eviction, nothing on disk, truncated at 50 000 characters per side.

- Capture tees the response bytes on their way to the client and parses once at
  the end. Nothing is decoded per token — that path is deliberately kept clear
  (see `BenchmarkCaptureWriter`, which separates the per-token write cost from
  the one-shot parse).
- What is recorded is what the client actually received, after think-block
  rewriting rather than before.
- Reasoning is kept and labelled, not folded into the answer. A thinking model
  with thinking enabled leaves `content` empty and puts everything in
  `reasoning_content`, so discarding it would blank the output for exactly the
  requests worth reading.
- A failed request stores its error body.
- A request whose prompt could not be extracted (multimodal content parts) now
  still gets an entry if it produced output.

### Full-page prompt view at `/prompt`

Replaces the dashboard's in-place modal. Rows in the Prompts list are ordinary
links with `target="_blank"`, so cmd-click, middle-click and *open in new tab*
all work — the workflow is fanning a batch of requests into background tabs and
reading them side by side, which a dialog cannot do. Each tab is titled with its
request id. The page shows prompt and output in separate panels with character
counts and a copy button each.

### Halt on the mesh dashboard

A header button (or `h`) freezes the whole view so rows stop moving while you
read or click them. It queues incoming events rather than merely skipping the
re-render: both client-side lists evict as they grow, so a frozen render over a
still-mutating store would leave rows on screen whose entries had already been
dropped underneath — and those rows are now links you are about to click. The
queue drains in arrival order on resume, capped at 1000 events.

### CORS

`server.cors` in `viiwork.yaml`. Ships allowing `*.ts.net`, `localhost`,
`127.0.0.1`, and literal Tailscale IPs (`100.64.0.0/10`,
`fd7a:115c:a1e0::/48`) — the deployment this project documents. A consuming
application's own origin is deployment-specific: add it to that node's
`viiwork.yaml`. `allow_origins: []` restores the previous behaviour of sending
no CORS header at all.

viiwork still authenticates nothing, so an origin allowlist is not protecting
the API from anyone who can already reach it — it stops a page in some browser
on your network from quietly driving your fleet through that browser's network
position. Keep the list short. Where a consumer has a backend of its own, a
server-side proxy is still the better answer: no allowlist entry needed, and it
can put authentication in front of an API that has none.

Two implementation details that were easy to get wrong and are pinned by tests:

- The SSE streams carry the allow header on their own `GET` response.
  `EventSource` is CORS-bound but sends no preflight, so an `OPTIONS` handler
  alone would have left `/v1/mesh/stream` unusable cross-origin.
- `OPTIONS` is answered ahead of routing. The router matches only GET and POST,
  so preflights previously fell through to 404 and no cross-origin POST could
  work. A refused preflight returns 403 rather than a bare 204, so a bad origin
  is diagnosable instead of looking like a dozen other failures.

### Fixed

- `make docker` now passes `VERSION` through to the build. The Dockerfile
  defaults `ARG VERSION=dev`, so without it the image reported `dev` from
  `/v1/cluster` and `/v1/status` no matter what the tree was tagged — worst
  precisely on a release build. `scripts/update.sh` and the gfx906 target
  already did this; this target was the odd one out.

## Upgrading

### To v1.2.0

Nothing in this release changes behaviour on an existing deployment until you
opt in, and the API additions are all `omitempty` — a node on 1.2.0 meshes with
one on 1.1.x, each simply showing blanks for what the other does not report.

Two things are worth doing per host:

- **Give one container per host the BMC device** (`- /dev/ipmi0:/dev/ipmi0`) to
  get node wattage on the dashboard. Without it a host reads 0 W exactly as
  before, and cost tracking stays off, since it gives up when power is
  unavailable.
- **Enable the energy store on exactly one instance per host**, with a volume
  that outlives the container. Node wattage is a whole-host measurement, so
  enabling it on several instances of a multi-model host records the same draw
  several times over.

If you consume `/v1/cluster` from your own application, read the notes on
`hostname`, `power_watts` and `host_mem_used_mb` in `docs/api-integration.md`:
per-host figures must be grouped by hostname rather than summed across
instances, and host memory on the pushed stream is now coarsened.

**Rolling upgrade is safe, and a mixed fleet works.** A v1.1.0 node meshes with
v1.0.0 peers in both directions:

- v1.1.0 asking a v1.0.0 peer for a prompt gets a payload with no `output` or
  `elapsed_ms`; the page renders that as "no output recorded" rather than
  failing.
- v1.0.0 asking a v1.1.0 peer ignores the added fields.
- `/v1/status` is unchanged, so peer polling and routing are unaffected.

Upgrade the always-on node first and the rest at leisure.

**One behaviour change on upgrade:** CORS defaults are active as soon as a node
runs v1.1.0 — it will start answering preflights and sending
`Access-Control-Allow-Origin` for the origins listed above. Set
`server.cors.allow_origins: []` before rolling if you do not want that.

**No config migration is required.** Every new key has a default.

**Check what a node is running** with the `version` field of `/v1/cluster`. It
is absent on builds old enough to predate it, which is itself the answer.
