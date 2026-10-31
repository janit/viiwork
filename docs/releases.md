# Releases

Every `v*` tag on the public repository gets release archives for linux/amd64,
linux/arm64 and darwin/arm64:

```
viiwork_vX.Y.Z_linux_amd64.tar.gz     viiwork, viiwork-accept, LICENSE
viiwork_vX.Y.Z_linux_arm64.tar.gz
viiwork_vX.Y.Z_darwin_arm64.tar.gz
SHA256SUMS                            sha256 of each archive
SHA256SUMS.sig                        ed25519 signature over the version and SHA256SUMS
```

CI builds the archives and uploads them with an **unsigned** `SHA256SUMS`. The
publisher then rebuilds the same version from their own checkout, checks that
every file matches CI's byte for byte, and only then signs. Nothing downloaded
is ever run on the machine that holds the key, so neither a compromised
workflow nor anyone who can move the public tag can get other code signed. The
signature covers the version, and only a `SHA256SUMS` listing exactly that
version's three archives is signed, so a signature can never be replayed for
another release. CI never holds the key, so a compromised
workflow can publish archives but cannot get them signed. A release without
`SHA256SUMS.sig` is not installable by `viiwork update`: a node refuses to
stage it.

## Builds are reproducible

Every binary is built by `scripts/gobuild.sh` — `make`, CI and the local rebuild
alike — with `CGO_ENABLED=0`, `-trimpath`, `-buildvcs=false` and only
`-X main.version` and the llama.cpp pin and digest from `docker/pins.env` in
`-ldflags`. `scripts/build-release.sh` pins the compiler to
go.mod's exact version (`GOTOOLCHAIN`) and writes deterministic archives.
`bash scripts/build-release_test.sh` builds the committed tree twice from two
directories and checks that every byte agrees.

## Publishing a release

1. Cut the release as usual (`CHANGELOG.md` heading, `scripts/publish.sh --tag vX.Y.Z …`).
   The tag push starts the `release` workflow on the public repository.
2. When the workflow has finished, on the publisher's machine:

   ```sh
   scripts/release-sign.sh vX.Y.Z [COMMIT]
   ```

   Run it in the private repository at the commit that was published (`HEAD`
   by default). It downloads the archives, rebuilds that commit, runs
   `viiwork-release compare` on every archive, signs `SHA256SUMS` for the
   version, checks the signature against that commit's compiled-in keys, and
   attaches `SHA256SUMS.sig`. `bash scripts/release-sign_test.sh` exercises it
   end to end against a stand-in `gh`.

   The workflow refuses to re-upload archives once a release carries a
   signature, and only its `publish` job, which runs nothing but `gh`, holds
   write access.

