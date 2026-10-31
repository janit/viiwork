# Security and the Trust Model

**viiwork authenticates nothing on inference or the dashboards.** Those endpoints
are open to any client that can reach the server. Reachability *is* the authorization
model, and the fleet is expected to sit on a tailnet or another trusted network.
If you expose viiwork to an untrusted network, put a reverse proxy (Caddy, nginx)
or firewall rules in front of it.

- [What that exposes](#what-that-exposes)
- [Browser origins (CORS)](#browser-origins-cors)
  - [Cross-site writes (CSRF)](#cross-site-writes-csrf)
- [Listening address](#listening-address)

## What that exposes

- **Prompt *and response* text is readable over the API.** The history
  (`activity.prompt_history` requests per node, 1000 by default, in memory) is
  served unauthenticated like everything else. If either side of the traffic on
  your fleet is sensitive, restrict access at the network layer. There is
  currently no switch to disable the history. See
  [dashboards.md](dashboards.md#prompt-and-output-history).
- **Client names and energy.** With a `clients` section, `/v1/status` and
  `/v1/cluster` list each named client and the energy spent on it. A client is
  named by source address, so anything that can send from a named address is
  counted as that client. `X-Viiwork-Client` from a client is removed; a node
  accepts it only on a forward it has verified, and it is not covered by the
  forward's signature, so any mesh member can attach a label, as any member can
  already forward a request. A client cannot name itself only once every node
  that clients call runs v2.10 or later: a node still on v2.9.x does not know
  the header, so it does not strip it from a client's request and copies it
  onto its signed forward, where a v2.10 node honours it. Add `clients` only
  after that upgrade. In an open mesh a verified forward is the source
  address of an alive member, so any process on a member machine can name
  itself.
- **Aliases change what every client gets.** In a secured mesh a write needs the
  mesh secret's signature; in an open mesh it is accepted only from the node's own
  machine. A reverse proxy on that machine in front of the API would make every
  request look local, so put one only in front of a secured mesh. See
  [mesh.md](mesh.md#aliases).
- **Membership is the mesh secret's job.** Without a secret, any viiwork node that
  reaches 7946 joins — which on a tailnet means the tailnet's ACLs are the
  control. See [mesh.md](mesh.md#secured-and-open).
- **Chassis power control is off by default and has no wildcard.** The allowlist
  is the whole authorization story; anyone who can reach the API can use what it
  names. See [power-and-energy.md](power-and-energy.md#power-control).

Two endpoints are deliberately narrowed, because an unauthenticated API on a
network that also carries IPMI would otherwise be a probe primitive. Writes that
change the fleet (aliases, updates, parking) are separate: a signature in a
secured mesh, a local program in an open one. `/v1/kvshare/*` accepts members
only.

- **`/v1/mesh/prompt` only forwards to mesh members.** The `addr` parameter is
  checked against the API addresses of alive members before anything is fetched.
- **`?host=` is compared, never dialled.** The value only filters routing
  candidates by node name; it never becomes an address. The same holds for
  the names in `?prefer=` and `X-Viiwork-Prefer`, and that header is removed
  before a request goes to a member or an engine.

**kv_share replicas are conversations on disk.** A replica is a session's
prompt, kept as files in `kv_share.dir` and `slot_save_path` on every member of
its group. See [models.md](models.md#sharing-sessions-between-hosts-kv_share).

Mesh headers are stripped from every incoming request right after verification,
so no client can smuggle a forwarding claim through and no engine ever sees one.

## Browser origins (CORS)

Server-side callers — curl, a backend proxying on behalf of its own UI — are
unaffected by any of this. It matters only when a page served from somewhere else
fetches viiwork directly from the browser.

Because viiwork authenticates nothing, an origin allowlist is not protecting the
API from anyone who can already reach it. What it stops is a page in some browser
on your network quietly driving your fleet through that browser's network
position. Treat the list as a real control and keep it short:

```yaml
api:
  cors:
    allow_origins: ["*.tail1234.ts.net", "localhost", "127.0.0.1", "*.your-app.example"]
    allow_tailnet_ips: true   # also 100.64.0.0/10 and fd7a:115c:a1e0::/48
```

`*.example.com` matches subdomains only, never the bare apex; every other entry
must match the host exactly. `allow_origins: []` sends no CORS header at all,
which is how viiwork behaved before v1.1.0.

**Leave `allow_origins` out and the node derives it** at startup: it asks
tailscaled (over `mesh.tailnet.socket`) for this tailnet's MagicDNS domain and
allows `*.<that domain>` — `*.tail1234.ts.net`, say — plus `localhost`,
`127.0.0.1` and, unless `allow_tailnet_ips: false`, tailnet IP literals. The log
line `api.cors: allowing browser origins …` shows the result. If tailscaled does
not answer, the node allows only the local origins and IP literals and logs how
to add the domain by hand. An explicit list, the empty one included, is used
exactly as written and tailscaled is not consulted.

Older releases shipped `*.ts.net` as the default. **Do not write it back**:
`ts.net` is every Tailscale customer's domain, and Tailscale Funnel serves
arbitrary public pages from it, so `*.ts.net` trusts pages anyone on the
internet can publish. Your own application's origin is deployment-specific: add
it in your `viiwork.yaml`, never in the repo's defaults.

Two behaviours are pinned by tests: **SSE streams carry the header on their own
GET response** (`EventSource` sends no preflight), and **`OPTIONS` is answered
before routing**, with a refused preflight returning 403.

Where the consumer has a backend of its own, **prefer a server-side proxy over
CORS**: it needs no allowlist entry, and it can put authentication in front of an
API that has none. [api-integration.md](api-integration.md) covers both modes.

### Cross-site writes (CSRF)

CORS decides which pages may *read* a response; it does not stop a browser from
*sending* one. A POST with a `text/plain` or form body is a "simple request"
that goes out with no preflight, so through v2.3.1 any web page a tailnet
member opened could switch a host off with
`POST /v1/mesh/power {"host":"gb2","action":"off"}` through that member's
browser. Two guards now run before routing:

- **A write from a refused origin is 403.** A `POST`, `PUT`, `PATCH` or
  `DELETE` carrying an `Origin` header that is neither the node's own (the
  dashboards, same host and port) nor allowed by `api.cors` is refused — on every
  endpoint, inference included, so a foreign page cannot spend GPU time either.
  With `allow_origins: []` only the node's own pages may write. Clients that are
  not browsers send no `Origin` and are unaffected: curl, SDKs, the alias CLI,
  member forwards and pipeline steps.
- **Control writes must be JSON.** `/v1/power`, `/v1/mesh/power`, every
  write under `/v1/aliases/` (set, delete, revert), every write under
  `/v1/update/` and `POST /v1/models/down|up` require
  `Content-Type: application/json`, else 415. A browser cannot send that
  cross-origin without a preflight, and a refused preflight is 403. The
  `viiwork alias` CLI sends it on bodiless deletes and reverts too. A write
  with no body and no `Content-Type` is let through, so an older CLI's
  `delete` and `revert` keep working against an upgraded node; a browser
  cannot use that gap, since a cross-origin write always carries `Origin`.

What neither guard can stop is DNS rebinding — a page that points its own name
at a node's address is same-origin by every check a server can make. The
answer to that is the same as to everything else here: keep the API off
networks whose browsers you do not trust.

## Listening address

`api.host` defaults to `0.0.0.0`, every interface: local health checks and the
compose examples expect it. With host networking that includes every LAN, VPN
and public interface the machine has, and the API behind them authenticates
nothing. The node logs `api: listening on every network interface …` at startup
when that is the case. To narrow it, set `api.host` to the machine's tailnet IP
(or its LAN address) — the node's own callers, pipeline steps included, follow
the address the listener actually bound. Health checks then have to use that
address instead of `localhost`.
