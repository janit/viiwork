# Changelog

## v2.9.2

**The status names each model's engine for people and says which version of it the machine runs.**

Two new fields on every entry of `models[]` in `GET /v1/status`, and so on
every `members[].status.models[]` of `GET /v1/cluster` and of the pushed
`/v1/mesh/stream` snapshots. Nothing else on the wire changed, and no request
parameter is needed to get them:

```json
{ "name": "some-model-27B", "engine": "llamacpp",
  "engine_name": "llama.cpp", "engine_version": "b11371",
  "slots": 2, "busy": 1, "queued": 0, "ctx": 49152 }
```

- **`engine`** is what it was: the stable id (`llamacpp`, `vllm`,
  `freetoken`, `strata`). Key and group on it. It never changes spelling.
- **`engine_name`** is the name to show: `llama.cpp`, `vLLM`, `FreeToken`,
  `Strata`. A string, present on every model a v2.9.2 node reports, parked
  models included. Each engine declares its own name in its own package
  (`engine.DisplayNamer`, which the conformance kit now requires of a new
  engine), so a reader needs no table of names and a new engine brings its
  name with it. For an id the node has no engine for, it is the id.
- **`engine_version`** is the version of the engine that machine runs, in the
  engine's own spelling: a llama.cpp build as `b11371`, a vLLM release as
  `0.31.0`. It is the value `GET /v1/update` reports under `engines` for that
  engine, now in the cluster snapshot, so a reader no longer needs one call
  per machine. A string, or **absent**. It is absent:
  - for an engine that cannot report a version (FreeToken, Strata);
  - when the engine's binary could not be run or printed no version;
  - for the first moments after a node starts, until the node has read it
    (about as long as the engine's `--version` takes; vLLM's is the slowest);
  - when what the engine printed is not a plain version. Only letters,
    digits and `. + _ -`, at most 40 characters, are published, because what
    an engine prints after its number can be a path or a host name, and a
    status is shown on public dashboards.

  Absent is "cannot say". It is never `""` and never a guess.
- **How to read them.** Show `engine_name` where there is one and `engine`
  where there is not, then `engine_version` where there is one:
  `llama.cpp b11371`, `Strata`, or `llamacpp` from a node older than v2.9.2,
  which sends neither field. A mesh of mixed versions needs no other rule.
- **When the values change.** The node reads its engines' versions once,
  in the background, when it starts (before v2.9.2: on the first
  `GET /v1/update`). Building a status never runs or waits for a process.
  The version is per engine and per process: a node that swaps its engine
  restarts to do it, and reports the new version after that restart. A model
  of an engine the node did not run at start, added by a reload, has no
  `engine_version` until the node restarts, and two models of one engine
  with different binaries both report the first one's version.
- `/v1/capacity` and `/v1/fleet/capacity` are unchanged: they carry `engine`
  only.

## v2.9.1

**Engine pins: Strata v0.1.42 and vLLM v0.31.0, and more than one slot per Strata backend.**

- **Both Strata images build Strata v0.1.42** (`docker/pins.env`, commit
  `61b3fb5d`). Since v0.1.40.1 upstream fixes a deadlock with more than one
  request waiting on a slot, evicts old conversation-cache entries instead of
  dropping a snapshot, and runs one pipeline group per GPU for batched
  requests. `/slots`, the defaults the node assumes and the JSON keys it
  checks are in v0.1.42 as in v0.1.41.
- **v0.1.42 served under the node on CUDA**, on an RTX 5090 and on two RTX
  4090 in a layer split, IQ3_XXS at 262,144 context, from images built on
  each machine: one to six slots, prompts up to 87,000 tokens, no failed
  request and no respawn. Against v0.1.40.1 on the RTX 5090 with one slot, a
  request alone decoded at 227 tokens per second where it had 207, and
  prompts were read as fast as before (4,060 tokens per second at 87,000
  tokens). **The gfx906 image builds at v0.1.42 and has not served a model
  on a Radeon VII**: what is measured on Radeon VIIs below is v0.1.41.
- **What more slots cost and buy, measured at v0.1.42.** Every slot keeps the
  model's full `context`. On the RTX 5090 each slot after the first took
  about 1 to 2 GiB of VRAM from the expert cache (24.4 GiB with one slot,
  20.6 with four) and 3 to 7 GB of RAM, and prompts were read about 25%
  slower with any number above one; on two RTX 4090 about 10% slower. Four
  short requests at once, four slots against one: all four started within 2
  seconds instead of the last waiting 6 to 7 for its turn, each decoded at
  about 53 tokens per second instead of about 200, and the total was the
  same within 15% (184 against 195 tokens per second on the RTX 5090, 186
  against 165 on two RTX 4090). Two and three slots gave less in total than
  either one or four on both machines. Four prompts of 7,000 tokens at once
  finished later with four slots than with one on the RTX 5090.