3. `release-sign.sh` then publishes the engine images, from the same local
   build it just compared: `ghcr.io/janit/viiwork-llamacpp-cuda` (amd64,
   arm64), `viiwork-llamacpp-vulkan` and `viiwork-vllm`, each tagged with the
   version. Each is the upstream engine image (pins in `docker/pins.env`) with
   `viiwork` appended as one layer, streamed registry-to-registry by `crane`.
   Its `gh` token needs the `write:packages` scope (`gh auth refresh -s
   write:packages`). A new package on ghcr is private until made public once
   in its settings. gfx906 (Radeon VII) has no published image: `make docker`
   builds it. `SKIP_IMAGES=1` signs without publishing. Every image records
   its base by digest (`org.opencontainers.image.base.digest`), and a released
   tag is written once. If a push fails after the signature is attached,
   `IMAGES_ONLY=1 scripts/release-sign.sh vX.Y.Z` rebuilds, compares, verifies
   the existing signature and publishes only the images. The registry login
   lives in the run's temporary directory and goes with it.

   The base images keep their own HEALTHCHECK (llama.cpp's probes :8080); a
   compose file sets its own.

viiwork-parrot follows the releases feed and seeds each release as a torrent
once its signature is attached.

## The signing key

`go run ./cmd/viiwork-release keygen` makes the pair once, on the publisher's
machine: the private key at `~/.config/viiwork/release.key` (mode 0600; `sign`
refuses a key others can read) and the public key at
`internal/release/keys/release.pub`, which is committed and compiled into every
build. `keygen` refuses to overwrite an existing key.

Back the private key up offline. Losing it, or suspecting it has leaked, means a
rotation: make a new pair under a new file name (`--pub
internal/release/keys/<year>.pub`), ship a release carrying both public keys,
sign later releases with the new key, and remove the old public key once no
node runs a version that lacks the new one.

## Checking a download by hand

The checksum proves the archive is the one listed in `SHA256SUMS`. The
signature proves `SHA256SUMS` is the publisher's, for exactly that version.
Checking the signature needs a checkout and Go 1.27.2:

```sh
curl -fLO "$base/SHA256SUMS.sig"                  # beside the archive and SHA256SUMS
sha256sum --check --ignore-missing SHA256SUMS      # on a Mac: shasum -a 256 --check --ignore-missing SHA256SUMS
git clone --branch vX.Y.Z https://github.com/janit/viiwork viiwork-src
(cd viiwork-src && go run ./cmd/viiwork-release verify --version vX.Y.Z ../SHA256SUMS ../SHA256SUMS.sig)
# prints "signature ok"
```

## How a node updates itself

A node takes part only with `update.enabled: true`. Everything lives under
`<state_dir>/releases/`: `state.json` (`current`, `last_good`, `pending`, the
recorded launcher), `previous` (the release that was last good before
`last_good`) and one directory per staged version. `previous` is a file of its
own because a launcher from v2.6.0-beta4 refuses any field in `state.json` it
does not know.

- **Stage** (`POST /v1/update/stage {"version": "vX.Y.Z"}`): the node downloads
  `SHA256SUMS`, its signature and this host's archive from
  `https://github.com/janit/viiwork/releases/download/vX.Y.Z/` (the only
  `update.source` a node accepts; a redirect is followed only to GitHub's
  asset hosts), verifies the signature for that version against
  the compiled-in keys, checks the archive, unpacks `viiwork` and
  `viiwork-accept`, runs the new binary with `--version` (it must run here and
  report that version) and `--engine-requirements` (this host's engines must be
  new enough). Anything that fails leaves nothing behind.

  `SHA256SUMS` and its signature always come from GitHub when GitHub
  answers. The archive comes from the node's viiwork-parrot
  (`viiwork_parrot.api`, `POST /ensure-release` with the other mesh members
  as peer hints), checked against GitHub's sums; when parrot is absent,
  refuses, shows no progress for 2 minutes, has not finished after 5, or
  hands over an archive that does not match, the node logs why and downloads
  the archive from GitHub (a parrot answering 503, which has no feed yet, is
  waited out within those limits). The host's engine helper on a Docker
  install stages through the same parrot. Only while GitHub is unreachable —
  no connection, a 30 s timeout, a connection dropped mid-answer, 5xx, 429 —
  does the node take parrot's copy of the signed files, verified against the
  same keys. A definite answer from GitHub (404, 403, a bad signature) is
  final, and so is any other answer: a redirect off GitHub, or signed files
  over their size cap.

  On a Mac set up by `viiwork init` (its `install.json` names a llama root),
  staging first reads the new binary's `--build-info`. When it carries a
  llama.cpp pin with its macOS digest, the node fetches that build into the
  llama root from the release's fixed URL, with no GitHub API call, and
  requires the digest to match; the engine requirement is then checked
  against that build, which is the one the release will run.
- **The Mac's engine follows the binary.** A model whose `llamacpp.binary` is
  exactly `<llama root>/<tag>/llama-<tag>/llama-server` runs the build of the
  pin the *running* viiwork carries, when that build is present, and the
  configured one otherwise. Activating a release therefore moves the engine
  with it, and a rollback moves it back, with nothing recorded. A binary
  anywhere else is used as written. When a release confirms, builds under the
  llama root that no kept release runs (current, last good, previous, the
  floor, this process, and any tag the config names) are removed; if any of
  those cannot be read, nothing is.
- **Activate** (`POST /v1/update/activate`): refused while any model is still
  loading. Otherwise it records the backends healthy right now as the
  baseline, marks the version pending, answers 202, shuts down in
  the usual order and execs the launcher, which runs the staged release in the
  same process.
- **Confirm**: the new release becomes last good once every baseline backend is
  healthy again. A baseline backend going dead, or the window passing
  (`update.confirm_timeout`, else the sum of the models' startup timeouts plus
  10 minutes, fetching time excluded), rolls it back. A release that crashes
  before it can confirm is started twice; the third start runs last good.
  Every start counts, a routine restart during the window included.
- **Rollback** (`POST /v1/update/rollback`): back to last good at once. On a
  host already on its last good release, back to the release that was last
  good before it (`previous` in `GET /v1/update`), which becomes last good
  again; refused when there is none, or its binary no longer verifies. It is
  remembered for that one last good release only, so after a newer image
  resets the node to its builtin binary there is nothing to go back to.
- A newer image or binary installed out of band always wins over an older
  staged release. A floor that *is* the pending release (the same version,
  exactly) runs it and keeps it pending, so it is still confirmed, not
  forgotten. A floor the node installed itself by following a confirmed
  release (a Mac, below) is not out of band: it is recorded in
  `releases/cli.json` with its sha256, and while the launcher is exactly that
  file the state still decides, so a rollback below it runs.

Writes need a meshauth signature in a secured mesh and come only from loopback
in an open one, exactly like alias writes. `GET /v1/update` reports the state,
the staged versions and the installed engine versions.

### The host's CLI follows the release

`viiwork update` moves the node; the `viiwork` on the host's PATH is a
separate file, and an old one lacks newer commands and asks for older
confirmation phrases. Since v2.7.2 it follows the release, only ever forward
(a CLI at or past the release, or a developer's build, is left alone), and
always by one rename in its own directory, with the CLI it replaced kept as
`<path>.prev` (overwritten each time):

- **Docker install made by `viiwork init`:** the engine helper does it (below),
  as root, from its own verification, to the path `install.json` records as
  the install's binary (`/usr/local/bin/viiwork`) and nowhere else.
- **Mac install made by `viiwork init`:** the CLI `~/.local/bin/viiwork` *is*
  the LaunchAgent's binary, the launcher that hands over to a staged release.
  The node runs as the user who owns it, so when a release confirms, the node
  installs that staged release there itself (the binary it was handed over
  to, re-checked against the sha256 recorded when it was staged), records it
  in `releases/cli.json` and moves the launcher's recorded sha256 with it.
  Without that record the next start would treat the new launcher as an
  out-of-band upgrade and forget the staged releases, rollback included.
- **Anything else** (a hand-built node, a Docker host not made by
  `viiwork init`, an install whose manifest records no binary): `viiwork
  update cli` by hand, below.

`viiwork update cli` (with `sudo` where the CLI is root's) asks the node on
loopback which release it runs (`GET /v1/update`, `running`), takes that
release's `viiwork` from `<state_dir>/releases/<version>/`, and verifies it as
staging does: the signed `SHA256SUMS` from GitHub, the archive from GitHub or
the host's viiwork-parrot, staged afresh beside the CLI with this binary's
keys, and the node's copy must be byte for byte that release's `viiwork`. It
then replaces the running CLI (links resolved), or `--to PATH`, keeping
`<path>.prev`. `--dry-run` says what it would do; `--state-dir` names the
state directory when the config (`--config`, else this machine's) does not.
It refuses when it cannot write the CLI's directory, when the release is not
in the state directory, or when anything fails to verify. On a host whose CLI
is also the node's own binary (a hand-built systemd node started from it),
the next start treats the new file like any newer binary installed out of
band.

A CLI must itself be v2.7.2 or newer to have the command. To bring an older
one up once, run a v2.7.2-or-newer `viiwork` from a release archive checked
by hand (above) as `sudo ./viiwork update cli --to <the old CLI>` — never the
copy in the state directory, which a container can write and which would run
as root. A Mac needs nothing: the node installs its CLI when v2.7.2 itself
confirms. A Docker install made by `viiwork init` with the engine helper needs
nothing either.

## On a Docker install: the image follows the release

A container cannot replace its own image, so on a Docker install made by
`viiwork init` a small helper on the host does it: `viiwork engine-sync`, run
as root from `/usr/local/bin/viiwork` by `viiwork-engine.path` (when the
node's `state.json` or image request changes) and `viiwork-engine.timer`
(every five minutes). Its one rule: the compose file's image tag is the
node's `current` release, or for `builtin` the tag the install had when the
helper first ran, which it records in its own root-only directory,
`/var/lib/viiwork-engine/sync.json`. There the release *is* the image, engine
included:

- **Stage** also asks the helper, through `releases/engine-request.json`
  (a version and a request id, nothing else), to verify and pull the
  release's image. The helper answers in `releases/engine-result.json`, with
  the engine version the image carries, which the release's engine
  requirement is checked against. A bad image fails the stage, before any
  host restarts, and activation never waits on a pull.
- **Activate** is refused (409) until the helper has prepared that exact
  version's image: stage it first. Activate and rollback then save the state
  and answer 202, and never
  exec a staged binary: the helper swaps the image and `docker compose up -d`
  restarts the node through its ordinary shutdown. The new container counts
  its starts of the pending release and the confirmer decides it as usual; a
  rollback, the confirmer's included, sets `current` back and the helper
  swaps the previous image in (still local, so nothing is pulled). If the
  node is still running two minutes after an activate or rollback — no
  helper, or a broken one — it restores the state it had and logs why.
- **Swap.** The helper verifies the release itself, with the keys compiled
  into the host binary and the same staging code a node uses, into
  `/var/lib/viiwork-engine/releases/`; pulls `<repo>:<version>`; reads the
  image's `/usr/local/bin/viiwork` out of a container it creates and never
  starts; and swaps only when that file is byte for byte the signed
  archive's. It then rewrites the compose file's `image:` line (the file as
  it was is kept as `docker-compose.yaml.bak`) and runs `docker compose up -d`.
  The engine layers under viiwork are trusted as at install time: by registry
  and tag.
- **Only forward, or back to what ran here.** The helper moves to a version
  older than the running tag only when it is the install's own tag or one of
  the last eight it swapped to itself (kept in `sync.json`); anything else is
  refused and logged, pre-pulls included. So a node that writes an older
  release into `state.json` cannot skip `/v1/update`'s consent for a
  downgrade; a rollback goes back to a release this host has run.
- **Disk.** An image already on the host is used, not pulled again. The
  helper removes every image it pulled itself once nothing needs it, keeping
  the running tag, the install's own, the last two it ran and the one
  prepared for the latest stage; an image it did not pull is never removed.
- **Itself, and the host's CLI.** Once the node has confirmed the release its
  image carries, the helper installs its own verified copy of that release's
  binary at the path `install.json` records as the install's binary
  (`/usr/local/bin/viiwork`, which is also the helper), so the host's CLI and
  the helper's own rules move with the release. It writes nowhere else, and
  nothing when the manifest records no binary. It skips a CLI whose
  `--version` already reports the release or a newer one, never takes the
  node's staged copy (the container can write it), replaces the file by
  rename and keeps the old one as `viiwork.prev`, and logs what it did. A
  release that fails to verify leaves the CLI and its `.prev` as they were.
  (Before v2.7.2 it wrote `/usr/local/bin/viiwork` unconditionally, compared
  its own version rather than the file's, and kept `viiwork.bak`.)

**What it trusts.** The helper runs as root, and the node's releases
directory is the container's to write. From it the helper takes `state.json`'s
version names and an image request's version and id, each validated as
exactly that, and nothing else: never a path, a repository or a URL. It reads
them through an `os.Root` on the state directory, refuses a link anywhere
there, reads only small regular files without blocking, and replaces its
result file by rename, so a planted link can make it neither read nor write
elsewhere. The repository comes from the host's compose file (and must be one
of the published `ghcr.io/janit/viiwork-*` images), the download root is
GitHub's (the only `update.source` a config may name), and where things are from the host's
`install.json`. A compromised node can still ask for any *signed* release,
newer than the running one, or one this host has already run.

A node knows it is on such an install from the `engine_helper` entry in the
`install.json` its container sees in `/etc/viiwork`. Anything else — a
hand-built node, or an install from before v2.6.1 until `sudo viiwork init`
adds the helper — updates its viiwork binary only, as before: the engine
stays as installed, and staging refuses a release whose engine requirement
the host does not meet. `journalctl -u viiwork-engine.service` shows what the
helper did and why.

## Rolling a release across the mesh

```sh
viiwork update status                        # every member: running, current, last good, previous, pending, engines
viiwork update                               # the latest signed release, everywhere it is enabled
viiwork update --to v2.6.0 --hosts node-a,node-b   # one version, some hosts
viiwork update --parallel 3                  # activate up to three hosts at a time
viiwork update rollback --host node-b              # one host back a release
```

After a host confirms, the rollout also waits until the node it talks to sees
that host alive on the new release, so a release that breaks mesh membership
stops the rollout at the first host.

A rollout lists its plan — every member with what will happen to it, or why it
is skipped (not alive, updates off, already there, newer, older than rolling
updates) — and asks for `YES I WANT TO UPDATE THE NODES ABOVE` (or `--confirm`). It
then stages the release on every chosen host — all at once, at most eight
together, since staging changes nothing a host runs — so a download, signature
or engine problem stops everything before any host restarts, and activates them
one at a time, the node it talks to last. Each host must come back on the new
release and confirm it before the next starts; a host that rolls itself back,
or misses its own confirmation deadline, stops the rollout with the rest
untouched.

`--parallel N` activates in waves of up to N hosts instead, and waits for
every host of a wave before the next. The node the CLI talks to is still
last and alone. No wave holds every alive host that lists a model in its
status, counting hosts outside the rollout, so a model served by two or more
machines never goes offline; a model on one machine is offline while it
restarts, as it is one at a time. The plan shows the waves before the phrase.
If any host of a wave fails, the CLI still follows the rest of that wave,
reports where each host ended up, and starts no later wave. In a secured mesh, run it where the mesh secret is loaded. On a Linux node
installed by `viiwork init`, `mesh.env` is readable by root only:
`sudo sh -c 'set -a; . /etc/viiwork/mesh.env; /usr/local/bin/viiwork update'`.
On a Mac installed by `viiwork init`, the CLI reads the secret from the node's
LaunchAgent. Without a secret the CLI says so before asking for anything. An open mesh accepts update writes only
from each node's own machine, so there a rollout updates only the node the CLI
talks to — run it on that machine against `127.0.0.1` — and lists the others as
skipped. A version below a host's installed binary is skipped too: the node's
newer-binary rule would undo it at the next start. Names in `--hosts` that are
not members are refused.
