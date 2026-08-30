# The viiwork energy store format (`VIIWENG1`)

This is the on-disk contract for `github.com/janit/viiwork/energy`. It is
specified here rather than left to the Go source because the format has more
than one producer: `viiwork` writes it from `ipmitool` and `rocm-smi`, and
`viiwork-nvidia` writes it from `nvidia-smi`. The Go package is the reference
implementation; this document is what a second one is written against.

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

*G* is the number of GPUs on the host. The six rings are **preallocated in
full** at creation and never grow: 2.6 MB total for *G*=10. Retention is the
wrap, not a purge job.

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

`CoveredS` **saturates at 65,535**, so a fully covered day bucket reports
65,535 rather than 86,400. Day-tier energy is therefore a slight underestimate
of a fully observed day; use the hour tier where that matters.

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
the same measured value to both fields.

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

This exists because the bytes are identical whichever was measured, and the two
readings differ by hundreds of watts on the same hardware. A store copied off a
host for inspection has no mesh context to ask.

**An absent or empty `source` means unknown, never a default.** Stores written
before v1.6.0 have no such file. This is the same rule the mesh contract
applies to absent fields, and for the same reason.

A producer that cannot name its source leaves the file alone rather than
writing a placeholder, so a label already earned by a history is not erased by a
build that happens not to know it.

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
  tier — not from the minute tier, whose ring is only 24 hours long.

## Durability and concurrency

- Record writes are `pwrite` without `fsync`. An explicit sync flushes all six
  rings, and the reference recorder syncs on shutdown. **A hard power loss can
  lose the last few minutes**, which is the intended trade for not fsyncing a
  2.6 MB file every minute on a host whose job is inference.
- Each record write is a single `pwrite` of one whole record at a
  naturally-sized offset, so a torn record is not a case the format has to
  handle on any sane filesystem.
- **A store directory has exactly one writer.** The reference implementation
  serialises writes per ring and is safe for concurrent use within a process,
  but nothing coordinates between processes. Two processes writing one directory
  will interleave roll-ups and produce nonsense.
- **Node wattage is a whole-host measurement.** A host running several node
  instances — one per model — must enable recording on exactly one of them, or
  the same chassis draw is recorded several times over. For the same reason a
  recorder should cover every GPU the host reports, not only the cards its own
  process owns: the marginal-power denominator has to span the host, or a
  co-tenant's load is charged to this instance's models.

## Compatibility

### What is frozen

The magic, the header layout, both record layouts, the field meanings, the slot
derivation, the `models.txt` grammar and the `source` vocabulary. Independently
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

Adding a sibling file — as `source` was in v1.6.0 — is not a format change. An
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
