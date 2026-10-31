# Power, Cost and Energy

Four related features, in increasing order of what they need from the machine:

| Feature | Answers | Needs |
|---|---|---|
| [Fleet power](#fleet-power) | What is the fleet drawing *now* | Nothing, or `/dev/ipmi0` for whole-chassis figures |
| [Cost tracking](#cost-tracking) | What is that costing *now* | An ENTSO-E API key |
| [Energy history](#energy-history) | Where did the kilowatt-hours *go* | A directory that outlives the container |
| [Power control](#power-control) | Switch a chassis on or off | An allowlist, and BMC credentials for powered-off hosts |

**Accuracy is ±15% on absolute watts** throughout. Compare models within a
machine freely, compare across machines with care, and never present any of it as
billing grade.

## Fleet power

The Fleet Power panel on [`/mesh`](dashboards.md#mesh-dashboard) needs no
configuration — it is drawn from the cluster snapshots the dashboard already
receives, so it adds no polling and no extra request. A machine appears in it as
soon as it can read its own power: from the BMC when the node has access to it,
else from the sum of its GPUs' board power.

```yaml
    devices:
      - /dev/ipmi0:/dev/ipmi0
```

Hosts without it are counted in the "n/m hosts reporting" line but contribute no
band, so a host that simply cannot be measured is never mistaken for a host
drawing nothing. The RAM strip below needs no BMC at all — it reads
`/proc/meminfo`, so every host appears in it.

Four things are worth knowing before reading numbers off it:

- **Wattage is per machine.** A BMC reading covers the whole chassis; a GPU sum
  covers only the cards. The table names which one each machine reports.
- **The window is since you opened the page**, capped at 720 readings. It is a
  live view, not history: a reload starts it over, and a halt leaves a gap rather
  than drawing a straight line across the pause. Durable per-host, per-model kWh
  is a separate feature — see [Energy history](#energy-history).
- **±15% on absolute watts.** Compare hosts and watch trends freely; do not bill
  anyone from it. The table names which source each host settled on (`dcmi`,
  `sdr`, `sensor:<name>`, or `rocm-smi`/`nvidia-smi` for a GPU sum).
- **A reading that stops refreshing goes absent, not flat.** When `ipmitool` or
  the GPU tool stops answering, the last good value counts for three sample
  periods (the observed `health.interval` tick, at least 5 s each) and is then
  reported unavailable until a read succeeds again. Status, cost and energy
  history all see "cannot say" for the outage rather than the last wattage
  repeated; a GPU whose samples stop drops out of `/v1/status` the same way.

If a host has the BMC device but still reports nothing, the probe found no source
that answers with a non-zero wattage — which is a real hardware answer, not a
bug: some boards expose the `Power Supply` sensor class as presence flags with no
watts. Startup logs name what was tried and what was adopted, and `power.source`
pins it if `auto` picks the wrong one:

```yaml
power:
  source: auto     # or dcmi | sdr | sensor:<NAME> | none
```

## Cost tracking

Real-time electricity cost per node, from ENTSO-E day-ahead spot prices.

1. Get an API key from the
   [ENTSO-E Transparency Platform](https://transparency.entsoe.eu/)
2. Put `ENTSOE_API_KEY=your-key-here` in the node's environment:
   `/etc/viiwork/mesh.env` with the compose example or the systemd unit, or a
   `.env` file in the directory the node runs from
3. Add a `cost` section to `viiwork.yaml` (see the example config) for your
   bidding zone and tariffs

The dashboards show per-node cost rate (EUR/h), daily accumulated cost and
cluster totals.

## Energy history

Cost tracking answers *what is this costing right now*. The energy store answers
*where did the kilowatt-hours go* — a durable per-host, per-model history of node
draw from IPMI and per-GPU draw from `rocm-smi`, kept for a year.

It is off by default, because it needs a directory that outlives the container:

```yaml
energy:
  enabled: true
  dir: /var/lib/viiwork/energy
  sample_interval: 30s   # 2x the BMC refresh; records are always 1/minute
```

```yaml
    devices:
      - /dev/ipmi0:/dev/ipmi0
    volumes:
      - /var/lib/viiwork/energy:/var/lib/viiwork/energy
```

Disk is fixed at creation and cannot grow: about 2.6 MB for a 10-GPU host, 660 KB
for two. Three preallocated ring files per series hold a day at one-minute
resolution, a year at one hour, and a year of daily totals; **retention is the
wrap**, so there is no purge job and a restart needs no recovery.

The node covers every GPU the machine reports, and takes each card's model from
its own `models[].gpus`, so one recorder produces a per-model split for the whole
box and a reload's layout is used at once. On a machine with no BMC, node power is
the sum of GPU board power (labelled `rocm-smi` or `nvidia-smi`) and every watt is
attributed to the cards directly.

Power is attributed **marginally**: each GPU is charged a share of node power in
proportion to how far it sits above its idle floor, and the baseline a host draws
just by being switched on (fans, CPU, idle cards, PSU losses) is reported
separately rather than smeared across models. Baseline plus every share equals
measured node power, so no total is invented.

Sampling is deliberately faster than recording. The BMC lags load and a
tensor-split pair's cards alternate between low and high draw, so only a mean over
the bucket is honest.

The on-disk format is a contract, specified byte by byte in
[energy-store-format.md](energy-store-format.md) — including how to read a store
without the Go package.

## Energy per client

The energy history says where the kilowatt-hours went, per machine and per
model. It can also say who used them. This is off until you name a client.

```yaml
clients:
  factory-a: [100.64.0.10]
  batch-b:   [100.64.0.20, 198.51.100.0/24]
```

A client is named by the address its requests come from, IPv4 or IPv6, a
single address or a prefix; the most specific prefix wins. A name is lower
case letters, digits, `.`, `_` and `-`, starts with a lower case letter or
digit, is at most 32 characters, and `other` is reserved. The same prefix under two names stops the node from starting. The
address is the one the connection came from: `X-Forwarded-For` is not read,
so a client behind a reverse proxy is seen as the proxy.

Write the section on **every node that clients call directly**. A node that
only receives forwards needs none: the name travels with the forward, in
`X-Viiwork-Client`. A client cannot send that header itself; it is dropped.

Each node that ran the work then publishes `client_energy` on `/v1/status`
and `/v1/cluster`: cumulative watt-hours per client.

What the figure is:

- **A model's energy, split by busy slot-seconds.** Two clients that held a
  model's slots for 45 and 15 seconds of a minute get three quarters and one
  quarter of what the model's cards drew above their idle floor in that
  minute. Tokens are recorded beside it and take no part in the split.
- **Not baseline.** What a machine draws for being switched on is in nobody's
  figure. On a host that measures each GPU board and has no BMC there is no
  baseline, so the cards' idle draw while a client was being served is in
  that client's figure.
- **Not energy nobody caused.** A model that draws above its floor with no
  request running (loading, warming up) is recorded against nobody.
- **Per node.** There is no mesh total, because a sum over machines that are
  switched off and on goes down, and a counter must not. A consumer keeps one
  counter per node.
- **Reading it.** Ignore a node's `w_1m` when its `through` is more than about
  three minutes old: the node has stopped recording, and the reading is not
  current. If no `client_energy` appears anywhere, the node you poll
  `/v1/cluster` through may be on an older version, which drops fields it does
  not know when it re-encodes member status.
- **About 60 to 90 seconds behind**, and within about 15% in absolute terms,
  like the rest of the store. Not billing grade.

Requests from an address no name matches are the client `other`. So are the
steps of a pipeline, which the node sends to itself (see Known limits below). If a client's figure is
unexpectedly zero, look at `other`: the node it calls probably has no mapping
for its address.

Known limits:

- **A request to a pipeline is not charged to the client that made it.** The
  pipeline's steps call back into the node from the node's own address, so
  they are counted as `other`, or under whichever name lists the node's own
  address. This is the one statement of the limit; other places point here.
- **Mixed versions.** A client cannot name itself only once every node that
  clients call runs v2.10 or later. A node still on v2.9.x does not know
  `X-Viiwork-Client`: it does not strip it from a client's request and copies
  it onto its signed forward, and a v2.10 node that executes the request
  honours it. Add `clients` only after every such node is upgraded.
- **Work already running is not in the ledger.** Unlabelled requests that were
  already running when a node first starts counting are not known to it, so
  the first labelled client carries their share until they end. This happens
  once per node.
- **Rolling back below v2.10.** Remove `clients`, `clients_reload`,
  `energy.clients` and `energy.client_lanes` from the config first: an older
  binary refuses a file with keys it does not know.
- **A restart splits a minute.** For the minute that spans a restart,
  `kwh_24h` and `kwh_30d` (read from the rings) can differ slightly from
  `wh_total` (the counter).
- **In an open mesh, a verified forward means the source address of an alive
  member.** Any process on a member machine can therefore name itself as any
  client. This is the mesh's existing trust model, and it affects accounting
  only.
- **A restart loses the rest of the minute.** At shutdown the minute in
  progress is split and counted once, so a restart inside the same minute does
  not count the remainder of it. Figures are low, never high.

It is optional three ways:

| Setting | Default | Effect |
|---|---|---|
| no `clients` section anywhere | | Nothing is labelled, recorded or published. |
| `energy.clients: false` | `true` | This node records and publishes nothing per client, whatever labels reach it. It still labels its own forwards if it has a `clients` section. |
| `clients_reload: true` | `false` | A changed `clients` section is applied on reload. Without it the change waits for a restart, like every section but `models`. |

A node needs the energy store (`energy.enabled: true`) to take part. A node
without one, or on a version before v2.10.0, contributes nothing, so during an
upgrade the figures are low. They are never high.

`energy.client_lanes` (8) is how many client and model pairs one minute can
hold on a node. With more, the smallest are recorded together as `other`.
Changing it discards the per-minute and per-hour client history; the
cumulative counters are kept.

If the counters file cannot be read it is moved aside as `client-totals.bad`
and counting starts again, with a new `since`. If it cannot be moved aside or
saved, for example on a full disk, the node still starts, logs the fault and
counts in memory; a file it could not read is never overwritten.

Anyone who can reach the mesh can read every client's name and energy, as
they can already read every model and every host's draw.

## Power control

Each host in the Fleet Power table can carry a power button. It is **off by
default** and there is no wildcard — only hosts you name can be targeted:

```yaml
power:
  control:
    enabled: true
    hosts: [node-a, node-b, node-c]
```

Hosts are node names. That much works immediately for hosts that are **running**:
the node on a machine controls it in-band through `/dev/ipmi0`, with no
credentials, and a request is forwarded to the member of that name.

A host that is **powered off** has no node to ask, so its BMC has to be reached
over the network. That needs credentials, and without them a host can be switched
off but not back on — the dashboard shows a disabled button saying so rather than
one that fails:

```yaml
    bmc:
      username: admin
      password_env: BMC_PASSWORD    # set in .env, not in the config file
      addresses:
        node-a: 192.0.2.65          # optional; see below
```

Addresses are optional per host. A node discovers its own BMC address in-band and
shares it, so a host seen online at least once needs no entry — which also means
a learned address cannot go stale the way a written one does when BMCs are on
DHCP.

The password is handed to `ipmitool` in `IPMI_PASSWORD` with `-E`, set on that
one command only, never as a `-P` argument: argv is readable by every process on
the host through `/proc/<pid>/cmdline`, and the containers run with `pid: host`.

Three things guard it, and **none of them is authentication** — viiwork has none,
and this does not add any:

- **The allowlist.** A host you did not name cannot be targeted, by the UI or by
  curl.
- **A node will not power off its own host.** Doing so would destroy the answer to
  the request and the dashboard asking it. Its button is disabled and the server
  refuses it too — open another node's `/mesh` to control that host.
- **A confirmation prompt** naming the host and the action.

Anyone who can reach the API can use it. That is the same trust model as the rest
of viiwork (see [security.md](security.md)), but the consequence is larger, so
keep the allowlist to the hosts you actually want reachable this way.
