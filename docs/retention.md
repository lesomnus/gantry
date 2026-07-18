# Image retention & garbage collection (GC)

gantry can reclaim disk on the engines it feeds by tracking each image's last-used
time and deleting images a policy no longer wants. Retention is configured **per
engine store** under `stores.<name>.retention` — there is no global policy — and
runs an adaptive, event-driven scheduler that applies a per-repo rule cascade,
honors exact and pattern pins, and (on docker) reaps images that have lost every
tag. This doc covers the data model, the policy evaluation order, the scheduler,
the full configuration reference, and the RPCs that inspect and drive GC.

## Overview & data model

An engine — docker or containerd — exposes no native "when was this image last
used" signal, so gantry maintains one. Each retention-enabled store owns an
independent **usage index**: a bbolt file at `retention.path`, holding a
per-`(engine, ref)` record. The three buckets are `img` (records), `pin` (pins),
and `unt` (untagged-image reap clocks), each namespaced by store name. Two stores
must not share a `path` (bbolt takes an exclusive lock — a second open fails with
an opaque timeout), and gantry rejects a config that does at startup.

A record carries:

- `ref` — the full local reference (the bbolt key), e.g. `cache.local/team/app:1.2`.
- `repo` — the host-qualified repository (`ggcr`'s `RepositoryStr()`), the grouping
  key for per-repo rules and for keep-N/max-N.
- `tag` / `digest` — the tag portion (empty for digest refs) and the resolved
  manifest digest when known.
- `date_last_used` — the pure usage signal, stamped from the daemon's container
  events; zero until an image is observed in use.
- `date_last_distributed` — set when gantry pushed the image to the engine (a job's
  engine pull, or `StoreService.Pull`); never bumps `date_last_used`.
- `date_first_seen` — when gantry first learned the ref exists.
- `pinned` — a derived flag: set on read when a pin protects the record.

**Effective last-used** (the age clock) falls back through this chain:
`date_last_used`, else `date_last_distributed`, else `date_first_seen`. So an image
gantry pushed but that no container has ever run still ages from its distribute
time, and an image merely observed on the daemon ages from first sighting.

Usage timestamps merge monotonically — a `Touch` only advances `date_last_used`
toward the newer time, never backward.

## Usage tracking (the watcher)

Each store runs a usage watcher (started by `StartWatchers`):

1. **Cold-start seed** — `SeedUsage` reports the images of already-running
   containers so the index reflects live usage the moment gantry starts.
2. **Watch loop** — `WatchUsage` streams "image used" events; each stamps
   `date_last_used` and pokes the scheduler (see below). When the stream ends the
   watcher marks itself disconnected, backs off 2s, **re-seeds** to catch the gap,
   and reconnects.

The watcher's health is exposed on `StoreService.GcStatus` (and metrics): whether
it is `connected`, `watching_since`, `date_last_event`, `date_last_seed`, the
`reconnects` count, and the `last_error`. A dead event stream freezes
`date_last_used` and silently degrades age GC — alert on `connected=false` or a
stale `date_last_event`.

### Heartbeat

The watcher can miss a container's start event (e.g. it was mid-reconnect). To
cover this, a **heartbeat** ticker (`retention.heartbeat`, default `5m`) periodically
lists the daemon's in-use images and stamps `date_last_used = now` for each, so an
image whose start event was missed still survives age GC after its container later
stops. Image-ID (`sha256:…`) keys are skipped — only tag and digest refs are index
keys. Set `heartbeat: "0s"` to disable it.

## Policy cascade & evaluation order

Records are grouped by `repo`. For each repo, the store's rules whose `repo`
doublestar pattern matches are **cascaded** into one effective policy:

- **Scalar fields** (`max_age`, `keep_n`, `max_n`, `max_idle`) — each takes the
  value from the **most specific** matching rule that sets it. Specificity is the
  longest literal prefix (characters before the first wildcard), then the most
  literal characters overall, then lexicographic order. An unset field inherits
  from a less specific matching rule; an explicit zero disables that dimension
  (`max_age: 0` = no age GC, `max_n: 0` = no cap).
- **`pins`** — the **union** of every matching rule's pins.

A repo that matches **no** rule is left **unmanaged** — kept, never deleted (reason
`unmanaged`).

Within a managed repo, protection/deletion is decided in this order:

1. **`in_use`** — the ref (or its resolved digest) is held by a live container.
   Always kept.
2. **`pinned`** — `Record.Pinned`, or a resolved pin pattern matches. Always kept.
3. **`idle_exceeded` (`max_idle`)** — a **hard idle cap**: an image whose effective
   last-used age exceeds `max_idle` is deleted **regardless of keep-N/max-N** — only
   `in_use` and pins (checked above) protect it — so a settled-but-ancient tag does
   not linger forever. Deferred during the grace window.
4. **`max_n_exceeded` (max-N cap)** — keep at most `max_n` digest-groups; delete
   every ref in the oldest groups beyond the cap, **even if still within
   `max_age`**. Deferred during the grace window (a just-restarted node has no usage
   history, so ordering is unreliable).
5. **`keep_n_recent` (keep-N)** — protect the `keep_n` most-recently-used
   digest-groups. Never deleted for age even if old.
6. **`within_max_age` (age GC)** — of whatever survives keep-N, delete refs whose
   effective last-used age exceeds `max_age`; keep the rest. With `max_age` unset or
   zero, everything is kept (reason `age_gc_disabled`).

Note the max-N cap (step 4) runs **before** keep-N (step 5): the oldest beyond the
cap are removed first, then keep-N protects the newest survivors. When both are set
the config requires `max_n >= keep_n`.

Ties in recency are broken by `ref` so the keep/delete boundary is deterministic.

### Counting by digest

`keep_n` and `max_n` count by **content, not by tag**. Records that share a
resolved `digest` form one group and count **once**, so "keep the 2 most recent"
keeps the 2 newest images — not 2 tags that may point at the same blob. A record
with no resolved digest is its own group. Groups (and records within a group) are
ordered most-recently-used first.

### Grace window

`retention.grace` (default `1h`) holds off **all** age-path deletions —
`idle_exceeded`, `max_n_exceeded`, and `age_exceeded` — for that long after the
process starts, since the freshly-loaded index has no usage history for the
downtime. During grace such records are kept with reason `grace`, and the store's
`grace_until` is reported on `GcStatus`. `in_use` and `pinned` still apply
normally; the grace window never resurrects a deletion those would allow.

## Pins

A pin exempts an image from GC. Pins come from two places, and are unioned:

- **Rule pins** — a rule's `pins` list, applied within matching repos.
- **API pins** — created via `PinService.Add` (persisted in the `pin` bucket),
  applied store-wide.

Each pin is either an **exact reference** or a **doublestar pattern**:

- An **exact** pin protects only its own literal `ref` — it never globs or
  short-name matches.
- A **pattern** pin is tried against three spellings of each record: the full ref,
  the `name:tag` short form (last repo segment + tag), and the bare tag. So
  `cache.local/a/app:1`, `*:stable`, and `prod-*` all match as intended.

Pins are deterministic-UUID addressable (`(store, value)`), so they survive
restarts. Creating a pin echoes its current **blast radius** — the index records it
would protect — as response metadata (`gantry-pin-matched-count` and a capped
`gantry-pin-matched` list), so a careless broad pattern like `*` that would
neutralize GC is visible at creation.

## Untagged reaper (docker only)

docker keeps an image on disk forever after its tag moves — e.g. the previous image
of a tag that was re-pulled becomes a dangling, untagged layer set. The **untagged
reaper** (`retention.untagged_after`, **default `1h`, on** for docker stores)
deletes an image this long after gantry **first observes it with no tags**. The
reap clock (persisted in the `unt` bucket) starts at first observation — the moment
the tag was actually lost is unknowable after the fact and is not needed. Set
`untagged_after: "0s"` to turn the reaper off.

Untagged images **bypass the per-repo rules** (there is no tag for a rule to
match); the reap is deferred by the same startup grace window as age GC (reason
`untagged_grace` while held). These protections still apply, in order:

1. **`digest_tracked`** — a live index record exists for one of the image's
   `repo@digest` refs (a digest-ref job or a manual digest pull deliberately
   pinned the content); the rule engine owns it. Ownership is matched on the
   canonical `(repo, digest)` pair so the daemon's familiar spelling
   (`nginx@sha256:…`) and gantry's canonical one
   (`index.docker.io/library/nginx@sha256:…`) collide.
2. **`in_use`** — a running container references the image ID or a digest ref.
3. **`pinned`** — a pin protects one of the digest refs or the bare image ID.
   **Tag-form pins cannot** protect an image that lost its tags — pin `repo@digest`
   (or the image ID) to protect content, tried in both the daemon and canonical
   spellings.

containerd needs none of this and **rejects the knob**: gantry drops the
pull-created digest record after retagging, so containerd's own GC reclaims
replaced content. The whole untagged axis requires the engine's inventory-scan
(`Reconciler`) capability, which only docker implements.

Because two docker stores pointed at the **same daemon** would each run an
independent reap clock and pin set over one shared image store — one deleting what
the other believes it protects — gantry refuses to start when two docker stores
reap the same daemon address; turn one off with `untagged_after: "0s"`. (The check
is by normalized address spelling, so symlinked sockets or host aliases evade it.)

## Inventory reconciliation & seeding

On docker (any store with the `Reconciler` capability), **every scheduled GC pass
first reconciles** the index against a full snapshot of the daemon's image store,
so images pulled, tagged, or untagged out-of-band — while gantry was down, or by a
human — converge on the configured rules:

- **Unknown tagged refs are seeded** — `Observe` creates a record with
  `date_first_seen = now` (the age clock starts at observation) without touching an
  existing record's usage signals. A human-pulled tag thus becomes managed by the
  matching rules; a repo matching no rule stays untouched. A `max_n` cap can apply
  to seeded refs on the next pass once the startup grace ends.
- **Newly untagged images start a reap clock** — `ObserveUntagged` is write-once,
  so a re-observation keeps the original clock.
- **Stale untagged entries are dropped** — an image that regained a tag or vanished
  since the last scan is removed from the `unt` bucket.

If the store has no scan capability (containerd) or the scan fails, GC proceeds on
the index records alone, exactly as before. A manual `GcPlan` / `GcApply` (HTTP
dry-run) fetches the inventory **read-only** and does not write reap clocks; the
scheduled pass scans once and reuses it for both reconciliation and planning.

## Adaptive scheduler

Each store runs its own GC loop (started by `StartScheduler`). It is **event-driven
and self-pacing**, not a fixed cron:

1. Reconcile (docker), plan, apply.
2. Sleep until the soonest currently-kept record could become deletable
   (`next_age_out` from the decision) — capped at `interval`, floored at
   `min_interval` — waking early when a usage or distribute event pokes it.

So the loop idles up to `interval` when nothing is aging, and wakes precisely when
a record is about to age/idle/cap out. A usage or distribute event pokes the loop;
after a poke it still waits out `min_interval` from the last run, debouncing event
bursts.

- **`interval`** (default `1h`) — the safety/idle cap: the longest it ever waits.
- **`min_interval`** (default `1m`) — the debounce floor between runs.
- **`grace`** (default `1h`) — the post-startup deletion hold-off (above).

A store with a non-positive `interval` runs manual GC only (via the RPCs); with the
default applied, the scheduler is on whenever retention is configured.

## Applying a GC pass

`Apply` executes a decision and syncs the index. Its result reports:

- `deleted` — content-hash IDs whose bytes were actually freed.
- `untagged` — refs whose tag was removed but whose content may remain (shared
  layers, another tag).
- `reaped` — untagged image IDs whose content the reaper freed.
- `skipped` — untagged IDs not reapable right now (re-tagged since the scan,
  referenced by a container, an in-flight pull, or an index-owned digest ref); the
  entry stays tracked and the next pass retries.
- `errors` — per-ref removal failures, as `"<ref>: <err>"`.
- `evaluated` — records considered (delete + keep).

An untagged reap **re-checks reapability at apply time**: the engine re-inspects
the daemon immediately before removing, and a live index digest record vetoes the
reap (a digest-`as` job finishing between plan and apply names content only through
a `RepoDigest` plus an index record, invisible to the daemon's tag re-check). A
`DELETE` of the reap clock between plan and apply also cancels it.

## Per-store configuration reference

Configured under `stores.<name>.retention` (engine stores only — a `retention`
block on an `oci` store is rejected). See [`../gantry.yaml`](../gantry.yaml) for the
full annotated example.

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | string | — (**required**) | bbolt file for this store's usage/pin index. Must be unique across stores. |
| `interval` | duration | `1h` | Scheduler idle cap — the longest wait between GC checks. |
| `min_interval` | duration | `1m` | Debounce floor between GC runs. |
| `grace` | duration | `1h` | Hold off age-path deletions this long after startup. |
| `heartbeat` | duration | `5m` | In-use scan cadence stamping last-used; `"0s"` disables. |
| `untagged_after` | duration | `1h` (docker), rejected on containerd | Reap an image this long after it is first seen with no tags; `"0s"` off. |
| `rules` | list | — | Per-repo rules (below). A repo matching none is unmanaged. |

Each entry in `rules`:

| Field | Type | Meaning |
|---|---|---|
| `repo` | doublestar pattern (**required**) | Matched against the host-qualified repository, e.g. `registry.internal/prod/**` or `**`. |
| `max_age` | duration | Delete an image whose effective last-used age exceeds it. Zero/unset = no age GC. |
| `keep_n` | int | Keep the N most-recently-used digest-groups, even if old. |
| `max_n` | int | Cap digest-groups kept; oldest beyond the cap deleted even before `max_age`. Zero = no cap. Must be `>= keep_n` when both set. |
| `max_idle` | duration | Hard idle cap: delete if unused longer than this regardless of keep-N/max-N (only `in_use`/pins protect). Zero disables. |
| `pins` | list | Never GC'd within a matching repo: exact refs or patterns. |

Scalar rule fields are optional (pointers): unset inherits from a less specific
matching rule, an explicit zero disables that dimension. `keep_n`/`max_n` must be
non-negative.

Retention is disabled for a store when it has no `retention` block (or an empty
`path`).

```yaml
stores:
  k3s:
    kind: "containerd"
    address: "/run/k3s/containerd/containerd.sock"
    namespace: "k8s.io"
    retention:
      path: "/var/lib/gantry/k3s.db"
      interval: "1h"
      min_interval: "1m"
      grace: "1h"
      heartbeat: "5m"
      # untagged_after: "1h"     # docker only
      rules:
        - repo: "**"                        # catch-all defaults (least specific)
          max_age: "168h"                   # 7d (keep_n protects)
          keep_n: 2
          max_n: 10
          max_idle: "2160h"                 # 90d hard cap
        - repo: "cache.cr.com/prod/**"      # overrides only what it sets
          max_age: "2160h"
          pins: ["*:stable"]
        - repo: "cache.cr.com/prod/critical"  # most specific: exact repo
          keep_n: 20
```

## Related RPCs

Retention state is inspected and driven over the gRPC API; see
[`api.md`](api.md) for the shared CRUD conventions.

- **`StoreService`** — `GcStatus` returns one store's scheduler state (running,
  `last_run`, `next_wake`, `grace_until`, schedule, rules, and index counts) plus
  the usage-watcher health. `GcPlan` dry-runs; `GcApply` executes. Both accept an
  optional one-shot policy **override** that replaces the configured per-repo rules
  with a single blanket policy for every repo (`max_age`, `keep_n`, `max_n`,
  `untagged_after`, `pins`) — an unset field is zero (disabled) for that call, like
  every other override field. An override's `untagged_after` cannot **enable** a
  reaper the config turned off with `"0s"` (that "0s" means this store must never
  reap), and cannot request reaping on a store without the capability. The GC RPCs
  require an engine store with `retention` configured — otherwise `NotFound`
  (non-engine/unknown) or `FailedPrecondition` (retention off). `StoreService.Pull`
  and `Remove` drive one daemon and keep the index in sync (a pull stamps the
  distribute signal; a remove drops the record).
- **`ImageService`** — the retention inventory. `Get` / `List` / `Erase` read the
  index; `List` filters by `repo` / `ref` / `pinned` / `in_use` (the live daemon
  set, fetched every call) and, on an unfiltered list, rides the untagged reap
  clocks along. `Erase` purges an orphan record (or an untagged reap clock when the
  ref is an image ID) **without touching the engine**. Image ids are deterministic
  UUIDs over `(store, ref)`.
- **`PinService`** — GC exemptions. `Add` upserts a pin (exact ref or `pattern`),
  refreshing `date_pinned`, and echoes the blast-radius trailers; `Get` / `List`
  read them; `Erase` is idempotent. A malformed pattern is rejected at `Add` (it
  would silently never match).

Deletions, pins, and manual pull/remove are recorded in the audit log when
`serve.events` is enabled, and every GC apply increments the retention counters —
see [`observability.md`](observability.md).

## See also

- [`stores.md`](stores.md) — declaring engine stores that retention manages.
- [`verification.md`](verification.md) — digest pinning, which produces the
  digest-tracked records that protect untagged content.
- [`observability.md`](observability.md) — the `gantry.retention.*` metrics and the
  audit log.
- [`../gantry.yaml`](../gantry.yaml) — the full annotated configuration.
</content>
</invoke>
