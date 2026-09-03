# Changelog

## v1.7.0

### Mesh gossip: peers.hosts becomes a seed list

With `peers.gossip.enabled: true` and a shared `VIIWORK_MESH_SECRET` (at
least 32 bytes, environment only, never YAML), a node learns its peers'
peers transitively: one reachable address is enough to join the mesh, and
N×N peer configuration goes away. Off by default — with it off, behaviour
is byte-identical to v1.6.x, on the wire included.

Membership is proved, not assumed. Every mesh-to-mesh call carries an
HMAC-SHA256 proof (`X-Viiwork-Auth` and friends) over pinned canonical
strings, with a 120s skew window. Endpoints stay readable without it, so
browsers, the gateway and viiwork-nvidia are untouched; what a proof buys
is standing — only a verified peer's cluster report is believed, only a
validated address (IP literal, allowed ranges, strict port) is ever
dialled, the learned-peer intake is capped, and an adopted address is not
routed to or advertised onward until it proves membership on its own
status poll. Configured peers stay first-class with or without a proof,
which is what makes a mixed-version rollout safe.

`ClusterPeerInfo` gains `origin` (`config` or `learned`), additive and
omitempty. `require_forward_proof` (default off) upgrades the forgeable
`X-Viiwork-Forwarded` claim to a signature over method, path and body with
a replay-closing nonce cache; flip it on only once the whole fleet signs.

The registry's peer set is now an atomic snapshot rather than a plain
slice, so it can grow at runtime without racing the request path.

## v1.6.3

### The mesh event stream no longer writes to a finished response

`/v1/mesh/stream` runs one goroutine per peer plus a cluster-snapshot loop, and
all of them wrote to the HTTP response directly, serialised by a mutex. The
mutex made those writes mutually exclusive but said nothing about when they
*stop*: the handler could return while a producer was still mid-write, and
touching an `http.ResponseWriter` after the handler returns is a data race and
a violation of `net/http`'s contract.

It was reproducible — the race detector flagged it on roughly two runs in three
— and it had been shipped in every build since the mesh stream gained peer
fan-out.

The handler is now the only writer of its own response. Producers hand encoded
frames to it over a channel and never hold a reference to the response at all,
so the problem is gone by construction rather than by lifetime bookkeeping.

Waiting for the producers instead would have deadlocked, which is worth
recording: an SSE response must not carry a `WriteTimeout`, so a connected but
stalled client can block a write indefinitely — and the activity log closes the
oldest subscriber's channel when it hits its subscriber limit, which returns the
handler with the client still perfectly healthy. Waiting there would have hung
the request on a producer that could not finish.

**Upgrading:** nothing to do. No configuration, endpoint, wire field or on-disk
format change, and the stream behaves identically from a consumer's side.

## v1.6.2

### Builds stamp the version they actually are

`make build`, `scripts/update.sh` and `scripts/rebuild.sh` each derived the
version with `git describe --tags`. That is right in the public repository,
which is tagged at release. It is wrong in the development tree, which carries
no tags — they are created on the public repo at publish time — so describe
reported the newest tag it could still see.

The effect was that v1.5.3, v1.6.0 and v1.6.1 all built as `v1.5.2-N-g<sha>`,
and reported that on `/v1/status` and in the dashboard. The version an operator
reads off a misbehaving host was two releases stale.

`scripts/version.sh` now answers the question once for all three callers. An
exact tag still wins, so a release build from the public repository is
unchanged and prints exactly its tag. Otherwise the version comes from
`CHANGELOG.md`'s top heading — the one place a tagless tree knows what it is,
and a file that is updated as part of cutting a release, so it cannot drift the
way a tag the repo does not carry can. A short sha and a `-dirty` marker are
appended between releases, and a tree with neither git nor a changelog falls
back to `dev` rather than failing the build.

**Upgrading:** nothing to do, and no code changed — this is build tooling only.
Hosts rebuilt from this version onward will start reporting their real version;
one built earlier keeps whatever it was stamped with until it is rebuilt.

## v1.6.1

### `energy`: attribution is now swappable, for nodes that measure each card

`energy/doc.go` told an implementation that measures per-board power to supply
`AttrW` and `RawW` as the same value. `Recorder` gave it no way to do that — it
ran the whole-chassis split unconditionally.

