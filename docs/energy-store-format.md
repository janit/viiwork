# The viiwork energy store format (`VIIWENG1`)

This is the on-disk contract for `github.com/janit/viiwork/energy`. It is
specified here rather than left to the Go source because the format has more
than one producer. `viiwork` writes it from `ipmitool`, `rocm-smi` or
`nvidia-smi`; `viiwork-nvidia`, which wrote it from `nvidia-smi`, is frozen
since 2026-09-13, and stores it wrote are still read. The Go package is the
reference implementation; this document is what a second one is written
against.

**Status:** frozen as of v1.6.0. See [Compatibility](#compatibility) before
changing anything in it.

Everything is **little-endian**. Floats are IEEE-754 `binary32`. Signed
integers are two's complement. There is no padding beyond the reserved bytes
named below, and no alignment requirement — every offset is absolute.

## Directory layout

A store is a directory. Nothing outside it is consulted, and the directory is
self-describing: geometry lives in each file's header, so a store can be copied
to another machine and read without its `viiwork.yaml`.

| file | slots | lanes | record | bytes |
|---|---|---|---|---|
| `node-minute.ring` | 1440 | 1 | 16 | 23,072 |
| `node-hour.ring` | 8760 | 1 | 16 | 140,192 |
| `node-day.ring` | 365 | 1 | 16 | 5,872 |
| `gpu-minute.ring` | 1440 | *G* | 24 | 32 + 34,560·*G* |
| `gpu-hour.ring` | 8760 | *G* | 24 | 32 + 210,240·*G* |
| `gpu-day.ring` | 365 | *G* | 24 | 32 + 8,760·*G* |
| `models.txt` | — | — | — | grows, bounded by 65,535 names |
| `source` | — | — | — | one line |
| `client-minute.ring` | 1440 | *L* | 24 | 32 + 34,560·*L* |
| `client-hour.ring` | 8760 | *L* | 24 | 32 + 210,240·*L* |
| `clients.txt` | — | — | — | grows, bounded by 65,535 names |
| `client-totals` | — | — | — | a few lines |

*G* is the number of GPUs on the host. The six rings are **preallocated in
full** at creation and never grow: 2.6 MB total for *G*=10. Retention is the
wrap, not a purge job.

The four client files exist only in a store where a client has been named. *L*
is `energy.client_lanes`, 8 by default: 1.9 MB for the two rings. See
[Client energy](#client-energy).

The slot counts above are the defaults (24 hours of minutes, one year of hours,
one year of days). They are configurable per deployment — see
[Geometry changes](#geometry-changes) — which is why every file records its own.

## Ring file

### Header — 32 bytes, at offset 0

| offset | size | type | field | value |
|---|---|---|---|---|
| 0 | 8 | ASCII | magic | `VIIWENG1`, not NUL-terminated |
| 8 | 2 | uint16 | record size | 16 for node rings, 24 for GPU rings |
| 10 | 4 | uint32 | slots | ring length in buckets |
| 14 | 4 | uint32 | lanes | records per slot: 1 for node, *G* for GPU |
| 18 | 14 | — | reserved | must be written as zero, must be ignored on read |

### Body

`slots × lanes` fixed-size records immediately after the header. The record for
a given slot and lane is at:

```
offset = 32 + (slot * lanes + lane) * recordSize
```

A slot that has never been written is all zeroes, which decodes to a record
with `TS == 0`. Readers must treat that as absent — a real bucket never lands
on the Unix epoch.

### Slot derivation

**A bucket's slot is derived from its own timestamp. There is no head pointer,
no write cursor and no sequence number anywhere in the format.**

```
bucket = bucketIndex(timestamp)      // see the tier table below
slot   = ((bucket % slots) + slots) % slots
```

The second modulo keeps the result non-negative for timestamps before the
epoch, which a clock that has not yet been set can briefly produce.

That one choice is what a second implementation most needs to get right,
because everything else follows from it:

- **Writes are idempotent.** Rewriting a bucket rewrites its own slot. A
  roll-up that runs twice converges instead of double counting.
- **A restart needs no recovery scan.** There is no state to rebuild.
- **Retention needs no purge.** A new record lands on the one exactly `slots`
  periods older and overwrites it.
- **Reads must filter by timestamp.** A slot may still hold a value from a
  previous lap; it is ignored on the strength of its `TS`, not skipped by
  position.

### Bucket index per tier

| tier | period | `bucket` | record `TS` |
|---|---|---|---|
| minute | 60 s | `floor(unix / 60)` | `bucket * 60` |
| hour | 3600 s | `floor(unix / 3600)` | start of the UTC hour |
| day | 1 day | `floor(localMidnightUnix / 86400)` | `localMidnightUnix` |

Minute and hour buckets are UTC-aligned. **Day buckets are keyed on local
midnight**, in the store's configured timezone, so that a "day" is the day the
operator lives in and matches the midnight reset that cost tracking already
does. A consumer reading a store must therefore know the timezone it was
written in to interpret the day tier's boundaries; the `TS` on each record is
the authoritative local-midnight instant either way.

> **Known edge.** Because the day bucket is `localMidnightUnix / 86400` rather
> than a count of local dates, two consecutive local days can share a slot in a
> timezone whose standard offset is UTC+0 and which observes DST — the second
> then overwrites the first. Verified affected: `Europe/London`, `Europe/Dublin`,
> `Europe/Lisbon`, `Atlantic/Canary`, `Atlantic/Faroe`, `Atlantic/Madeira`,
> `Africa/Casablanca`, `Antarctica/Troll` and their aliases, on the
> spring-forward date, losing one day-tier bucket per year. All other offsets,
> including `Europe/Helsinki` on the reference fleet, are unaffected, and the
> minute and hour tiers never are. Reproduce it before changing it: a different
> slot rule is a format change and must bump the magic.

## Records

### `NodeRecord` — 16 bytes

One time bucket of whole-node power.

| offset | size | type | field | meaning |
|---|---|---|---|---|
| 0 | 8 | int64 | `TS` | bucket start, Unix seconds. `0` = never written |
| 8 | 4 | float32 | `Watts` | mean draw over the **covered** part of the bucket |
| 12 | 2 | uint16 | `CoveredS` | seconds of the bucket actually sampled |
| 14 | 2 | — | reserved | zero |

### `GPURecord` — 24 bytes

One time bucket for one GPU. Its lane within the slot is the GPU's position in
the store's configured GPU-id list, which is *not* recorded in the file —
`GPUID` in the record is authoritative, and a reader should use it rather than
inferring identity from the lane.

| offset | size | type | field | meaning |
|---|---|---|---|---|
| 0 | 8 | int64 | `TS` | bucket start, Unix seconds. `0` = never written |
| 8 | 2 | uint16 | `GPUID` | host-local GPU index |
| 10 | 2 | uint16 | `ModelIdx` | row in `models.txt`; `0` = no model resident |
| 12 | 4 | float32 | `AttrW` | this GPU's share of measured node power |
| 16 | 4 | float32 | `RawW` | the unreconciled per-card reading |
| 20 | 2 | uint16 | `CoveredS` | seconds of the bucket actually sampled |
| 22 | 2 | — | reserved | zero |

### `CoveredS`, and why energy is not `Watts × period`

`CoveredS` is the load-bearing field for honesty across restarts. A minute that
saw only 20 seconds of samples contributes 20 seconds of energy, not a full
minute extrapolated from a fragment:

```
kWh = Watts * CoveredS / 3600 / 1000          // node
kWh = AttrW * CoveredS / 3600 / 1000          // GPU, attributed
```

Sum those over the records in a window to total it. Never reconstruct energy
from the bucket period.

`CoveredS` **saturates at 65,535** (it is a `uint16`), so any day bucket
covering more than 65,535 s — about 18.2 hours — reports 65,535. A fully
covered 86,400 s day therefore totals **about 24% low** from the day tier
(65,535 / 86,400 ≈ 0.76), and a 25-hour DST day more. Minute (60 s) and hour
(3,600 s) buckets always fit. **Total energy from the hour tier**; read the day
tier for mean watts, or for history older than the hour ring retains, knowing
its kWh is a lower bound. viiwork's own `energy_kwh_30d` sums the hour tier for
this reason. Widening the field would be a format change, which bumps the magic.

### `AttrW` versus `RawW`

`RawW` is what the sensor said. `AttrW` is that card's share of the *measured
node figure*, and it is the number to sum for "what did this model cost".

They differ because whole-chassis measurement includes power no GPU explains.
Each card is charged in proportion to how far it sits above its idle floor; the
residual — fans, CPU, idle cards, PSU losses — is reported as baseline rather
than smeared across models. The split always reconciles: baseline plus every
share equals measured node power, so no total is invented. `AttrW` therefore
sums to less than node power, and to more or less than `RawW` per card.

A producer that measures each board directly has nothing to infer, and writes
the same measured value to both fields — `AttrW == RawW`, with the shares
summing to `NodeRecord.Watts` and no baseline. The Go package spells that
mapping as `energy.Direct`, passed to `NewRecorderWithAttribution`; a
non-Go implementation just writes the reading into both fields.

## `models.txt`

Newline-separated UTF-8 model names, one per line, **append-only**. Line *N*
(zero-based) is `ModelIdx` *N*.

- **Line 0 is the empty string**, reserved for "no model was resident". That is
  a real state, not a null: a GPU can be powered and idle between deployments.
  The file therefore begins with a blank line.
- An index, once assigned, keeps its meaning for as long as the rings that
  reference it. A name is never reused for another model, and lines are never
  removed or reordered. This is what makes a per-model roll-up exact across a
  reconfiguration.
- A trailing `\r` is trimmed on read, so a file touched on Windows still loads.
- An index beyond the end of the table reads as unknown rather than as another
  model — a ring written by a build with a longer table degrades to blank, not
  to a wrong attribution.
- Bounded at 65,535 entries by `ModelIdx` being a `uint16`.
- Each append is `fsync`ed before the index is used, so a crash cannot leave a
  ring referencing a name that was never written.
- **Every line ends in `\n`.** A last line without one is a torn append whose
  index was never handed out; a reader ignores it, and a writer truncates it off
  on open so its next append starts a fresh line rather than extending the
  fragment and shifting every later index.

The file is deliberately plain text: it is the one part of a store a human
reads directly.

## `source`

A single line of UTF-8 naming what `NodeRecord.Watts` in this store actually
measures, with a trailing newline. Read back trimmed of surrounding whitespace,
and truncated at 256 bytes.

It uses **the same vocabulary as the mesh wire field `power_source`**:

| value | meaning |
|---|---|
| `dcmi` | whole-chassis draw via IPMI DCMI |
| `sdr` | whole-chassis draw via the `Power Supply` SDR sensor class |
| `sensor:<NAME>` | whole-chassis draw via a named IPMI sensor |
| `nvidia-smi` | **sum of GPU board power** — excludes CPU, fans, drives, PSU losses |
| `rocm-smi` | **sum of GPU package power reported by rocm-smi** — excludes CPU, fans, drives, PSU losses |

Adding a value to this vocabulary is not a format change: the ring files, their
headers and the one-line shape of this file are unchanged, so the magic stays
`VIIWENG1`.

This exists because the bytes are identical whichever was measured, and the two
readings differ by hundreds of watts on the same hardware. A store copied off a
host for inspection has no mesh context to ask.

**An absent or empty `source` means unknown, never a default.** Stores written
before v1.6.0 have no such file. This is the same rule the mesh contract
applies to absent fields, and for the same reason.

A producer that cannot name its source leaves the file alone rather than
writing a placeholder, so a label already earned by a history is not erased by a
build that happens not to know it.

## Client energy

Added in v2.10.0 as sibling files. Nothing above changes: the six rings,
`models.txt` and `source` are byte for byte what they were, and the magic is
still `VIIWENG1`. A reader that does not know these files ignores them. A
reader that does treats a store without them as one where no client was ever
named.

The files are created by the first minute that names a client, never by
opening the store.

### `ClientRecord` — 24 bytes

| offset | size | type | field | meaning |
|---|---|---|---|---|
| 0 | 8 | int64 | `TS` | bucket start, Unix seconds. `0` = never written |
| 8 | 2 | uint16 | `ClientIdx` | row in `clients.txt`; `0` = nobody |
| 10 | 2 | uint16 | `ModelIdx` | row in `models.txt` |
| 12 | 4 | float32 | `Wh` | energy attributed to this pair in the bucket |
| 16 | 4 | uint32 | `BusyS` | slot-seconds of this pair in the bucket |
| 20 | 4 | uint32 | `Tokens` | completion tokens of requests that ended in it |

A record carries energy, not power, so there is no `CoveredS` and a coarser
bucket is a plain sum.

The two rings have the header and the slot derivation of every other ring.
A lane holds one (client, model) pair of the bucket. **Which pair is in which
lane is not fixed**: read by `ClientIdx` and `ModelIdx`, never by lane number.

- Every write of a bucket writes all *L* lanes in one `pwrite`. Lanes not
  needed are zero, so a record of a previous lap cannot survive beside new
  ones.
- A bucket with more than *L* pairs keeps the *L* − 1 largest by `Wh`. The
  rest are added into one record with the client `other` and `ModelIdx` 0. No
  energy is lost.
- `ClientIdx` 0 is "nobody": energy a model drew in a bucket where no request
  was running. Baseline is in no client record at all.

For one model in one minute, the `Wh` of its records add up to the sum of
`AttrW × CoveredS / 3600` over that model's `GPURecord`s, unless the bucket was
folded: the folded `other` record has `ModelIdx` 0, so the per-model sum no
longer holds, while the total over all records still does.

At the default slot counts (1,440 minute slots, 8,760 hour slots, 8 lanes) the
two rings are about 2 MB together.

### `clients.txt`

The grammar of `models.txt`: newline-separated, append-only, line *N* is
`ClientIdx` *N*, every line ends in `\n`, an append is synced before its index
is used, and a last line without its newline is a torn append to be dropped.
**Line 0 is the empty string.** A name has no whitespace, is not `-`, and is at
most 64 bytes (`maxClientNameLen`; the node's own rule for the names an
operator writes is stricter, 32 characters).

### `client-hour.ring`

Writing a minute recomputes its hour from the minute ring: records are grouped
by pair and `Wh`, `BusyS` and `Tokens` are summed. The hour is rewritten, not
added to. There is no day ring: the hour ring holds a year at the default 8,760 slots.

### `client-totals`

Cumulative counters, as text, replaced by writing `client-totals.tmp` and
renaming it:

```
since 1790000040 through 1791526500
- 310.250 0 0
factory-a 18234.512 412300 9120044
other 2210.004 51020 804113
```

The first line is the start of the first minute counted and the start of the
last one. Each following line is a name, watt-hours, busy seconds and tokens
since `since`, sorted by name; `-` is nobody.

- A minute is added once, after its ring write, when its `TS` is greater than
  `through`. It is added as soon as it is in the minute ring, before the hour
  roll-up.
- On open, minute records with a `TS` greater than `through` are added first,
  so a crash between the ring write and the rename loses nothing the ring
  still holds.
- `since` never changes in a store. A store that is recreated has a new one,
  and that is how a consumer knows the counters started again.
- Counters are kept and written at three decimals of a watt-hour, so what a
  node published before a restart is what it reads back; a counter never steps
  down across one.
- A row whose watt-hours are not finite (`NaN`, `Inf`) or are negative makes the
  file unreadable, like any other parse failure. A record with such an energy is
  not added to the counters.
- A client ring that cannot be opened when the store opens (a wrong magic, say)
  is logged, and the store runs without the client dimension: node and GPU
  recording are unaffected and the file is left as it is.
- A file that cannot be parsed is renamed `client-totals.bad` and a new one is
  started, with a new `since`. If it cannot be renamed, or the new file cannot
  be saved (a full disk, for example), the store still opens, the fault is
  logged and counting carries on in memory. A file that could not be read is
  never overwritten.
- At shutdown the minute in progress is split like any other, with the busy
  time so far, and it is counted once. A restart inside the same minute does
  not add it again, so the rest of that minute is not counted: the figures are
  low, never high.

`energy.client_lanes` is geometry: changing it recreates the two client rings
and says so in the log. The totals are kept.

## Roll-up

Writing a finalised minute also recomputes the hour and the day it belongs to.
Roll-ups are **recomputed from the finer tier, never accumulated**, which is
what makes them idempotent: a restart mid-hour, or a repeated write, converges
on the same value instead of double counting.

- Coarse `Watts` / `AttrW` / `RawW` are the mean of the finer records **weighted
  by `CoveredS`**, so a barely-observed minute cannot pull an hour as hard as a
  fully observed one.
- Coarse `CoveredS` is the sum of the finer ones, clamped to 65,535.
- Coarse `ModelIdx` is the model that held the card for the most covered time
  in the period, ties broken by the lower index. A model can change mid-period;
  taking whichever record sorted first would be arbitrary.
- The hour tier rolls up from the minute tier, and the day tier from the **hour**
  tier — not from the minute tier, whose ring is only 24 hours long. A day rolls
  up the hours from its local midnight to the **next** local midnight, so a DST
  day aggregates its 23 or 25 hours rather than a fixed 24.
## Durability and concurrency

- Record writes are `pwrite` without `fsync`. An explicit sync flushes every
  ring, and the reference recorder syncs on shutdown. **A hard power loss can
  lose the last few minutes**, which is the intended trade for not fsyncing a
  2.6 MB file every minute on a host whose job is inference.
- Each record write is a single `pwrite` of one whole record at a
  naturally-sized offset, so a torn record is not a case the format has to
  handle on any sane filesystem.
- **A store directory has exactly one writer.** The reference implementation
  serialises writes per ring and is safe for concurrent use within a process,
  but nothing coordinates between processes. Two processes writing one directory
  will interleave roll-ups and produce nonsense.
- **Node wattage is a whole-host measurement.** A host has one node, and so one
  recorder. That recorder should cover every GPU the host reports, not only the
  cards the node's own models use: the marginal-power denominator has to span
  the host, or a co-tenant's load is charged to this node's models.

## Compatibility

### What is frozen

The magic, the header layout, the three record layouts (`NodeRecord`,
`GPURecord`, `ClientRecord`), the field meanings, the slot derivation, the
grammar of `models.txt` and `clients.txt`, the layout of `client-totals` and the
`source` vocabulary. Independently
upgraded machines and two implementations mean several versions of this format
are live at once, so a producer cannot assume the consumer of a directory is
itself.

Reserved bytes are reserved. They read as zero today, but an older build that
rewrites a slot during a roll-up will zero them again — so **using them for a
new field is a format change**, not an additive one.

### Changing the format

Any change to record sizes, field meanings, lane layout or slot derivation
**bumps the magic to `VIIWENG2`**. That is the entire mechanism by which such a
change becomes visible:

- A reader meeting an unrecognised magic, or a record size that disagrees with
  its own, **must refuse to open the store** and leave the file untouched.
- It must not reinitialise. With two producers, a silent reinit destroys the
  other's year of data and looks exactly like a node that was never switched
  on — the operator's first clue is a hole in the history months later.
- Refusing is recoverable: the operator moves the directory aside. A silent
  reinit is not.

Adding a sibling file — as `source` was in v1.6.0, and the four client files in
v2.10.0 — is not a format change. An
older reader ignores a file it does not know about, and a newer reader treats a
missing one as unknown.

### Geometry changes

Slot counts and lane counts are **per-deployment configuration**, not format:
a host legitimately gains a GPU, and an operator legitimately wants a longer
minute ring. A reader meeting a header whose magic and record size match but
whose slot or lane count differs **recreates the file** at the configured
geometry and says so out loud in its log. That discards history, which is
correct — the slot derivation makes records from a different ring length
meaningless — but it must be reported, because a silent reset looks exactly
like a node that had simply been quiet.

## Reading a store without the Go package

Totalling whole-node energy over the last 24 hours, which is what
`energy_kwh_24h` on the mesh carries:

```python
import struct, time

HEADER = 32
with open("node-minute.ring", "rb") as f:
    head = f.read(HEADER)
    assert head[0:8] == b"VIIWENG1", head[0:8]
    rec_size, slots, lanes = struct.unpack_from("<HII", head, 8)
    assert rec_size == 16 and lanes == 1
    body = f.read(slots * lanes * rec_size)

now = time.time()
lo, hi = now - 24 * 3600, now + 60
kwh = 0.0
for i in range(slots):
    ts, watts, covered = struct.unpack_from("<qfH", body, i * rec_size)
    if ts == 0 or not (lo <= ts < hi):      # never written, or a previous lap
        continue
    kwh += watts * covered / 3600 / 1000
print(f"{kwh:.3f} kWh")
```

Note what the loop does *not* do: it never consults a head pointer, and it
never assumes slot order is time order. Filtering every slot by its own `TS` is
the only correct way to read a ring in this format.

## See also

- `energy/doc.go` — the Go API, the `NodeWattsFunc` / `GPUReadingsFunc` seam,
  and the attribution model.
- `meshapi/doc.go` — the mesh wire contract, including `power_source` and the
  `energy_kwh_24h` / `energy_kwh_30d` fields this store feeds.
- `CLAUDE.md`, "Energy Store" — why the design is shaped this way.
