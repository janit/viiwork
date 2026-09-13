# Changelog

## v2.0.0

**viiwork 2.0.** One process per machine supervising every model on it, a mesh
that forms itself, and routing that follows free slots. The two betas put it in
front of a converted machine serving production traffic and of a second
implementation building against its contracts; what they found is fixed, and
this is that code called finished.

**This is a breaking release, and the break is the mesh protocol.** v2 nodes
gossip their membership on 7946 rather than polling a written list of peers
over HTTP, so a v1 node and a v2 node never see each other — a fleet converts
together, or runs as two meshes until the last machine is across. A v1
`viiwork.yaml` is refused at startup rather than guessed at, the Go module path
is `github.com/janit/viiwork/v2`, and `meshapi`'s wire types moved with the
protocol. `docs/migrating-to-v2.md` maps every key and covers rollback;
`viiwork-accept config` validates the new file while v1 is still serving. The
entries below under `v2.0.0-beta2`, `-beta1`, `-rc.2`, `-rc.1` and `-alpha.1`
are kept because they say why each part is the way it is.

### Since beta2

- **Dependencies are current.** The three direct dependencies —
  `hashicorp/memberlist` v0.6.0, `hashicorp/mdns` v1.0.7 and `gopkg.in/yaml.v3`
  v3.0.1 — were already at their latest releases. The indirect graph was not:
  `golang.org/x/net` moved v0.55.0 → v0.59.0, which clears GO-2026-5942 (a
  panic parsing a malformed SVCB or HTTPS DNS record). Nothing in viiwork calls
  it, but `mdns` is fed records off the LAN, so shipping a 2.0.0 with a known
  advisory in the graph was not worth the argument. `golang-lru` v0.5.0 →
  v1.0.2, `miekg/dns` v1.1.73, `hashicorp/go-metrics` v0.6.1, `x/sync`, `x/sys`
  likewise; `x/mod` and `x/tools` left the graph entirely. `armon/go-metrics`
  stays at v0.4.1 because later versions renamed the module path — v0.4.1 is
  the last release under the old one. `govulncheck` is clean.
- **The Go toolchain pin is 1.27.1** in `go.mod` and every Dockerfile, up from
  1.27.0 — fixes to the runtime, the compiler and `net/http`.
- **`viiwork-mcp` reported its version as `1.0.0`.** The string in the MCP
  `initialize` response was a literal, not the build stamp, so every assistant
  that connected to a viiwork cluster was told 1.0.0 — through the whole of
  viiwork 1.x and into 2.0. It is `main.version` now, `make mcp` stamps it like
  the node and the acceptance checker, and a test pins that it comes from the
  variable. It was the only hardcoded version left in the tree; every other
  surface — `/v1/status`, `/v1/cluster`, both dashboards, `viiwork-accept
  --version`, the image build args — already flowed from `scripts/version.sh`.
- **The README says what v2 breaks, above the fold.** A reader arriving at the
  repository met a feature tour and found out about the protocol change nine
  sections in, under Scripts.
- **`docker-compose.yaml.example` is gone from the repository root.** It was a
  v1 node — port 8080, bridged networking, no `pid: host`, no
  `stop_grace_period` — and `BUILDS.md` called it the stable-track example, so
  the one file a reader would copy was the one that produced a node that could
  not gossip, could not verify its GPUs and was killed mid-drain on stop.
  `configs/docker-compose.v2.example.yaml` is the single answer, which is what
  the README's Quick Start already copied.
- **`configs/viiwork.tensor-split.yaml.example` says it is v1 in its first
  line**, with the `gpus_per_backend` form beside it. It is the only remaining
  `.example` in a v1 vocabulary, and its own text invited comparison with the
  root example, which is v2.

### Not in this release

- `llamacpp` is the only engine. The vLLM and FreeToken engines land in v2.1.0.
- `scripts/setup-node.sh` and `deploy.sh` still write and drive v1 layouts.

## v2.0.0-beta2