That split is right when the node figure measures more than the GPUs do: fans,
CPU, drives and PSU losses are drawn whether or not a model is serving, and
charging them to a model would be wrong. It is meaningless when node power is
*itself* the sum of the per-card readings, because then any residual is not
overhead but idle draw on cards that exist to serve the resident model.

The failure was not a small skew. Idle floors fall back to the lowest current
reading when the store has no history, so on a fresh store an evenly loaded host
has no marginal power at all and **every card is attributed 0 W** — per-model
energy stays empty until a genuinely idle minute is observed, which a busy
inference node may not see for weeks.

- **`NewRecorderWithAttribution`** takes an `AttributeFunc` deciding how a
  bucket's node figure is split between cards.
- **`Direct`** is that function for a per-board producer: each card is charged
  what it drew, the shares sum to the node figure, and the baseline is honestly
  zero.
- **`NewRecorder` is unchanged**, in signature and in behaviour — it passes nil
  and gets the existing split. A test pins the two as byte-identical.

Everything around the attribution step stays shared, which is the point: the
per-minute averaging and the `CoveredS` accounting that keeps a restart
mid-bucket from being extrapolated to a full minute are exactly what you would
least want a second copy of.

**Upgrading:** nothing to do. Additive API, no configuration, endpoint, wire
field or on-disk format change, and what viiwork records is unchanged.

## v1.6.0

### The energy store is now a public package

`internal/energy` has moved to `github.com/janit/viiwork/energy`. It keeps a
durable per-host, per-model kWh history — node draw, per-GPU draw, and a split
of one between the models causing it — in a fixed-size on-disk store that never
grows past a couple of megabytes.

It moved for the same reason `meshapi` did in v1.5.3: the fleet has a second
implementation. `viiwork-nvidia` drives vLLM on CUDA hardware and reports the
same `energy_kwh_24h` to the same dashboard, and Go does not allow importing
`internal/` across module boundaries. The alternative was a second copy of the
ring, tier and model-table code — two independent producers of one binary
format, with nothing keeping them in step.

The seam that makes this work was already there. The package never runs a
command or reads a sensor; it takes a `NodeWattsFunc` and a `GPUReadingsFunc`
and knows nothing else about where power comes from. viiwork fills them from
`ipmitool` and `rocm-smi`, and another implementation fills them from
`nvidia-smi`.

Three things came with the move, because a format with two producers is not the
same object as a format with one:

- **The on-disk layout is documented as a contract.**
  `docs/energy-store-format.md` specifies it byte by byte — header, record
  layouts, the timestamp-derived slot rule, the model table, roll-up weighting
  — and states what may change and what may not. `energy/doc.go` covers the Go
  API.

- **A format mismatch is now refused rather than silently repaired.** Opening a
  store whose magic or record size disagrees with the running build used to
  recreate the file, which was a local annoyance with one producer and the
  destruction of a year of somebody else's history with two — indistinguishable,
  months later, from a node that had never been switched on. It now fails with
  an error naming the file, and leaves the bytes untouched. Changing a slot
  count or adding a GPU is *not* that kind of change: those are per-deployment
  configuration and still recreate the file, saying so in the log as before.

- **The store records what its node wattage actually measured.** A
  whole-chassis IPMI reading and a sum of GPU board power are the same bytes in
  the same field, and differ by hundreds of watts. A `source` file in the store
  directory now carries the same label the mesh publishes as `power_source`, and
  an absent one reads as unknown rather than as a default.

**Upgrading:** nothing to do. No configuration, no endpoint and no wire field
changed, existing stores are read as-is, and energy tracking stays off by
default. Anything importing `viiwork/internal/energy` — which nothing outside
this repository could — updates its import path.

## v1.5.3

### The mesh protocol is now a public package: `meshapi`

Everything a node publishes to other nodes and to the dashboard — the
`/v1/status` payload, the `/v1/cluster` snapshot, the activity event stream, the
prompt lookup, the endpoint paths — now lives in `github.com/janit/viiwork/meshapi`
instead of being spread across `internal/`. No wire format changed; this is the
same bytes on the network, defined in one importable place.

It moved because the mesh has a second implementation. `viiwork-nvidia` drives
vLLM on CUDA hardware and joins the same mesh and the same dashboard, and Go
does not allow importing `internal/` across module boundaries. Rather than let
a second copy of every struct drift on another repo's schedule, the contract is
published and both sides depend on it.