- **The gfx906 patch is down to one fix** (`docker/strata/gfx906-v0.1.42.patch`):
  the two stream-priority aliases. Their one use is compiled out of a HIP
  build, and the engine was seen to compile for gfx906 without the patch as
  well as with it; it stays for this release. The checkpoint copy on a
  stream of the saving thread's own, the fix for `saving a checkpoint part
  failed`, is upstream in v0.1.42, and the two compile fixes v0.1.40.1 needed
  are upstream since v0.1.41.
- **On NVIDIA two new upstream defaults change answers slightly**, on a
  backend of one GPU or a layer split whose experts do not all fit in VRAM
  and that has one slot: missed experts ranked 7th or lower are skipped
  (upstream measured 13 to 16% faster decoding), and on one GPU the PCIe
  share is set from what the CPU measures in the first decode windows.
  `"STRATA_ROUTE_TAIL_SKIP": "0"` and `"STRATA_PCIE_FRAC_DEFAULT": "old"` in
  the JSON's `env` block restore v0.1.41's answers. Neither applies on AMD
  or with `"parallel"` above 1.
- **`"replicas"` in the Strata JSON is not checked.** Upstream's new opt-in
  runs several engines on card groups behind one server; the node has not
  been run against it, so leave it unset.
- **Served at v0.1.41 on Radeon VIIs under the node**, five cards (four
  Radeon VIIs and an Instinct MI60 in one layer split), IQ3_XXS at 262,144 context: loaded in
  five minutes, a 76,087-token first prompt after the load read at 885 tokens
  per second and answered correctly, the next turn reused the cache (2.3
  seconds to the first token), a tool call and streamed reasoning, 34 to 42
  tokens per second generated, no respawn.
- **The CUDA image served at v0.1.41** on two RTX 4090 at 262,144 context: a
  76,087-token prompt read at 6,482 tokens per second, about 200 tokens per
  second generated, cache reuse and a tool call. **Build it with BuildKit,
  on the machine that runs it** (`BUILDS.md`): Docker's legacy builder skips
  the step that compiles the engine and reports success, and the engine is
  now compiled for the building machine's CPU. On one NVIDIA GPU upstream
  lets the CPU take part of a short prompt, which changes the last bits of
  an answer; `"STRATA_PREFILL_CPU_SHARE": "0"` in the JSON's `env` block
  turns it off.
- **A Strata model can have more than one slot.** `models[].parallel` from 1
  to 8, with the same `"parallel"` in the Strata JSON. v0.1.41 lists its
  batch slots on `/slots` where earlier versions listed one whatever the
  file said, and the node publishes the count it finds there: the engine
  runs fewer slots than asked when they do not fit in VRAM. Each slot has the
  model's full `context`. A request that runs alone is decoded outside the
  slots, so the node's own count of requests in flight is what marks that
  slot taken. On an older Strata such a model publishes one slot. Measured
  with two slots on two RTX 4090: two requests at once generated 85 to 90
  tokens per second each, against 120 to 200 for one alone, and a third
  waited at the node. Two long prompts are still read one after the other,
  and a slot's decoding pauses while another's prompt is read: on five
  Radeon VIIs under agent traffic, two slots per backend dropped requests
  to between 1 and 7 tokens per second, where one slot gave 16 to 40.
- **vLLM v0.31.0** (`VLLM_VERSION`). The image's `vllm serve` still takes
  every flag the node generates, and none is in upstream's list of removed
  or renamed options. It has not served a model on a fleet host.
- llama.cpp stays at b11371 and FreeToken at 0.1.3, which is still upstream's
  latest.

## v2.9.0

**A request can name the machines it would rather run on.**

- **`?prefer=node-a,node-b`** on an inference endpoint names nodes in order of
  preference. The first one with a free slot for the model runs the request,
  ahead of performance routing and of session affinity; when none of them has
  room the request routes exactly as it would without the list, and a queued
  request walks its list again each time a slot frees. Unlike the `?host=`
  pin it never fails a request: an unknown name, a host that is off or one
  that is full is passed over. A malformed value, or more than eight names,
  is `400`.
- **`X-Viiwork-Prefer`** carries the same list for a client that can set a
  header but not a query parameter. The parameter wins when both are present,
  `?host=` wins over both, and the header is not sent on to a member or an
  engine.
- A session that ran elsewhere while its preferred host was full moves to
  that host on its next turn and reads its prompt there again; send no list
  with a session whose cache matters more than the machine.

## v2.8.3

**Strata v0.1.40.1, and the Radeon VII checkpoint failure fixed.**

- **Both Strata images build Strata v0.1.40.1** (`docker/pins.env`). Upstream
  rewrote its git history on 2026-10-06, which moved every tag: the v0.1.39
  commit that v2.8.0 to v2.8.2 pin is no longer what the tag names, so
  `make docker-strata` of those versions refuses to build.
- **`saving a checkpoint part failed` is fixed in the gfx906 image.** It was
  the first long prompt after a load, on a backend of more than one card.
  The engine saves a checkpoint every 16,384 prompt tokens with a copy on the
  default stream, while another thread is still capturing its prompt graphs.
  CUDA lets that through; the ROCm runtime refuses it (`operation would make
  the legacy stream depend on a capturing blocking stream`), and the refusal
  invalidates the capture as well. The image's gfx906 patch makes the copy on
  a stream of the saving thread's own. On nine Radeon VIIs, v0.1.40.1 without
  the fix failed that prompt two times in two; with it, seven cold first long
  prompts were read (41,000 to 47,000 tokens, six of them three at once on
  three backends), and a 200,244-token prompt at 837 tokens per second.
- **v0.1.40.1 does not compile for gfx906 as released.** The patch carries
  three more fixes for that ([BUILDS.md](BUILDS.md)); the two v0.1.39 needed
  are upstream.
- **The 30-second warm-up stays at its default** and still does not matter to
  this failure; `strata: {warmup: 0s}` turns it off.
- **The CUDA image served at v0.1.40.1** on three RTX A4000 at 262,144
  context: a 200,244-token cold prompt read at 2,527 tokens per second, a
  41,221-token first prompt after the load at 2,474, 57 to 74 tokens per
  second generated, cache reuse, streamed reasoning and a tool call, with no
  respawn. It needs no patch.
- **Measured, no pin change:** llama.cpp b11451 against the pinned b11371 on
  two Radeon VIIs (Qwen3.8-27B, 98,304 context): the same answers, tool call
  and cache reuse, the same decode speed, prompt reading 200 against 208
  tokens per second at 41,000 tokens. Nothing to gain; the pin stays.

## v2.8.2

**Fixes from a six-lens review** of the tree, mostly of what v2.7 and v2.8
added: security, error handling, type safety, performance, architecture and
simplicity. No new keys; nothing to change in a config.

- **Routing: a host can no longer publish a prefill rate of zero.** When most
  long prompts in a host's window answered within its overhead, the fitted
  rate was zero, was published as 1 ms per 1,000 tokens and was saved as the
  host's baseline. Every other node then predicted that host an order of
  magnitude faster than any other for a long prompt and sent it all of them.
  Such a window now measures no rate, a saved baseline without one is dropped
  at start, and a peer's score with a negative overhead is not used.
- **Routing: a request that waited in the queue keeps its session.** The
  queue dropped the session header's key and the prompt's size, so on a busy
  fleet a session's turn went to whichever host the choice fell on, onto a
  cold prompt cache.
- **Routing: a think block that is generated and held back is not counted as
  prompt reading.** With `think` off, a llama.cpp model's `<think>` block is
  suppressed; the performance sample ran to the first byte the client saw,
  so a model that reasons made its host look slow at reading prompts.
- **Updates: `viiwork update` no longer stops on two refusals that are not
  failures.** A host with a model still loading (after a crash, a rollback or
  a restart, or in a Strata warm-up) is asked again until the model is
  healthy, for up to 30 minutes, and a host another rollout already put on
  the release counts as done. Both used to end the rollout with
  `activate failed: HTTP 409` and the fleet on two versions.
- **Updates: parking or removing a model during a release's confirmation
  window no longer rolls the release back.** The confirmer waited for that
  model's backends, which were gone on purpose, until the deadline.
- **Updates (Mac):** the record of the CLI a node installed is written before
  the binary is put in place. A failure between the two made the next start
  read the new launcher as an out-of-band install and forget every staged
  release.
- **`POST /v1/models/down` and `up` refuse a body that names no models by
  accident.** No models means every model, so `{"model": "x"}`, a list or
  `null` parked the whole node. `{}` and no body still mean every model;
  `viiwork down` and `up` always sent the right key.
- **`viiwork up` after editing a Strata JSON drops the model's saved
  performance baseline**, as the documentation said it would. Only a reload
  or a restart did.
- **An engine process no longer inherits the node's secrets.** The mesh
  secrets, `ENTSOE_API_KEY` and the BMC password are taken out of the
  environment a backend starts with. A model that needs one of those names
  sets it in its own `env:`.
- **A Strata backend that stops answering ready during its warm-up** no
  longer shows `warming up` while it is not loaded.
- **`perf.json` is written when a baseline's values change, and otherwise at
  most every ten minutes.** A busy node rewrote it, with two fsyncs, every
  minute.
- **Docs:** routing by measured speed, the session headers and the `routing`
  keys ([mesh.md](docs/mesh.md#routing),
  [configuration.md](docs/configuration.md#routing-routing)); the score,
  `parked`, `output_tokens`, `gen_ms` and the park endpoints
  ([api-integration.md](docs/api-integration.md)).

**A correction to v2.8.1.** The 30-second Strata warm-up does not prevent
`saving a checkpoint part failed`: under v2.8.1 a backend that had been ready
for five minutes still failed its first long prompt that way. The cause is
inside Strata v0.1.39 and is still not known. The wait stays, as it costs 30
seconds per load, but it is not a workaround; `strata: {warmup: 0s}` turns it
off.

## v2.8.1

**A Strata backend waits 30 seconds after loading before it takes work.**

- **`strata: {warmup: 30s}`** is the new key, with that default; `0s` turns
  the wait off and `10m` is the most it accepts. A backend whose model has
  loaded shows phase `warming up`, stays in `starting`, takes no request and
  keeps its place in the load gate until it has answered ready for that long
  without a break. The wait counts against `startup_timeout`, and it applies
  to every start of a backend, respawns included.
- **Why:** on Radeon VIIs with Strata v0.1.39, a long prompt that reached a
  backend within 30 seconds of its `/health` saying loaded failed at its
  first checkpoint (`saving a checkpoint part failed`), the request ended in
  an error and the backend had to load again. That happened three times; a
  backend whose first long prompt came later read it. The cause inside Strata
  is not known, so this is a wait and not a fix.
- **For engine authors:** the wait is an optional capability,
  `engine.WarmUpper` ([adding-an-engine.md](docs/adding-an-engine.md)). No
  other engine declares one, so nothing changes for them.
- **Measured, no code:** with three Radeon VIIs per backend the whole KV cache
  of a 262,144-token context fits on the cards, and leaving `--kv-resident`
  out of the Strata JSON raised three coding sessions' total output from 37
  to 60 tokens per second ([models.md](docs/models.md#strata-one-moe-family-across-several-cards)).

## v2.8.0

**A fourth engine: Strata.** `engine: strata` runs
[Strata](https://github.com/Niko1221/Strata) (Qwen3.8-Flash-Next, its layers
split across a backend's cards) as an ordinary model on the mesh.

- **`models[].path` is Strata's own JSON config**, written by the operator or
  by upstream's `setup.py`. One file serves every backend of the model: the
  node passes host, port and cards as flags. The model's block is
  `strata: {dir: <the Strata checkout>, python: python3}`.
- **A backend refuses to start when the file disagrees with the node**, and
  the error names the key: the model's name must be `model_name` or one of
  `aliases`, `--max-context` in `args` must equal `context` and be written as
  two arguments (Strata does not read `--max-context=N`), and `parallel`,
  `lazy_load`, `idle_unload_s` and `api_key` must be off. The file is read at
  launch; after editing it, `viiwork down` and `up` the model.
- **`parallel` must be 1.** Strata v0.1.39 reports one slot whatever its own
  `parallel` says.
- **Cards:** on Radeon the engine numbers them inside the node's pin; on
  NVIDIA it passes the real card numbers, because Strata's server sets
  `CUDA_VISIBLE_DEVICES` itself.
- **A streamed reply is passed through as the server sent it**: reasoning in
  `delta.reasoning_content`, the answer in `delta.content`, whatever `think`
  says. The `/chat` page shows the reasoning as it arrives, and a client that
  does not know the field ignores it. The plain (non-streamed) reply drops
  the reasoning, and other engines keep the rule they had.
- **Editing a Strata JSON drops the model's saved performance baseline**, as
  an `args` change does: the pack, KV format and layer split that decide its
  speed live in that file. The file is read again at start and on reload.
- **Images:** `make docker-strata` (gfx906, with the two-line HIP compile fix
  v0.1.39 needs) and `make docker-strata-cuda` (on upstream's own image).
  Serving was verified on ten Radeon VIIs, and the CUDA image on three RTX
  A4000 at the model's full 262,144-token context: a 258,663-token cold
  prompt answered in 88.5 s, and four minutes each of steady load, growing
  sessions and overload on one slot gave no error other than the 429 an
  overfull queue is meant to return. A killed backend was respawned and
  serving again in 23 s.
- Known limit: Strata's server ends a streamed reply by closing the
  connection, so a stream cut short by a backend that died ends cleanly
  instead of as an error. A complete stream ends with `data: [DONE]`.

**The `/prompt` page shows the reply's size and average tok/s**:
`296 tokens · 57.5 tok/s`, measured from the first generated token to the
end, thinking included even when the reasoning is not sent. Where the node saw
no first token (a plain reply, or a request another node executed) the rate is
over the whole request and says so.

**Engine pins: llama.cpp b11371 and vLLM v0.30.0.** b11371 replaced
`--no-mmap` with `--load-mode none`; the node reads the binary's `--help` and
generates whichever the binary takes.

- An operator's own `--no-mmap` in `args` is passed as `--load-mode none` to a
  build that no longer reads `--no-mmap` (b11371), instead of letting the
  backend exit at argument parsing.
- Respawns that ask about one binary at the same moment share one `--help`.
- A `--help` that cannot be read is not remembered: the binary is asked again
  after 30 seconds, and until then the operator's flags are left as written.

**Thinking**

- A plain reply whose answer is a tool call no longer carries the model's
  reasoning in `content` beside the call.
- Keep-alive comments from a backend are flushed to the client as they arrive
  while thinking is suppressed.

**Fixed**

- A backend whose server writes JSON with a space after the colon (a Python
  server, Strata among them) never marked a first token, so its host never
  earned a performance score.

**Changed**

- `memberlist` v0.7.0.
- For engine authors: `enginetest.Case.NameInConfig`, for an engine whose
  server takes the model name from a config file, and two optional
  capabilities, `ReasoningSeparator` and `PerfKeyer`
  (`docs/adding-an-engine.md`).
- For API consumers: `PromptEntry` gains `output_tokens` and `gen_ms`, both
  omitted when not known.

## v2.7.2

**The host's `viiwork` CLI follows the release.** `viiwork update` moved the
node but left the CLI on the PATH behind, so an old one lacked newer commands
(`down`, `up`) and asked for an older confirmation phrase.

- **Docker install made by `viiwork init`:** after the node confirms a
  release, the engine helper installs its own verified copy of it at the
  binary path `install.json` records, and nowhere else. It now checks what the
  file's `--version` reports, not its own version, keeps the old file as
  `viiwork.prev` (was `.bak`), and leaves the CLI and `.prev` untouched when
  the release fails to verify.
- **Mac install made by `viiwork init`:** the CLI is the LaunchAgent's binary.
  When a release confirms, the node installs that staged release there itself
  and records it (`releases/cli.json`), so the next start keeps the state and
  a rollback still runs, instead of treating the new file as an out-of-band
  upgrade.
- **`viiwork update cli`** does it by hand on any host with updates set up:
  it asks the local node which release it runs, verifies that release's
  `viiwork` from the state directory against the signed release with the
  stager's own code, and replaces the running CLI (or `--to PATH`) in one
  rename, keeping `<path>.prev`. `--dry-run`, `--state-dir`. Run it with
  `sudo` where the CLI is root's.
- Every path only ever moves the CLI forward.

## v2.7.1

**`viiwork down` and `viiwork up`: free a host's GPUs without leaving the mesh.**

- **`viiwork down [model...]` parks this machine's models**, every configured
  one or those named. The node stops admitting requests to them, gives the
  ones in flight `health.respawn_grace`, then stops their engines. It stays in
  the mesh: dashboards, `viiwork top` and routing to other hosts keep working,
  and members send those models elsewhere.
- **`viiwork up [model...]` loads them again** from the running config through
  the load gate and returns once they are loading.
- Parking lasts until `viiwork up` or a restart, and survives a reload. A
  parked model shows as `down` in `viiwork top` and on `/mesh`, and as
  `parked: true` with no backends on `/v1/status` (additive, `omitempty`).
- Do not run `down` while `viiwork update` waits for this host to confirm a
  release: the parked model's backends never come back, so the update rolls
  back.
- `stop`/`start` take the whole node out of the mesh; `down`/`up` only its
  models. Both new commands reach the node and load the mesh secret exactly
  as `viiwork update` does; the node authorises them like alias writes
  (`POST /v1/models/down`, `/v1/models/up`).

## v2.7.0

**Routing follows measured speed, and a session stays on its warm cache.**
Everything from v2.7.0-beta1 to beta5 below, plus one fix found in the last
test.

- Every host measures its own time to first token and publishes it in
  `/v1/capacity`. Routing picks by predicted speed among hosts with a free
  slot (`routing.performance`, default on; `false` is the old routing).
- Hosts within 1.25× of the best count as equal. Among them a session's turns
  go to one host, chosen the same way from any entry node, and inside that
  host to one backend.
- Scores are fitted by medians, so one slow request or a host's mix of short
  prompts does not set its score.
- The prompt history records coding-agent turns: their tool results as the
  prompt, their tool calls as the output.
- Everything from v2.6.2: parallel staging and `--parallel N` waves for
  `viiwork update`.

**New since beta5: a session follows its host before any score exists.** A
fresh start, or a change to a model's flags, drops every host's score. With no
score anywhere, routing fell back to local-first and ignored the session, so a
session's turns crossed hosts until the scores came back. Now a session goes to
its own host among those with a free slot, the same host it gets once scores
exist. A request without a session is still served locally first.

**Measured on two identical hosts** (gb2 and gb3, four Radeon VII pairs each,
16 coding-agent sessions entering through gb2, 28 minutes):

| | mean TTFT | p90 TTFT | prompt cache hits | turns that moved host / backend |
|---|---|---|---|---|
| beta3 | 25.0 s | 75.0 s | 82.9% | 12% / 7% |
| beta5, once both hosts had scores | 13.8 s | 18.4 s | 94.7% | 4–8% / 0% |
| one host alone, 8 sessions | 13.7 s | 34.3 s | 92.7% | — / 4% |

With scores, two hosts carry twice the sessions at the speed one host gives
half as many. Beta5's first fifteen minutes, before either host had a score,
looked like beta3; that is the fix above.

## v2.7.0-beta5

**A session stays on its warm cache, across hosts and inside one.** A test on
two identical hosts, gb2 and gb3, had 16 coding-agent sessions all entering
through gb2. 12% of turns crossed to the other host and 7% moved to another
backend on the same host, and nearly all of those turns started on a cold KV
cache. Mean time to first token was 25.0 s, against 13.7 s for 8 sessions on
gb2 alone.

- **The session's host comes first.** In beta3 and beta4 the entry node served
  any request it had a free slot for, before looking at the session. Any free
  slot on gb2 pulled back a session whose cache was on gb3. Now a request with
  `X-Session-Affinity` or `X-Session-Id` goes to its session's host in the
  band, this node included. A request without a session is served locally if
  this node is in the band, as before.
- **A session keeps its backend.** Inside a node, the backend used to be the
  one with the most free slots. Now a session returns to the same backend
  while it has a free slot, chosen the same way as the host. A forwarded turn
  carries the session too, so it lands on the same backend on the receiving
  host.

**Scores are steadier.** Two identical hosts published 2.1 s and 5.1 s of
fixed overhead, and on another host one slow request became the score.

- **A short prompt's prefill is not overhead.** Overhead was the median time
  to first token of prompts under 512 uncached tokens, prefill included: up to
  3.5 s on a Radeon VII pair. Each sample's own prefill is now taken off, and
  overhead and rate are fitted against each other.
- **Medians, and enough samples.** The rate is a median rather than a ratio
  of sums. The window's overhead needs at least three short samples, and only
  five long samples replace the saved baseline. A single 130 s request no
  longer sets a host's score.

**The prompt history shows what a turn added.** For a coding agent's turn the
prompt page showed the original task, the same on every turn. It now shows
the turn's new input: each tool result, labelled with the tool it answers,
and any user message sent in the same turn. A chat turn still shows its user
message.

## v2.7.0-beta4

**The prompt history records coding-agent turns.** Requests from agent
clients such as pi opened to an empty prompt page.

- **Prompt text from content parts.** pi sends every user message as an array
  of content parts rather than a plain string. The prompt history read only
  strings, so it stored no prompt for these requests. It now joins the parts'
  text. Image parts contribute nothing.
- **Tool calls are kept as output.** Most agent turns answer with tool calls
  and no text, so they left no output either. Tool calls are now recorded, one
  per line as `name arguments`, both from streamed and whole responses. When a
  response carries more than text, the output labels its parts
  `[reasoning]`, `[answer]` and `[tool calls]`; a plain text answer reads as
  before.
- **The `/mesh` prompt link names the backend.** A finished request's link
  carried "Qwen3.8-27B/0 done (1m23.272s)" as its destination. It now keeps
  the destination from the request's start.

## v2.7.0-beta3

**Performance routing keeps a session on one host.** An A/B on the fleet
with 8 coding-agent sessions, routing by speed off and on, split two ways.
Sessions entering through teddy, clearly the fastest host, improved: mean
time to first token 13.0 → 9.6 s. Sessions entering through gb2, where gb1–gb4
score about the same, got much worse: mean 16.8 → 34.9 s, p90 36 → 87 s, and
the prompt cache hit rate fell from 90.5% to 72.7%. The random draw spread
each session's turns across near-equal hosts, so most turns started on a cold
KV cache. Host switches between consecutive turns of a session doubled.

- **Near-equal hosts form a band.** Only hosts whose predicted time to first
  token is within 1.25× of the best one are considered. A clearly faster host
  still wins outright.
- **This node first.** If this node is in the band, it serves the request
  locally.
- **Then the session's host.** A request carrying `X-Session-Affinity` or
  `X-Session-Id` goes to the band member chosen by a rendezvous hash of the
  session and the host name. Every entry node makes the same choice, so a
  session returns to the same host while it has a free slot. When that host is
  full the next one in the band takes the turn, and the session goes back once
  a slot frees up.
- **Otherwise the weighted draw, over the band only.** A slower host outside
  the band is no longer drawn at all. The 5% trickle to unscored hosts is
  unchanged.

## v2.7.0-beta2

**Performance routing now measures llama.cpp models that answer through
`reasoning_content`.** In beta1, a request without the `think` field (most
clients) never produced a sample on such a model: granite-4.2-8b on gb1 and
n100, for example. The fleet therefore showed no scores. Without `think`,
viiwork renames `reasoning_content` to `content`, and it writes the new key and
its value as separate pieces. The first-token check looked at each piece on its
own and never saw one. It now also checks where consecutive writes meet. The
extra check uses a small fixed buffer, so the per-token path still allocates
nothing, and the bytes the client receives are unchanged.

## v2.7.0-beta1

A pre-release, published so the fleet can measure performance routing before
v2.7.0. It also carries everything listed under v2.6.2, which was never
published on its own.

**Routing follows measured speed.** Hosts differ: an A4000 pair prefills about
three times faster than a Radeon VII pair. Until now the router only counted
free slots and always served on the entry node first.

- **Every host measures itself.** The node that runs a request records its
  time to first token, from the moment a local slot takes the request to the
  first streamed chunk that carries text or a tool call. The time is split
  into a fixed overhead and milliseconds per 1,000 uncached prompt tokens.
  Queueing and the network hop are left out, because free slots and RTT
  already cover them.
- **Scores are shared through the capacity report.** `/v1/capacity` carries
  three new fields: `ttft_overhead_ms`, `prefill_ms_per_1k` and
  `perf_samples`. All are additive and omitted when the node cannot say, so a
  node one version behind simply publishes no score.
- **The last ten minutes count, with a remembered baseline.** A score comes
  from one-minute buckets, ten at most. When fewer than five samples are
  recent, it leans on the host's last well-sampled value, which is saved in
  `state_dir/perf.json`. Changing a model's engine, weights, GPUs or args
  drops the saved value.
- **Every node picks by predicted speed.** Among hosts with a free slot, this
  node included, a node picks at random, weighted by 1/prediction³. The
  prediction is overhead + rate × prompt size + RTT. A host twice as fast gets
  about eight times the traffic, and slower hosts still get enough to stay
  measured.
- **New hosts earn their place.** A host that publishes no score is priced at
  the fleet median and gets a 5% share of traffic until it is measured.
- **Nothing changes until there is data.** With no scores anywhere, routing is
  exactly as before: the local backend first, then the member with the most
  free slots. Pins, forwards, refusal marks, `stale_after`, reservations and
  the queue all work as before.
- **`routing.performance`** (default `true`). Set it to `false` for the old
  routing. Measuring continues either way, and `/v1/status` still shows the
  scores.

**Engines are asked for token counts.** To know how many prompt tokens were
actually prefilled, the executing node adds `stream_options.include_usage` to
streamed requests for llama.cpp and vLLM. It removes the resulting usage chunk
again for a client that did not ask for it, so that client receives the same
bytes as before. vLLM backends now start with
`--enable-prompt-tokens-details`. FreeToken reports no cached-token count, so
FreeToken hosts are not scored. A side effect: `tokens_total` now also counts
streamed requests from clients that never asked for usage.

**Seeing it.** `/v1/status` shows each model's score under `perf`, `/mesh`
shows each host's prefill rate beside its slots, and `viiwork top` shows it in
the host line.

Before v2.7.0 this beta is measured on the fleet with performance routing off
and on. The measurement records mean and p90 time to first token and the cache
hit rate for each entry node.

## v2.6.2

**Faster rollouts.**

- **`viiwork update` stages every host at once**, at most eight together.
  Staging changes nothing a host runs, so there was no reason to download and
  verify one host at a time. Each host reports as it finishes, and any failure
  still activates nothing.
- **`--parallel N` activates in waves** of up to N hosts, waiting for the whole
  wave to confirm and rejoin the mesh before the next. The default, 1, is the
  old one-at-a-time rollout. The node the CLI talks to is still last and
  alone, and no wave takes down every live host of a model, counting hosts
  outside the rollout. The plan shows the waves before the phrase. A host that
  fails stops the rollout after its wave, with every host of that wave
  reported.

**`viiwork stop` and `viiwork start`.**

- **`viiwork stop` stops this machine's node and every model it runs** (sudo
  on Linux): the node leaves the mesh and drains first, and the command
  returns once its API has gone quiet. On a Mac it boots out the LaunchAgent,
  so KeepAlive does not start it again; on Linux it stops the engine helper,
  then the compose project. It stays stopped until `viiwork start` or the
  next boot or login. Like uninstall it goes by the install manifest, and on
  a host set up by hand it says so rather than guess.

**Releases come from GitHub and nowhere else.**

- **`update.source` accepts only `https://github.com/janit/viiwork/releases/download`.**
  A release was always verified against the compiled-in key, so another host
  could not smuggle code in, but a mirror could still choose which signed
  releases a node saw, withhold them, or watch the fleet ask. Any other value
  is now refused at start, and by the Docker engine helper; the key remains so
  existing files keep parsing.
