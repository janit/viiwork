# Sizing a consumer's concurrency from the fleet

How an application decides how many requests to have in flight against viiwork,
using `/v1/fleet/capacity` instead of a number in its config.

Written for routemap4, which is the concrete example throughout, but nothing
here is specific to it.

## The problem this solves

A consumer's concurrency is usually a constant someone chose once:

```
LLM_BACKEND_TRANSLATE_URL=http://gb1:8086
LLM_BACKEND_TRANSLATE_MODEL=translategemma-27b-it
LLM_BACKEND_TRANSLATE_CAPACITY=4
```

On 2026-09-13 that model had **36 slots** across gb1, gb2 and gb3. The constant
said 4.

The shortfall is worse than the arithmetic suggests. **viiwork fills a node's
own slots before forwarding to a peer**, and gb1 alone has 12. Four requests in
flight never cross that threshold, so gb2 and gb3 are never asked for anything.
24 slots were idle by construction — no amount of load would have reached them.

And `/v1/capacity`, the endpoint a consumer naturally reaches for, cannot tell
you this: it is node-local, so gb1 answers `12`, not `36`.

## What the endpoint gives you

```
GET /v1/fleet/capacity?model=translategemma-27b-it
```

```json
{
  "view": "teddy",
  "stale_after_s": 3,
  "models": [{
    "name": "translategemma-27b-it",
    "slots": 36, "busy": 2, "free": 34, "queued": 0, "ctx": 4096,
    "hosts": [
      {"node":"gb1","api":"100.95.94.105:8086","slots":12,"busy":2,"free":10,"ctx":4096,"age_ms":950},
      {"node":"gb2","api":"100.91.141.75:8086","slots":12,"busy":0,"free":12,"ctx":4096,"age_ms":880},
      {"node":"gb3","api":"100.98.152.73:8086","slots":12,"busy":0,"free":12,"ctx":4096,"age_ms":910}
    ]
  }]
}
```

Two fields carry the integration: **`slots`** is the depth to size to, and
**`hosts[].api`** is where to send.

A host viiwork has lost sight of is still listed, with `"stale": true`, its
`age_ms`, and **no numbers at all**. That is deliberate: absent is not zero. It
lets you tell "the fleet is smaller" from "we cannot see gb3 right now", which
are different operational events. Treat a stale host as capacity you do not
have, and log it — do not treat it as a host with zero slots.

## The integration, in five parts

### 1. Poll on a cache, not per request

**Every 5 minutes.** This is not a hot path and must not become one. One fetch
per model per process per 5 minutes is the intended load; the platform will not
rate-limit you, so the discipline has to live here.

Cache the parsed result with that TTL. Serve every concurrency decision from
the cache. A request must never trigger a fetch.

### 2. Damp the changes

The 5-minute TTL is itself the main damping — it is why a host bouncing does not
make your worker depth oscillate. Add two cheap rules on top:

- **Ignore small changes.** Only re-size when the new depth differs from the
  current one by more than a few slots (say 10%, minimum 1). A fleet gaining and
  losing one backend should not churn your pool.
- **Grow fast, shrink slow.** Apply an increase immediately. Apply a decrease by
  letting in-flight work drain rather than cancelling it — you are lowering a
  ceiling, not revoking permission for work already accepted.

### 3. Keep the configured value as the floor

**This is the part that must not be skipped.** `LLM_BACKEND_*_CAPACITY` stays in
the config and changes meaning: it stops being *the* number and becomes the
**fallback and floor**.

Use it whenever the fleet view is unusable:

| Condition | Action |
|---|---|
| Endpoint unreachable, times out, or 5xx | fall back to configured capacity |
| **404 — node too old to serve it** | fall back to configured capacity |
| Model absent from `models[]` | fall back to configured capacity |
| Every host for the model is `stale` | fall back to configured capacity |
| `slots` resolves below the configured value | use the configured value |

The consumer is then never *less* capable than it is today, and viiwork is
allowed to fail without taking the consumer down with it. A capacity probe that
can take production down is worse than a constant.

### 4. Spread ingress across the hosts

A single URL works — viiwork forwards — but that node then carries every
request body and every SSE stream for the whole fleet.

This is where routemap4's existing design pays off. Its semaphore is already
**per base URL**, which is exactly the shape `hosts[]` returns. So the mapping
is direct:

```
urls     = hosts[].api        (filtered to !stale)
capacity = that host's slots  (per URL, not divided)
```

Three hosts at 12 slots each becomes three URLs at capacity 12 — 36 in flight,
spread evenly, no forwarding hop for most requests. That is the configuration
the existing code was built for and could not previously discover.

Keep `LLM_BACKEND_*_URL` as the **probe address and fallback URL**: the host you
ask, and the one you use when the list is unusable.

### 5. Let the queue handle the last mile

A 5-minute-old view will sometimes be wrong — a host dies at minute one. That
is survivable, and it is why none of this needs reservations or leases.

viiwork already has backpressure: a FIFO queue at the origin bounded by
`routing.queue_timeout`, 429 when a forward hits a full node, and 503 with
`Retry-After: 5` when nothing serves the model. Transient oversubscription
becomes **latency, not errors**.

So size to fleet capacity and let the queue absorb the rest. Do not subtract a
safety margin "to be safe" — self-limiting below what the fleet can take is how
a consumer ends up at 11%, and the platform is built to absorb exactly the
overshoot you would be protecting against. Do handle 503 with `Retry-After` as
a retry signal rather than an error.

## Rollout order matters

**The endpoint exists only on viiwork v2.2.0-beta2 and later.** An older node
returns **404** — verified on 2026-09-13: gb1 on v2.0.0 answers 404 while a
beta2 node answers 200.

So on a mixed fleet, either:

- point the probe at a node already upgraded, or
- ship the consumer change first and let it sit on its configured fallback until
  the node it asks is upgraded.

Both are safe, because 404 is in the fallback table. Ship the consumer change
whenever you like; it degrades to today's behaviour until the platform catches
up. Nothing has to be sequenced.

## What not to do

- **Do not poll per request.** 5 minutes, cached.
- **Do not divide `slots` between your workers by hand.** The number is already
  the fleet's true depth; dividing it recreates the problem this replaces.
- **Do not treat a stale host as zero-capacity.** It has no numbers on purpose.
  Exclude it and log it.
- **Do not remove the config value.** It is the floor, and the reason a platform
  outage is not a consumer outage.
- **Do not assume your view is the fleet's.** `view` names the node that
  answered. A partitioned node reports less; that is honest, not a bug. If it
  matters, ask two nodes and take the larger.

## Checklist for routemap4

- [ ] A capacity probe module: `GET {url}/v1/fleet/capacity?model={model}`, 5-minute
      TTL cache, per model.
- [ ] The fallback ladder above, defaulting to `LLM_BACKEND_<ID>_CAPACITY` in
      every failure case including 404.
- [ ] `ResolvedBackend.urls` populated from non-stale `hosts[].api`, with
      `LLM_BACKEND_<ID>_URL` as probe address and fallback.
- [ ] `ResolvedBackend.capacity` set per URL from that host's `slots`.
- [ ] `reserved` (the interactive slice) recomputed against the new capacity, so
      it stays a proportion rather than an accidental majority when the fleet
      shrinks.
- [ ] Re-size damping: 10% threshold, grow immediately, shrink by draining.
- [ ] 503 + `Retry-After` handled as a retry signal, not a failure.
- [ ] A log line whenever the effective capacity changes, naming the old value,
      the new one, and whether it came from the fleet or the fallback. Without
      it, "why is it running 36 wide today" is unanswerable.