Room in the mesh dashboard's backends table, which has to fit a whole fleet on
one screen. Two columns were spending width on things their own numbers already
said.

- **The GPU column is compact.** A tensor-split group reads `gpu-4+5` rather
  than `gpu-4 + gpu-5`: the repeated prefix was the widest thing in the table
  and said nothing new. GPU ids are also coerced to integers before rendering,
  since they are peer-supplied and that string is interpolated into the page.
- **The VRAM column has lost its bar.** A 46px meter sat beside
  `25.5G/32.0G`, showing the same ratio the numbers give. Only its warning
  survives, as the colour of the text — amber above 70% of the card, red above
  90% — so the signal is kept at no width. The Tokens column keeps its meter,
  where there is no second reading of the same thing.
- **The parts that decide who may do what are now covered by tests.**
  `internal/power`'s chassis control had none at all: the allowlist (no
  wildcard, no suffix matching, an empty list permitting nothing), the fixed
  action set, `Local` refusing to cut its own power before reaching
  `ipmitool`, and the diagnostic path that must never echo the BMC password.
  `internal/activity` went from a third covered to nearly all of it, and the
  mesh address check gained the NAT64 and IPv4-mapped cases that prove an
  address cannot be smuggled past a range check by encoding it in another form.
- **A member could put an alias in the table that no operator could.** The
  gossip merge validated the alias *name* but never the entry, while the local
  write path refuses an empty target ("target is required"). A live entry with
  no target is now ignored on merge too; a tombstone legitimately has none.
  Found by the repository's first fuzz targets, which cover the two entry
  points that consume bytes from another member — `Service.NotifyMsg` and
  `Service.MergeRemoteState` — and assert the table stays within its cap, holds
  no unnamed or unvalidated entry, and still marshals. 720,000 executions found
  nothing further.
- **`viiwork-mcp` pointed at the wrong port.** Its default was
  `http://localhost:8080`, which is viiwork 1's port and serves nothing on a v2
  fleet, so running it without `--url` or `VIIWORK_URL` only ever produced a
  connection refusal. Now 8086, in the flag help, the doc comment and
  `.mcp.json`. It also calls `meshapi.PathModels`, `PathCluster` and
  `PathChatCompletions` rather than repeating the strings, and has tests: it
  previously had none.
- **`mesh.Options` documents two things a second implementation lost time to.**
  `OnChange` may be called *before* `Start` returns — the dispatcher runs
  before memberlist does, and memberlist notifies about the local node while
  bootstrapping, so a handler reading something built from `Start`'s return
  value races its own assignment. And `TailnetSocket` and `LocalAPISocket` are
  not alternatives: the first turns discovery on, the second answers "what is
  my own tailnet address" and is needed even with discovery off. Both found by
  the gateway building against beta1; no behaviour changed.
- **Fixed a panic in the activity log.** `emit` sent to a snapshot of the
  subscribers taken under the lock and released before sending, while
  `Subscribe` closes a subscriber's channel when the log is at capacity. The
  two interleaving meant a send on a closed channel, which panics — a select's
  `default` case does not prevent that. Sends now happen under the lock, which
  cannot stall because they are already non-blocking. The capacity is 16 and a
  browser tab or a connected mesh-stream client is one each, so this was not a
  stress-only path. `TestEmitDoesNotPanicWhileSubscribersAreEvicted` reproduces
  it in 200 rounds against the old code.
- **Truncated prompts are cut on a rune boundary.** The 50,000-character cap is
  applied in bytes, which lands mid-character in any non-ASCII script — the
  normal case on a translation fleet — and the tail reached `/v1/prompts` as
  invalid UTF-8 for the JSON encoder to silently replace with U+FFFD.
