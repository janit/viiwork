# macOS (Apple Silicon) nodes

A Mac is just another node. One native `viiwork` process runs `llama-server`
through the ordinary `llamacpp` engine. On a Mac that engine uses Metal. The
node joins the mesh like any Linux host and serves the same GGUF files under
the same model names, with the same flags. There is no separate macOS engine.
Fleet compatibility comes from running the same llama.cpp release as the Linux
nodes: the pin in `docker/pins.env` (`LLAMA_CPP_VERSION`), which ggml-org also
publishes as a macOS arm64 build.

Supported: Apple Silicon (M1 and later). Not supported: Intel Macs and Docker
on macOS, because Docker Desktop has no Metal. A Mac node always runs natively.

## Install

The quickest way is to download the `darwin_arm64` archive
([setup.md](setup.md#download-and-verify)) and run `./viiwork init` from it,
without sudo. It fetches llama.cpp at the pin, writes the config and the
LaunchAgent below, and starts the node; see [setup.md](setup.md#on-a-mac). The
manual steps follow.

```sh
# 1. The node and its acceptance checker. Build them natively on the Mac,
#    or cross-compile from any host with `make build-darwin`.
make build-darwin
mkdir -p ~/.local/bin && cp bin/darwin-arm64/viiwork bin/darwin-arm64/viiwork-accept ~/.local/bin/

# 2. llama.cpp at the fleet's pinned release. Prints the llama-server path.
scripts/macos/fetch-llama.sh
#   ~/.local/share/viiwork/llama.cpp/b12359/llama-b12359/llama-server

# 3. Check that Metal sees the GPU.
~/.local/share/viiwork/llama.cpp/b12359/llama-b12359/llama-server --list-devices
#   MTL0: Apple M3 Max (28753 MiB, 28753 MiB free)
```

`fetch-llama.sh` unpacks each release tag into a directory of its own, so
moving the pin never touches the build a running node uses. It also clears the
quarantine attribute that would otherwise make Gatekeeper refuse to start
`llama-server`. Do not install llama.cpp from Homebrew: that formula changes
daily and is not pinned to the fleet's release.

## Configuration

`~/.config/viiwork/viiwork.yaml`:

```yaml
node:
  name: mac-a                        # set it: a Mac's hostname can be "Name-MacBook-Pro"
  state_dir: /Users/YOU/.local/state/viiwork

mesh:
  network: tailnet
  open: false                        # secret from VIIWORK_MESH_SECRET, as on Linux

gpu:
  vendor: apple                      # required for several models to share the GPU

models:
  - name: Qwen3.8-27B                # the same name and GGUF as on the fleet
    engine: llamacpp
    path: /Users/YOU/models/Qwen3.8-27B-UD-Q4_K_XL.gguf
    gpus: [0]
    context: 32768                   # tokens PER SLOT, as everywhere
    parallel: 2
    llamacpp:
      binary: /Users/YOU/.local/share/viiwork/llama.cpp/b12359/llama-b12359/llama-server
```

The config is a normal v2 file. Only a few values are specific to the Mac:

- **`gpu.vendor: apple`.** A Mac has one GPU. With `vendor: apple`, several
  models may each list `gpus: [0]`, and each model's concurrency comes from
  its `parallel`. Leaving the vendor at `auto` still detects the GPU and still
  gives you telemetry. It does **not** relax the one-model-per-GPU rule,
  though. That keeps validation a function of the file alone, so a Mac's
  config checks the same from any machine. One model listing `gpus: [0, 0]`
  is refused under every vendor.
- **`gpus: [0]`** puts every layer on the GPU (`--n-gpu-layers -1`). The
  command line is the same as a single-GPU Linux backend's, minus the pinning
  variable. `--parallel` and `--ctx-size` are passed explicitly, so the KV
  cache is split per slot exactly as on Linux.
- **Paths are absolute.** `state_dir` must outlive restarts, like a Docker
  volume on Linux.
- **Copy the fleet's `api.cors` block.** A browser app that talks to nodes
  directly, such as a chat page with `?host=` pinned to this machine, is
  refused with `403 origin not allowed` by any node whose allowlist lacks the
  app's origin. Forwards between nodes are unaffected, so the gap only shows
  when the request reaches this node directly. An explicit `allow_origins`
  replaces the derived list, so repeat the tailnet entries in it.

### Memory

Unified memory is shared by every model and by everything else on the Mac.
Metal lets the GPU wire about 75% of RAM by default: `sysctl
iogpu.wired_limit_mb` reads 0, which means that default. On a 36 GB machine
that is about 28 GB, which is the figure `--list-devices` prints. The wizard
plans against that budget; the node itself enforces none, so a config you edit
by hand is yours to size. An overcommitted config shows up
as a failed load, or as swapping. The health ladder reports both.

## Models from viiwork-parrot

A model can name a catalog entry instead of a local file, exactly as on the
fleet:

```yaml
  - name: granite-4.2-8b
    engine: llamacpp
    source: viiwork-parrot:granite4.2-8b-q8_0
    gpus: [0]
    context: 16384            # match the fleet: /v1/fleet/capacity serves the minimum
    parallel: 2
```

That needs viiwork-parrot running on the Mac, on `127.0.0.1:7950`. It builds
natively (`make build` in the viiwork-parrot repository) and runs from a user
LaunchAgent, `scripts/macos/fi.viiwork.parrot.plist.example`. Its config is
the ordinary one (`deploy/viiwork-parrot.yaml.example` there), with a few
changes for a laptop:

- `node.data_dir` and `node.state_dir` go under your home directory, for
  example `~/.local/share/viiwork-parrot/models` and
  `~/.local/state/viiwork-parrot`.
- `models.want` lists only what this Mac serves.
- Set `network.upnp: false`, so the daemon never opens ports on whichever
  router the laptop is behind, and lower `limits.upload`.
- `local.tailscale: true` finds tailnet peers with `tailscale status --json`
  from `PATH`, which the App Store build does not provide. Put this wrapper
  at `~/.local/bin/tailscale`. The agent's `PATH` lists that directory first.
  A plain symlink to the app binary does not work.

  ```sh
  #!/bin/sh
  # The app binary acts as the CLI only when TERM is set; a LaunchAgent has none.
  exec env TERM="${TERM:-dumb}" /Applications/Tailscale.app/Contents/MacOS/Tailscale "$@"
  ```

  (The viiwork node needs no wrapper: it sets `TERM` for that binary itself.)

While parrot downloads, the backend shows phase `fetching` with parrot's
progress, and the time does not count against `startup_timeout`. Parrot
verifies every sha256 and then seeds the file. Macs are untested in
viiwork-parrot's own suite: its unit tests trip over macOS's `/var` →
`/private/var` temp-directory symlink, but the daemon works.

## Tailscale

A tailnet mesh learns the Mac's own address and the MagicDNS suffix from
tailscaled. Which Tailscale build you run changes where tailscaled is:

| Build | Where the node finds it |
|---|---|
| App Store (sandboxed) | the CLI inside the app bundle. There is no socket, and nothing on `PATH` |
| Standalone (from tailscale.com) | the same app-bundle CLI, or `tailscale` on `PATH` |
| Homebrew `tailscaled` | the `/var/run/tailscaled.socket` socket |

Leave `mesh.tailnet.socket` unset. When it is at its default, the node tries
the Homebrew socket first, then `tailscale` on `PATH`, then
`/Applications/Tailscale.app/Contents/MacOS/Tailscale`. The node never
second-guesses a socket you set explicitly. The MagicDNS suffix read this way
also feeds the default CORS allowlist, so `/mesh` opens in a tailnet browser
as it does on a Linux node.

LAN mode (`mesh.network: lan`) takes the default route's interface from
`route -n get default`. A VPN client that captures the default route (Mullvad,
for example) changes that interface. Tailnet mode is unaffected.

## Running under launchd

`scripts/macos/fi.viiwork.node.plist.example` is a user LaunchAgent. It starts
the node at login and restarts it when it exits, and it allows a 90 s stop
grace that covers the node's ordered shutdown. Install, reload and restart
commands are in the file's header comment. `SIGHUP` reloads the config as on
Linux. Logs go to `~/Library/Logs/viiwork/viiwork.log`.

On a Mac set up by `viiwork init`, `viiwork stop` boots the agent out, which
stops the node and its llama.cpp backends and frees the GPU until
`viiwork start` or the next login. Killing the process does not: KeepAlive
starts it again.

## Updates

A Mac set up by `viiwork init` with updates enabled moves its llama.cpp with
each release: every release carries its llama.cpp pin and that build's
sha256, staging fetches the build into `~/.local/share/viiwork/llama.cpp`
beside the running one, and the node runs the build of its own binary's pin.
A rollback therefore takes the engine back with the binary. A Mac set up by
hand has no install manifest, and its `llamacpp.binary` is always used as
written.

## Sleep

A sleeping Mac stops answering gossip. Within a few seconds the fleet marks it
dead and routes that model's requests to other members, or queues them.
Requests the Mac was serving when the lid closed fail. After wake, the node
rejoins within `mesh.rejoin_interval`, and its models reappear once its
capacity reports are fresh again. To keep a lid-open laptop serving, stop it
from idle-sleeping: run `caffeinate -s` while it is on power, or change the
Energy settings.

## What reads as unavailable

These are absent rather than zero, as on any host that cannot measure them:

- **Power, energy and cost.** Reading power needs root (`powermetrics`) or
  private APIs, so `PowerAvailable` is false and the energy store has nothing
  to record.
- **GPU name and UUID** in `/v1/status`. GPU utilisation and memory are
  present. They come from `ioreg`, and memory is counted against total RAM
  because the pool is unified.
- **Per-backend RSS** and the process-tree view, which read `/proc` on Linux.
- **The on-GPU PID check.** macOS has no per-process GPU accounting, so the
  check is skipped, as for `vendor: none`.

## Acceptance

`viiwork-accept` runs on the Mac as on any host. It is read-only: it never
starts, stops or configures the node. The wizard does not install it: run it
from the unpacked release archive.

## Uninstall

A Mac set up by `viiwork init` is removed with `viiwork uninstall` (no
`sudo`), which removes exactly what its manifest lists; see
[setup.md](setup.md#uninstalling). A Mac set up by hand, following the steps
above, has no install manifest, so remove it by hand. viiwork-parrot has no
uninstall command either. Nothing is installed outside your home directory,
so removal needs no `sudo`:

```sh
# 1. Stop both agents. The node leaves the mesh first; other members route
#    elsewhere within seconds.
launchctl bootout gui/$(id -u)/fi.viiwork.node
launchctl bootout gui/$(id -u)/fi.viiwork.parrot
rm ~/Library/LaunchAgents/fi.viiwork.node.plist ~/Library/LaunchAgents/fi.viiwork.parrot.plist

# 2. Binaries, and the tailscale wrapper if you created it for parrot.
rm ~/.local/bin/viiwork ~/.local/bin/viiwork-accept ~/.local/bin/viiwork-parrot
rm ~/.local/bin/tailscale

# 3. Config, state and logs. The alias table in the node's state_dir is
#    replicated on every member, so nothing is lost with it.
rm -rf ~/.config/viiwork ~/.config/viiwork-parrot
rm -rf ~/.local/state/viiwork ~/.local/state/viiwork-parrot
rm -rf ~/Library/Logs/viiwork

# 4. Downloads: llama.cpp builds, and the model weights parrot fetched.
#    The weights are the large part.
rm -rf ~/.local/share/viiwork
rm -rf ~/.local/share/viiwork-parrot
```

Use the paths from your own configs if they differ. The mesh needs no
cleanup: a departed node drops out of `/v1/cluster` on its own. An alias
that pointed only at this Mac's models falls back or answers 503, like any
alias whose target is no longer served.