- **A download follows a redirect only to GitHub's asset hosts**
  (`*.githubusercontent.com`, over https) or within the source's own host.
  Anything else is refused before the other host is asked.

**Releases through viiwork-parrot.**

- **A node takes the release archive from its host's viiwork-parrot**, with
  the other mesh members as peer hints, checked against the signed
  `SHA256SUMS` it fetched from GitHub. Parrot absent, refusing, stalled for
  2 minutes, or wrong: the node logs why and downloads the archive from
  GitHub. Parrot's copy of the signed files is used only while GitHub is
  unreachable, and a definite answer from GitHub (404, a bad signature) is
  final. viiwork-parrot does not serve releases yet, so every node downloads
  from GitHub until it does.

**Fixes from a six-lens review.**

- **Routing:** a refusal mark is cleared, and in-flight forwards are counted,
  from when a capacity report was requested, not received — a poll in flight
  across a refusal carries the peer's state from before it, and sent the retry
  straight back to the member that just refused.
- **Routing:** a forward for a model the receiver does not serve (dropped on
  reload, or a restart with another config) is a 503 refusal the origin retries
  elsewhere, not a 404 handed to the client.
- **Pipelines:** a step refused for capacity answers 429 with Retry-After, not
  502.
- **Discovery:** a node advertises the smallest context its healthy backends
  report, the one every backend the router may choose can honour.
- **/v1/cluster:** a member status older than four polls is dropped, so a
  member whose API is wedged shows no status instead of frozen numbers, and its
  cost stops counting toward the cluster's.
- **Power:** a GPU-sum reading counts only while every card that has reported
  still reports a wattage; a stale or powerless card is not 0 W.
- **Open mesh:** a write must come from a program on the machine — a request
  carrying `Origin`, or naming a non-loopback `Host`, is refused, so a web page
  in a local browser cannot change aliases or trigger an update.
- **`viiwork alias`** reads the mesh secret from a Mac install's LaunchAgent,
  as `viiwork update` does.
- **Updates:** the confirmer keeps polling through an unreadable
  `state.json`; a failed `sudo viiwork init` engine-helper add is not recorded,
  so it can be retried.
- **Performance:** the output capture grows by doubling (a full 2 MiB capture
  allocates 4.4 MB, not 10.8 MB); a failed catalogue upstream is not retried
  for 30 s.

## v2.6.1

**The engine follows the release, and the four issues v2.6.0 shipped as
known are fixed.**

**The engine follows the release.** In v2.6.0 a wizard install's
self-update moved the viiwork binary only.

- **Docker:** the wizard installs a host helper, `viiwork engine-sync`, run as
  root by the systemd units `viiwork-engine.path` and `viiwork-engine.timer`.
  It keeps the compose file's image on the release the node runs: it pulls
  and checks a release's image at stage time, swaps it in on activation, and
  swaps it back on a rollback. It verifies every release's signature itself
  and requires the image's viiwork to be byte-identical to the signed one.
  From the state directory, which the container can write, it takes only a
  version name. Once a release confirms, it also updates
  `/usr/local/bin/viiwork`, so the host's CLI moves with the fleet.
- **A machine installed by v2.6.0 or a beta has no helper.** Its updates move
  the binary only until `sudo ./viiwork init` from v2.6.1 adds it. On an
  existing install that is all init does: the config, the compose file and the
  running node are left alone.
- **The helper goes back only to an image this host has run.** A container
  that wrote an older signed version into `state.json` would otherwise pick
  the release its host runs, past `/v1/update`'s downgrade gate. Rollbacks are
  unaffected; `--allow-downgrade` to a version the host never ran is refused
  there. The helper removes the images it pulled once nothing needs them, and
  activation waits until the release's image is pulled and verified.
- **Mac:** the llama.cpp build is pinned in the signed binary with its
  sha256. Staging fetches the release's build beside the running one, and a
  model running the wizard's llama-server always runs the build of its own
  viiwork's pin, so a rollback takes the engine back too.

**Also fixed:**

- **`viiwork update rollback` works after a host confirms.** It goes to the
  release that was last good before, remembered in `releases/previous`, not
  in `state.json`, so a beta4 or v2.6.0 launcher still reads the state.
  `viiwork update status` shows it.
- **`viiwork top` and `/mesh` drop an in-flight row when its node restarted**
  under the same name. `/mesh` used to keep such a row until the page
  reconnected, even when the member was gone for good.
- **The pinned llama.cpp download makes no GitHub API call,** so it cannot hit
  the API's rate limit. A lookup for any other tag sends `GITHUB_TOKEN` when
  it is set.

**Known:** `viiwork uninstall` on a Mac leaves llama.cpp builds that updates
fetched later under the llama root.

## v2.6.0

**Set up a machine with one command, add the next with one code, update the
fleet with one more.** Everything from v2.6.0-beta1 to beta4 below, tested on
real hardware, plus a documentation pass and an adversarial review of every
new feature.

- `viiwork init`: the first-run wizard. Linux (NVIDIA, in Docker) and Apple
  Silicon (native, under a LaunchAgent). `viiwork join-code`, and
  `viiwork uninstall`.
- Signed, reproducible releases; nodes that update themselves; `viiwork update`
  rolling a release across the mesh one host at a time.
- `viiwork top`, and macOS as a first-class node.

**Tested on real hardware:**

- `init`:
  - an NVIDIA host (three A4000 cards reached through CDI), beta3;
  - an M3 Max MacBook with App Store Tailscale and viiwork-parrot models,
    beta4.
- `join-code` from a second new machine.
- `uninstall` on both.
- A beta3 → beta4 self-update, and a fleet of seven machines taken to beta4
  with rolling updates enabled.

### Since beta4

**Documentation, reviewed as a newcomer would read it.**

- The README's Quick Start and `docs/setup.md`:
  - download, check and run for Linux and the Mac, prerequisites first;
  - what to do when the wizard writes the config only (AMD);
  - the logs on both platforms;
  - a new "Updating" section.
- `docs/releases.md` shows how to verify a signature from a download.
- `docs/configuration.md` no longer says there is no interactive setup.
- `viiwork update` on a Mac reads the mesh secret from the node's LaunchAgent,
  as `join-code` does.
- The usage line lists what each command takes.
- On a Mac, the wizard says how to put `~/.local/bin` on `PATH`.

**Found by an adversarial review, fixed:**

- **`state.json` is read leniently.** The launcher is the oldest binary, and it
  must read what a newer release wrote, or a rolled-back node could not start.
- **A host still loading a model is not activated.** Its baseline would have
  left that model out, so a release that could not load it would have
  confirmed.
- **A rollout waits for each host to be back in the mesh before the next.** A
  release that broke membership would otherwise confirm host after host.
- **llama-server gets `--log-verbosity 1` only when its `--help` lists the
  levels.** An older build reads the number as a threshold that also turns on
  debug, request bodies included, so it keeps `--log-disable`, and prompts
  never reach the log.
- **The wizard's model scan:**
  - it follows a symlinked models directory;
  - it maps an absolute link to the file it names inside the container's
    mount;
  - it skips anything that is not a regular file (a FIFO would hang it);
  - it cleans text from GGUF headers and file names before printing it.
- **A Mac setup that stops before starting removes its LaunchAgent,** so nothing
  starts half-installed at the next login.
- **SIGHUP is caught before startup.** Its default action ends the process, and
  systemd's `Restart=on-failure` counts that as a clean stop.
- A private build of a pre-release (`v2.6.0-beta4-g<sha>`) compares as that
  pre-release.
- The confirmation phrase is `YES I WANT TO UPDATE THE NODES ABOVE`: it is
  true for a partial rollout too.
- Smaller:
  - `viiwork top` cleans a node's version in its failure line;
  - the open-mesh and missing-secret messages say what actually works;
  - the example compose file uses `restart: always` and mounts `/etc/viiwork`
    as a directory.

**Known, for v2.6.1 or later:**

- After a host confirms a release, `viiwork update rollback` has nothing to
  roll back to. Going back is a manual binary or image change.
- A self-update moves the viiwork binary only. A wizard install's engine (the
  Docker image, or the Mac's llama.cpp) stays as installed until a reinstall.
- `viiwork top` can keep an in-flight row after a member crashes and comes
  back under the same name.
- `llama.cpp` release lookups use GitHub's API without a token, so behind a
  shared address they can hit its rate limit.

## v2.6.0-beta4

### What the first Mac install found

beta3's `viiwork init` installed a MacBook (M3 Max, App Store Tailscale,
models from viiwork-parrot). It joined the fleet with a code, from a host
other than the first, and served granite. It reused the llama.cpp build
already on the Mac and left it unclaimed, as designed. It needed one manual
fix on the way, and these came out of it:

- **A Mac node finds its tailnet under launchd.** The App Store Tailscale
  binary acts as its CLI only when `TERM` is set. A LaunchAgent has no
  `TERM`, so the binary tried to start its GUI and printed "The Tailscale GUI
  failed to start" instead of the JSON status. The node then waited for its
  tailnet address, and launchd restarted it every few minutes. The node now
  runs that CLI with `TERM=dumb` when its own environment has none. A
  hand-built Mac node worked only because a shell wrapper on its `PATH`
  happened to supply the missing piece.
- **A status that is not JSON is quoted in the log.** "invalid character 'T'"
  alone hid the message above.
- **The wizard's wait notices a restart loop.** A node that keeps exiting
  answers its API after every start, so the wait held for its whole budget.
  Uptime going backwards between polls now ends the wait with the reason.
- **The models-directory prompt:**
  - it suggests viiwork-parrot's `data_dir` when parrot is set up;
  - an empty answer asks for the directory, instead of reporting a malformed
    path;
  - `~` means the home directory.

## v2.6.0-beta3

### What the first real install found

beta2's `viiwork init` installed a host with three RTX A4000 cards and
Docker reaching them through CDI. It joined the fleet with a join code and
served TranslateGemma, but only after three fixes, all found on the way:

- **A backend that cannot load now says why.** The llamacpp engine started
  llama-server with `--log-disable`, which silenced its errors too, so a model
  whose chat template llama.cpp could not parse died as `dead` with no reason
  anywhere. Backends now run with `--log-verbosity 1`: errors reach the node's
  log, and the per-request info lines stay out.
- **Config edits reach the node.** The generated compose file mounted
  `viiwork.yaml` as a single file. That pins the inode, so an edit saved as a
  new file (`sed -i`, many editors) stayed invisible, and a SIGHUP reloaded
  the old config. The wizard now mounts `/etc/viiwork` as a directory, and so
  does `configs/docker-compose.v2.example.yaml`. Existing hand-built hosts
  keep the old mount until their compose file is edited.
- **Models that need llama.cpp flags are documented.** A chat template that
  llama.cpp's Jinja handling rejects needs `args` such as
  `["--no-jinja", "--chat-template", "gemma"]`. The wizard cannot see this in
  a model's header, so `docs/setup.md` says what the log shows and what to
  add.
- The manifest records the compose project's volumes as `[]`, not `null`.

## v2.6.0-beta2

### Found on the first real host

- **The wizard accepts NVIDIA cards reached through CDI.** On an Ubuntu
  26.04 host with the NVIDIA Container Toolkit installed, Docker had no
  `nvidia` runtime registered. So `driver: nvidia` failed, while CDI devices
  (`nvidia.com/gpu=all`) worked. beta1 refused such a host.
  - The wizard now checks the devices Docker has discovered, and writes a CDI
    compose file when the cards are there.
  - It falls back to the registered runtime otherwise.
  - Its refusal message now names both fixes.
- **A beta is a GitHub pre-release.** The release workflow creates a tag with
  a pre-release suffix as a pre-release, so `releases/latest` stays on the
  last full release.
- **The README's Quick Start runs the setup wizard.** Setting a machine up by
  hand is a subsection below it.

### From an audit round

- **Member clients never follow a redirect.** No member endpoint redirects,
  and a member answering 3xx must not steer this node's request to another
  address, so the 3xx comes back as the response.
- **A client gone during a non-streaming write is recorded as aborted**, not
  as a completed request.
- **A failed restore of an earlier update stage is reported**, instead of
  being silently dropped.
- **Pipelines:**
  - a step failure names its model;
  - the source text is read from content given as parts too;
  - a recursive step is refused as a start error, rather than by
    `log.Fatalf`.

## v2.6.0-beta1

### Download it, run it, answer a few questions

**`viiwork init` sets up a machine.** Run `sudo ./viiwork init` on Linux, or
`./viiwork init` on a Mac. `./viiwork` on a terminal with no config starts it
too. The wizard finds the GPUs, then scans a models directory and lists each
GGUF with its architecture and size. It proposes a layout, which you can
accept, edit or trim, and asks about the mesh. Before writing anything it shows
every file with its content, the secret masked, and on a yes it writes them,
starts the node, and runs the `viiwork-accept` config, ready and join checks.
`docs/setup.md` is the guide.

- **Nothing is written before the last yes.** Ctrl-C or end of input at any
  question leaves the host untouched. The config goes through the node's own
  `config.Parse` and `Validate` first, so a layout the node would refuse is
  shown with the reason and asked again.
- **The layout comes from the model files' headers.**
  - Weights come from the file size; the KV cache from the attention layers
    only, since hybrid models such as Qwen3.8 keep a cache in one layer in
    four.
  - Discrete cards are grouped by the smallest card times the group size,
    because llama.cpp splits evenly.
  - A Mac plans against its Metal budget, shared by every model.
  - A model that does not fit is shown with what it needs.
- **Linux runs the node in Docker** from a published engine image (NVIDIA
  today), with `restart: always`, the state directory as a host path, and a
  90 s stop grace. Other GPUs get the config and no start, with the reason:
  gfx906 has no published image, and the Vulkan build cannot pin a backend to
  its cards. A development build is config-only too.
- **A Mac runs natively** under a user LaunchAgent, with no sudo. llama.cpp is
  fetched at the binary's pin and checked against the sha256 GitHub publishes
  for the asset. The archive is unpacked through `os.Root`, so no entry can
  land outside the build directory. A build already fetched by hand is reused
  and left alone.
