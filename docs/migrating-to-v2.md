# Migrating to viiwork 2

viiwork 2 refuses a v1 `viiwork.yaml` at startup with
`v1 config: see docs/migrating-to-v2.md`. This page is where that points: what
changed, how several v1 instance files become one v2 file, and how to run and
roll back a converted machine.

## 1. What changes

- **One node per machine.** v1 ran one viiwork instance per model, each on its
  own port. v2 runs one process per machine, and that process supervises every
  model on it. Its `models:` list replaces the instance files.
- **Two fixed ports, the same on every machine.** The API and every dashboard
  are on **8086**, and membership gossip is on **7946** (tcp and udp). The
  per-family port schemes and `server.mesh_port` are gone.
- **A mesh that forms itself.** Nodes find each other through tailscaled
  (`mesh.network: tailnet`, the default), mDNS on a LAN (`mesh.network: lan`),
  or `mesh.seeds`. You no longer write `peers.hosts` lists, and adding a machine
  means writing that machine's config and nothing anywhere else. A machine that
  stops is dropped from routing within seconds.
- **Explicit model names.** v1 derived a model's id from its GGUF file name. v2
  serves exactly `models[].name`, which is what clients send.
- **Context is per slot.** v1 `model.context_size` was llama.cpp's total,
  divided across slots. v2 `models[].context` is the context of **one slot**,
  for every engine.
- **Slot-aware routing.** A request goes to a local backend with a free slot,
  else to the member with the most free slots, else it waits in a short queue on
  the node that received it.
- **Aliases.** A stable name such as `stable-coder` can point at a real model
  mesh-wide and be switched once, from any machine, with `viiwork alias set`.

## 2. One file per machine

List every v1 instance file on the machine. Each one becomes one entry under
`models:` in a single v2 file:

- `model.path` becomes the entry's `path`, and you choose its `name`;
- the instance's GPUs become the entry's `gpus`, as host GPU indices;
- a tensor-split instance keeps its group size as `gpus_per_backend`;
- per-instance `server`, `peers` and `balancer` settings are dropped. The node
  has one API, one mesh membership and one router for all of its models.

Machine-wide sections (`power`, `energy`, `cost`, `pipelines`, `activity`,
`health`) appear once. When the v1 instances disagreed on one of them, pick the
value the machine should have. `energy` in particular was enabled on only one
instance per host in v1; in v2 there is only one node to enable it on.

A GPU index may appear in only one model, and the node refuses a file that
lists one twice.

## 3. Key mapping

| v1 | v2 |
| --- | --- |
| `server.host`, `server.port` | `api.host`, `api.port` (8086 on every machine) |
| `server.mesh_port` | removed; `/mesh` is served on `api.port` |
| `server.cors.*` | `api.cors.*` |
| `model.path` | `models[].path`, one entry per v1 instance |
| model id from the file name | `models[].name`, explicit |
| `model.context_size` | `models[].context` = `context_size / parallel` |
| `model.parallel` | `models[].parallel` |
| `model.n_gpu_layers` | removed; always full offload on GPU (add `--n-gpu-layers` to `args` to override) |
| `gpus.devices`, or `gpus.count` with `gpus.offset` | `models[].gpus`, explicit host indices |
| `gpus.base_port` | removed; backends take loopback ports |
| `gpus.power_limit_watts` | `gpu.power_limit_watts` (AMD) |
| `gpus.tensor_split.enabled` with `group_size` | `models[].gpus_per_backend` (`group_size`, or all GPUs when it was 0) |
| `gpus.tensor_split.mode`, `weights`, `main_gpu` | `llamacpp.split_mode`, `split_weights`, `main_gpu` |
| `backend.binary`, `backend.threads` | `llamacpp.binary`, `llamacpp.threads` |
| `backend.extra_args` | `models[].args` |
| `health.interval`, `timeout`, `max_failures`, `respawn_grace` | unchanged |
| `health.evict_on_hard_failure` | removed; always on |
| (fixed at 3) | `health.max_respawns` |
| `activity.prompt_history` | unchanged |
| `balancer.*` | removed; routing by free slots |
| `peers.hosts`, `poll_interval`, `timeout` | removed; discovery is automatic, `mesh.seeds` only where needed |
| `peers.gossip.*` | removed; membership is always on |
| `peers.gossip.secret_env` | `mesh.secret_env`; the value is now base64 of exactly 32 bytes (`openssl rand -base64 32`), or run `mesh.open: true` |
| `power`, `energy`, `cost`, `pipelines` | unchanged |
| `--section.key value` flags | removed; the config file is the only input |

