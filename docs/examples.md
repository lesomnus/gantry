# Worked examples: remote → cache → engine

The common gantry deployment moves an image from a **remote** registry, through a
**local cache** registry, onto an **engine** (a docker/containerd node):

```
cr.example.com  ──▶  cache.rack1.internal  ──▶  node
   (remote)              (local cache)         (the daemon pulls)
```

There are two ways to express it, and they behave differently — especially when
the remote is unreachable. This doc walks both through concrete jobs and shows,
for each, what the job's `transfers` end up looking like and what state it
reaches. It assumes the model above; for the mechanics behind each behavior see
[stores.md](stores.md) (`fallback_to_origin`, `source_wait`, the `cache` route)
and [observability.md](observability.md) (the metrics named here).

Throughout, a job's `transfers` are read as:

| # | store (target) | ◀── source | state | note |
| - | -------------- | ---------- | ----- | ---- |

Rows sharing a `#` (step) are **alternatives** — the step needed any one of them,
so a `failed` row followed by a `done` row of the same step is one source that
could not serve the image followed by one that could. Rows with different `#` are
**consecutive hops**, each of which had to happen. A job's own `source`/`target`
always report **what the caller requested**, not whichever transfer served it.

The base configuration for every example:

```yaml
stores:
  cloud:                        # the remote; every byte fetched from here is billed
    kind: oci
    host: cr.example.com

  cache:                        # the local cache the nodes pull from
    kind: oci
    host: cache.rack1.internal
    insecure: true
    mode: copy

  node:                         # a docker daemon
    kind: docker
    address: /var/run/docker.sock

worker:
  # defaults shown explicitly
  source_wait: "30s"            # wait this long for an in-flight fill before giving up
  fallback_to_origin: false     # a per-job flag defaults to this
```

---

## Part A — two explicit jobs (`source` names the cache)

Here the client drives both hops itself. The node's `source` is the **cache**, so
the node never names the remote:

```
hop1  JobService.Add {ref: "cr.example.com/team/app:1", source: cloud, target: cache}
hop2  JobService.Add {ref: "cr.example.com/team/app:1", source: cache, target: node}
```

Nothing links the two jobs; the ordering and the "what if the cache is empty"
questions are what the flags below answer.

### A1 — steady state: the cache is warm

`hop2` alone, cache already holds the image.

| # | store | ◀── source | state |
| - | ----- | ---------- | ----- |
| 0 | node  | ◀── cache  | done  |

`job: done` · `source: cache` · `target: node`. The remote is not touched at all.
This is the case the cache exists for, and it is unaffected by everything below.

### A2 — cold cache, hops run in order

The client submits `hop1`, watches it to `done`, then submits `hop2`. `hop1` is a
registry copy (gantry pulls each blob from the remote and pushes it into the
cache); `hop2` is then A1.

```
hop1   | 0 | cache | ◀── cloud | done |      → job done
hop2   | 0 | node  | ◀── cache | done |      → job done
```

The remote is read once (by `hop1`), the node pulls from the LAN cache.

### A3 — cold cache, both hops submitted together

If the client submits `hop2` **alongside** `hop1` instead of after it, `hop2` can
reach the still-empty cache first. `worker.source_wait` (default `30s`) handles
it: `hop2` misses, discovers that `hop1` is filling exactly this reference right
now, waits for it, and re-attempts the same source.

| # | store | ◀── source | state | note |
| - | ----- | ---------- | ----- | ---- |
| 0 | node  | ◀── cache  | done  | missed once, waited for the fill, then served |

`job: done`. The wait happens **only after a real miss**, so a warm cache (A1) is
never delayed by it. This is also what collapses a rollout burst — many nodes
submitted together for a cold image — onto a single remote read: every `hop2`
waits on the one `hop1` rather than each triggering its own remote fetch.

`gantry.job.source_wait{outcome="served"}` records the wait paid off. On a single
worker (`max_concurrent_jobs: 1`) waiting cannot help — the pipeline is serial —
so the default is forced to `0` there.

### A4 — the cache cannot serve, fall back to the remote

`hop2` with the fallback enabled:

```
JobService.Add {ref: "cr.example.com/team/app:1", source: cache, target: node,
                fallback_to_origin: true}
```

The cache is cold and nothing is filling it (or the fill failed). Rather than fail
the job, the node re-pulls from the registry named in `ref` — the remote:

| # | store | ◀── source | state  | note |
| - | ----- | ---------- | ------ | ---- |
| 0 | node  | ◀── cache  | failed | 404 / manifest unknown |
| 0 | node  | ◀── cloud  | done   | the origin named in `ref` |

`job: done` · `source: cache` · `target: node`. The failed attempt stays on the
record; `gantry.job.fallback{from=cache, to=cloud, reason=planned}` and a durable
`job_fallback` audit event fire, so "the cache is quietly not being used" is
visible rather than looking like an ordinary success. **The cache is an
optimization, not a dependency.**

The fallback is byte-safe for a pinned job: a digest ref, or a verified source,
makes the node pull the *same digest* from the remote — the remote cannot serve
different content. An unpinned tag resolves at the remote, which is the tag's own
authority.

### A5 — the remote is down, but the cache is warm

Because the node's `source` is the **cache**, a remote outage does not reach it:

| # | store | ◀── source | state |
| - | ----- | ---------- | ----- |
| 0 | node  | ◀── cache  | done  |

`job: done`. This is the resilience the two-job model buys — the node depends on
the cache, and the cache keeps working while the remote does not. (Contrast B6.)

### A6 — cache cold *and* remote unreachable

`hop2` with `fallback_to_origin: true`, cache empty, remote down:

| # | store | ◀── source | state  | note |
| - | ----- | ---------- | ------ | ---- |
| 0 | node  | ◀── cache  | failed | manifest unknown |
| 0 | node  | ◀── cloud  | failed | dial tcp: timeout |

`job: failed`. The error joins both attempts, so nothing is lost about why.

---

## Part B — one routed job (the remote declares a `cache`)

Add one line to the remote store and the client submits a **single** job naming
the remote as its source; gantry inserts the cache itself:

```yaml
stores:
  cloud:
    kind: oci
    host: cr.example.com
    cache: cache            # ← reading from me may go through this store
```

```
JobService.Add {ref: "cr.example.com/team/app:1", source: cloud, target: node}
```

The caller never names the cache and always gets the remote's content, byte for
byte. The route is gantry's own cost optimization — it changes how many times the
remote is read, not what the job delivers. Every routing decision is counted at
admission in `gantry.job.route`.

### B1 — cold cache: fill it, then read it

The remote is read once to settle the tag (one manifest request), the cache is
probed by digest and found empty, so gantry fills it and then delivers from it:

| # | store | ◀── source | state | note |
| - | ----- | ---------- | ----- | ---- |
| 0 | cache | ◀── cloud  | done  | the fill hop (gantry copies the blobs) |
| 1 | node  | ◀── cache  | done  | the delivery hop |

`job: done` · `source: cloud` · `target: node`. `gantry.job.route{decision="filled"}`.
The remote's image content crosses the billed link **once**, into the cache; the
node pulls it over the LAN.

### B2 — warm cache: one hop, the remote serves one manifest

A later job for the same image finds the cache warm. The remote is asked what the
tag means (one `GET manifests`) and nothing more:

| # | store | ◀── source | state |
| - | ----- | ---------- | ----- |
| 0 | node  | ◀── cache  | done  |

`job: done` · `source: cloud` · `target: node`. `gantry.job.route{decision="warm"}`.
This is the case the feature exists for: the remote served one manifest request,
the image bytes came from the cache.

### B3 — a second node, same image

The site is warm from B1, so this is B2 again — a single hop off the cache. No
coordination between the two jobs was needed; the second simply found the content
there. (If the two land while the cache is still being filled, `source_wait`
collapses them onto the one fill, exactly as in A3.)

### B4 — the cache cannot be written (read-only credentials)

gantry tries to fill the cache, the push is refused, and the route is **abandoned
rather than failed** — the caller asked for the remote's content and gets it, from
the remote directly:

| # | store | ◀── source | state  | note |
| - | ----- | ---------- | ------ | ---- |
| 0 | cache | ◀── cloud  | failed | 403 push access denied |
| 1 | node  | ◀── cloud  | done   | the route pruned; the caller's source served |

`job: done` · `source: cloud` · `target: node`. The fill hop is one gantry added
for itself, so its failure is tolerated; the delivery hop's read-the-cache attempt
needs the fill to have delivered, so it is skipped and the direct read of the
remote runs instead. The failed hop and `gantry.job.route` say the route is not
working — a configuration problem the operator can see, not a job the caller has
to handle. (A target that refuses the write is never re-read from another source:
that failure belongs to the target, not to whoever was reading.)