- **Preflight refuses rather than overwrites.** It refuses when:
  - a config, `mesh.env`, compose file or install manifest already exists;
  - a container named `viiwork`, or a loaded agent, shows a node set up by
    hand;
  - Docker, Compose v2 or the NVIDIA Container Toolkit is missing;
  - a port is busy;
  - a Mac has no GUI session.
- **A node that does not come up keeps its files.** The wizard prints each
  unhealthy backend's status and phase and the last log lines, then how to
  retry and how to undo. A large model's load is waited for model by model,
  since `/health` stays 503 until one loads. A backend that dies ends the wait
  at once.

**Join codes.** `viiwork join-code` prints one pasteable string that carries
the mesh secret and a seed address (`viiwork1-…`, with a checksum). The next
machine's wizard takes it at the mesh question. The code *is* the secret and
says so. On a Mac, join-code reads the secret from the agent's plist.

**`viiwork uninstall` removes exactly what the install created.**
- It trusts the install manifest (`install.json`) only within the paths an
  install uses. Entries elsewhere, other projects' images, and paths that run
  through a symlinked directory are skipped and named.
- It stops the node the normal way first, and a failed stop removes nothing.
- Nothing is ever pruned.
- Model weights are kept unless `--delete-models`, which lists them and asks
  separately.
- A host set up by hand has no manifest, so uninstall refuses;
  `--from-config` only lists what its config points at.

### Signed releases and rolling updates

- **Releases are reproducible archives with a signed `SHA256SUMS`.**
  - CI builds them.
  - The publisher rebuilds, compares and signs locally with ed25519
    (`scripts/release-sign.sh`, `viiwork-release`). The signature covers the
    version too.
  - The public keys are compiled into every build. `docs/releases.md`.
- **A node can update itself.** Enable it with `update.enabled: true`, default
  off.
  - `/v1/update` stages a release: it verifies the signature and checksum,
    checks the binary runs and reports its version, and checks this host's
    engines meet the release's minimum versions.
  - It then activates and confirms against the backends that were healthy
    before. A dead backend, or a missed deadline, rolls back.
  - Writes are authorised like alias writes.
- **`viiwork update` rolls a release across the mesh.** It asks for a typed
  phrase, stages everywhere first, then activates one host at a time with the
  entry node last, and stops at the first rollback. `status` and `rollback`
  act on one node.
- **Engine images** (`viiwork-llamacpp-cuda`, `-vulkan`, `viiwork-vllm`) are
  upstream engine images with the verified `viiwork` added as one layer.
  Their base is pinned by digest in `docker/pins.env`, and each tag is written
  once.

### `viiwork top`

A live full-screen view of the whole mesh from any terminal:
- hosts, model totals and in-flight requests;
- per-host GPU history graphs and backends.

It is read only, built on `golang.org/x/term`, which is the fourth dependency.
`--once` prints one frame, and `--host` opens a host. Everything drawn from the
network is sanitised.

### macOS (Apple Silicon) as a node

A Mac runs llama.cpp on Metal as a normal mesh member:
- `gpu.vendor: apple` lets several models share its one GPU;
- GPU telemetry comes from `ioreg`, and host RAM on darwin;
- tailnet status works with any Tailscale build;
- LAN mode uses the darwin default route.

`docs/macos.md`. The default `--config` on a Mac is
`~/.config/viiwork/viiwork.yaml`.

### Smaller

- `/chat` shows live tokens per second.
- `internal/durable` writes each file through a temporary file of its own, so
  concurrent writers never tear the target.
- The llama.cpp version is read from `llama-server`'s current version line.

Additive only: the `update:` config block and the `/v1/update` wire types are
new, and no existing key or field changed.

At the time of beta1 none of this had run on real hardware; beta2 to beta4
below are what that found.

## v2.5.0

### A six-lens review, and everything it found

A review of the whole tree through six lenses — security, error handling, type
safety, performance, architecture and simplicity — and the fixes for all of it.

**Security: the browser boundary now holds.**

- **The engine's CORS header no longer leaks through.** llama-server echoes any
  `Origin` into `Access-Control-Allow-Origin`, and the proxy copied it back, so
  a refused origin could read inference output. `Origin` is no longer sent to
  engines or peers, and every `Access-Control-*` header from them is dropped:
  the node's CORS layer is the only authority.
- **Cross-site writes are refused before routing.** A state-changing request
  whose `Origin` is neither the node's own page nor allowlisted gets 403 — on
  every endpoint, inference included. Power and alias writes must be
  `application/json` (415 otherwise), so no browser can send one as a simple
  request; a bodiless untyped write is still accepted, so an older alias CLI
  keeps working. Before this, any page could power a host off.
- **The CORS default is this tailnet, not every tailnet.** `*.ts.net` admitted
  anyone's Funnel pages; with `allow_origins` unset the node now derives
  `*.<tailnet>.ts.net` from tailscaled, falling back to local origins.
- **Secrets stay out of logs and argv.** The ENTSO-E key is redacted from fetch
  errors, and the BMC password goes to `ipmitool` through `IPMI_PASSWORD`.
- A node bound to every interface (`api.host: 0.0.0.0`, the default) says so
  at startup.

**Failures are no longer reported as success or as measurements.**

- **A stream cut off mid-response is aborted**, not closed as a complete 200;
  the request is recorded as aborted and its slot is always released, panics
  included.
- **A capacity refusal is a 503 with `Retry-After`**, not a 502.
- **Stale power readings go absent.** A BMC or `rocm-smi` that stops answering
  no longer freezes its last wattage into the energy rings, the cost tracker or
  `/v1/status`; the cost tracker no longer bills an outage at the rate that
  ends it.
- **`energy_kwh_30d` is read from the hour tier.** The day tier caps covered
  seconds at 65 535, so full days read about 24% low. Day roll-ups follow local
  midnight across DST, and a torn or failed `models.txt` append no longer
  shifts model indices.
- Alias push/pull applies the same validation as broadcasts (a live entry
  needs a target, versions are bounded), and the meshauth skew check no longer
  overflows.

**Performance.**

- The activity ring is a real circular buffer: an emit no longer copies the
  whole ring (386 KB per event at the default size, now 264 B).
- `/mesh` fan-out is one hub per node — one follower per member and one
  snapshot loop — instead of per viewer, so dashboards no longer evict each
  other's followers at a member's subscriber cap. Follower backoff resets after
  a healthy stream.
- Untagged `reasoning_content` is renamed on the bytes (about 52 → 6 allocs
  per token); aliased requests splice the model name instead of re-encoding the
  body; captured output stops decoding past the history cap; the origin queue
  no longer re-picks every waiter on each wake; energy reads touch only the
  slots in range; `/mesh` updates prompt rows in place.

**Architecture and simplicity.**

- The route table sends every path in the C8 dialect registry to inference,
  and engine blank imports live in one place, `internal/engine/all`.
- Pipelines dial the address the API actually bound, not `127.0.0.1`.
- The fleet capacity types are frozen in `meshapi/wire_test.go`.
- The status and capacity pollers share one core, member traffic uses one
  client constructor (`mesh/meshclient`), JSON responses go through
  `internal/httpjson`, and `internal/alias` no longer imports the proxy.
- The v1 configs, compose files and scripts moved to
  `configs/private/v1-archive/` and are no longer published; `docs/models.md`
  gives its recommended layouts as v2 `models:` entries.

## v2.4.0

### Models from viiwork-parrot

**A model can name a viiwork-parrot catalog id instead of a file, and the host
keeps one copy of it.** `models[].source: viiwork-parrot:<id>` asks the host's
viiwork-parrot node (`viiwork_parrot.api`, default `127.0.0.1:7950`) for the
model's path at start; viiwork-parrot downloads it if needed, verifies every
sha256, seeds it and hands back the path. viiwork never downloads and keeps no
copy of its own. `docs/models.md` is the guide.

- **The fetch runs before the model's backends join the load gate.** A
  download of any length holds up no other model's load and never counts
  against `startup_timeout`; the backends show `starting` with phase
  `fetching`, and the node logs progress every 10%.
- **Start order doesn't matter.** If viiwork-parrot isn't running yet, the node
  waits for it, retrying up to once a minute.
- **A refusal is terminal.** An unknown id, a model not available on this
  host, a failed verification or no disk space marks the model's backends dead
  with viiwork-parrot's own message.
- **Paths must match.** viiwork-parrot answers with host paths, so a
  containerised node mounts its data directory at the same path; the node
  checks the path exists and says so if it does not.
- **llamacpp refuses a directory.** A folder model (a safetensors directory)
  needs an engine that takes one — vllm or freetoken.
- **`viiwork-accept config` checks a sourced model read-only**, through
  viiwork-parrot's `GET /status`.

Additive config keys only (`models[].source`, `viiwork_parrot.api`); no change
to the mesh contract.

## v2.3.1

### A dashboard loads the present, not the backlog

**Opening `/mesh` now replays the last 30 seconds of activity, not every
member's whole event ring.** Each activity stream replayed its node's ring when
it opened, and `/mesh` follows one per member, so a page load on a 12-member
fleet waded through thousands of events before it showed the present.

- **Replay is bounded by time** (`activity.ReplayWindow`, 30 s): every event
  from the window, plus — from before it — the events of any request still in
  flight. The dashboards rebuild their in-flight rows from replayed starts, so
  an inference running for minutes stays on a reloaded page instead of
  vanishing while it is still running.
- **Prompt history on `/mesh` starts short after a load.** It is filled from
  the stream, so it now opens with the last 30 seconds and grows from there.
  Older history is still on each node, via `/v1/activity`.
- The effect is per node: a member one version behind still replays its whole
  ring to whoever follows it.

## v2.3.0

### The fleet describes itself

**Point a coding client at any node and it discovers the models, the context
window it can actually rely on, and the capabilities — with no model list
written into a client config and no numbers typed in by hand.** Nothing to
regenerate when the fleet changes, because the fleet is the source.

The market standardised the transport and never standardised discovery.
OpenAI's `/v1/models` returns `{id, object, created, owned_by}` and has never
been extended, so every client either ships a hardcoded table of the models it
has heard of or makes the user type the numbers in. viiwork is unusually well
placed to answer: every format trying to close that gap assumes one host with
one model loaded, while a node already computes the minimum context across the
hosts its router would route to. That figure — the **served context** — is what
a client can safely size a prompt against, and it is what all three surfaces
below publish. `docs/autodiscovery.md` is the guide.

- **`/v1/models` now carries the window**, in both spellings the ecosystem
  converged on: `max_model_len` (vLLM's) and `context_length` (OpenRouter's).
  Both formats define their field as a prompt+completion total, which is what
  makes one figure correct as both; LiteLLM's prompt-only `max_input_tokens` is
  a different quantity and is deliberately not here. Widest reach for the least
  code, and a deliberate C4 change in its permitted form — additive, `omitempty`,
  so a node one version behind says "cannot say" rather than zero.
- **`/api.json` is the OpenCode model catalogue.** OpenCode has no `/v1/models`
  discovery for openai-compatible providers — an open feature request, not a
  feature — and no vLLM provider either, so a config naming one does nothing.
  What it has is `OPENCODE_MODELS_URL`, from which it fetches `<url>/api.json`.
  The client config is `"provider": { "viiwork": {} }` and one environment
  variable.
- **`/v1/model/info` is LiteLLM's shape, and it is what makes Roo Code work
  with no configuration at all** — the same client that otherwise makes you
  hand-type every number. Serving the document does not make a node a LiteLLM
  proxy: inference stays on the OpenAI-compatible endpoints, which is the
  transport these clients speak anyway. A node must never advertise a dialect
  it will not then accept requests in.
- **One canonical record behind all three.** `internal/discovery` holds what
  viiwork actually knows and every format is a thin serializer over it. Three
  formats each assembling their own view would be three things to keep true,
  and the first one written would quietly become the source of truth for the
  rest. A node test asserts all three name the same models and report the same
  number, because no single client would ever notice if they drifted.

**What viiwork will not claim.** There is no output ceiling distinct from the
shared window — a slot's context is one prompt+completion budget and the split
is the client's to choose — so `/v1/models` and `/v1/model/info` emit none, and
the models.dev document's required `limit.output` is documented in the code as
the client-side budgeting hint it is rather than as a capability. No cost, no
modality beyond text, and no architectural maximum, which no node knows.
`max_input_tokens` is the one knowing approximation, and it is a decision: it
is a slightly generous true bound, and the alternative is not "no claim" but
the 200000 Roo Code invents when the field is absent.

**The claim NOT to make**: universal zero-config for every client. Cline, and
Roo Code configured as a generic openai-compatible provider, still require
manual entry and will not read the enriched fields. OpenCode's own five-minute
catalogue cache also means discovery is up to five minutes stale during churn,
whatever viiwork does.

### The context floor is a promise again

**`/v1/fleet/capacity`'s `ctx` is now the minimum across fresh hosts that have
slots**, where it used to be the minimum across any fresh host. A draining or
slot-less backend could depress the window every client sizes its prompts
against while contributing no capacity the router would ever use — and it is
the smallest windows that drain first, so the wrong number was the likely one.
When no host has a slot the figure is now absent rather than borrowed from a
host that cannot serve it. This changes published numbers, so it is a
deliberate refinement with its own tests.

- **`api.catalog.upstream` chains a hosted catalogue back in.** Setting
  `OPENCODE_MODELS_URL` replaces the client's whole catalogue, so on its own it
  costs the user Anthropic, OpenAI and the rest; with an upstream configured the
  node serves both, its own entry winning on a collision. Off by default, fetched
  once per `upstream_ttl` (which must be positive, or every request would
  refetch), and an enrichment rather than a dependency — an unreachable
  upstream is logged and the fleet is served alone. A node on a tailnet does not
  acquire a route off it because a config file said nothing.
- **`api.catalog.provider_name` decides where the fleet sits in the picker.**
  OpenCode sorts providers by name, byte by byte, so the default is `" viiwork"`
  with a leading space — that is the whole mechanism, and a plain `viiwork`
  sorts below every capitalised hosted provider.
- **`api` is the node the client reached**, not a name baked in at build time:
  any node is an entry point and every node serves the whole mesh.
- `scripts/setup-opencode.sh` writes the two-line config against a node that
  serves a catalogue, and falls back to enumerating `/v1/models` once against
  one that does not.

### FreeToken 0.1.3

**The `freetoken` engine now generates FreeToken 0.1.3's command line, and
0.1.3 is the floor.** Upstream renamed `--moe-backend` to `--moe-strategy` and
left the old spelling as a deprecated alias that warns on every start; viiwork
generates the new one, which an engine below 0.1.3 rejects outright — the
backend dies at load with an argparse error rather than degrading. The pypi
channel of `docker/Dockerfile.freetoken` defaults to `0.1.3` instead of to
"whatever PyPI has today", so a rebuild reproduces the inference stack.

- **`models[].freetoken.moe_backend` keeps its name.** C1 freezes the YAML an
  operator writes; only the generated flag moved, and a second key spelled
  `moe_strategy` would buy nothing but a rule about which one wins.
- **Card selection stays the node's**, through `CUDA_DEVICE_ORDER` and
  `CUDA_VISIBLE_DEVICES`. 0.1.3 added `ft serve --gpu`, taking an `nvidia-smi`
  index or a UUID, and that is an argument for generating nothing rather than
  against: an index resolves against whatever ordering the process was handed,
  and the node has already narrowed the child to one card. The design decision
  to leave card selection to the node now rests on that instead of on the
  flag's absence.