- **The single-node dashboard escapes for attribute position too.** Its
  escaper set `textContent` and read back `innerHTML`, which leaves `"` and
  `'` untouched — safe between tags, unsafe inside an attribute, and it was
  used inside one (`class="status ..."`). A value carrying a quote would have
  closed the attribute and turned the rest into markup. Nothing reached it:
  that value is the node's own backend state, from a fixed vocabulary. Fixed
  anyway, along with an event type that went to the page unescaped and GPU ids
  that are now coerced to integers before being rendered as markup, so the
  page no longer depends on where its data happens to come from. `/mesh`,
  which does render other members' data, was already strict.
- **Endpoint paths are pinned** in `meshapi/wire_test.go`, for the same reason
  field names are: a consumer in another repository dials them or compares
  against them, so changing one silently breaks something that cannot be
  updated in the same commit. Only machine-to-machine paths are listed. The
  browser pages and `/v1/metrics`, which only the dashboard fetches, are
  deliberately left out — freezing a UI route in a package with no migration
  path would make an HTML URL a permanent commitment, and the boundary this
  contract draws is what nodes say to each other.
- The README describes viiwork as built *originally for* Radeon VII rather than
  *on* it: the config already accepts NVIDIA hardware and other engines, so the
  old phrasing read as a hardware requirement rather than as provenance.

Both dashboard changes are in `web/`, which `viiwork-nvidia` imports rather
than forking, so they reach that fleet view too.

## v2.0.0-beta1

**First public release of viiwork 2.** Everything below under `v2.0.0-rc.2`,
`v2.0.0-rc.1` and `v2.0.0-alpha.1` was developed privately and is released
together here; those entries are kept because they say why each part is the way
it is.

Beta rather than a release candidate because this is the first build outside
the fleet it was written on: one machine has been converted and is serving
production traffic, and `llamacpp` is the only engine. See the rc.2 notes for
what shipped most recently, and `docs/migrating-to-v2.md` to convert a 1.x host.

## v2.0.0-rc.2

### Acceptance tooling, a conversion guide, and three fixes from the first real node

**`viiwork-accept`** is a new command that checks a node and a mesh without
changing either: `config` validates a v2 file and summarises what it will run,
`ports serve`/`probe` prove gossip reaches a machine on tcp and udp, `join`,
`ready` and `gone` time a node in and out of the cluster, `models` exercises
content and tool calls per model, `saturate` overflows a model's local slots to
confirm the mesh takes the overflow, and `alias` checks a name resolves through
every entry point. It is read-only by design — it reads state and sends
inference requests, and never starts or stops anything.

**`docs/migrating-to-v2.md`** converts a host: the v1-to-v2 key mapping, a
worked example, mesh modes, and a step-by-step procedure that validates the new
file while v1 is still serving, so an invalid file is never discovered after the
old instances have stopped. `update.sh` and `rebuild.sh` now wait on
`:8086/health`.

**Fixes**

- **`/health` no longer reports a joining node as healthy.** The API listener
  starts before the mesh and the supervisor, so a node still joining — or one
  that cannot reach tailscaled, and so never will — answered 200 `"ok"` with no
  backends, indefinitely. The model count now comes from the running
  configuration rather than from the supervisor, so a machine with models
  configured and nothing serving answers 503. A node with no models configured
  is still healthy: a pure router is a valid node.
- **A pipeline can no longer be an alias target.** The alias table is
  replicated to every member; a pipeline runs only on the node that configures
  it, so such an alias could never resolve anywhere else. It is now refused in
  targets and fallbacks, and `--force` no longer creates one — previously the
  refusal message invited `--force`, which produced an alias that resolved
  `unavailable` forever.
- **A `[debug]` line escaped `VIIWORK_DEBUG`.** The think-disabled stream
  logged its scanner error unconditionally, and a client that stops reading
  ends a stream in error, so ordinary aborted generations wrote to the log on a
  per-request path in every deployment.

### Not in this release

- `llamacpp` is the only engine. The vLLM and FreeToken engines land in v2.1.0.
- `scripts/setup-node.sh` and `deploy.sh` still write and drive v1 layouts.

## v2.0.0-rc.1

### One binary, one node per machine, a mesh that forms itself