`llamacpp.*` keys sit inside the model entry they apply to, for example
`models[0].llamacpp.threads`.

## 4. A worked example

A machine ran two v1 instances. The first served a 27B model on a pair of cards:

```yaml
# viiwork.qwen38.yaml (v1)
server:
  port: 9101
model:
  path: /models/Qwen3.8-27B-Q6_K.gguf
  context_size: 98304
  parallel: 2
gpus:
  devices: [0, 1]
  base_port: 9001
  tensor_split:
    enabled: true
    group_size: 2
backend:
  extra_args: ["--spec-type", "draft-mtp", "--spec-draft-n-max", "1"]
peers:
  hosts: ["127.0.0.1:9102"]
```

The second served an 8B model on one card:

```yaml
# viiwork.granite.yaml (v1)
server:
  port: 9102
model:
  path: /models/granite-4.2-8b-Q4_K_M.gguf
  context_size: 32768
  parallel: 2
gpus:
  devices: [2]
  base_port: 9011
peers:
  hosts: ["127.0.0.1:9101"]
```

Both become one v2 file:

```yaml
# viiwork.yaml (v2)
node:
  name: node-a
  state_dir: /var/lib/viiwork

api:
  port: 8086

mesh:
  network: tailnet

models:
  - name: Qwen3.8-27B
    engine: llamacpp
    path: /models/Qwen3.8-27B-Q6_K.gguf
    gpus: [0, 1]
    gpus_per_backend: 2       # group_size 2: one backend across both cards
    context: 49152            # 98304 total / 2 slots
    parallel: 2
    startup_timeout: 45m      # split across two cards: allow a long load (section 9)
    args: ["--spec-type", "draft-mtp", "--spec-draft-n-max", "1"]

  - name: granite-4.2-8b
    engine: llamacpp
    path: /models/granite-4.2-8b-Q4_K_M.gguf
    gpus: [2]
    context: 16384            # 32768 total / 2 slots
    parallel: 2
```

The context arithmetic is the one change that silently matters. Copying
`context_size: 98304` into `context` would ask llama.cpp for 196,608 tokens
(`context × parallel`) and very likely fail to fit in VRAM.

The v1 model ids were `Qwen3.8-27B-Q6_K` and `granite-4.2-8b-Q4_K_M`, taken from
the file names. Under v2 clients send `Qwen3.8-27B` and `granite-4.2-8b`, or an
alias (section 10).

## 5. Mesh mode

A mesh is either **secured** or **open**, and every member must be the same.

- **Secured:** set `VIIWORK_MESH_SECRET` to the base64 of exactly 32 bytes
  (`openssl rand -base64 32`), the same on every machine. Gossip is encrypted
  and authenticated, a node without the secret cannot join, forwards between
  nodes are signed, and `viiwork alias` writes must carry a signature.
- **Open:** leave the secret unset and set `mesh.open: true`. Gossip is
  plaintext and any viiwork node that can reach port 7946 joins. On a tailnet
  that boundary is the tailnet itself, and transit is already encrypted. On a
  LAN it is everyone on the LAN, so the node logs a warning. Alias writes are
  accepted only from the node's own machine.

The node refuses to start with neither a secret nor `mesh.open: true`, and with
both. The error names the environment variable it looked for, so a missing or
mistyped variable fails loudly instead of quietly running a node unsecured.

A node in the wrong mode cannot gossip with the mesh. It logs
`mesh mode mismatch` with the peer's address and never appears as a member.

**Preflight.** Machines must reach each other on 7946 tcp **and** udp, and on
8086 tcp. On a tailnet, check the ACLs allow all three before converting the
first machine.

**Adding a secret to a running open mesh**, without downtime. Do one step on
every machine before starting the next:

1. Remove `mesh.open: true`, set the secret, and set `mesh.secret_enforce: none`.
2. Set `mesh.secret_enforce: outgoing`.
3. Set `mesh.secret_enforce: full` (the default).

