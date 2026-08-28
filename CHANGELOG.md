# Changelog

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
