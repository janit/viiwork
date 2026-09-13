# Sizing a consumer's concurrency from the fleet

How an application decides how many requests to have in flight against viiwork,
using `/v1/fleet/capacity` instead of a number in its config.

Written while wiring up the first consumer, so the examples are concrete, but
nothing here is specific to that consumer. Hosts are named `node-a`, `node-b`,
`node-c` throughout, and the answering node `node-d` — a node need not serve a
model to report the fleet's capacity for it.

## The problem this solves

A consumer's concurrency is usually a constant someone chose once:

```
TRANSLATE_URL=http://node-a:8086
TRANSLATE_MODEL=translategemma-27b-it
TRANSLATE_CAPACITY=4
```

At the time that was written the model had **36 slots** across node-a, node-b
and node-c. The constant said 4.

The shortfall is worse than the arithmetic suggests. **viiwork fills a node's
own slots before forwarding to a peer**, and node-a alone has 12. Four requests
in flight never cross that threshold, so node-b and node-c are never asked for
anything. 24 slots were idle by construction — no amount of load would have
reached them.

And `/v1/capacity`, the endpoint a consumer naturally reaches for, cannot tell
you this: it is node-local, so node-a answers `12`, not `36`.

## What the endpoint gives you

```
GET /v1/fleet/capacity?model=translategemma-27b-it
```

```json
{
  "view": "node-d",
  "stale_after_s": 3,
  "models": [{
    "name": "translategemma-27b-it",
    "slots": 36, "busy": 2, "free": 34, "queued": 0, "ctx": 4096,
    "hosts": [
      {"node":"node-a","api":"100.64.0.11:8086","slots":12,"busy":2,"free":10,"ctx":4096,"age_ms":950},
      {"node":"node-b","api":"100.64.0.12:8086","slots":12,"busy":0,"free":12,"ctx":4096,"age_ms":880},
      {"node":"node-c","api":"100.64.0.13:8086","slots":12,"busy":0,"free":12,"ctx":4096,"age_ms":910}
    ]
  }]
}
```

Two fields carry the integration: **`slots`** is the depth to size to, and
**`hosts[].api`** is where to send.

### `?model=` accepts an alias (from beta3)

An alias is a name for capacity, so it names capacity here too: from
v2.2.0-beta3, `?model=stable-translate` answers with the capacity of whatever
that alias currently resolves to. Configuring the consumer with the alias —
which is what we recommend, since it is the whole point of aliases — works.

The response reports the **real** model in `name`, and echoes the alias in a
`resolved_from` field, present only when an alias was actually resolved:

```json
{"view":"node-d","resolved_from":"stable-translate",
 "models":[{"name":"translategemma-27b-it","slots":36, ...}]}
```

That matches inference, where a request through an alias also reports the real
model in the response body. Match the response against `name`, not against the
name you sent.

An alias whose target nothing currently serves comes back `200` with an empty
`models[]`, not `503`: it has exactly zero capacity, which the empty list
already says, and a powered-down target is a normal operating state rather than
an outage. It therefore lands in the "Model absent from `models[]`" row below.

**Against a beta2 node an alias returns an empty `models[]`** — beta2 matched
`?model=` as an exact string and never consulted the resolver. That is the same
fallback row, so an older node degrades to your configured capacity rather than
erroring; it is silent, so log which branch you took (see the checklist).

A host viiwork has lost sight of is still listed, with `"stale": true`, its
`age_ms`, and **no numbers at all**. That is deliberate: absent is not zero. It
lets you tell "the fleet is smaller" from "we cannot see node-c right now", which
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

**This is the part that must not be skipped.** The capacity constant stays in
the config and changes meaning: it stops being *the* number and becomes the
**fallback and floor**.

Use it whenever the fleet view is unusable:

| Condition | Action |
|---|---|
| Endpoint unreachable, times out, or 5xx | fall back to configured capacity |
| **404 — node too old to serve it** | fall back to configured capacity |
| Model absent from `models[]` (including an alias nothing serves, or any alias against a pre-beta3 node) | fall back to configured capacity |
| Every host for the model is `stale` | fall back to configured capacity |
| `slots` resolves below the configured value | use the configured value |

The consumer is then never *less* capable than it is today, and viiwork is
allowed to fail without taking the consumer down with it. A capacity probe that
can take production down is worse than a constant.

### 4. Spread ingress across the hosts

A single URL works — viiwork forwards — but that node then carries every
request body and every SSE stream for the whole fleet.

If your client already limits concurrency **per base URL** — most connection
pools and semaphores do — the mapping is direct, because that is exactly the
shape `hosts[]` returns:

```
urls     = hosts[].api        (filtered to !stale)
capacity = that host's slots  (per URL, not divided)
```

Three hosts at 12 slots each becomes three URLs at capacity 12 — 36 in flight,
spread evenly, no forwarding hop for most requests. Note the per-URL figure is
that host's `slots`, **not** the fleet total: setting the fleet total on each of
three URLs asks for 108.

Keep the configured URL as the **probe address and fallback URL**: the host you
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
returns **404**, and a mixed fleet is normal — machines are upgraded one at a
time. (Alias support in `?model=` needs beta3; against beta2 an alias lands in
the "model absent" fallback row above.)

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