`internal/peer` and `internal/activity` keep their familiar names through type
aliases, so `peer.StatusResponse` and `meshapi.StatusResponse` are the same
type and no call site changed. What did change is that 215 lines of duplicated
definitions are gone.

Three things came with it that were previously implicit:

- **The activity message grammar is documented as wire format**, because that
  is what it is. A request event reads `"<model> → <destination>"` with a
  terminal suffix once it finishes, and every dashboard in the fleet
  reconstructs its in-flight rows by splitting that string and matching the
  terminal word — there is no server-side registry of running jobs anywhere.
  `meshapi.RequestStarted`/`RequestDone`/`RequestAborted` build the messages and
  `SplitRequestMessage`/`IsRequestTerminal` take them apart; `handler.go` and
  `balancer.Label` now go through them rather than spelling the format out.

- **The compatibility rules are stated and tested.** New fields are additive and
  `omitempty`, and absent is not zero — a consumer must read a missing number as
  "unknown", never as a measured zero. That is what lets a fleet whose machines
  are upgraded days apart render at all. `wire_test.go` pins every field name,
  so a rename fails the build rather than silently stranding a column on a host
  you did not upgrade.

- **The package is stdlib-only and self-contained**, which keeps the option of
  extracting it into its own module cheap should that ever be worth doing.

**Upgrading:** nothing to do. No configuration, no endpoint and no field
changed, and a node on this version is wire-compatible in both directions with
every node that predates it.

## v1.5.2

### The mesh dashboard has a fixed address: port 8086 on every host

`http://<any-host>:8086/` now serves the cluster view, and the number is the
same on every host in the fleet.

The cluster view was always served by every node; the problem was reaching one.
A host runs one viiwork instance per model, each on its own `server.port`, so
opening the dashboard meant knowing which instance was up on which host —
precisely what you do not have when something is wrong.

The port is **contended, not assigned**. Every instance asks for it at startup
and the OS gives it to exactly one; the rest keep asking every 15 seconds. So
it is up as long as *any* viiwork on that host is, it hands over on its own
when the instance holding it restarts, and it needs no designated node, no
per-host configuration and no reverse proxy. Which instance answers does not
matter — the mesh view is built from peer state every node already has.

A node reached on 8086 is an ordinary node in every other respect: only `/`
moves, so the page's own `/v1/mesh/stream`, `/v1/mesh/power` and `/prompt`
calls resolve against the same origin, and CORS applies as usual.

**Upgrading:** this binds a second port that previous versions did not, on by
default. Nothing else changes — `server.port` and the per-node dashboard are
untouched, and a host where something else already holds 8086 simply never
binds it and runs exactly as before. `server.mesh_port: 0` opts out;
`server.mesh_port: <n>` moves it. There is a matching `--server.mesh_port`
CLI override.

## v1.5.1

Documentation only, no code change.

- **New README screenshot**, showing the mesh dashboard as of v1.5.0: fleet
  totals, live power with 24-hour and 30-day energy, the per-host RAM strip,
  and the listen ports on model groups. The previous one predated all of it.

## v1.5.0

### Chassis power control from the mesh dashboard

Each host in the Fleet Power table can carry a power button. **Off by default**,
and there is no wildcard: only hosts named in `power.control.hosts` can be
targeted, by the dashboard or by anything else.

A running host is controlled in-band by the node living on it, with no
credentials — the mesh forwards the request to whichever node owns the target.
A host that is **powered off** has no node to ask, so reaching it needs BMC
credentials; without them it can be switched off but not back on, and the
button says so rather than failing. Hosts in the allowlist appear in the table
even when they are absent from the mesh, which is exactly the state a
powered-off host is in.

Guards, none of which is authentication: the allowlist, a confirmation prompt
naming host and action, and a node's refusal to power off its own host — that
would destroy the answer to the request and the page asking it, so its button
is disabled and the server refuses it too.

### Ports on model groups

Grouped by model, each backends group header lists the ports that model is
served on. Not shown when grouped by host, where the ports belong to different
models and listing them together would imply an addressing that does not exist.

### A 30-day energy total alongside the 24-hour one

The Fleet Power headline carries the rolling 30-day total at its far right,
opposite the live reading, and each host's row now has `now`, `24h` and `30d`
columns rather than one packed value. Two bare kWh figures side by side are
indistinguishable, so the columns are named once in a header instead of
repeating the window on every row.