- **`kv_reserve_tokens` is settled.** The clean-room spike had left it
  unverified. `--kv-reserve-tokens` is real and documented, the engine enables
  the expert cache it guards on its own for any offload-family strategy, and
  `kv_reserve_tokens: 0` does not mean "no reserve" — the flag is simply not
  generated and the engine keeps its own floor of 8192 tokens. The comment
  that called the flag unverifiable, and the example config, both said
  otherwise.
- **`/v1/stats` reports the bound card from 0.1.3 on**, so `BoundGPU` gets a
  real UUID rather than the "unknown" every 0.1.2 backend returned. The
  graceful-unknown branch stays: the engine still sends an empty list when it
  has nothing to say, and unknown is not a mismatch.
- **Nothing of 0.1.3's new surface is generated.** Image input
  (`--text-model-only`, `--image-max-tokens`, `--allowed-media-domains`) and
  the rebuilt quantization path (`--quant-backend layer[.kind]=name`, which
  retires `--nvfp4-backend`) are model properties, and model properties belong
  in `models[].args`. The engine package generates what the *node* decides.

**Upgrading a FreeToken host is not only a version bump.** Multimodal
checkpoints build their vision tower by default now, so an FTW converted before
its family served images carries no encoder and `ft serve` refuses it — a
backend that loaded yesterday walks down the health ladder to `dead` today.
`--text-model-only` in `models[].args` is the quick answer; NVFP4 dense exports
and Qwen3.8-Flash-Next may need a real repair (`ft checkpoint`, or upstream's
`scripts/ftw_hotfix.py`). `BUILDS.md` has the list.

### The README is a front door again

The README had grown to 949 lines and still opened
with a v2.0 migration notice and a Radeon-VII-only premise that the code had
outgrown — three engines now, ROCm *and* CUDA. It is 325 lines, and the detail it
used to carry lives in seven new references that are actually linked.

A visitor now meets the screenshot and what viiwork is; an operator follows one
hyperlink to the depth they came for. Outbound documentation links went from
**2 to 26**, and every one is checked.

#### The fan-out

| New document | What moved into it |
|---|---|
| `docs/configuration.md` | Every config key: models, tensor-split, reloading, pipelines, GPU power limits, environment variables, host requirements |
| `docs/mesh.md` | Discovery, secured and open mode, routing and refusal handling, aliases |
| `docs/dashboards.md` | `/`, `/mesh`, `/chat`, `/prompt`; how the live view is reconstructed; prompt and output history |
| `docs/power-and-energy.md` | Fleet power, ENTSO-E cost tracking, the durable energy store, IPMI chassis control |
| `docs/models.md` | The measured catalogue, the gfx906 tuning rules, and **large models on one card via FreeToken's MoE offload** |
| `docs/security.md` | The trust model, what the lack of authentication exposes, CORS |
| `docs/operations.md` | Scripts, `viiwork-accept` acceptance checks, the MCP server |

Content moved largely verbatim; this is a relocation, not a rewrite. Seven
existing documents that the README had never linked — `api-integration`,
`consuming-fleet-capacity`, `thinking-models`, `energy-store-format`,
`tensor-split-design` among them — are now reachable from a Documentation index.

#### FreeToken's large-model story is written down

The README stated the "must fit in one card or pay a ~3× tax" rule as universal.
It is not: that is a property of dense models on llama.cpp, and the `freetoken`
lane breaks it deliberately. FreeToken runs **one card per process** and reaches
models larger than the card by keeping only a sparse MoE's hot experts resident
and streaming the rest from host memory — which is how the fleet serves
DeepSeek-V4-Flash on RTX 5090s.

Three things an operator needs before planning one, now documented: the slow load
**is** the offload working (hence the 30-minute default `startup_timeout` against
llama.cpp's 10); `kv_reserve_tokens` must be set to at least `context`, because
the engine enables its expert cache on its own and `0` leaves an 8192-token
floor; and **the advertised context is not the servable
context** — a live capture shows 1,048,576 advertised against a 64,128 KV pool, a
factor of 16, which viiwork does not correct.

#### Fixes carried in the move

- `BUILDS.md` absorbs the README's duplicate "Docker Build" section, and gains
  the missing reason the gfx906 FP8 patch exists
- Host requirements covered AMD only; CUDA and CDI are now there
- Cost tracking said "Nord Pool" and then configured ENTSO-E
- The `setup-node.sh` removal was explained twice, at length
- Scripts gained `deploy.sh`, `version.sh`, `verify-environment.sh`,
  `download-*.sh` and `fetch-engine.py`
- Prose cross-references ("see *Energy History*") are anchors that click


## v2.2.0

**Three engines, and the fleet now runs all three at once.** llama.cpp on
gb0-gb4, vLLM on plexie, FreeToken on grizzly and yeti — one binary per machine,
one mesh, one OpenAI-compatible API, and a request goes to a free slot wherever
one exists no matter which engine answers. The entries below under
`v2.2.0-beta1`, `-beta2` and `-beta3` are kept because they say why each part is
the way it is.

**beta1 said this becomes v2.2.0 when a real model runs on the hardware it is
meant for.** It has. FreeToken serves DeepSeek-V4-Flash on RTX 5090s — the
Blackwell path beta1 listed as untouched — and a 60-minute fleet soak put 104
slots across 7 models and all three engines through 0.1x to 4x of capacity.

### What the soak found

Fleet-wide on beta3: 10 nodes, 7 models, 104 slots, six phases from warm to 4x
and back to half.

- **Backpressure is well formed.** 15,591 over-capacity requests — 5,115 served,
  10,471 refused, every refusal a 429 carrying `Retry-After`. No 503s, because
  the models were always *served*, just full; zero dropped connections and zero
  timeouts under load.
- **Admission control protects throughput.** Served counts were effectively
  identical at 2x and 4x, so doubling the offered load changed only how much was
  shed. `busy` stayed pinned at 104/104 throughout — a fleet in congestion
  collapse would show it falling.
- **Queue depth is predictable.** It tracked in-flight minus slots to within two
  requests: 106 observed against 104 predicted at 2x, 313 against 312 at 4x.
- **Recovery is complete.** 314 queued back to 0, TTFB to 1.08x of the baseline,
  nothing stranded — so the refusal mark, where a refused forward marks that
  (member, model) full until a report received *after* the refusal, holds at
  fleet scale.
- **A quarter of the traffic was addressed by alias** rather than by real name,
  exercising beta3's per-request resolution on the origin node, with zero
  transport failures.
- **One finding, and its shape is warm-up rather than overload.** Five requests
  exceeded a 180 s client timeout, all on `Ornith-1.5-35B-A3B`, all early and
  all under light load; a 35B MoE at 262144 context on gfx906 pages in slowly,
  and the model got faster as the run went on. Nothing after t+767 s, including
  under 4x. Worth knowing before anyone sets a shorter client timeout.

### Since beta3

- **The mesh dashboard stopped painting under load; it now paints on a clock.**
  `/v1/mesh/stream` replays the local event ring and every member's, so opening
  `/mesh` against a fleet under soak delivered some 15,700 events back to back —
  and the page rendered once per event, each one rebuilding the thousand-row
  prompt list, the activity feed and the in-flight list from scratch. Measured
  in a browser against the live fleet: **2 frames in 30 seconds**, single tasks
  blocking the main thread for **57 s**, and the page so far behind its own
  stream that it showed 78 in-flight rows against the 2 actually running.

  Repaints are coalesced onto the 200 ms tick that already existed for in-flight
  ages, so a burst paints once with every event of it folded in. Same
  measurement after: **1,705 frames**, worst gap **0.5 s**, **1.3 s** blocking,
  and the in-flight list correct. Nothing is dropped — events fold into the same
  maps under the same caps, and only repaints that would have drawn the same
  rows are merged. `web/pages_test.go` fails if a renderer is ever called from a
  per-event handler again.

  A viewer's browser was the whole of it: nodes, routing and the API never saw
  this.

### Not in this release

- `scripts/deploy.sh` still drives v1.x layouts and has not been converted.

## v2.2.0-beta3

**`/v1/fleet/capacity?model=` resolves aliases.** beta2 matched the query as an
exact string, and capacity reports carry real model names only — aliases are a
router concept, resolved on the inference path and never reaching this one. So
`?model=stable-translate` returned an empty `models[]` for a live alias every
node serves.

Not cosmetic. An empty `models[]` is the documented signal for "the fleet
cannot serve this", and it sits in the fallback table in
`docs/consuming-fleet-capacity.md`. A consumer whose model is an alias — which
is the configuration we recommend, because it is the whole point of aliases —
fell back to its hardcoded constant forever, silently, with no error anywhere:
the pre-endpoint behaviour, while believing it was fleet-sized. Found against a
live beta2 fleet while wiring up the first consumer.

The filter now asks the resolver the handler already holds. Resolve is identity
for a real, unknown or shadowed name, so the inference path's rules hold here
unchanged — a real model of the same name still wins.

**An alias nothing serves is `200` with an empty list, not `503`.** On the
inference path the 503 is right: the caller asked for work to be done and it
cannot be. A capacity query is a different question, and this endpoint has
already answered it — zero capacity is exactly what the empty list says. A 503
would read to a consumer as "the platform is down" and trip the same branch as
an unreachable node, over a normal operating state.

**New: `resolved_from`.** The response reports the real model in `name` and
echoes the alias that was asked for in `resolved_from`, present only when
resolution actually rewrote the query. The alias never goes in `name` — that
would make two nodes disagree about a model's name depending on how it was
asked for. Inference already reports the real model for an aliased request, so
this is one rule rather than two.

Additive to C4. Against a beta2 node an alias lands in the existing "model
absent from `models[]`" fallback row rather than erroring — degraded, not
broken, but silent, so log which branch was taken.

## v2.2.0-beta2

**`/v1/fleet/capacity`** — one node's view of what the whole mesh can serve,
per model, with a per-host breakdown. Added after beta1 rather than folded into
it silently: beta1 is on the fleet being tested, and what is under test should
be what ships.

It exists because a consumer sizing its own concurrency had no way to see the
fleet. `/v1/capacity` is node-local, so gb1 answers 12 for a model the fleet
serves 36 of. routemap4 was configured at 4 — and because viiwork fills a
node's own slots before forwarding, four in flight never crossed gb1's 12, so
gb2 and gb3 were never reached at all. 24 slots idle by construction.

Aggregated from capacity reports every node already holds, so it adds no
inter-node traffic, and `/v1/capacity` is untouched. Totals count only reports
`capacity.Fresh()` accepts — the router's own predicate, so what a consumer is
told matches what the router will use. A host the node has lost sight of is
listed with `stale: true` and **no numbers**: absent is not zero, so "the fleet
shrank" stays distinguishable from "we cannot see gb3". `view` names the
answering node, and `ctx` is the minimum across fresh hosts.

`docs/consuming-fleet-capacity.md` is the integration guide: poll on a
5-minute cache, keep the configured capacity as the floor for every failure
case (**including the 404 an older node returns**), spread ingress across
`hosts[].api`, and let the existing queue absorb the overshoot as latency
rather than errors.

Additive to C4. A beta1 node and a beta2 node interoperate; the older one
simply does not serve the path.

## v2.2.0-beta1

**A beta, and the reason is the scale of what has been served.** Both engines
have now run for real: on teddy, one node served `vllm serve` and `ft serve`
backends side by side, each on its own A4000, both reaching healthy and
answering requests, with the node joined to the fleet's secured mesh and both
models visible from gb1. What has NOT happened is a production checkpoint — the
smoke test ran Qwen2.5-0.5B, and A4000 is Ampere, so NVFP4 and FreeToken's
arch-specific kernel cache remain unexercised. This becomes v2.2.0 when a real
model runs on the hardware it is meant for.

**The vLLM and FreeToken engines ship in the binary.** viiwork now serves three
engines from one process: llama.cpp on ROCm, vLLM, and FreeToken. A node can run
all three at once, each model's backends pinned to their own cards, and the mesh
does not care which engine answers — a request goes to a free slot wherever one
exists.

**This is the real review of v2.1.0's contract, and the contract held.** Adding
these two engines touched the two engine packages and three blank imports, and
nothing else. `internal/config`, `internal/supervisor`, `internal/node`,
`internal/route` and `internal/proxy` are untouched by both. That was the
v2.1.0 claim, and it is now tested rather than asserted.

**Engines are named for the engine, never for a GPU vendor.** FreeToken runs on
CUDA today and may add AMD or Intel; vLLM already has a ROCm build. Nothing here
— package, config key, image name or prose — pairs an engine with a vendor, and
`Spec.Vendor` stays informational rather than a gate.

- **`internal/engine/vllm`** — `vllm serve` backends, ready when `/health`
  answers 200, occupancy scraped from the Prometheus text at `/metrics`. Counts
  are summed across data-parallel engines because the backend's load is the
  total; KV-cache occupancy takes the maximum, because it is a pressure signal
  and the engine closest to preemption is the one that makes the backend slow.
  A build that publishes no running-sequence gauge makes `Load` return an error
  rather than a zero: a zero would freeze "idle" into the mesh for a busy
  backend, while an error degrades visibly to the node's own in-flight count.
- **`internal/engine/freetoken`** — `ft serve` backends, one card per process,
  occupancy from `/v1/stats`. **Its readiness rule is the one to read twice:**
  FreeToken answers `/health` with 200 in *every* lifecycle state, including the
  minutes a frontier MoE model spends loading and any window a live cache
  rebuild takes it out of service. Both 503 every generation request. A probe
  that stopped at the status code would advertise the backend to the mesh and
  have every routed request come back 503, so the body is the rule and the
  reported phase reaches `/v1/status`.
- **Two images**, `docker/Dockerfile.vllm` and `docker/Dockerfile.freetoken`,
  with `make docker-vllm` and `make docker-freetoken`. The vLLM image drops the
  binary into vLLM's published image and clears its entrypoint, which otherwise
  launches an API server and never runs `CMD`. The FreeToken image installs the
  engine into its own virtualenv on a **devel** CUDA base, because the engine
  JIT-compiles kernels and needs `nvcc` at run time, not only at build time.
- **Compose examples for both**, which grant GPUs through CDI rather than the
  NVIDIA runtime. `nvidia-ctk runtime configure` restarts the Docker daemon and
  so bounces every other container on the machine; a generated
  `/etc/cdi/nvidia.yaml` does not. Neither image contains `nvidia-smi` or
  `libcuda` on purpose: the container runtime injects them from the host, so
  they always match the running driver.

### What was tested, and on what

The engines were written clean-room against the v2.1.0 documentation by someone
who did not read the foundation implementation — which is how three defects in
that documentation were found and fixed before this release rather than after.

Verified on **teddy** (3× RTX A4000, driver 595.91.07, CUDA 13.3, Docker 29.1.3):

- Both images build; `vllm 0.11.2+cu129` and `ft 0.1.2` run inside them; GPUs
  and `nvidia-smi` reach a container through CDI.
- **One node, both engines, live.** `vllm serve` on GPU 0 and `ft serve` on
  GPU 1, each reaching healthy with 4 slots at 8192 ctx, each answering a real
  completion through `/v1/chat/completions`.
- **FreeToken's readiness rule proved itself on contact.** During warmup the
  engine answered `/health` with HTTP **200** and `{"status":"loading",
  "phase":"warmup"}`, and the node correctly published zero slots for it. A
  probe that stopped at the status code would have advertised that backend and
  had every routed request come back 503.
- **The mesh spans three versions.** teddy on v2.2.0-beta1 joined the secured
  mesh alongside gb0 on v2.1.0 and gb1-gb4 on v2.0.0; gb1 sees both of teddy's
  models, their slots, their health and their request counters. C3, C4 and C5
  hold across all three at once.
- The full suite, the integration suites and `-race` are green. The node tests
  additionally run all three engines against fakes that assert the command line
  each engine generates, so the flags are exercised without a GPU.

**Still unproven: a production checkpoint.** The smoke test ran Qwen2.5-0.5B.
A4000 is Ampere, so the NVFP4 path and FreeToken's arch-specific kernel cache —
both Blackwell concerns — are untouched, and nothing here measures throughput.

## v2.1.2

- `scripts/setup-opencode.sh` writes the provider's display name in lower case,
  so a generated `opencode.json` groups its models under `viiwork`.

## v2.1.1

Documentation and sample configs only; no code changes.

- **Granite 4.1 becomes Granite 4.2 everywhere it is a recommendation or a
  sample.** The reference fleet has run 4.2 for a while and the 4.1 references
  were stale: the README model table and quick-start config, the migration
  guide's worked examples, and the `configs/granite-*` bench and test configs.
- **The download scripts are renamed and re-pointed**:
  `scripts/download-granite42-8b.sh` and `-30b.sh` fetch
  `ibm-granite/granite-4.2-{8b,30b}-GGUF`, the vendor's own repositories, rather
  than the third-party 4.1 mirrors they used to. Both repository names and both
  `Q4_K_M` filenames were checked against the Hugging Face API rather than
  guessed, because a wrong name here is a download that 404s.

- **The README now says what the reference fleet actually runs**, read from
  `/v1/capacity` rather than remembered: five models, their spread across hosts
  and their context per slot. The table below it stays what it was — an
  evaluation catalogue of everything benchmarked, deployed or not — which is why
  the two had drifted apart. `translategemma-27b-it` and `Ornith-1.5-35B-A3B`
  are in service and were documented nowhere; Laguna-XS-2.1 was labelled
  "deployed on the reference host" and is served by no host.

Past CHANGELOG entries keep their 4.1 references: they describe what shipped at
the time, and a changelog that rewrites its own history to match a later rename
is worse than one that reads slightly dated.

## v2.1.0

**The engine plugin foundation.** Adding an inference engine to viiwork is now
one package and one blank import. `internal/engine` stopped being a place where
engines happen to live and became a documented extension point: an engine owns
its own YAML block, its own defaults, its own validation and its own image, and
nothing in `internal/config`, `internal/supervisor`, `internal/node`,
`internal/route` or `internal/proxy` knows its name.

**No operator config file changes.** The `llamacpp:`, `vllm:` and `freetoken:`
blocks keep exactly the keys they had; what changed is who owns them. A v2.0.0
`viiwork.yaml` parses and validates identically, which is asserted against a
v2.0.0-era test config carrying all three blocks and against the shipped
example.

**llama.cpp on Radeon VII is the reference implementation**, not a special case.
Its option types moved into its own package, its "can run on CPU" exception
became a declared capability rather than a name compared in `internal/config`,
and it passes the same conformance kit any new engine would. That is a promotion:
the reference implementation is the one others are checked against.

- **`docs/adding-an-engine.md`**, the contract an engine is written against: the
  five methods and what each must guarantee, four optional capabilities found by
  type assertion, and — the section that saves the most time — what the node
  already does, so an engine author writes none of it.
- **`internal/engine/enginetest`**, a conformance kit an engine runs against
  itself. It is tested against a deliberately broken engine in a subprocess, so
  that "my engine passes" is evidence rather than decoration.
- **A report-only GPU binding check.** An engine that can say which card it
  bound is cross-checked against the host inventory, and a mismatch is reported
  and left alone: the backend is serving correctly, and taking it out of the
  mesh would trade a wrong label for a lost GPU. The failure it catches is
  otherwise silent — a backend pinned to the wrong card serves perfectly while
  the dashboard and the energy store bill a neighbour.
- **`Load` is handed its `Spec`**, and `Slots`/`CtxPerSlot` are documented as
  what the backend will *actually* serve rather than an echo of the config. The
  first cut of the contract said they "restate what the node asked for … so a
  clamped value is visible as a disagreement", which cannot be true — a
  disagreement needs two opinions — and left both fields unfillable by any
  engine that cannot observe them. Found by a clean-room spike that wrote two
  engines against the documentation alone; both independently reached the same
  unsound workaround, a map from backend address to the `Spec` `Command` saw,
  which misreports as soon as the node reassigns a port.
- **Contract C8**, the client API seam. `/v1/chat/completions`,
  `/v1/completions` and `/v1/embeddings` come from a registry rather than three
  string literals, so adding a client API shape later is a package rather than
  an excavation. There is no second dialect and none is planned. The
  OpenAI-compatible API stays native and pass-through: allocation counts on
  every proxy and router benchmark are unchanged, and a dialect is edge-only, so
  the mesh still forwards OpenAI whatever a client spoke.
- **Every Dockerfile moved under `docker/`.** The root `Dockerfile` became
  `docker/Dockerfile.rocm`: it always meant "llama.cpp for ROCm/gfx906" and said
  so nowhere, which with three engines coming is a trap. `make docker` still
  builds it.
- **The gfx906 fork build track is removed** — `Dockerfile.gfx906`,
  `make docker-gfx906`, `scripts/switch-node-build.sh`. It was retired on
  2026-08-30 with its record kept internally; its measurements were real (+3.0% sustained tok/s over a 4 h A/B soak) but its
  base was ~2000 tags behind, its architecture prune is what made it unable to
  load three of the fleet's five models, and the published repo was advertising
  a build no outside reader could make.
- **`scripts/setup-node.sh` is removed.** It wrote viiwork 1.x layouts that a v2
  node refuses, and its first prompt offered a choice between the ROCm image and
  the now-retired fork image. Copy `configs/docker-compose.v2.example.yaml` and
  `viiwork.yaml.example` instead; `docs/migrating-to-v2.md` converts a v1 host.

**Reviewed from the outside.** A clean-room spike wrote the vLLM and FreeToken
engines against these documents without reading the implementation, and
integration touched exactly two files outside the two engine packages — the
blank imports — which is the footprint this contract claims. It also found the
`Load` defect above, two places where the prose promised more than the code
does, and a live capture showing an engine serving 64,128 tokens of context
while advertising 1,048,576. The contract corrections are in this release; the
engines and the context fix are v2.2.

**Tested without an engine, a GPU or a network**, as everything here is: unit
tests, node tests against fake binaries, the race detector on the supervisor,
and before/after allocation benchmarks on the request path. The vLLM and
FreeToken engines, which are this contract's first outside consumers and its
real review, arrive in v2.2.0.

## v2.0.0

**viiwork 2.0.** One process per machine supervising every model on it, a mesh
that forms itself, and routing that follows free slots. The two betas put it in
front of a converted machine serving production traffic and of a second
implementation building against its contracts; what they found is fixed, and
this is that code called finished.

**This is a breaking release, and the break is the mesh protocol.** v2 nodes
gossip their membership on 7946 rather than polling a written list of peers
over HTTP, so a v1 node and a v2 node never see each other — a fleet converts
together, or runs as two meshes until the last machine is across. A v1
`viiwork.yaml` is refused at startup rather than guessed at, the Go module path
is `github.com/janit/viiwork/v2`, and `meshapi`'s wire types moved with the
protocol. `docs/migrating-to-v2.md` maps every key and covers rollback;
`viiwork-accept config` validates the new file while v1 is still serving. The
entries below under `v2.0.0-beta2`, `-beta1`, `-alpha.3`, `-alpha.2` and
`-alpha.1` are kept because they say why each part is the way it is.

### Since beta2

- **Dependencies are current.** The three direct dependencies —
  `hashicorp/memberlist` v0.6.0, `hashicorp/mdns` v1.0.7 and `gopkg.in/yaml.v3`
  v3.0.1 — were already at their latest releases. The indirect graph was not:
  `golang.org/x/net` moved v0.55.0 → v0.59.0, which clears GO-2026-5942 (a
  panic parsing a malformed SVCB or HTTPS DNS record). Nothing in viiwork calls
  it, but `mdns` is fed records off the LAN, so shipping a 2.0.0 with a known
  advisory in the graph was not worth the argument. `golang-lru` v0.5.0 →
  v1.0.2, `miekg/dns` v1.1.73, `hashicorp/go-metrics` v0.6.1, `x/sync`, `x/sys`
  likewise; `x/mod` and `x/tools` left the graph entirely. `armon/go-metrics`
  stays at v0.4.1 because later versions renamed the module path — v0.4.1 is
  the last release under the old one. `govulncheck` is clean.
- **The Go toolchain pin is 1.27.1** in `go.mod` and every Dockerfile, up from
  1.27.0 — fixes to the runtime, the compiler and `net/http`.
- **`viiwork-mcp` reported its version as `1.0.0`.** The string in the MCP
  `initialize` response was a literal, not the build stamp, so every assistant
  that connected to a viiwork cluster was told 1.0.0 — through the whole of
  viiwork 1.x and into 2.0. It is `main.version` now, `make mcp` stamps it like
  the node and the acceptance checker, and a test pins that it comes from the
  variable. It was the only hardcoded version left in the tree; every other
  surface — `/v1/status`, `/v1/cluster`, both dashboards, `viiwork-accept
  --version`, the image build args — already flowed from `scripts/version.sh`.
- **The README says what v2 breaks, above the fold.** A reader arriving at the
  repository met a feature tour and found out about the protocol change nine
  sections in, under Scripts.
- **`docker-compose.yaml.example` is gone from the repository root.** It was a
  v1 node — port 8080, bridged networking, no `pid: host`, no
  `stop_grace_period` — and `BUILDS.md` called it the stable-track example, so
  the one file a reader would copy was the one that produced a node that could
  not gossip, could not verify its GPUs and was killed mid-drain on stop.
  `configs/docker-compose.v2.example.yaml` is the single answer, which is what
  the README's Quick Start already copied.
- **`configs/viiwork.tensor-split.yaml.example` says it is v1 in its first
  line**, with the `gpus_per_backend` form beside it. It is the only remaining
  `.example` in a v1 vocabulary, and its own text invited comparison with the
  root example, which is v2.

### Not in this release

- `llamacpp` is the only engine. The vLLM and FreeToken engines land in v2.1.0.
- `scripts/setup-node.sh` and `deploy.sh` still write and drive v1 layouts.

## v2.0.0-beta2

Room in the mesh dashboard's backends table, which has to fit a whole fleet on
one screen. Two columns were spending width on things their own numbers already
said.

- **The GPU column is compact.** A tensor-split group reads `gpu-4+5` rather
  than `gpu-4 + gpu-5`: the repeated prefix was the widest thing in the table
  and said nothing new. GPU ids are also coerced to integers before rendering,
  since they are peer-supplied and that string is interpolated into the page.
- **The VRAM column has lost its bar.** A 46px meter sat beside
  `25.5G/32.0G`, showing the same ratio the numbers give. Only its warning
  survives, as the colour of the text — amber above 70% of the card, red above
  90% — so the signal is kept at no width. The Tokens column keeps its meter,
  where there is no second reading of the same thing.
- **The parts that decide who may do what are now covered by tests.**
  `internal/power`'s chassis control had none at all: the allowlist (no
  wildcard, no suffix matching, an empty list permitting nothing), the fixed
  action set, `Local` refusing to cut its own power before reaching
  `ipmitool`, and the diagnostic path that must never echo the BMC password.
  `internal/activity` went from a third covered to nearly all of it, and the
  mesh address check gained the NAT64 and IPv4-mapped cases that prove an
  address cannot be smuggled past a range check by encoding it in another form.