viiwork 2 runs **one process per machine**. That node supervises every model
configured on the machine — a `models:` list replaces the v1 instance files —
and serves the API and every dashboard on **port 8086**, with membership gossip
on **7946** (tcp and udp). Per-model instance ports, `server.mesh_port` and the
port contention behind it are gone: every machine is `http://<machine>:8086`.

**The mesh forms itself.** Nodes find each other through tailscaled
(`mesh.network: tailnet`, the default), mDNS on a LAN (`mesh.network: lan`) or
`mesh.seeds`, so adding a machine means writing that machine's config and
nothing anywhere else, and a machine that stops leaves routing within seconds.
A mesh is either **secured** — `VIIWORK_MESH_SECRET`, base64 of 32 bytes, the
same everywhere: encrypted gossip and signed forwards — or declared **open**
with `mesh.open: true`. A node refuses to start with neither or both, and a
node in the other mode logs `mesh mode mismatch` instead of joining. Of two
processes started with the same node name, the newer one exits.

**Routing follows free slots.** Every node polls each member's `/v1/capacity`
once a second. A request runs on a local backend with a free slot, else goes to
the member with the most free slots, else waits in a FIFO queue on the node that
received it, for up to `routing.queue_timeout` (20 s). A member that refuses a
forward before its first byte is retried elsewhere and not picked again until it
reports fresh capacity. A receiving node admits a forward only into a free slot
and never forwards it again. Responses carry `X-Viiwork-Node` (where it ran),
`X-Viiwork-Origin` (where it arrived) and `X-Viiwork-Queued-Ms` when it waited.

**Aliases.** `viiwork alias set stable-coder Qwen3.8-27B --fallback granite-4.1-8b`
points a stable name at a real model across the whole mesh; `ls`, `rm`,
`revert`, `history`, `export` and `import` complete the command, which reports
when every member has the change. An alias resolves on the node that receives
the request, to its target if any member serves it, else to its first served
fallback, else 503. Aliases gossip between nodes and persist in
`node.state_dir`. Writes need a signature in a secured mesh and must come from
the node's own machine in an open one. `/v1/models` lists aliases with
`owned_by: alias`, and aliased responses carry `X-Viiwork-Alias` and
`X-Viiwork-Model`.

**Reload on SIGHUP.** `docker kill -s HUP viiwork` or `systemctl reload viiwork`
re-reads the config: added models start, removed ones drain, and a changed entry
restarts that model alone. An invalid file keeps the running configuration, and
changes outside `models` are logged as needing a restart.

**GPUs.** Backends are pinned with `ROCR_VISIBLE_DEVICES` or
`CUDA_VISIBLE_DEVICES`, inherited device variables removed first. Within a minute
of a backend turning healthy, the node checks with `rocm-smi` or `nvidia-smi`
that its processes hold the cards they were given, and takes a backend found on
another card out of service. Docker nodes need `pid: host` for that check;
without it the verdict is "unknown" and nothing is stopped. An `nvidia-smi`
telemetry collector joins the ROCm one, so dashboards and the energy store read
both vendors.

**Dashboards** keep their look on the new payloads. `/mesh` gains an aliases
section and backends per member, and `/chat` lists aliases beside models and
shows which node answered.

**Upgrading.** A v1 `viiwork.yaml` is refused at startup with
`v1 config: see docs/migrating-to-v2.md`. That guide maps every key — note that
`models[].context` is now **per slot** — and covers mesh modes, Docker and
systemd, large models (`startup_timeout`) and rollback.
`configs/docker-compose.v2.example.yaml` is a complete node. A clean stop drains
in-flight requests for up to `health.respawn_grace`, so give the container
`stop_grace_period: 90s`. v1 and v2 nodes do not form one mesh.

**Removed:** `server.mesh_port` and per-instance ports; `--section.key`
overrides on the command line (the config file is the only input); `balancer`
(routing is by free slots); `peers` (discovery is automatic);
`model.n_gpu_layers` and `health.evict_on_hard_failure`; the v1 packages kept
alongside alpha.1.