Published as `energy_kwh_30d` beside `energy_kwh_24h`. It reads the day tier —
30 buckets against 720 hourly ones — and like the 24-hour figure it is a
whole-host number: group by `hostname` rather than summing across instances. A
store younger than a day reports the same value for both windows.

## v1.4.0

### Energy beside live power

The mesh dashboard's Fleet Power headline now reads `1,751 W / 12.4 kWh (24h)`
— what the fleet is drawing now, and what it has drawn over the rolling last
24 hours. Each host's row carries the same pair.

The figure rides on `/v1/status` and `/v1/cluster` as `energy_kwh_24h`, so the
dashboard reads it off the snapshots it already receives rather than polling a
new endpoint. It is a **whole-host** number like `power_watts`: the durable
store runs on one instance per host, so consumers must group by hostname rather
than summing across instances. It reads the minute tier, whose ring is exactly
24 hours, so the window and the retention are the same span.

Energy is opt-in per host while power is not, so the header says `· kWh from N`
whenever fewer hosts contribute energy than are reporting power — a total from
one host should not read as fleet-wide.

## v1.3.0

### Stale in-flight rows after a sleep or a background tab

The mesh and node dashboards reconstruct in-flight requests from the event
stream — a start event adds a row, a done event removes it — and nothing
replayed the events lost while a browser was away. A laptop waking from sleep,
or a tab throttled in the background, came back with rows that had finished
hours earlier and counted up in red forever.

Both streams now replay their event ring when the connection opens, marked
`"replay": true`, and both dashboards clear the reconstructed set on reconnect
and let the replay rebuild it. A gap longer than the ring is still not
recoverable, but the result is then a short count rather than invented work.
Opening the page mid-flight now also shows the jobs already running, which it
never did before.

If you consume `/v1/mesh/stream` or `/v1/activity/stream` yourself, see the
notes in `docs/api-integration.md`: rebuild on reconnect rather than carrying
state across, and deduplicate anything you display, since replayed events
repeat what a visible log already shows.

### Fleet totals on the mesh dashboard

GPUs busy, VRAM and host RAM across the whole mesh, as three readings above the
model list. All three are counted per host: `rocm-smi` reports every card on a
machine rather than only the ones an instance owns, so a naive sum over the
payload reported 110 GPUs for 50 actual cards on a fleet with co-located
instances.

Hosts now sort by name in the power rows, the stacked bands and the RAM strip,
instead of by value.

### Fixed

- **The single-node dashboard showed every activity line twice** once streams
  began replaying, because it also fetched `/v1/activity` separately. The fetch
  is gone; the stream carries it.
- **`stream reconnecting…` accumulated** on the mesh header, once per retry,
  while a network was down.

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

### To v1.5.0

Nothing changes on a node until you opt in. Power control is off by default and
has no wildcard: it does nothing until you name hosts.

```yaml
power:
  control:
    enabled: true
    hosts: [host-a, host-b]
```

That much controls hosts that are **running**, in-band, with no credentials. To
reach a host that is **powered off** you also need a `bmc` block with
credentials — see *Power control* in the README. Without it a host can be
switched off but not back on, and the dashboard shows a disabled button saying
so.

Enabling it is a real grant: **the API authenticates nothing**, so anything that
can reach a node can power off any host in that node's allowlist. Keep the list
to the hosts you actually want reachable this way. A node still refuses to power
off its own host, so a dashboard cannot kill the machine serving it.

New consumer-visible fields: `power_control` on `/v1/cluster` (absent unless
enabled), and `energy_kwh_30d` beside `energy_kwh_24h`.

### To v1.4.0

Nothing to do on a node, and nothing to change in a consumer. The one addition
is `energy_kwh_24h` on `/v1/status` and `/v1/cluster`, which appears only where
the energy store is enabled.

If you read it, treat it as a **whole-host** figure like `power_watts`: the
store runs on one instance per host, so group by `hostname` rather than summing
across instances. It covers the rolling last 24 hours, not the store's full
history.

### To v1.3.0

Nothing to do on a node. The change that matters is for anything consuming
`/v1/mesh/stream` or `/v1/activity/stream` in your own code: both now replay
their recent event ring when a connection opens, so a consumer that appends
every event it receives to a visible list will show the replayed ones a second
time. Deduplicate on node, request id, timestamp and message, and rebuild
reconstructed state on reconnect rather than carrying it across. See
`docs/api-integration.md` §6.

Dashboards served by viiwork already handle both.

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