- **A member could put an alias in the table that no operator could.** The
  gossip merge validated the alias *name* but never the entry, while the local
  write path refuses an empty target ("target is required"). A live entry with
  no target is now ignored on merge too; a tombstone legitimately has none.
  Found by the repository's first fuzz targets, which cover the two entry
  points that consume bytes from another member — `Service.NotifyMsg` and
  `Service.MergeRemoteState` — and assert the table stays within its cap, holds
  no unnamed or unvalidated entry, and still marshals. 720,000 executions found
  nothing further.
- **`viiwork-mcp` pointed at the wrong port.** Its default was
  `http://localhost:8080`, which is viiwork 1's port and serves nothing on a v2
  fleet, so running it without `--url` or `VIIWORK_URL` only ever produced a
  connection refusal. Now 8086, in the flag help, the doc comment and
  `.mcp.json`. It also calls `meshapi.PathModels`, `PathCluster` and
  `PathChatCompletions` rather than repeating the strings, and has tests: it
  previously had none.
- **`mesh.Options` documents two things a second implementation lost time to.**
  `OnChange` may be called *before* `Start` returns — the dispatcher runs
  before memberlist does, and memberlist notifies about the local node while
  bootstrapping, so a handler reading something built from `Start`'s return
  value races its own assignment. And `TailnetSocket` and `LocalAPISocket` are
  not alternatives: the first turns discovery on, the second answers "what is
  my own tailnet address" and is needed even with discovery off. Both found by
  the gateway building against beta1; no behaviour changed.
- **Fixed a panic in the activity log.** `emit` sent to a snapshot of the
  subscribers taken under the lock and released before sending, while
  `Subscribe` closes a subscriber's channel when the log is at capacity. The
  two interleaving meant a send on a closed channel, which panics — a select's
  `default` case does not prevent that. Sends now happen under the lock, which
  cannot stall because they are already non-blocking. The capacity is 16 and a
  browser tab or a connected mesh-stream client is one each, so this was not a
  stress-only path. `TestEmitDoesNotPanicWhileSubscribersAreEvicted` reproduces
  it in 200 rounds against the old code.
- **Truncated prompts are cut on a rune boundary.** The 50,000-character cap is
  applied in bytes, which lands mid-character in any non-ASCII script — the
  normal case on a translation fleet — and the tail reached `/v1/prompts` as
  invalid UTF-8 for the JSON encoder to silently replace with U+FFFD.
- **The single-node dashboard escapes for attribute position too.** Its
  escaper set `textContent` and read back `innerHTML`, which leaves `"` and
  `'` untouched — safe between tags, unsafe inside an attribute, and it was
  used inside one (`class="status ..."`). A value carrying a quote would have
  closed the attribute and turned the rest into markup. Nothing reached it:
  that value is the node's own backend state, from a fixed vocabulary. Fixed
  anyway, along with an event type that went to the page unescaped and GPU ids
  that are now coerced to integers before being rendered as markup, so the
  page no longer depends on where its data happens to come from. `/mesh`,
  which does render other members' data, was already strict.
- **Endpoint paths are pinned** in `meshapi/wire_test.go`, for the same reason
  field names are: a consumer in another repository dials them or compares
  against them, so changing one silently breaks something that cannot be
  updated in the same commit. Only machine-to-machine paths are listed. The
  browser pages and `/v1/metrics`, which only the dashboard fetches, are
  deliberately left out — freezing a UI route in a package with no migration
  path would make an HTML URL a permanent commitment, and the boundary this
  contract draws is what nodes say to each other.
- The README describes viiwork as built *originally for* Radeon VII rather than
  *on* it: the config already accepts NVIDIA hardware and other engines, so the
  old phrasing read as a hardware requirement rather than as provenance.

Both dashboard changes are in `web/`, which `viiwork-nvidia` imports rather
than forking, so they reach that fleet view too.

## v2.0.0-beta1

**First public release of viiwork 2.** Everything below under `v2.0.0-alpha.3`,
`v2.0.0-alpha.2` and `v2.0.0-alpha.1` was developed privately and is released
together here; those entries are kept because they say why each part is the way
it is.