**Dependencies:** `hashicorp/memberlist` and `hashicorp/mdns` join
`gopkg.in/yaml.v3`. The binary links 16 modules; `go list -m all` lists 104,
most of them only in the new modules' own dependency graphs.

### Not in this release

- The vLLM and FreeToken engines follow later; `llamacpp` is the only engine.
- `scripts/setup-node.sh`, `deploy.sh`, `update.sh` and `rebuild.sh` still write
  and drive v1 layouts.

## v2.0.0-alpha.1

### Contracts for viiwork 2, no runtime change yet

The module path is now `github.com/janit/viiwork/v2`. This pre-release ships
the frozen contracts of the viiwork 2.0 design as compiled,
tested code so the engine, gateway and RouteMap work can build against them:
config v2 (`internal/config`), the engine interface (`internal/engine`), node
metadata (`mesh`), the wire types and headers (`meshapi`), and the alias table
with its merge rule.

The binary still runs v1. `cmd/viiwork` uses the v1 config and mesh packages,
moved unchanged to `internal/v1/config` and `internal/v1/meshapi`, until
2.0.0-rc.1 switches it over. Contracts change only by bumping the alpha.

## v1.8.1

### Peer polling no longer stalls routing behind dead hosts

`Registry.PollOnce` polled peers one at a time, and a peer whose host is
powered off drops packets rather than refusing the connection, so each such
peer cost a full `peers.timeout`. A round therefore took *dead peers ×
timeout* — on an 11-instance fleet with 8 hosts down and a 5s timeout, about
40s against a configured 10s `poll_interval`, and the ticker simply dropped
the ticks it could not keep up with. For that whole window every peer's
reachability, model list and in-flight count stood still. A node that had
just died kept its routes, and because its last-polled in-flight count was
frozen low while the live backends filled up, `PickRoute` *preferred* it:
the mesh converged onto the dead node, and each request sent there waited in
`proxyToPeer` on a host that was never going to answer. That is the bimodal
latency seen on multi-homed models — a steady sub-second mode and a second
mode of 9–40s stalls on 10–20% of requests — and why a host serving only
single-homed models never showed it: its requests never took a peer hop.

Peers are now polled concurrently, so a round costs about one timeout
however many peers are dark, and a discovery round skips a verified peer
whose status poll just failed rather than running the timeout out again on
its cluster poll. The forwarding client bounds the TCP handshake to a peer
at 5s while keeping its 120s overall timeout, which a streaming completion
needs; a dial to a powered-off host previously ran to the kernel's own
connect timeout. On a three-node reproduction mesh with eight blackholed
peers, peer discovery went from 50s to 15s and a wedged node's p90 from
26.9s to 0.15s.

Requests dispatched inside the detection window — between a node going dark
and the next poll noticing, floored by `poll_interval` — can still stall.
Marking a peer unreachable write-through on a failed forward and retrying on
another route would close that, and is the intended follow-up.

## v1.8.0

### Open Chat from the mesh view, pinned to a host

`/mesh` gains an **Open Chat** section: one link per model, opening `/chat`
for that model in a new tab. `/chat` reads `?model=` and `?host=` from its
URL and reflects every change back into it, so a chat state is linkable; a
**backend** selector offers `mesh` (routing as before) or any host currently
serving the model, and each reply says where it ran.

Under it, `/v1/chat/completions`, `/v1/completions` and `/v1/embeddings`
accept `?host=<hostname>`, which narrows routing to that machine — several
co-located instances count as one host and are still balanced across.
Absent or `mesh` routes exactly as before. A host that does not serve the
model is a `404` naming both, never a quiet fallback to the mesh; a malformed
value is a `400`. The value is only compared against hostnames the node
already knows and is never dialled, so it cannot reach anything normal
routing could not. The query string already travels with a forward, so the
pin holds across a hop and works from `curl`. `peer.Route` gains a `Host`
field and `peer.FilterByHost` applies the pin.

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