Members stay alive throughout. To rotate a secret later, set the new one with
the old one in `VIIWORK_MESH_SECRET_PREV` everywhere, then remove the old one.

## 6. Running it

### Docker

[`configs/docker-compose.v2.example.yaml`](../configs/docker-compose.v2.example.yaml)
runs one node per machine. What it needs, and why:

- **`network_mode: host`.** Gossip binds the machine's tailnet or LAN address,
  not `0.0.0.0`, and members dial each other's real addresses. A bridged
  container has neither.
- **`pid: host`.** Within a minute of a backend turning healthy, the node checks
  that the backend's processes really run on their assigned cards. `rocm-smi`
  and `nvidia-smi` report **host** PIDs, which a container with its own PID
  namespace cannot match. Without `pid: host` the check reports "unknown" and
  never kills anything, so mispinned backends go unnoticed.
- **`ipc: host`.** A second ROCm container on the same machine fails to
  initialise its GPUs without it: a v1 instance still running during a
  conversion, or a benchmark.
- **A state directory, bind-mounted at `/var/lib/viiwork`.** The alias table
  lives there. It must survive recreating the container, or the machine starts
  with an empty table until its first sync with the mesh. It is a host
  directory rather than a named volume, because `docker volume prune` deletes a
  named volume whenever its container is stopped.
- **tailscaled's socket directory, read-only.** On a tailnet the node learns its
  own address and finds other machines through tailscaled's local API. The
  directory is mounted rather than the socket file: a mounted file keeps its old
  inode when tailscaled restarts, which would leave the node without the local
  API until the container is recreated.
- **`/etc/viiwork/mesh.env`**, a secured mesh's secret (section 7, step 5). The
  file is optional, so an open mesh runs without one. It can also carry
  `ENTSOE_API_KEY` for cost tracking.
- **`stop_grace_period: 90s`.** A clean stop leaves the mesh, lets in-flight
  requests finish for `health.respawn_grace` (60 s by default) and then stops
  the backends. Docker's default of 10 s would kill the node mid-drain.

Reload the model list after editing `viiwork.yaml`:

```bash
docker kill -s HUP viiwork
```

### systemd

```ini
[Unit]
Description=viiwork node
After=network-online.target tailscaled.service
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/viiwork --config /etc/viiwork/viiwork.yaml
ExecReload=/bin/kill -HUP $MAINPID
EnvironmentFile=-/etc/viiwork/mesh.env
Restart=on-failure
TimeoutStopSec=90

[Install]
WantedBy=multi-user.target
```

`/etc/viiwork/mesh.env` holds `VIIWORK_MESH_SECRET=...` (section 7, step 5) and
can also hold `ENTSOE_API_KEY` for cost tracking. `systemctl reload viiwork`
applies a changed model list.

### What a reload does

Reload re-reads the file and validates it first; an invalid file is logged and
the running configuration kept. Models are compared by name: an added model
starts, a removed one stops admitting requests and drains, and a changed entry
restarts **that model only**. Changes to any other section are logged as
needing a restart and are not applied.

## 7. Converting a host

Convert one machine at a time. The procedure proves the new configuration before
the machine's v1 instances stop, and records the numbers the mesh actually
achieves. Every check is a `viiwork-accept` command (section 8), which only reads
node state and sends inference requests.

The examples convert `node-a`, and `node-b` is a machine already running v2.

1. **Inventory.** For every v1 instance on the machine, record its config file,
   GPUs, tensor-split layout (`group_size`), `context_size` and `parallel`,
   `extra_args`, and its `power`, `energy` and `cost` settings.

2. **Write one v2 file** with the key table (section 3) and the worked example
   (section 4). Set `node.state_dir` to a host directory, such as
   `/var/lib/viiwork`, and on Docker bind-mount it as the compose example does.
   A named volume is riskier: `docker volume prune` deletes it while the node is
   stopped, and the machine's alias table with it.

3. **Validate the file** on the machine, while v1 still runs:

   ```bash
   viiwork-accept config --file viiwork.yaml --dummy-secret --models-root /srv/models
   ```

   `--dummy-secret` stands in a random key for the mesh secret, so validation
   needs no secret yet. `--models-root` maps the file's container paths under
   `/models/` to the host directory mounted there; leave it out when the paths
   are host paths. Add `--require-mesh secured` (or `open`) once the mesh's mode
   is decided. Every check must pass before going on: an invalid file found
   after v1 has stopped is an outage.