Beta rather than a release candidate because this is the first build outside
the fleet it was written on: one machine has been converted and is serving
production traffic, and `llamacpp` is the only engine. See the alpha.3 notes for
what shipped most recently, and `docs/migrating-to-v2.md` to convert a 1.x host.

## v2.0.0-alpha.3

### Acceptance tooling, a conversion guide, and three fixes from the first real node

**`viiwork-accept`** is a new command that checks a node and a mesh without
changing either: `config` validates a v2 file and summarises what it will run,
`ports serve`/`probe` prove gossip reaches a machine on tcp and udp, `join`,
`ready` and `gone` time a node in and out of the cluster, `models` exercises
content and tool calls per model, `saturate` overflows a model's local slots to
confirm the mesh takes the overflow, and `alias` checks a name resolves through
every entry point. It is read-only by design — it reads state and sends
inference requests, and never starts or stops anything.

**`docs/migrating-to-v2.md`** converts a host: the v1-to-v2 key mapping, a
worked example, mesh modes, and a step-by-step procedure that validates the new
file while v1 is still serving, so an invalid file is never discovered after the
old instances have stopped. `update.sh` and `rebuild.sh` now wait on
`:8086/health`.

**Fixes**

- **`/health` no longer reports a joining node as healthy.** The API listener
  starts before the mesh and the supervisor, so a node still joining — or one
  that cannot reach tailscaled, and so never will — answered 200 `"ok"` with no
  backends, indefinitely. The model count now comes from the running
  configuration rather than from the supervisor, so a machine with models
  configured and nothing serving answers 503. A node with no models configured
  is still healthy: a pure router is a valid node.
- **A pipeline can no longer be an alias target.** The alias table is
  replicated to every member; a pipeline runs only on the node that configures
  it, so such an alias could never resolve anywhere else. It is now refused in
  targets and fallbacks, and `--force` no longer creates one — previously the
  refusal message invited `--force`, which produced an alias that resolved
  `unavailable` forever.
- **A `[debug]` line escaped `VIIWORK_DEBUG`.** The think-disabled stream
  logged its scanner error unconditionally, and a client that stops reading
  ends a stream in error, so ordinary aborted generations wrote to the log on a
  per-request path in every deployment.

### Not in this release

- `llamacpp` is the only engine. The vLLM and FreeToken engines land in v2.1.0.
- `scripts/setup-node.sh` and `deploy.sh` still write and drive v1 layouts.

## v2.0.0-alpha.2

### One binary, one node per machine, a mesh that forms itself

viiwork 2 runs **one process per machine**. That node supervises every model
configured on the machine — a `models:` list replaces the v1 instance files —
and serves the API and every dashboard on **port 8086**, with membership gossip
on **7946** (tcp and udp). Per-model instance ports, `server.mesh_port` and the
port contention behind it are gone: every machine is `http://<machine>:8086`.

**The mesh forms itself.** Nodes find each other through tailscaled
(`mesh.network: tailnet`, the default), mDNS on a LAN (`mesh.network: lan`) or
`mesh.seeds`, so adding a machine means writing that machine's config and
nothing anywhere else, and a machine that stops leaves routing within seconds.
A mesh is either **secured** — `VIIWORK_MESH_SECRET`, base64 of 32 bytes, the
same everywhere: encrypted gossip and signed forwards — or declared **open**
with `mesh.open: true`. A node refuses to start with neither or both, and a
node in the other mode logs `mesh mode mismatch` instead of joining. Of two
processes started with the same node name, the newer one exits.

**Routing follows free slots.** Every node polls each member's `/v1/capacity`
once a second. A request runs on a local backend with a free slot, else goes to
the member with the most free slots, else waits in a FIFO queue on the node that
received it, for up to `routing.queue_timeout` (20 s). A member that refuses a
forward before its first byte is retried elsewhere and not picked again until it
reports fresh capacity. A receiving node admits a forward only into a free slot
and never forwards it again. Responses carry `X-Viiwork-Node` (where it ran),
`X-Viiwork-Origin` (where it arrived) and `X-Viiwork-Queued-Ms` when it waited.

**Aliases.** `viiwork alias set stable-coder Qwen3.8-27B --fallback granite-4.1-8b`
points a stable name at a real model across the whole mesh; `ls`, `rm`,
`revert`, `history`, `export` and `import` complete the command, which reports
when every member has the change. An alias resolves on the node that receives
the request, to its target if any member serves it, else to its first served
fallback, else 503. Aliases gossip between nodes and persist in
`node.state_dir`. Writes need a signature in a secured mesh and must come from
the node's own machine in an open one. `/v1/models` lists aliases with
`owned_by: alias`, and aliased responses carry `X-Viiwork-Alias` and
`X-Viiwork-Model`.

**Reload on SIGHUP.** `docker kill -s HUP viiwork` or `systemctl reload viiwork`
re-reads the config: added models start, removed ones drain, and a changed entry
restarts that model alone. An invalid file keeps the running configuration, and
changes outside `models` are logged as needing a restart.

**GPUs.** Backends are pinned with `ROCR_VISIBLE_DEVICES` or
`CUDA_VISIBLE_DEVICES`, inherited device variables removed first. Within a minute
of a backend turning healthy, the node checks with `rocm-smi` or `nvidia-smi`
that its processes hold the cards they were given, and takes a backend found on
another card out of service. Docker nodes need `pid: host` for that check;
without it the verdict is "unknown" and nothing is stopped. An `nvidia-smi`
telemetry collector joins the ROCm one, so dashboards and the energy store read
both vendors.

**Dashboards** keep their look on the new payloads. `/mesh` gains an aliases
section and backends per member, and `/chat` lists aliases beside models and
shows which node answered.

**Upgrading.** A v1 `viiwork.yaml` is refused at startup with
`v1 config: see docs/migrating-to-v2.md`. That guide maps every key — note that
`models[].context` is now **per slot** — and covers mesh modes, Docker and
systemd, large models (`startup_timeout`) and rollback.
`configs/docker-compose.v2.example.yaml` is a complete node. A clean stop drains
in-flight requests for up to `health.respawn_grace`, so give the container
`stop_grace_period: 90s`. v1 and v2 nodes do not form one mesh.

**Removed:** `server.mesh_port` and per-instance ports; `--section.key`
overrides on the command line (the config file is the only input); `balancer`
(routing is by free slots); `peers` (discovery is automatic);
`model.n_gpu_layers` and `health.evict_on_hard_failure`; the v1 packages kept
alongside alpha.1.

**Dependencies:** `hashicorp/memberlist` and `hashicorp/mdns` join
`gopkg.in/yaml.v3`. The binary links 16 modules; `go list -m all` lists 104,
most of them only in the new modules' own dependency graphs.

### Not in this release

- The vLLM and FreeToken engines follow later; `llamacpp` is the only engine.
- `scripts/setup-node.sh`, `deploy.sh`, `update.sh` and `rebuild.sh` still write
  and drive v1 layouts.

## v2.0.0-alpha.1

### Contracts for viiwork 2, no runtime change yet

The module path is now `github.com/janit/viiwork/v2`. This pre-release ships
the frozen contracts of the viiwork 2.0 design as compiled,
tested code so the engine, gateway and RouteMap work can build against them:
config v2 (`internal/config`), the engine interface (`internal/engine`), node
metadata (`mesh`), the wire types and headers (`meshapi`), and the alias table
with its merge rule.

The binary still runs v1. `cmd/viiwork` uses the v1 config and mesh packages,
moved unchanged to `internal/v1/config` and `internal/v1/meshapi`, until
2.0.0-alpha.2 switches it over. Contracts change only by bumping the alpha.

## v1.8.1

### Peer polling no longer stalls routing behind dead hosts

`Registry.PollOnce` polled peers one at a time, and a peer whose host is
powered off drops packets rather than refusing the connection, so each such
peer cost a full `peers.timeout`. A round therefore took *dead peers ×
timeout* — on an 11-instance fleet with 8 hosts down and a 5s timeout, about
40s against a configured 10s `poll_interval`, and the ticker simply dropped
the ticks it could not keep up with. For that whole window every peer's
reachability, model list and in-flight count stood still. A node that had
just died kept its routes, and because its last-polled in-flight count was
frozen low while the live backends filled up, `PickRoute` *preferred* it:
the mesh converged onto the dead node, and each request sent there waited in
`proxyToPeer` on a host that was never going to answer. That is the bimodal
latency seen on multi-homed models — a steady sub-second mode and a second
mode of 9–40s stalls on 10–20% of requests — and why a host serving only
single-homed models never showed it: its requests never took a peer hop.

Peers are now polled concurrently, so a round costs about one timeout
however many peers are dark, and a discovery round skips a verified peer
whose status poll just failed rather than running the timeout out again on
its cluster poll. The forwarding client bounds the TCP handshake to a peer
at 5s while keeping its 120s overall timeout, which a streaming completion
needs; a dial to a powered-off host previously ran to the kernel's own
connect timeout. On a three-node reproduction mesh with eight blackholed
peers, peer discovery went from 50s to 15s and a wedged node's p90 from
26.9s to 0.15s.

Requests dispatched inside the detection window — between a node going dark
and the next poll noticing, floored by `poll_interval` — can still stall.
Marking a peer unreachable write-through on a failed forward and retrying on
another route would close that, and is the intended follow-up.

## v1.8.0

### Open Chat from the mesh view, pinned to a host

`/mesh` gains an **Open Chat** section: one link per model, opening `/chat`
for that model in a new tab. `/chat` reads `?model=` and `?host=` from its
URL and reflects every change back into it, so a chat state is linkable; a
**backend** selector offers `mesh` (routing as before) or any host currently
serving the model, and each reply says where it ran.

Under it, `/v1/chat/completions`, `/v1/completions` and `/v1/embeddings`
accept `?host=<hostname>`, which narrows routing to that machine — several
co-located instances count as one host and are still balanced across.
Absent or `mesh` routes exactly as before. A host that does not serve the
model is a `404` naming both, never a quiet fallback to the mesh; a malformed
value is a `400`. The value is only compared against hostnames the node
already knows and is never dialled, so it cannot reach anything normal
routing could not. The query string already travels with a forward, so the
pin holds across a hop and works from `curl`. `peer.Route` gains a `Host`
field and `peer.FilterByHost` applies the pin.

## v1.7.0

### Mesh gossip: peers.hosts becomes a seed list

With `peers.gossip.enabled: true` and a shared `VIIWORK_MESH_SECRET` (at
least 32 bytes, environment only, never YAML), a node learns its peers'
peers transitively: one reachable address is enough to join the mesh, and
N×N peer configuration goes away. Off by default — with it off, behaviour
is byte-identical to v1.6.x, on the wire included.

Membership is proved, not assumed. Every mesh-to-mesh call carries an
HMAC-SHA256 proof (`X-Viiwork-Auth` and friends) over pinned canonical
strings, with a 120s skew window. Endpoints stay readable without it, so
browsers, the gateway and viiwork-nvidia are untouched; what a proof buys
is standing — only a verified peer's cluster report is believed, only a
validated address (IP literal, allowed ranges, strict port) is ever
dialled, the learned-peer intake is capped, and an adopted address is not
routed to or advertised onward until it proves membership on its own
status poll. Configured peers stay first-class with or without a proof,
which is what makes a mixed-version rollout safe.

`ClusterPeerInfo` gains `origin` (`config` or `learned`), additive and
omitempty. `require_forward_proof` (default off) upgrades the forgeable
`X-Viiwork-Forwarded` claim to a signature over method, path and body with
a replay-closing nonce cache; flip it on only once the whole fleet signs.

The registry's peer set is now an atomic snapshot rather than a plain
slice, so it can grow at runtime without racing the request path.

## v1.6.3

### The mesh event stream no longer writes to a finished response

`/v1/mesh/stream` runs one goroutine per peer plus a cluster-snapshot loop, and
all of them wrote to the HTTP response directly, serialised by a mutex. The
mutex made those writes mutually exclusive but said nothing about when they
*stop*: the handler could return while a producer was still mid-write, and
touching an `http.ResponseWriter` after the handler returns is a data race and
a violation of `net/http`'s contract.

It was reproducible — the race detector flagged it on roughly two runs in three
— and it had been shipped in every build since the mesh stream gained peer
fan-out.

The handler is now the only writer of its own response. Producers hand encoded
frames to it over a channel and never hold a reference to the response at all,
so the problem is gone by construction rather than by lifetime bookkeeping.

Waiting for the producers instead would have deadlocked, which is worth
recording: an SSE response must not carry a `WriteTimeout`, so a connected but
stalled client can block a write indefinitely — and the activity log closes the
oldest subscriber's channel when it hits its subscriber limit, which returns the
handler with the client still perfectly healthy. Waiting there would have hung
the request on a producer that could not finish.

**Upgrading:** nothing to do. No configuration, endpoint, wire field or on-disk
format change, and the stream behaves identically from a consumer's side.

## v1.6.2

### Builds stamp the version they actually are

`make build`, `scripts/update.sh` and `scripts/rebuild.sh` each derived the
version with `git describe --tags`. That is right in the public repository,
which is tagged at release. It is wrong in the development tree, which carries
no tags — they are created on the public repo at publish time — so describe
reported the newest tag it could still see.

The effect was that v1.5.3, v1.6.0 and v1.6.1 all built as `v1.5.2-N-g<sha>`,
and reported that on `/v1/status` and in the dashboard. The version an operator
reads off a misbehaving host was two releases stale.

`scripts/version.sh` now answers the question once for all three callers. An
exact tag still wins, so a release build from the public repository is
unchanged and prints exactly its tag. Otherwise the version comes from
`CHANGELOG.md`'s top heading — the one place a tagless tree knows what it is,
and a file that is updated as part of cutting a release, so it cannot drift the
way a tag the repo does not carry can. A short sha and a `-dirty` marker are
appended between releases, and a tree with neither git nor a changelog falls
back to `dev` rather than failing the build.

**Upgrading:** nothing to do, and no code changed — this is build tooling only.
Hosts rebuilt from this version onward will start reporting their real version;
one built earlier keeps whatever it was stamped with until it is rebuilt.

## v1.6.1

### `energy`: attribution is now swappable, for nodes that measure each card

`energy/doc.go` told an implementation that measures per-board power to supply
`AttrW` and `RawW` as the same value. `Recorder` gave it no way to do that — it
ran the whole-chassis split unconditionally.

That split is right when the node figure measures more than the GPUs do: fans,
CPU, drives and PSU losses are drawn whether or not a model is serving, and
charging them to a model would be wrong. It is meaningless when node power is
*itself* the sum of the per-card readings, because then any residual is not
overhead but idle draw on cards that exist to serve the resident model.

The failure was not a small skew. Idle floors fall back to the lowest current
reading when the store has no history, so on a fresh store an evenly loaded host
has no marginal power at all and **every card is attributed 0 W** — per-model
energy stays empty until a genuinely idle minute is observed, which a busy
inference node may not see for weeks.

- **`NewRecorderWithAttribution`** takes an `AttributeFunc` deciding how a
  bucket's node figure is split between cards.
- **`Direct`** is that function for a per-board producer: each card is charged
  what it drew, the shares sum to the node figure, and the baseline is honestly
  zero.
- **`NewRecorder` is unchanged**, in signature and in behaviour — it passes nil
  and gets the existing split. A test pins the two as byte-identical.

Everything around the attribution step stays shared, which is the point: the
per-minute averaging and the `CoveredS` accounting that keeps a restart
mid-bucket from being extrapolated to a full minute are exactly what you would
least want a second copy of.

**Upgrading:** nothing to do. Additive API, no configuration, endpoint, wire
field or on-disk format change, and what viiwork records is unchanged.

