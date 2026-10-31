# The Mesh

Nodes form one mesh without peer lists. Switch a machine on and its models are
usable from every node within seconds; switch it off and routing stops sending
to it first, while membership follows.

- [Discovery](#discovery)
- [Secured and open](#secured-and-open)
- [Routing](#routing)
- [Aliases](#aliases)

## Discovery

```yaml
mesh:
  network: tailnet     # tailnet (default) | lan
  seeds: []            # optional "ip:7946" of any member
```

- **`tailnet`:** a node advertises its Tailscale address and finds the other
  online machines through tailscaled's local API
  (`/var/run/tailscale/tailscaled.sock`). Offline devices are never dialled.
- **`lan`:** a node advertises the address of its default-route interface and
  finds others with mDNS (`_viiwork._tcp`).
- **`seeds`:** addresses to join where discovery cannot reach.

Discovery repeats every `mesh.rejoin_interval` (60 s), which is also how a healed
network split merges back. Gossip binds the advertise address only, never
`0.0.0.0`.

**Two fixed ports on every machine.** The API and every dashboard are on 8086
tcp; membership gossip is on 7946, **tcp and udp**. Machines must reach each
other on both. `viiwork-accept ports` probes this before you convert a host —
see [operations.md](operations.md#acceptance-checks).

## Secured and open

- **Secured:** set `VIIWORK_MESH_SECRET` on every machine. Gossip is encrypted
  and authenticated, a node without the secret cannot join, forwards between
  nodes are signed, and alias writes need a signature.
- **Open:** leave it unset and set `mesh.open: true`. Gossip is plaintext and any
  viiwork node that can reach 7946 joins. Alias writes are accepted only from the
  node's own machine.

A node with neither refuses to start and names the variable it looked for; a node
started in the wrong mode logs `mesh mode mismatch` and stays out. An open mesh
gains a secret without downtime in three fleet-wide steps
(`mesh.secret_enforce: none`, `outgoing`, `full`), described in the
[migration guide](migrating-to-v2.md#5-mesh-mode).

Membership is the mesh secret's job. Without a secret, any viiwork node that
reaches 7946 joins — which on a tailnet means the tailnet's ACLs are the control.
See [security.md](security.md).

## Routing

A request for a model goes to a host with a free slot, chosen by measured
speed. When no host has one it waits in a FIFO queue on the node that received
it for up to `routing.queue_timeout` (20 s), then gets 429 with
`Retry-After: 2`.

- **Images and saved sessions come first.** A request with an image goes only
  to hosts that read images
  ([models.md](models.md#models-that-read-images)), and a session with a saved
  replica prices that host warm
  ([models.md](models.md#sharing-sessions-between-hosts-kv_share)).
- **Routing follows measured speed** (`routing.performance`, on by default).
  Every node measures its own time to first token per model and publishes the
  score on `/v1/capacity`. For a request, each host with a free slot gets a
  predicted time from its score and the prompt's size, and only hosts within
  1.25 times the best count. Among those the node that received the request
  runs it when it is one of them; otherwise the faster a host, the more often
  it is chosen. A host with no score yet is priced at the fleet's median and
  gets one request in twenty, so it can earn one.
- **A session stays on one host.** A request carrying `X-Session-Affinity` or
  `X-Session-Id` goes to the same host among those that count, whichever node
  it enters by, and to the same backend on that host while it has a free
  slot, so a conversation's turns reuse one warm prompt cache. A coding agent
  should send one of the two headers with a value that is stable for the
  session.
- **With no score anywhere, or `routing.performance: false`,** a request goes
  to a local backend with a free slot, else to the member with the most free
  slots. With no scores a session header still picks one host; with the key
  set to false it is ignored.

- **Capacity is polled, membership is gossiped.** Every node polls every alive
  member's `/v1/capacity` once a second. A report older than
  `routing.stale_after` (3 s) is ignored for routing, while `/v1/models` still
  lists its models so one late poll does not make a model flicker out of client
  lists.
- **A forward refused before its first byte** (429, 503, no connection) is retried
  on another route, and marks that member-and-model full until a fresher report
  arrives. Retries happen only before the first byte.
- **The receiver is strict.** A node that receives a forward admits it only into a
  free local slot, else 429 at once. Forwards are never queued and never
  forwarded again, so requests cannot bounce between nodes.
- **`?host=<node name>` pins a request to one machine.** The value only filters
  candidates by name; it is never dialled as an address, and a forwarded request
  ignores it.
- **`?prefer=<node>,<node>` asks for machines in order, when they are free.**
  The first named node with a free slot for the model runs the request, ahead
  of measured speed and of the session rule. When none of them has room the
  request routes as if the list were absent, so a preference never fails or
  delays a request; a queued request walks its list again each time a slot
  frees. A name the mesh does not know, or a node that is off, is passed
  over. `X-Viiwork-Prefer` carries the same list for a client that can set a
  header but not a query parameter. Like the pin, the names are only compared,
  and a forwarded request ignores them.
- **A forward carries the client's name.** A forward also carries
  `X-Viiwork-Client`, the client's name as the entry node resolved it, when
  that node has a `clients` section. The receiver uses it for energy per client
  and passes it no further.

## Aliases

An alias is a stable, mesh-wide name for a real model: point clients at
`stable-coder`, and switch which model it means once, from any machine.

```bash
viiwork alias set stable-coder Qwen3.8-27B --fallback granite-4.2-8b
viiwork alias ls
viiwork alias history stable-coder
viiwork alias revert stable-coder      # swap back to the previous version
viiwork alias rm stable-coder
viiwork alias export > aliases.json    # an off-fleet copy
viiwork alias import aliases.json
```

`set`, `rm` and `revert` report how many members have the change
(`stable-coder -> Qwen3.8-27B (ver 6): 9/9 alive members`); a count below the
member total shows a lagging node or a split network. The CLI talks to the node
on the same machine (`--node host:port` for another) and, in a secured mesh,
signs with the secret named by `mesh.secret_env`.

**Resolution happens once, on the node that receives the request:** to the
alias's target when any member serves it, else to the first served fallback, else
503 with `Retry-After: 5`. A target that is served but full queues rather than
falling back. A real model name always wins over an alias of the same name.

Responses carry `X-Viiwork-Alias` and `X-Viiwork-Model`, and
[`/mesh`](dashboards.md#mesh-dashboard) shows every alias's state — `shadowed`
and `unavailable` are highlighted. Every node persists the table in
`node.state_dir`, so a machine that was off serves its last table and catches up
on its first sync.

**A pipeline cannot be an alias target**, and `--force` does not change that. A
pipeline is node-local — absent from capacity reports, dispatched only on the
origin node — so an alias pointing at one would dangle on every node but the one
that configures it. A pipeline name is refused as an alias *name* too, like a
real model's.