4. **Check the ports.** Gossip needs 7946 over tcp **and** udp between machines,
   and the API needs 8086. On this machine, echo on its mesh address (its
   tailnet IP, or its LAN address in a LAN mesh):

   ```bash
   viiwork-accept ports serve --addr 192.0.2.10
   ```

   and from another machine in the mesh:

   ```bash
   viiwork-accept ports probe --host node-a
   ```

   `api tcp` passes when anything listens on 8086, which on a v1 machine is
   usually an instance's `mesh_port`. When nothing does yet, repeat the probe
   after step 8.

5. **Install the secret** (secured mesh only). Generate it once for the whole
   mesh, on one machine, into a file readable by root and the operators' group:

   ```bash
   OPS_GROUP=operators    # the group your operators share
   sudo install -d -m 0755 /etc/viiwork
   sudo install -m 0640 -g "$OPS_GROUP" /dev/null /etc/viiwork/mesh.env
   echo "VIIWORK_MESH_SECRET=$(openssl rand -base64 32)" | sudo tee /etc/viiwork/mesh.env >/dev/null
   ```

   Copy the file to every other machine the same way, without printing it, and
   keep its mode `0640`. The group matters: `docker compose` reads the file as
   the user running it, and `viiwork alias` needs the secret in the operator's
   environment. Never commit the secret, and never pass it on a command line,
   where it lands in shell history and process listings. Check each copy by its
   decoded length, which must print `32`:

   ```bash
   sh -c '. /etc/viiwork/mesh.env && printf %s "$VIIWORK_MESH_SECRET" | base64 -d | wc -c'
   ```

6. **Stop every v1 instance on the machine.** They hold the GPUs the node is
   about to use. Until step 8, the machine's models are served by the rest of
   the mesh or not at all.

7. **Start the node and time its join.** Start the timer first, on another
   node's view of the mesh:

   ```bash
   viiwork-accept join --observer node-b:8086 --node node-a --expect Qwen3.8-27B,granite-4.2-8b
   ```

   then start the node (`docker compose up -d`, or `systemctl start viiwork`).
   On the first machine of a new mesh there is no other node yet, so the
   observer is the node itself (`--observer node-a:8086`).

8. **Wait for the models to load**, with a timeout of the largest
   `startup_timeout` in the file plus a margin:

   ```bash
   viiwork-accept ready --node node-a:8086 --timeout 50m
   ```

9. **Check inference**, through another node as well as directly:

   ```bash
   viiwork-accept models --node node-a:8086 --via node-b:8086
   ```

   and then saturate each model in turn:

   ```bash
   viiwork-accept saturate --node node-a:8086 --model Qwen3.8-27B
   ```

10. **Time departures**, from another node. Start the timer, then hard-stop the
    node (`docker kill viiwork`):

    ```bash
    viiwork-accept gone --observer node-b:8086 --node node-a --expect-state dead
    ```

    Start the node again and wait for `ready`. Then time a graceful stop
    (`docker stop viiwork`) the same way, and start the node again afterwards:

    ```bash
    viiwork-accept gone --observer node-b:8086 --node node-a --expect-state left
    ```

    The first machine of a new mesh has no other node to observe from; time its
    departures once a second machine is converted.

11. **Point clients** at `http://node-a:8086`. Every request names its model,
    and aliases (section 10) give clients stable names.

**Rollback**, at any point after step 6: stop the v2 node, start the v1
instances, and point clients back (section 11).

## 8. Acceptance

`viiwork-accept` is the checking tool the conversion uses. It only reads node
status and sends inference requests; it never starts, stops or configures
anything, so it is safe to run against a live mesh. It is one static binary that
runs from any machine:

```bash
CGO_ENABLED=0 go build -o viiwork-accept ./cmd/viiwork-accept
```

Each command prints one line per check (`PASS` or `FAIL`, the check, how long it
took, and a detail) and a tally. Nodes are given as `host:port`.

### ports serve

