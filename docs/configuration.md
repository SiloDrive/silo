# Configuration

Silo needs no config file. It runs on compiled-in defaults, which a `silo.conf`
passed with `-C` overrides, which an environment variable overrides in turn —
so the same binary can be pointed at different deployments without editing
files.

Sections documented elsewhere: `[quota]` in [quota.md](quota.md), `[history]`
in [quota.md](quota.md#retention), `[fileserver]` and the rest of the
environment in the [README](../README.md#environment-variables).

This page is the `[storage]` section: how large a pack grows, how long it may
stay open, and what the collector considers old enough to touch. These were
compiled-in constants until they were collected here, and the numbers are
mostly the ones Silo has always used — the change is that they are now visible,
nameable, and arguable.

They are settings for whoever runs the process, not for the people using it:
nothing here is reachable from a client, and no library owner can change how
their own data is packed. The expectation is that a deployment leaves the whole
section unwritten and the defaults stand. It exists so that a number that turns
out to be wrong for a particular store can be changed without a build, and so
that the wrong number can be named when we argue about it.

```ini
[storage]
pack_size              = 512mb
pack_max_age           = 5m
pack_sweep             = 30s
orphan_age             = 24h
compact_threshold      = 0.5
compact_min_age        = 24h
compact_budget         = 0
commit_attempts        = 5
library_fault_interval = 5m
```

| Key | Environment | Default | What it does |
| --- | --- | --- | --- |
| `pack_size` | `SILO_PACK_SIZE` | `512mb` | The size at which a pack is sealed |
| `pack_max_age` | `SILO_PACK_MAX_AGE` | `5m` | How long a pack may hold its oldest frame before it is sealed regardless of size |
| `pack_sweep` | `SILO_PACK_SWEEP` | `30s` | How often the age sealer looks |
| `orphan_age` | `SILO_ORPHAN_AGE` | `24h` | How long an unreferenced object must have sat there before `gc -orphans` will consider it |
| `compact_threshold` | `SILO_COMPACT_THRESHOLD` | `0.5` | The dead fraction at which a pack is worth rewriting |
| `compact_min_age` | `SILO_COMPACT_MIN_AGE` | `24h` | How long ago a pack must have sealed before `gc -compact` will touch it |
| `compact_budget` | `SILO_COMPACT_BUDGET` | `0` | Live bytes one compaction run may copy; `0` is no cap |
| `commit_attempts` | `SILO_COMMIT_ATTEMPTS` | `5` | How many times a commit retries the head compare-and-swap |
| `library_fault_interval` | `SILO_LIBRARY_FAULT_INTERVAL` | `5m` | How long a library fault is held before it is logged again |

Sizes need their unit — `512mb`, `10gb` — because a bare number already means
gigabytes under `[quota]`, and one spelling with two meanings is a footgun with
no upside. `0` is the exception; it is the same size in every unit. Durations
are Go durations: `30s`, `5m`, `72h`.

A value that cannot be read keeps its default and logs a warning. None of these
have a spelling that means "unset", so a silent fallback would leave you
looking at a server that ignored the line you wrote.

The `gc` flags `-min-age`, `-compact-threshold` and `-compact-budget` override
these for one run. Left off, `-min-age` takes `orphan_age` for the orphan sweep
and `compact_min_age` for compaction; typed, it sets both.

## Choosing them

**`pack_max_age` is the setting that actually decides how large packs get.** A
server that is not ingesting `pack_size` bytes inside that window seals on age
every time, and the size target is never reached — so on any deployment that
isn't running near a gigabyte a minute, five minutes means packs that are
nothing like 512 MB. That matters because a lookup fans out across every sealed
pack in the library, so pack count is a running cost on every read.

Raising it produces fewer, larger packs. The cost is that recent frames stay
unsealed for longer: they are still durable, because the sidecar index is
fsynced as they are written, but they are not yet replicable, since a tier
copies sealed packs as files. The trade is replication latency against pack
count, and five minutes is a conservative point on it rather than a measured
one.

`pack_sweep` must not exceed `pack_max_age`, or the age rule quietly stops
holding — the server refuses a pair that says otherwise rather than starting
with a promise it cannot keep.

**`orphan_age` is a safety margin before it is a knob.** An upload still in
progress and an upload that died halfway leave the same trace — objects nothing
points at — and elapsed time is the only thing that tells them apart. A day is
far longer than any client takes to go from its first chunk to its head move,
including one retrying on a bad connection. Lowering it logs a warning, because
what it prevents is silent: a sweep inside the window a slow client is still
uploading in deletes chunks that client believes it has stored and will never
send again.

**`compact_threshold` at 0.5 is where a rewrite breaks even.** Compaction copies
what is live and reclaims what is dead, so at half the copy and the reclaim are
the same number of bytes; below it the run costs more I/O than it gives back.
An operator with a full disk and idle spindles lowers it. `compact_budget` caps
one run, which a nightly cron generally should not do — uncapped, it catches up
rather than falling further behind every night.

`compact_min_age` guards against rewriting a pack whose frames a client is
still in the middle of committing, the same hazard `orphan_age` covers, so the
two default to the same day.

## Applying them

`pack_max_age` and `pack_sweep` are read once, when a library's pack store is
created, so they are settled at startup and a change needs a restart. The rest
are read as they are used.