### B5 — scoping the route per rack

One cache per source cannot express a per-rack topology. The long form is an
ordered list, first match wins:

```yaml
stores:
  cloud:
    kind: oci
    host: cr.example.com
    caches:
      - store: rack1
        for_targets: [node1a, node1b]   # nodes in rack 1 read the rack-1 cache
      - store: rack2
        for_targets: [node2a, node2b]
      - store: site
        for_repos: ["team/**"]          # everything else, but only our own repos
```

`Add {ref: "cr.example.com/team/app:1", source: cloud, target: node1a}` routes
through `rack1`; the same image to `node2a` routes through `rack2`; `team/web` to a
node in neither rack routes through `site`; a third-party repo to such a node is
not routed at all and reads the remote directly (not a failure, and not counted as
a decline — it was never a candidate). Routing is **one level deep**: a cache that
declares its own `cache` is a separate route for jobs whose *source* is that
cache, never a continuation of this one.

### B6 — the remote is unreachable (routed engine target)

This is where the routed model differs from the two-job model. gantry cannot
settle the tag at the remote, so it has no digest to probe the cache with or to
check that the cache holds the image's signatures — and an engine delivery is
verified after the fact against the store it was pulled from. Unable to establish
that safely, gantry **declines to route** and the job reads its named source (the
remote) directly, which is down:

| # | store | ◀── source | state  | note |
| - | ----- | ---------- | ------ | ---- |
| 0 | node  | ◀── cloud  | failed | dial tcp: timeout |

`job: failed`. `gantry.job.route{decision="declined", reason="referrers_unverifiable"}`.

So a routed **engine** job does not survive a remote outage the way the two-job
model (A5) does — there the node's source *is* the cache. If surviving a remote
outage matters more than the one-job ergonomics, point the node at the cache
explicitly (Part A). If both matter, keep the routed job and warm the cache ahead
of time so the outage window is covered by B2, not B6.

---

## Which model, and what determines the outcome

| | **Part A** — two jobs | **Part B** — routed |
| --- | --- | --- |
| Jobs the client submits | two (`→cache`, then `cache→node`) | one (`cloud→node`) |
| Node's `source` | the cache | the remote (gantry inserts the cache) |
| Config to enable | none | `cache:` / `caches:` on the remote |
| Ordering of the two hops | the client's problem (or `source_wait`) | guaranteed by construction |
| Survives a remote outage (warm cache) | **yes** (A5) | no for an engine target (B6) |
| Per-rack / per-repo routing | pick the source per job | `caches:` scopes (B5) |
| Remote read on a warm image | zero (A1) | one manifest request (B2) |

What decides where a submitted job's bytes come from, in order:

1. **Is the source routed?** Only if the source store declares a `cache`/`caches`
   route that covers this job (target + repo). Otherwise the job reads its named
   source directly. Counted in `gantry.job.route`.
2. **Can the route be established?** The remote must answer the tag; for an engine
   target the cache's signatures must be checkable. If not, the route is declined
   (B6) or, when the cache is cold, filled first (B1) — a fill that fails is
   abandoned, not fatal (B4).
3. **Did the chosen source serve it?** If not and `fallback_to_origin` is set (or
   defaulted on), an engine pull re-attempts the registry named in `ref` (A4).
   A digest-pinned job gets byte-identical content from the fallback.
4. **Was the source merely not filled yet?** If `source_wait > 0` (default `30s`)
   and another job is filling exactly this reference, the attempt waits for it and
   retries before giving up (A3).

### Defaults, and when to change them

| Setting | Default | Change it when |
| ------- | ------- | -------------- |
| `worker.source_wait` | `30s` (`0` on a single worker) | set `0s` to accept one remote read per node on a cold image; raise it if fills routinely take longer |
| `worker.fallback_to_origin` | `false` | turn on (or set per job) to make the cache an optimization, not a dependency (A4) |
| `worker.require_authority` | `false` | turn on to refuse a routed job whose remote could not confirm the reference, rather than serve the cache on faith — only ever consulted for a routed **registry**-target job |
| `cache:` / `caches:` on a store | unset (no routing) | declare the local cache so a single `source: cloud` job routes itself (Part B) |

Every setting above defaults to the pre-cache behavior, so an existing deployment
behaves exactly as before until it opts in.