Echoes on a port over tcp and udp until stopped, so that `ports probe` on another
machine can prove both protocols get through. A port it cannot bind fails at
once, which is how "something already holds 7946" shows. Every connection and
datagram is logged with its sender.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--addr` | required | this machine's mesh address |
| `--port` | `7946` | the port to echo on, tcp and udp |
| `--for` | `10m` | stop after this long; Ctrl-C also stops it |

It passes when it listened until stopped:

```bash
viiwork-accept ports serve --addr 192.0.2.10 --for 30m
```

```text
echoing on tcp and udp 192.0.2.10:7946
tcp 192.0.2.11:50814: echoed 32 bytes
udp 192.0.2.11:41377: echoed 32 bytes
PASS  echo tcp+udp 192.0.2.10:7946  1m12.4s  stopped
ports serve 192.0.2.10:7946: 1/1 passed
```

### ports probe

Checks, from the machine it runs on, `api tcp` (a TCP connect to the API port),
`gossip tcp` and `gossip udp` (a nonce sent to `ports serve` comes back
unchanged; udp tries three times, a second apart).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--host` | required | the machine to probe |
| `--api-port` | `8086` | checked by TCP connect |
| `--gossip-port` | `7946` | checked by tcp and udp echo |
| `--timeout` | `5s` | limit per check |

Passing means firewalls and tailnet ACLs let all three through:

```bash
viiwork-accept ports probe --host node-a --timeout 5s
```

```text
PASS  api tcp node-a:8086  0s
PASS  gossip tcp node-a:7946  0s
FAIL  gossip udp node-a:7946  5s  no reply
ports probe node-a: 2/3 passed
```

A udp failure like this one otherwise shows up only later, as membership that
flaps.

### config