## v1.6.0

### The energy store is now a public package

`internal/energy` has moved to `github.com/janit/viiwork/energy`. It keeps a
durable per-host, per-model kWh history — node draw, per-GPU draw, and a split
of one between the models causing it — in a fixed-size on-disk store that never
grows past a couple of megabytes.

It moved for the same reason `meshapi` did in v1.5.3: the fleet has a second
implementation. `viiwork-nvidia` drives vLLM on CUDA hardware and reports the
same `energy_kwh_24h` to the same dashboard, and Go does not allow importing
`internal/` across module boundaries. The alternative was a second copy of the
ring, tier and model-table code — two independent producers of one binary
format, with nothing keeping them in step.

The seam that makes this work was already there. The package never runs a
command or reads a sensor; it takes a `NodeWattsFunc` and a `GPUReadingsFunc`
and knows nothing else about where power comes from. viiwork fills them from
`ipmitool` and `rocm-smi`, and another implementation fills them from
`nvidia-smi`.

Three things came with the move, because a format with two producers is not the
same object as a format with one:

- **The on-disk layout is documented as a contract.**
  `docs/energy-store-format.md` specifies it byte by byte — header, record
  layouts, the timestamp-derived slot rule, the model table, roll-up weighting
  — and states what may change and what may not. `energy/doc.go` covers the Go
  API.

- **A format mismatch is now refused rather than silently repaired.** Opening a
  store whose magic or record size disagrees with the running build used to
  recreate the file, which was a local annoyance with one producer and the
  destruction of a year of somebody else's history with two — indistinguishable,
  months later, from a node that had never been switched on. It now fails with
  an error naming the file, and leaves the bytes untouched. Changing a slot
  count or adding a GPU is *not* that kind of change: those are per-deployment
  configuration and still recreate the file, saying so in the log as before.

- **The store records what its node wattage actually measured.** A
  whole-chassis IPMI reading and a sum of GPU board power are the same bytes in
  the same field, and differ by hundreds of watts. A `source` file in the store
  directory now carries the same label the mesh publishes as `power_source`, and
  an absent one reads as unknown rather than as a default.

**Upgrading:** nothing to do. No configuration, no endpoint and no wire field
changed, existing stores are read as-is, and energy tracking stays off by
default. Anything importing `viiwork/internal/energy` — which nothing outside
this repository could — updates its import path.

## v1.5.3

### The mesh protocol is now a public package: `meshapi`

Everything a node publishes to other nodes and to the dashboard — the
`/v1/status` payload, the `/v1/cluster` snapshot, the activity event stream, the
prompt lookup, the endpoint paths — now lives in `github.com/janit/viiwork/meshapi`
instead of being spread across `internal/`. No wire format changed; this is the
same bytes on the network, defined in one importable place.

It moved because the mesh has a second implementation. `viiwork-nvidia` drives
vLLM on CUDA hardware and joins the same mesh and the same dashboard, and Go
does not allow importing `internal/` across module boundaries. Rather than let
a second copy of every struct drift on another repo's schedule, the contract is
published and both sides depend on it.

`internal/peer` and `internal/activity` keep their familiar names through type
aliases, so `peer.StatusResponse` and `meshapi.StatusResponse` are the same
type and no call site changed. What did change is that 215 lines of duplicated
definitions are gone.

Three things came with it that were previously implicit:

- **The activity message grammar is documented as wire format**, because that
  is what it is. A request event reads `"<model> → <destination>"` with a
  terminal suffix once it finishes, and every dashboard in the fleet
  reconstructs its in-flight rows by splitting that string and matching the
  terminal word — there is no server-side registry of running jobs anywhere.
  `meshapi.RequestStarted`/`RequestDone`/`RequestAborted` build the messages and
  `SplitRequestMessage`/`IsRequestTerminal` take them apart; `handler.go` and
  `balancer.Label` now go through them rather than spelling the format out.

- **The compatibility rules are stated and tested.** New fields are additive and
  `omitempty`, and absent is not zero — a consumer must read a missing number as
  "unknown", never as a measured zero. That is what lets a fleet whose machines
  are upgraded days apart render at all. `wire_test.go` pins every field name,
  so a rename fails the build rather than silently stranding a column on a host
  you did not upgrade.

- **The package is stdlib-only and self-contained**, which keeps the option of
  extracting it into its own module cheap should that ever be worth doing.

**Upgrading:** nothing to do. No configuration, no endpoint and no field
changed, and a node on this version is wire-compatible in both directions with
every node that predates it.

## v1.5.2

### The mesh dashboard has a fixed address: port 8086 on every host

`http://<any-host>:8086/` now serves the cluster view, and the number is the
same on every host in the fleet.

The cluster view was always served by every node; the problem was reaching one.
A host runs one viiwork instance per model, each on its own `server.port`, so
opening the dashboard meant knowing which instance was up on which host —
precisely what you do not have when something is wrong.

The port is **contended, not assigned**. Every instance asks for it at startup
and the OS gives it to exactly one; the rest keep asking every 15 seconds. So
it is up as long as *any* viiwork on that host is, it hands over on its own
when the instance holding it restarts, and it needs no designated node, no
per-host configuration and no reverse proxy. Which instance answers does not
matter — the mesh view is built from peer state every node already has.

A node reached on 8086 is an ordinary node in every other respect: only `/`
moves, so the page's own `/v1/mesh/stream`, `/v1/mesh/power` and `/prompt`
calls resolve against the same origin, and CORS applies as usual.

**Upgrading:** this binds a second port that previous versions did not, on by
default. Nothing else changes — `server.port` and the per-node dashboard are
untouched, and a host where something else already holds 8086 simply never
binds it and runs exactly as before. `server.mesh_port: 0` opts out;
`server.mesh_port: <n>` moves it. There is a matching `--server.mesh_port`
CLI override.

## v1.5.1

Documentation only, no code change.

- **New README screenshot**, showing the mesh dashboard as of v1.5.0: fleet
  totals, live power with 24-hour and 30-day energy, the per-host RAM strip,
  and the listen ports on model groups. The previous one predated all of it.

## v1.5.0

### Chassis power control from the mesh dashboard

Each host in the Fleet Power table can carry a power button. **Off by default**,
and there is no wildcard: only hosts named in `power.control.hosts` can be
targeted, by the dashboard or by anything else.

A running host is controlled in-band by the node living on it, with no
credentials — the mesh forwards the request to whichever node owns the target.
A host that is **powered off** has no node to ask, so reaching it needs BMC
credentials; without them it can be switched off but not back on, and the
button says so rather than failing. Hosts in the allowlist appear in the table
even when they are absent from the mesh, which is exactly the state a
powered-off host is in.

Guards, none of which is authentication: the allowlist, a confirmation prompt
naming host and action, and a node's refusal to power off its own host — that
would destroy the answer to the request and the page asking it, so its button
is disabled and the server refuses it too.

### Ports on model groups

Grouped by model, each backends group header lists the ports that model is
served on. Not shown when grouped by host, where the ports belong to different
models and listing them together would imply an addressing that does not exist.

### A 30-day energy total alongside the 24-hour one

The Fleet Power headline carries the rolling 30-day total at its far right,
opposite the live reading, and each host's row now has `now`, `24h` and `30d`
columns rather than one packed value. Two bare kWh figures side by side are
indistinguishable, so the columns are named once in a header instead of
repeating the window on every row.

Published as `energy_kwh_30d` beside `energy_kwh_24h`. It reads the day tier —
30 buckets against 720 hourly ones — and like the 24-hour figure it is a
whole-host number: group by `hostname` rather than summing across instances. A
store younger than a day reports the same value for both windows.

## v1.4.0

### Energy beside live power

The mesh dashboard's Fleet Power headline now reads `1,751 W / 12.4 kWh (24h)`
— what the fleet is drawing now, and what it has drawn over the rolling last
24 hours. Each host's row carries the same pair.

The figure rides on `/v1/status` and `/v1/cluster` as `energy_kwh_24h`, so the
dashboard reads it off the snapshots it already receives rather than polling a
new endpoint. It is a **whole-host** number like `power_watts`: the durable
store runs on one instance per host, so consumers must group by hostname rather
than summing across instances. It reads the minute tier, whose ring is exactly
24 hours, so the window and the retention are the same span.

Energy is opt-in per host while power is not, so the header says `· kWh from N`
whenever fewer hosts contribute energy than are reporting power — a total from
one host should not read as fleet-wide.

## v1.3.0

### Stale in-flight rows after a sleep or a background tab

The mesh and node dashboards reconstruct in-flight requests from the event
stream — a start event adds a row, a done event removes it — and nothing
replayed the events lost while a browser was away. A laptop waking from sleep,
or a tab throttled in the background, came back with rows that had finished
hours earlier and counted up in red forever.

Both streams now replay their event ring when the connection opens, marked
`"replay": true`, and both dashboards clear the reconstructed set on reconnect
and let the replay rebuild it. A gap longer than the ring is still not
recoverable, but the result is then a short count rather than invented work.
Opening the page mid-flight now also shows the jobs already running, which it
never did before.

If you consume `/v1/mesh/stream` or `/v1/activity/stream` yourself, see the
notes in `docs/api-integration.md`: rebuild on reconnect rather than carrying
state across, and deduplicate anything you display, since replayed events
repeat what a visible log already shows.

### Fleet totals on the mesh dashboard

GPUs busy, VRAM and host RAM across the whole mesh, as three readings above the
model list. All three are counted per host: `rocm-smi` reports every card on a
machine rather than only the ones an instance owns, so a naive sum over the
payload reported 110 GPUs for 50 actual cards on a fleet with co-located
instances.

Hosts now sort by name in the power rows, the stacked bands and the RAM strip,
instead of by value.

### Fixed

- **The single-node dashboard showed every activity line twice** once streams
  began replaying, because it also fetched `/v1/activity` separately. The fetch
  is gone; the stream carries it.
- **`stream reconnecting…` accumulated** on the mesh header, once per retry,
  while a network was down.

## v1.2.0

### Power probing, per-GPU wattage, and a durable energy store

Node power had been silently reporting 0 W across the whole gfx906 fleet, which
also kept cost tracking switched off — `cost.Tracker` gives up when power is
unavailable. Every Gigabyte board here answers `sdr type "Power Supply"` with
presence flags and no wattage, so the previous hardcoded command summed nothing
and reported a confident zero. The sampler now probes for a reading that is
actually above zero: DCMI first (the standardised whole-node reading), then the
`Power Supply` sensor class, then any Watts-valued sensor. `power.source` pins
it to `dcmi`, `sdr`, `sensor:<NAME>` or `none` if you would rather not probe,
and whichever source was adopted is logged and published as `power_source`.

`rocm-smi` is now also asked for per-GPU package power, with a fallback to the
original flags if that arg set is rejected — wattage is a bonus, and losing
utilisation and VRAM to gain it would be a bad trade.

On top of those, `internal/energy` keeps a durable per-host, per-model kWh
history: node draw, per-GPU draw, and a marginal-power split between the models
causing the load and the baseline a host draws just by being switched on. Off by
default; see *Energy History* in the README. Enable it on exactly one instance
per host — node wattage is a whole-host measurement.

### The mesh dashboard shows fleet power

`/mesh` gains a **Fleet Power** panel: the mesh total as a headline number, a
stacked graph with one band per host, and a table naming each host's draw and
which IPMI reading it came from. Grouped by host, the backends table also carries
each host's wattage on its group header.

It needs no configuration and adds no polling — the samples come off the cluster
snapshots the dashboard already receives. The window is since page load, capped
at 720 readings; it is a live view, not history.

Under it is a film strip of per-host RAM: one small sparkline per host, sharing
one time window, each scaled 0 to that host's total so height reads as memory
pressure. Host memory now travels on `/v1/status`, so the strip covers every
host rather than only the one serving the page.

Host memory used to be stripped from the pushed mesh snapshot, because an exact
figure moves every second on a live host and made the stream push a full
snapshot that often. It is now coarsened to 64 buckets with a deadband instead —
a bucket is under a pixel on the strip, and `/v1/cluster` still carries the exact
figures.

### Fixed

- **Cluster wattage counted multi-instance hosts several times.** A host running
  one viiwork instance per model reports the same whole-host BMC reading from
  each of them, and both dashboards summed the payload as it arrived — three
  times over on a three-tenant host. Both now key by hostname and count each
  host once.
- **Co-located instances appeared as two hosts.** `/v1/cluster` derived a peer's
  hostname from the address it is dialled on, but co-located peers are
  configured by IP, so one machine showed up under both its hostname and its
  address.
  The hostname the peer actually reports is now preferred, with the
  address-derived one kept as the fallback for peers too old to report one.
- **A single energy recorder left most of its host unattributed.** Recording
  runs on one instance per host, and that instance labelled only the GPUs it
  owns — one card in ten on a 10-GPU host. Model labels for a co-tenant's cards
  now come from the peer poll, which already carries `gpu_ids`, a model and a
  hostname.

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

### To v1.5.0

Nothing changes on a node until you opt in. Power control is off by default and
has no wildcard: it does nothing until you name hosts.

```yaml
power:
  control:
    enabled: true
    hosts: [host-a, host-b]
```

That much controls hosts that are **running**, in-band, with no credentials. To
reach a host that is **powered off** you also need a `bmc` block with
credentials — see *Power control* in the README. Without it a host can be
switched off but not back on, and the dashboard shows a disabled button saying
so.

Enabling it is a real grant: **the API authenticates nothing**, so anything that
can reach a node can power off any host in that node's allowlist. Keep the list
to the hosts you actually want reachable this way. A node still refuses to power
off its own host, so a dashboard cannot kill the machine serving it.

New consumer-visible fields: `power_control` on `/v1/cluster` (absent unless
enabled), and `energy_kwh_30d` beside `energy_kwh_24h`.

### To v1.4.0

Nothing to do on a node, and nothing to change in a consumer. The one addition
is `energy_kwh_24h` on `/v1/status` and `/v1/cluster`, which appears only where
the energy store is enabled.

If you read it, treat it as a **whole-host** figure like `power_watts`: the
store runs on one instance per host, so group by `hostname` rather than summing
across instances. It covers the rolling last 24 hours, not the store's full
history.

### To v1.3.0

Nothing to do on a node. The change that matters is for anything consuming
`/v1/mesh/stream` or `/v1/activity/stream` in your own code: both now replay
their recent event ring when a connection opens, so a consumer that appends
every event it receives to a visible list will show the replayed ones a second
time. Deduplicate on node, request id, timestamp and message, and rebuild
reconstructed state on reconnect rather than carrying it across. See
`docs/api-integration.md` §6.

Dashboards served by viiwork already handle both.

### To v1.2.0

Nothing in this release changes behaviour on an existing deployment until you
opt in, and the API additions are all `omitempty` — a node on 1.2.0 meshes with
one on 1.1.x, each simply showing blanks for what the other does not report.

Two things are worth doing per host:

- **Give one container per host the BMC device** (`- /dev/ipmi0:/dev/ipmi0`) to
  get node wattage on the dashboard. Without it a host reads 0 W exactly as
  before, and cost tracking stays off, since it gives up when power is
  unavailable.
- **Enable the energy store on exactly one instance per host**, with a volume
  that outlives the container. Node wattage is a whole-host measurement, so
  enabling it on several instances of a multi-model host records the same draw
  several times over.

If you consume `/v1/cluster` from your own application, read the notes on
`hostname`, `power_watts` and `host_mem_used_mb` in `docs/api-integration.md`:
per-host figures must be grouped by hostname rather than summed across
instances, and host memory on the pushed stream is now coarsened.

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
