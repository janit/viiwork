# Setting up a machine

`viiwork init` turns a machine with GPUs and some GGUF model files into a
running node. It asks a few questions and shows every file it will write. It
writes nothing until you say yes, then starts the node and checks it. Running
`viiwork` on a terminal with no config file starts the same wizard.

On Linux the engine runs in Docker. On an Apple Silicon Mac it runs natively
under a user LaunchAgent: see [On a Mac](#on-a-mac).

## Before you start

**Linux, AMD Radeon VII, MI50 or MI60 (gfx906)** — the reference fleet:

- The in-kernel `amdgpu` driver (`/dev/kfd` exists). No ROCm on the host: the
  wizard reads the cards from the kernel's KFD topology, and ROCm 6.2.4 is in
  the image.
- Docker Engine with the Compose v2 plugin.
- A checkout of the release to build the ROCm image from: gfx906 has no
  published image, so the wizard writes the config and you start the node; see
  [When it writes the config only](#when-it-writes-the-config-only).

**Linux, NVIDIA:**

- An NVIDIA GPU with its driver (`nvidia-smi` works).
- Docker Engine with the Compose v2 plugin (`docker compose version` works).
- The [NVIDIA Container Toolkit](https://docs.nvidia.com/datacenter/cloud-native/container-toolkit/).
  With Docker 25 or newer, its CDI spec is enough
  (`sudo nvidia-ctk cdi generate --output=/etc/cdi/nvidia.yaml`). Otherwise,
  register its runtime with `sudo nvidia-ctk runtime configure
  --runtime=docker` and restart Docker.

Other AMD cards are found too, but no image is verified for them yet; see
[When it writes the config only](#when-it-writes-the-config-only). A machine
with no supported GPU is refused.

**Mac:** Apple Silicon. Run the wizard from Terminal on the Mac itself, while
logged in: the node runs as a LaunchAgent in your login session, so an SSH
session with nobody logged in is refused.

**Both:**

- GGUF model files in one directory. The wizard downloads nothing. For
  example, `hf download <repo> <file>.gguf --local-dir ~/models`, or the
  `data_dir` of a viiwork-parrot node, which the wizard suggests by itself.
- Ports 8086/tcp (the API and dashboards) and 7946/tcp+udp (the mesh) free on
  this machine, and open between the machines of the mesh.
- For more than one machine: every machine on the same Tailscale tailnet (the
  default when tailscaled runs), or on the same LAN.

## Download and verify

Take the newest release from the
[releases](https://github.com/janit/viiwork/releases). Check the download
against `SHA256SUMS`; [releases.md](releases.md#checking-a-download-by-hand)
shows how to verify its signature too.

```sh
v=vX.Y.Z                     # the newest release
os=linux_amd64               # or linux_arm64, darwin_arm64
base=https://github.com/janit/viiwork/releases/download/$v
curl -fLO "$base/viiwork_${v}_${os}.tar.gz" -fLO "$base/SHA256SUMS"
sha256sum --check --ignore-missing SHA256SUMS
# on a Mac instead: shasum -a 256 --check --ignore-missing SHA256SUMS
tar xzf "viiwork_${v}_${os}.tar.gz" && cd "viiwork_${v}_${os}"

sudo ./viiwork init          # on a Mac: ./viiwork init (no sudo)
```

## What it asks

1. **Preflight.** On Linux the wizard needs root (it writes `/etc/viiwork`),
   Docker with Compose v2, and the ports free. It stops if a config or an
   earlier install is already there, or if a container named `viiwork` exists:
   a node set up by hand is left alone.
2. **Hardware.** It lists the GPUs and names the engine image it will run.
3. **Models.** It asks for your models directory, lists each model it finds
   with its architecture and size, and asks which to serve and what to call
   them. Give each model the name the rest of your mesh uses for it, so that
   requests for it can go to any machine. Second shards of a split model and
   vision projectors are not listed. `~` means your home directory.
4. **Layout.** It proposes which cards each model gets, the context per slot,
   and how many slots. You can accept the layout, edit one model's line, or
   drop a model. A layout the node would refuse is shown with the reason.
5. **Mesh.** First it looks for nodes nearby, on your tailnet and on the LAN.
   Then it offers three choices:
   - join an existing mesh with a **join code** (see below);
   - start a new secured mesh;
   - start an open mesh (no secret), with a warning.

   The network is `tailnet` when tailscaled runs on this machine, else `lan`.
   Change `mesh.network` in the config afterwards if you need to. A join code
   carries the first node's address, so the new machine must be able to reach
   it.
6. **Updates.** Choose whether this machine accepts mesh-wide updates
   (`viiwork update`, see [Updating](#updating)). Only a secured mesh can have
   them.
7. **Node name.** The default comes from the host name. A name a node found
   nearby already uses is refused.
8. **Review.** It shows every file with its content, the secret masked. On a
   yes it writes the files, pulls the engine image (several GB on Linux),
   starts the node, and waits until every model has loaded.
9. **Finish.** It runs the `viiwork-accept` config and ready checks, plus the
   join check when the node joined a mesh. Then it prints how to reload and
   restart. A new secured mesh also gets the join code for the next machine.

Pressing Ctrl-C or Ctrl-D at any question leaves nothing behind.

## What it writes

| Path | Mode | What |
|---|---|---|
| `/etc/viiwork/viiwork.yaml` | 0644 | The config. Edit it freely, then `sudo docker kill -s HUP viiwork`. |
| `/etc/viiwork/mesh.env` | 0640 | The mesh secret (secured mesh only). |
| `/etc/viiwork/docker-compose.yaml` | 0644 | One service: host networking, the GPUs, the models directory read-only at `/models`, `/etc/viiwork` read-only, `restart: always`. |
| `/etc/viiwork/install.json` | 0644 | The manifest: exactly what `viiwork uninstall` removes. |
| `/usr/local/bin/viiwork` | 0755 | A copy of the binary. |
| `/var/lib/viiwork/` | — | Node state: the alias table and staged releases. |
| `/etc/systemd/system/viiwork-engine.{service,path,timer}` | 0644 | The engine helper, which makes the image follow the release ([below](#updating)). |
| `/var/lib/viiwork-engine/` | 0700 | The helper's own directory: the image tag the install started with, and the releases it verified. |

Model weights are never copied or moved.

## When it writes the config only

Only some machines get an engine image and a started node. In these cases the
wizard writes the config, `mesh.env`, the manifest and
`/usr/local/bin/viiwork`, creates the state directory, starts nothing, and
says why:

- **A development build.** Only a release (`vX.Y.Z`, or a pre-release such as
  `v2.6.0-beta4`) has published images. Use a release.
- **An AMD Radeon VII or MI50 (gfx906).** It has no published image, so build
  one from a checkout and start it with the example compose file:

  ```sh
  git clone https://github.com/janit/viiwork && cd viiwork && git checkout vX.Y.Z
  make docker                  # gfx906; needs the ROCm base images, see BUILDS.md
  sudo cp configs/docker-compose.v2.example.yaml /etc/viiwork/docker-compose.yaml
  # point the models mount at your models directory:
  sudo sed -i 's#- /models:/models:ro#- /path/to/models:/models:ro#' /etc/viiwork/docker-compose.yaml
  sudo docker compose -f /etc/viiwork/docker-compose.yaml -p viiwork up -d
  ```

  The wizard's config and `mesh.env` are already in `/etc/viiwork`, where the
  example compose file expects them.
- **Other AMD cards.** No image is verified yet: viiwork pins a backend to its
  cards with `ROCR_VISIBLE_DEVICES`, which the Vulkan build ignores. Build an
  image for your card ([BUILDS.md](../BUILDS.md)) and start it as above.

The config's model paths are `/models/...`, so mount your models directory
there. A compose project you started by hand is not in the manifest: before
`viiwork uninstall`, stop it with
`sudo docker compose -f /etc/viiwork/docker-compose.yaml -p viiwork down`.

## Joining the next machine

On any node of a secured mesh, print a join code:

```sh
sudo sh -c 'set -a; . /etc/viiwork/mesh.env; /usr/local/bin/viiwork join-code'   # Linux
~/.local/bin/viiwork join-code                                                    # Mac (reads the agent's plist)
```

It prints a code starting with `viiwork1-`. The code **is** the mesh secret:
treat it like `mesh.env`, and never post it anywhere. Run the wizard on the new
machine and paste the code at the mesh question. In an open mesh,
`viiwork join-code --open` prints a code without a secret. If the wizard
stopped before printing its join code, get one this way.

## When the node does not come up

The files are kept. The wizard prints each backend that is not healthy, with
its status and phase, followed by the node's last log lines. llama.cpp's own
errors are in those lines:

- Linux: `sudo docker logs viiwork` shows them all.
- Mac: `tail -f ~/Library/Logs/viiwork/viiwork.log`.

The usual causes:

- **The model does not fit** at the chosen context. Lower `context`, or give
  it more `gpus`, in the config.
- **The model needs llama.cpp flags.** Some models' chat templates cannot be
  parsed by llama.cpp's default (Jinja) handling. The log then says `chat
  template parsing error` and suggests `--no-jinja`. The wizard cannot know
  this from a model's header. Add the model's flags to its entry, for example
  for TranslateGemma:

  ```yaml
      args: ["--no-jinja", "--chat-template", "gemma"]
  ```

After editing the config, reload the node. It restarts the models whose
entries changed:

```sh
sudo docker kill -s HUP viiwork                                   # Linux
launchctl kill HUP gui/$(id -u)/fi.viiwork.node                   # Mac
```

To stop the node and every model it runs, and to start it again:

```sh
sudo viiwork stop     # Linux; `viiwork stop` on a Mac, without sudo
sudo viiwork start
```

`stop` leaves the mesh first and finishes in-flight requests, so other members
take the traffic, and returns once the node's API has gone quiet. On Linux it
also stops the engine helper, which could otherwise bring the container back.
The node stays stopped until `viiwork start`, or the next boot (Linux) or
login (Mac). Only this machine is stopped. Both commands work from the install
manifest, so a host set up by hand is stopped the way it was started.

To start over, `viiwork uninstall` (with `sudo` on Linux) removes everything
in the manifest and nothing else.

## Updating

A machine that accepted mesh-wide updates updates itself when you roll a
release across the mesh from any node:

```sh
sudo sh -c 'set -a; . /etc/viiwork/mesh.env; /usr/local/bin/viiwork update'      # from a Linux node
~/.local/bin/viiwork update                                                       # from a Mac node
```

It stages the newest signed release on every node, then activates them one at
a time, and stops if a node rolls itself back or does not rejoin the mesh.

On a Mac the llama.cpp build moves with the release: staging fetches the
build the release is pinned to, checked against the sha256 signed into that
release, beside the one running now, and the node always runs the build of
its own binary's pin, so a rollback takes the engine back too. Builds no kept
release runs are removed once a release confirms. A `llamacpp.binary` you
pointed anywhere else is left exactly as written.

On Linux the engine moves with the release. The node cannot swap its own
Docker image, so the wizard installs a small helper on the host,
`viiwork engine-sync`, run as root by the systemd units
`viiwork-engine.path` and `viiwork-engine.timer`. When a release is staged
the helper pulls and checks its image, so a bad image fails before any
machine restarts; when it is activated the helper points the compose file at
it and restarts the node, and on a rollback it points it back. It verifies
every release's signature itself, with the keys in the host's own binary, and
requires the image's viiwork to be exactly the signed release's; from the
node's state directory, which the container can write, it takes nothing but
a version name. Once the node confirms a release, the helper also updates
`/usr/local/bin/viiwork`. `journalctl -u viiwork-engine.service` shows what
it did. [releases.md](releases.md#on-a-docker-install-the-image-follows-the-release)
has the details, and what each node checks.

**A machine set up by an earlier release** (v2.6.0 or a beta) has no helper, and
its updates move the viiwork binary only. To add it, run init from the new
release's binary (downloaded and checked as [above](#download-and-verify)):

```sh
sudo ./viiwork init
```

On an existing Docker install init offers only this: it writes the three
units, installs itself as `/usr/local/bin/viiwork`, records both in
`install.json` so that uninstall removes them, and starts the helper. The
config, the compose file and the running node are left alone. Until then, if
a release ever needs a newer engine, staging on that machine is refused with
the reason. To opt in later, add this to the config and restart the node (a reload
does not apply it):

```yaml
update:
  enabled: true
```

## Uninstalling

```sh
sudo /usr/local/bin/viiwork uninstall [--yes] [--delete-models] [--keep-images]   # Linux
~/.local/bin/viiwork uninstall [--yes] [--delete-models]                           # Mac, no sudo
```

Uninstall reads the manifest and lists everything it will remove, with sizes.
Then it asks you to type `uninstall`. If this node is the mesh's last member,
it says so first: the alias table, and in a secured mesh the only copy of the
secret, go with it. After you confirm, it:

1. stops the node the normal way, so it leaves the mesh and in-flight requests
   finish (if the node cannot be stopped, it removes nothing);
2. on Linux, runs `docker compose down` for its own project only, and removes
   the engine image unless you pass `--keep-images` (or another container
   still uses it);
3. removes the files and the binary, the engine helper's units (stopped
   before the node, so it cannot start it again) and directory, and the state
   directory if the install created it (a state directory that was there
   before is left alone).

It never prunes, never touches another compose project, and leaves any file
you added to `/etc/viiwork`.

**Model weights are kept**, so a reinstall downloads nothing.
`--delete-models` also deletes the model files this node served. It lists them
first and asks for its own confirmation, and it never deletes anything else in
the models directory.

**A host set up by hand** has no manifest, so uninstall refuses rather than
guess. `viiwork uninstall --from-config` lists what the config points at and
removes nothing.

## On a Mac

On an Apple Silicon Mac, run `./viiwork init` **without sudo**. The wizard asks
the same questions, with two differences:

- **Planning.** The Mac has one GPU, whose memory it shares with everything
  else on the machine. Models are planned against the Metal budget
  (`iogpu.wired_limit_mb`, about 75% of RAM by default), minus headroom. A
  model that does not fit is shown with the reason.
- **What gets installed.** No Docker. Instead the wizard fetches llama.cpp at
  the release this binary was built against. It downloads it from the
  release's fixed URL, without GitHub's API, and checks it against the sha256
  signed into this binary, then installs a LaunchAgent.

| Path | Mode | What |
|---|---|---|
| `~/.config/viiwork/viiwork.yaml` | 0644 | The config: absolute paths, `gpu.vendor: apple`, each model's `llamacpp.binary`. |
| `~/Library/LaunchAgents/fi.viiwork.node.plist` | 0600 | The agent, with the mesh secret in its environment. |
| `~/.local/share/viiwork/llama.cpp/<tag>/` | — | The llama.cpp build, quarantine attribute cleared. A build already there (from `scripts/macos/fetch-llama.sh`) is reused and left alone by uninstall. The manifest records the directory above it (`llama_root`), where updates fetch their builds. |
| `~/.local/bin/viiwork` | 0755 | A copy of the binary. |
| `~/.local/state/viiwork/`, `~/Library/Logs/viiwork/` | — | Node state, and the log. |
| `~/.config/viiwork/install.json` | 0644 | The manifest. |

`~/.local/bin` is not on a Mac's default `PATH`. Add it once, so that
`viiwork join-code`, `viiwork top` and the rest work by name:

```sh
echo 'export PATH="$HOME/.local/bin:$PATH"' >> ~/.zprofile && . ~/.zprofile
```

After setup:

- Reload the config with `launchctl kill HUP gui/$(id -u)/fi.viiwork.node`.
- Restart the node with `launchctl kickstart -k gui/$(id -u)/fi.viiwork.node`.
- The node runs while you are logged in. A sleeping Mac leaves the mesh, so run
  `caffeinate -s` while it serves on power.
- Remove the install with `viiwork uninstall`, without sudo.
- [macos.md](macos.md) covers the rest: Tailscale builds, memory, and what
  reads as unavailable.