Checks `config valid` (the node's own validation, the mesh secret included),
`mesh secured` or `mesh open`, `api port 8086` and `gossip port 7946`, then per
model:

- `weights <model>`: the weights exist (every shard of a split GGUF, every file
  of a model directory);
- `startup_timeout <model>`: a model split across GPUs, or with more than 20 GiB
  of weights, has a `startup_timeout` of at least 45 minutes (section 9).

After the checks it prints a summary of what the node will run.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--file` | required | the v2 `viiwork.yaml` |
| `--models-root` | none | host directory that `/models/` paths refer to |
| `--dummy-secret` | off | a random key for the file's secret variable when it is unset; never printed, and not used for an open mesh |
| `--require-mesh` | none | fail unless the mode is `secured` or `open` |

Passing means the node will accept the file and find its weights. Without the
`startup_timeout: 45m` of the worked example, the split model fails:

```bash
viiwork-accept config --file viiwork.yaml --models-root /srv/models --dummy-secret
```

```text
PASS  config valid
PASS  mesh secured
PASS  api port 8086
PASS  gossip port 7946
PASS  weights Qwen3.8-27B  /srv/models/Qwen3.8-27B-Q6_K.gguf 22.1 GiB
FAIL  startup_timeout Qwen3.8-27B  gpus_per_backend 2, weights 22.1 GiB: need at least 45m0s, have engine default
PASS  weights granite-4.2-8b  /srv/models/granite-4.2-8b-Q4_K_M.gguf 4.6 GiB
PASS  startup_timeout granite-4.2-8b  engine default
config viiwork.yaml: 7/8 passed

node node-a  state_dir /var/lib/viiwork  network tailnet  mesh secured  api 8086  gossip 7946
MODEL           ENGINE    GPUS  BACKENDS  SLOTS  CTX/SLOT  STARTUP  WEIGHTS
Qwen3.8-27B     llamacpp  0,1   1         2      49152     default  22.1 GiB
granite-4.2-8b  llamacpp  2     1         2      16384     default  4.6 GiB
```

### join

Checks `joined <node>`: the observer's `/v1/cluster` lists the node `alive`, with
a status that names every expected model. The time counts from starting the
command. A node's status reaches other nodes on their 5-second status poll, so
expect a few seconds on top of the join itself.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--observer` | required | a node already in the mesh |
| `--node` | required | the name of the node expected to join |
| `--expect` | none | comma-separated models the node must list |
| `--timeout` | `15s` | give up after this long |
| `--poll` | `250ms` | time between polls |

```bash
viiwork-accept join --observer node-b:8086 --node node-a --expect Qwen3.8-27B,granite-4.2-8b
```

```text
PASS  joined node-a  6.3s  models Qwen3.8-27B, granite-4.2-8b
join node-a: 1/1 passed
```

On a timeout the detail says what was seen last, such as
`alive, missing granite-4.2-8b`, `not a member`, or `last error: ...` when the
observer did not answer.

### ready

Checks `ready <model>` for every model the node lists, timed to the first
healthy backend of each, and finally `all backends healthy`.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--node` | required | the node's API |
| `--timeout` | `60m` | give up after this long |
| `--poll` | `2s` | time between polls |

```bash
viiwork-accept ready --node node-a:8086 --timeout 50m
```

```text
PASS  ready Qwen3.8-27B  3m14.2s
PASS  ready granite-4.2-8b  4m1.9s
PASS  all backends healthy
ready node-a:8086: 3/3 passed
```

A model that does not load in time fails with each backend's state and phase,
such as `Qwen3.8-27B/0 starting (loading)`.

### gone

Checks `gone <node>`: the observer no longer lists the node as `alive`. With
`--expect-state left` or `dead` it also checks `state <state>`, the state the
node settles in. A gracefully stopped node turns `left` at once. A hard-stopped
one stays `alive` while the other members suspect it, typically several seconds,
and then turns `dead`; requests stop going to it sooner, once its capacity
reports are 3 s old. Times count from starting the command, so stop the node
right after starting it.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--observer` | required | a node that stays in the mesh |
| `--node` | required | the name of the node expected to go |
| `--expect-state` | `any` | `any`, `left` or `dead` |
| `--timeout` | `120s` | give up after this long |
| `--poll` | `250ms` | time between polls |

```bash
viiwork-accept gone --observer node-b:8086 --node node-a --expect-state dead
```

```text
PASS  gone node-a  11.6s  dead
PASS  state dead  11.6s  dead
gone node-a: 2/2 passed
```

### models

Per model, checks:

- `content <model>`: a real chat completion with thinking off, pinned to the
  node with `?host=`, returns non-empty content, and `X-Viiwork-Node` names the
  node;
- `tools <model>`: a request offering a `get_weather` tool returns
  `finish_reason: tool_calls` with a `get_weather` call;
- `pin <model>`, with `--via`: the content request sent to another node and
  pinned to this one comes back from this node (`X-Viiwork-Node`) by way of the
  other (`X-Viiwork-Origin`).

| Flag | Default | Meaning |
| --- | --- | --- |
| `--node` | required | the node's API |
| `--model` | every model the node lists | a model to check; repeatable |
| `--via` | none | another node's API for the pin check |
| `--timeout` | `10m` | limit per request |

```bash
viiwork-accept models --node node-a:8086 --via node-b:8086
```

```text
PASS  content Qwen3.8-27B  1.9s  ready
PASS  tools Qwen3.8-27B  3.4s  finish_reason tool_calls, 1 tool call
PASS  pin Qwen3.8-27B  1.2s  node-a via node-b
PASS  content granite-4.2-8b  400ms  ready
PASS  tools granite-4.2-8b  900ms  finish_reason tool_calls, 1 tool call
PASS  pin granite-4.2-8b  300ms  node-a via node-b
models node-a:8086: 6/6 passed
```

The llamacpp engine passes no `--jinja`: current llama.cpp builds enable Jinja
chat templates by default, and tool calls depend on them. On an older build
whose `tools` check fails with `finish_reason stop`, add `--jinja` to that
model's `args`.

### saturate

Sends a model more concurrent requests than the node has healthy slots for it
(slots plus `--extra`), without a pin, and checks:

- `no 429 with free slots`: no request was refused while the mesh had at least
  as many free slots as requests, and every answer was 200 or 429;
- `spilled to a peer`: when another member serves the model and the mesh has
  more free slots than the node, at least one answer came from another node
  (otherwise `not applicable`);
- `distribution`: always passes, and counts the answers per node and how many
  waited in the queue.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--node` | required | the node's API |
| `--model` | required | the model to saturate |
| `--extra` | `4` | requests beyond the node's slots |
| `--max-tokens` | `200` | `max_tokens` per request |
| `--timeout` | `10m` | limit per request |

Free slots are read once before sending, so saturate a model that is otherwise
quiet:

```bash
viiwork-accept saturate --node node-a:8086 --model Qwen3.8-27B
```

```text
PASS  no 429 with free slots  n=6 200:6 meshFree=6
PASS  spilled to a peer  other members: node-b
PASS  distribution  node-a:2 node-b:4 queued:1
saturate node-a:8086: 3/3 passed
```

### alias

Sends the content request with `model` set to the alias through every entry
point, without a pin, and checks `alias <alias> via <entry>`: the answer is 200,
`X-Viiwork-Alias` names the alias, and both `X-Viiwork-Model` and the response's
`model` name the expected model. An entry point is a node's `host:port` or a
gateway URL.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--entry` | required | an entry point; repeatable |
| `--alias` | required | the alias to request |
| `--expect-model` | required | the model it must resolve to |
| `--bearer-env` | none | variable holding a bearer token for the entry points |
| `--timeout` | `10m` | limit per request |

Aliases are accepted once for the whole mesh: set one, check it through two
nodes, move it and check again, then revert it and check that every node
follows:

```bash
viiwork alias set stable-coder Qwen3.8-27B
viiwork-accept alias --entry node-a:8086 --entry node-b:8086 --alias stable-coder --expect-model Qwen3.8-27B
viiwork alias set stable-coder granite-4.2-8b
viiwork-accept alias --entry node-a:8086 --entry node-b:8086 --alias stable-coder --expect-model granite-4.2-8b
viiwork alias revert stable-coder
viiwork-accept alias --entry node-a:8086 --entry node-b:8086 --alias stable-coder --expect-model Qwen3.8-27B
```

```text
PASS  alias stable-coder via node-a:8086  1.8s  Qwen3.8-27B on node-a
PASS  alias stable-coder via node-b:8086  2.1s  Qwen3.8-27B on node-a
alias stable-coder: 2/2 passed
```

Run `viiwork alias` where the node's config and, in a secured mesh, its secret
are available: inside the container (`docker exec viiwork viiwork alias ls`), or
in a shell that has loaded the secret (`set -a; . /etc/viiwork/mesh.env; set +a`).
An entry point that needs a key, such as a gateway, takes it from the
environment:

```bash
viiwork-accept alias --entry https://gateway.example.com --alias stable-coder --expect-model Qwen3.8-27B --bearer-env GATEWAY_KEY
```

### Output and exit codes

Every command takes `--json`, which writes the report as JSON instead of text
(for `config`, with the summary under `summary`; the progress lines of
`ports serve` then go to stderr). The exit code is 0 when every check passed, 1
when any failed, and 2 for a usage error. Ctrl-C ends a waiting command, and its
open check fails as `interrupted`. The token named by `--bearer-env` is sent as
`Authorization: Bearer <token>` and never printed; reading it from the
environment keeps it off the command line. Each command's `--help` lists its
flags with their defaults.

## 9. Large models

A backend has `startup_timeout` to become healthy, and the llama.cpp default is
**10 minutes**. v1 allowed a model 30-45 minutes to load. A large GGUF on slow
storage needs longer, or the node gives up on it mid-load:

```yaml
models:
  - name: gpt-oss-120b
    startup_timeout: 45m
```

`viiwork-accept config` fails a model split across GPUs, or with more than
20 GiB of weights, whose `startup_timeout` is below 45 minutes.

## 10. Clients

- **One URL per machine:** `http://<machine>:8086`. Every node routes every
  model in the mesh, so any machine works as an entry point.
- **Name the model in every request.** Model names are the `name`s in the
  configs, listed on `GET /v1/models` of any node.
- **Use aliases as stable names.** Point a client at an alias instead of a
  model, and switch the model without touching the client:

  ```bash
  viiwork alias set stable-coder Qwen3.8-27B --fallback granite-4.2-8b
  viiwork alias ls
  viiwork alias revert stable-coder
  ```

  Responses to an aliased request carry `X-Viiwork-Alias` and `X-Viiwork-Model`,
  naming the alias and the model that answered.
- **Pin a machine** with `?host=<node name>` on the request, as in v1.8, now
  compared against node names.

## 11. Rollback

Keep the v1 image, compose files and instance configs until the machine has run
v2 for a while. To roll a machine back:

1. Stop the v2 node (`docker compose down`, or `systemctl stop viiwork`).
2. Start the v1 instances as before.
3. If its clients were already moved in step 11 of the conversion (section 7),
   point them back at the v1 ports and model ids.

v1 and v2 are separate meshes and do not route to each other, so a mixed fleet
needs clients to know which machine runs which during the transition.
