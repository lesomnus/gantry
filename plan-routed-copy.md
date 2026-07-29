# Plan — routing a copy through a store's cache (A → A' → B)

Status: **Phase 1 landed** · Phase 2 next · follows `plan-source-fallback.md` (already landed on `feat/source-fallback`).

## 1. Goal

gantry keeps `A → B` copy jobs running in the background. Nothing about that changes.
What is added is that gantry may, **for its own cost efficiency**, satisfy an `A → B`
request by going `A → A' → B`, where `A'` is a nearer copy of `A`'s content — a corporate
registry standing in front of a cloud one.

The caller does not know about `A'` and does not need to: it asked for "B ends up holding
this image", and that is what it gets, byte for byte. The routing exists to avoid paying
cloud egress twice, not to change what the job means.

This is **not** a dependency between the caller's jobs, and **not** a store-graph model.
It is one job with two steps.

## 2. Design

### 2.1 One job, two steps

`Job.Transfers` is already `repeated` and already documented as *"the job's steps, in
execution order"*. A routed copy is exactly that:

```
JobService.Add {ref: "cr.example.com/app:1", source: "cr.example.com", target: local}

  transfers[0]   site  ◀── cr.example.com     gantry's chosen route
  transfers[1]   local ◀── site
```

One job id, one `Watch` stream carrying both steps' per-layer progress, ordering
guaranteed by construction. Nothing waits, nothing is scheduled, no job depends on
another job.

An earlier sketch made this two jobs. Everything expensive in that sketch — a
`depends_on` field, a dispatcher, a job that occupies a worker while waiting, a decision
about what `Add` returns — was a consequence of that one choice, and all of it disappears
here.

### 2.2 Where the route is declared

On the **source** store, naming the store that caches it:

```yaml
stores:
  "cr.example.com":
    kind: oci
    cache: "site"        # when reading from me, gantry may go through this store
```

That is the right place: the field describes a property of `cr.example.com` (something
else holds copies of its content), it is declared once per origin rather than repeated per
target, and it sits where the cost being avoided actually lives — the origin's egress.

`cache` names a **declared registry store**. Empty (the default) means no routing, which
is every existing configuration.

### 2.3 The routing decision, at admission

For a job whose source store declares a `cache`:

```
1. resolve the tag at the SOURCE (the authority)          -> digest
2. HEAD  <cache>/repo@digest                              -> present?
      present  -> single step:  B ◀── A'
      absent   -> two steps:    A' ◀── A   then   B ◀── A'
```

The authority round trip is one manifest request against bytes that would otherwise be
transferred in full, so it is not a cost worth optimising away — and it buys the thing
that makes the whole feature safe: **the digest**. Probing `A'` by digest rather than by
tag removes any question of which image `A'` holds.

This is the one place a probe is justified. Elsewhere in gantry the first attempt *is* the
probe, because the attempt answers the same question. Here the probe answers a different
one — *how many steps does this job have* — which no attempt can answer.

### 2.4 Content identity

Because the digest is pinned at the authority and every step is anchored to it, `A'`
cannot substitute different content: step 1 commits that digest into `A'` (verbatim, as a
digest-preserving copy already does), and step 2 copies that digest out of it. The
reference landing in `B` is unchanged either way — `dstRef` substitutes only the host and
preserves repo and tag — so the routing is invisible in the result.

### 2.5 When the authority cannot be reached

Then there is no digest and no probe. gantry falls back to resolving the tag at `A'` and
serving from it, which is the useful behaviour: the corporate registry keeps working while
the cloud one does not. It is also the only case where the caller can receive content the
authority never confirmed, so it is opt-out:

```
require_authority: bool     # per job; server default worker.require_authority
```

`true` fails such a job instead. It is a no-op for a job that is already digest-pinned (a
digest ref, or a verified source) and for any job that is not routed, since there the
requested source *is* the authority.

## 3. Decisions

### 3.1 Routing applies once, to the caller's job

The steps gantry generates are never themselves routed, even if their source declares a
`cache`. One level, no recursion, no cycle detection needed because no graph is walked.

### 3.2 Degenerate cases are not routed

- The job's **target is the cache store** (`site ◀── cr.example.com` submitted directly) —
  that already *is* the copy the caller asked for.
- The job's **source is the cache store** — nothing to route through.
- The cache store is **`mode: proxy`** — a pull-through cache fills itself when read, so
  step 2 alone warms it. Routing collapses to a single step with `A'` as the source; no
  fill step, no probe, and no write access to `A'` required. This is the read-only mirror
  case, and it falls out of the existing `mode` rather than needing a switch of its own.

### 3.3 A route that does not work is not a failure

If the fill step cannot run or fails — no write credentials for `A'`, `A'` unreachable,
a rejected push — gantry performs the copy the caller actually asked for: `B ◀── A`. The
failed step stays on the record as a failed transfer of a job that completed, exactly as a
source fallback does today.

That is why there is no `fill_on_miss`-style switch: "may gantry write to `A'`" is answered
by whether it works, and the answer costs one failed attempt.

### 3.4 Referrers travel with each step

`copy_referrers` is a property of the job, applied per step against that step's own source:
step 1 copies the authority's referrer artifacts into `A'`, step 2 copies them out of `A'`
into `B`. A signature that only ever existed at the authority therefore still reaches `B`,
and `A'` is left able to serve the next request completely.

### 3.5 `platforms`, `verbatim`, verification

Unchanged in meaning, applied to every step: the same platform set is copied at each hop,
a digest-pinned job commits verbatim at each hop, and verification runs once at admission
against the **authority** — which is where signatures live, and which is already what
`plan()` does.

### 3.6 `job.source` / `job.target` become the request, not the transfers

This is a semantic change and it is forced by steps existing.

`jobToPB` currently derives the job's source and target from the transfer that served it —
a rule written for *attempts*, which are alternatives, exactly one of which serves the
job. **Steps are not alternatives.** In a routed job two different stores each served part
of the work, and the first step's target (`site`) is not the job's target at all.

So: `job.source` and `job.target` report **what the caller requested**, and `transfers`
report what actually happened. Never ambiguous, and it needs no rule about which row wins.

The cost: a source-fallback job will report `source: cache` (what was asked for) rather
than `source: origin` (what served it). The information is not lost — it is in
`transfers[].source`, in `gantry.job.fallback`, and in the `job_fallback` audit event,
which exist precisely to make that visible. One e2e assertion changes with it.

## 4. Example configuration

```yaml
stores:
  # The cloud registry. Everything is ultimately pulled from here, and every byte
  # fetched from it is billed — so declare where its content is cached.
  "cr.example.com":
    kind: oci
    cache: "site"

  # The corporate registry: one copy of upstream content for the whole site.
  site:
    kind: oci
    host: registry.corp.internal
    username: gantry
    password: "…"

  # A rack-local cache the nodes actually pull from.
  local:
    kind: oci
    host: cache.rack1.corp.internal
    insecure: true

  # A node's docker daemon.
  node1:
    kind: docker
    address: /var/run/docker.sock

worker:
  # Optional: refuse a job whose digest the authority could not confirm, instead
  # of serving whatever the cache holds. Default false.
  require_authority: false
```

Nothing else in the file changes, and no job request changes.

## 5. Scenarios

### 5.1 Cold site — the fill step runs

```
Add {ref: "cr.example.com/app:1", source: "cr.example.com", target: local}

admission   GET  cr.example.com/v2/app/manifests/1        -> sha256:ab…
            HEAD registry.corp.internal/v2/app/manifests/sha256:ab…  -> 404

transfers[0]  site  ◀── cr.example.com   done   142 MB    ← cloud egress, once
transfers[1]  local ◀── site             done   142 MB    ← corporate LAN
job           done       source: cr.example.com  target: local
```

### 5.2 Warm site — one step, no cloud transfer

```
admission   GET  cr.example.com/…/manifests/1             -> sha256:ab…
            HEAD registry.corp.internal/…/sha256:ab…      -> 200

transfers[0]  local ◀── site             done   142 MB
job           done       source: cr.example.com  target: local
```

The cloud registry served one manifest request and nothing else. This is the case the
feature exists for.

### 5.3 A second rack, same image

```
Add {ref: "cr.example.com/app:1", source: "cr.example.com", target: local2}

transfers[0]  local2 ◀── site            done   142 MB
```

The site is already warm from 5.1, so this is 5.2. No coordination between the two jobs
was needed — the second one simply found the content there.

### 5.4 The site cannot be written to

```
transfers[0]  site  ◀── cr.example.com   failed  "401 UNAUTHORIZED: push access denied"
transfers[1]  local ◀── cr.example.com   done    142 MB
job           done       source: cr.example.com  target: local
```

The caller got what it asked for. The failed step and `gantry.job.fallback` say the route
is not working, which is a configuration problem the operator can see rather than a job
failure the caller has to handle.

### 5.5 The cloud registry is down

```
admission   GET  cr.example.com/…/manifests/1             -> dial tcp: timeout
            (no digest; the site is asked what the tag means)

require_authority: false   transfers[0]  local ◀── site   done      job: done
require_authority: true    admission rejected: FAILED_PRECONDITION
                                       "the authority could not confirm cr.example.com/app:1"
```

### 5.6 Engine target

```
Add {ref: "cr.example.com/app:1", source: "cr.example.com", target: node1}

transfers[0]  site  ◀── cr.example.com   done              ← gantry copies
transfers[1]  node1 ◀── site             done              ← the daemon pulls
```

The final step is an engine pull, so everything already built for it applies unchanged:
`as` names, digest anchoring, and the source fallback (`node1 ◀── cr.example.com` if the
site cannot serve the pull).

### 5.7 A proxy cache

```yaml
site: { kind: oci, host: registry.corp.internal, mode: proxy }
```

```
transfers[0]  local ◀── site             done   142 MB
```

One step. Reading through the proxy is what fills it, so there is no fill step and gantry
never needs write access. No probe either.

## 6. Does this break existing users?

**No.** The assessment, surface by surface:

| Surface | Change | Breaking? |
| ------- | ------ | --------- |
| `StoreConfig.cache` | new field, empty default | No — an existing config parses and behaves identically |
| `worker.require_authority` | new field, `false` default | No |
| `Job.require_authority` | new proto field 22, EXPLICIT presence, absent = server default | No — additive; unset behaves as today |
| Job requests | unchanged | No |
| Number of jobs created | unchanged (one submit, one job) | No |
| `Job.transfers` | a routed job has 2 rows | **Behaviour change, not API change** — the field is `repeated` and documented as steps; a source-fallback job can already have 2 rows on the branch that just landed |
| `job.source` / `job.target` | now the request rather than the serving transfer | **Semantic change, but only against unreleased behaviour** — see below |
| `gantry.bytes` / `job_done.bytes` | a routed job moves the image twice, so it reports ~2× the image size | Honest ("bytes moved between stores"), but worth a doc note |
| Admission latency | one manifest GET + one HEAD, for routed jobs only | No |
| Everything else | untouched | No |

Three things worth stating plainly:

1. **Nothing routes until an operator adds `cache` to a store.** The feature is inert in
   every existing deployment, including the config in `gantry.yaml`.

2. **The `job.source`/`job.target` change only affects the fallback behaviour added in
   `plan-source-fallback.md`**, which is on an unmerged branch and has never shipped. For
   every job that existed before that branch — a single-step job — "the transfer that
   served it" and "what was requested" are the same store, so the reported values are
   unchanged. There is no released behaviour to break.

3. **A client that assumed `len(transfers) == 1`** would be surprised, but that assumption
   was already invalidated by the source fallback, and the field has been `repeated` with
   an "in execution order" contract since it was introduced. Clients read
   `transfers[].state` and per-layer progress; the aggregate job state is unchanged.

The one genuine risk is operational rather than API: an operator who sets `cache` to a
store gantry cannot write to gets 5.4 on every cold image — a wasted push attempt per
job, silently succeeding via the direct path. `gantry.job.fallback` surfaces it, and
`JobService.Plan` should report the resolved route so it is visible before submitting.

## 7. Change list

| File | Change |
| ---- | ------ |
| `cmd/config/serve.go` | `StoreConfig.Cache`, `WorkerConfig.RequireAuthority`; validate that `cache` names a declared registry, is not the store itself, and is absent on `mode: proxy` stores |
| `proto/gantry/job.proto` | `Job.require_authority = 22` |
| `proto.svc/gantry/job_svc.proto` | `JobPlanRequest.require_authority`, `JobPlanResponse.route` (the resolved step list) |
| `internal/cpx/cpx.go` | `jobExec` gains a step list; `plan()` resolves the route (authority pin + cache probe); `execute()` iterates steps; `runCopy` takes its source/refs/flags per step instead of reading `jobExec` |
| `internal/cpx/source.go` | a helper to resolve the authority digest and probe a store for it |
| `internal/rpc/convert.go` | `job.source`/`job.target` from the request; plan route in the response |
| `docs/stores.md` | the `cache` field, the route, the degenerate cases, the proxy collapse |
| `docs/api.md` | `require_authority`, the `Plan` route field, `transfers` as steps |
| `docs/observability.md` | note that a routed job reports bytes for both hops |
| `gantry.yaml` | commented example |

## 7a. Prerequisite found while reading: the layer abort was job-scoped

`copyLayers` called `job.Cancel()` on the first layer error, to abort the sibling layer
copies still in flight. That cancels the **job's** context, which is fine when a job is one
hop with one attempt and about to fail anyway — and wrong the moment a job has a later step
or a second attempt, because they would start on a dead context. Worse, `sourceFallbackWorthy`
reads a dead context as *"the job is being cancelled"* and suppresses the retry, so the very
mechanism meant to recover from the failure would be disabled by it.

Latent today (registry targets have no attempts, and engine pulls do not use `copyLayers`),
load-bearing for every shape in this plan. Fixed ahead of the refactor:

- the abort is a child context scoped to the layer fan-out, and the layer goroutines receive
  that context, so siblings still abort and nothing beyond them does;
- the one thing `job.Cancel()` was doing usefully — keeping new callers from coalescing onto a
  move that is already failing — is now `Job.sealed`, which says that without touching any
  context. `finish()`'s workaround comment about layer failures being recorded as failed
  becomes unnecessary.

The direct `copyLayers` test now pins all of it, and a mutation putting `job.Cancel()` back is
caught.

## 8. Phases

- **Phase 1 — steps.** `execute()` over a step list and `runCopy` parameterised per step,
  with the step list always length 1. Pure refactor, no behaviour change, fully covered by
  the existing suite.
- **Phase 2 — the route.** `StoreConfig.cache`, the authority pin, the probe, the two-step
  plan, the degenerate rules, `job.source`/`job.target`, docs, tests.
- **Phase 3 — `require_authority`.** The proto field, the config default, the offline path.

Phase 1 landing on its own is worth it: it is where the risk is (it touches the copy
pipeline), and it is verifiable without any new semantics.

## 9. Tests

| Level | Case |
| ----- | ---- |
| unit | route resolved when the cache misses → 2 steps, in order, second reads the first's output |
| unit | cache hit → 1 step, source is the cache, the authority serves only the manifest |
| unit | fill step fails → direct copy runs, job done, failed step on the record |
| unit | `mode: proxy` cache → 1 step, no probe |
| unit | target == cache, source == cache → not routed |
| unit | a generated step is not itself routed |
| unit | digest-pinned end to end: both steps anchored to the authority's digest |
| unit | `copy_referrers` → referrers present in both the cache and the target |
| unit | authority unreachable → served from the cache; with `require_authority` → rejected |
| unit | engine target → step 2 is a pull, `as` names and the source fallback still apply |
| unit | `job.source`/`job.target` report the request for every shape above |
| unit | config: `cache` naming an engine store / itself / a proxy store → rejected at load |
| e2e | L1: cold site, then a second target hitting the warm site |

## Progress

Legend: ☐ todo · ◐ in progress · ☑ done

| # | Item | State | Notes |
| - | ---- | ----- | ----- |
| 0 | This plan | ☑ | |
| 0.1 | Scope the layer abort to the fan-out (§7a) | ☑ | `Job.sealed` replaces the coalescing side effect; mutation-checked |
| 0.2 | Execution-model redesign: recon + two independent designs + judge | ◐ | recon in; two designs and the judge still running |
| 0.3 | `Job.Source`/`Target` carried on the record, not derived from a transfer (§3.6) | ☑ | + `docs/api.md` section and an rpc test; one e2e assertion updated |
| 0.4 | `StoreConfig.Cache` + cross-store validation | ☑ | declared, a registry, not itself, and not on an engine store; a store that is someone's cache may declare its own (routing is one level) |
| 0.5 | Recon bugs in the prerequisite: seal on every exit, `Filling` skips sealed | ☑ | both pinned by tests |
| 0.6 | Execution-model verdict + 11 open decisions settled | ☑ | `execPlan` (steps × attempts) wins; 9 ideas grafted from the runner-up |
| — | **Phase 1 — steps** | ☑ | behaviour-free: `-685/+310` lines, whole suite green unchanged apart from two documented semantics |
| 1.0 | Free `copyLayers` from `jobExec` | ☑ | takes the two repositories; the last `jobExec` literal outside `plan()` is gone |
| 1.1 | `Transfer.Step` + `LAYER_STATE_COPIED` (one proto regen) | ☑ | the enum gap was live: every registry copy reported `LAYER_STATE_UNSPECIFIED` per layer |
| 1.2 | `execPlan`/`execStep`/`execAttempt` + `mover` | ☑ | `plan.go` / `move.go` / `run.go`; `jobExec`, `sourceBinding`, `runCopy`, `runPull`, `pullFrom`, `bindSources`, `sourceFallbackWorthy` all deleted |
| 1.3 | `plan.validate()` + tests | ☑ | delivery-only-last, attempt bound, indices, runner present |
| — | **Phase 2 — the route** | ☐ | |
| — | **Phase 3 — `require_authority`** | ☐ | |

### Findings from the recon pass (folded into the design, not yet implemented)

Four readers mapped what the single-hop model bakes in. The load-bearing ones:

- **`Transfer` needs step identity.** With both attempts (alternatives) and steps (a
  sequence) in one flat list, five concrete row-sets are ambiguous — including
  `[site←origin failed, local←origin done]` from §5.4, where row 1 is *not* a retry of row 0
  but the abandoned route falling through, and the mixed `[site←origin done, node←site
  failed, node←origin done]` from §5.6. One added field settles it: `Transfer.step`, with
  rows sharing a step being alternatives. A nested message and a job-level step list were
  both considered and cost more (the ORM derives `JobAddRequest`/`JobSelect`/`JobPatchRequest`
  from `Job`, so a shape change there propagates).
- **The warm probe can report a miss on content the cache holds.** `dstRef` preserves the
  tag, but a non-verbatim commit *rebuilds* a platform-filtered index, so its digest differs
  from the authority's. Probing `A'/repo@digest` would then 404 on every job. The fill step
  must therefore be **verbatim regardless** of `copy_referrers` or whether the cache ref is a
  digest — which also means a routed fill copies every platform.
- **A digest-pinned routed job leaves `A'` holding the manifest with no tag**, which matters
  to retention on the intermediate.
- **`Job.Fills` must become per-step**, and `memStore.Filling` has no way to exclude the
  asking job — harmless today (an engine job fills nothing) and a self-deadlock once a job
  has a fill step whose ref it then reads.
- **`sourceFallbackWorthy` cannot be reused verbatim on the copy path**: "the cache refused
  the push" is a reason to abandon the *route*, not to re-attempt the same step elsewhere.
- Smaller: `PlannedLayer.Repo` is written and never read; duplicate blob digests within one
  `Plan` are filled concurrently with no dedup; `copyReferrers` per step doubles a
  fail-closed surface; nothing cleans up a partially-filled destination, which stops being
  incidental once a route can be abandoned midway.

Two of the recon findings were bugs in the prerequisite that had just landed, both fixed
before anything else was built:

- `Job.sealed` was written after the layer fan-out, but the dispatch loop returns as soon as
  the abort reaches it — so the flag was set only when every layer goroutine finished
  normally, i.e. never in the case it exists for. It is now written where the failure is
  recorded, and a test drives the early-return path specifically.
- `memStore.Filling` did not exclude sealed jobs, so a waiter could park on a move that had
  already failed and burn its whole `source_wait`.

### The execution model: `execPlan` = steps × attempts

Two independent designs were written against the recon facts and judged. The winner models a
job as **two axes**, so every shape gantry runs is a point in them rather than a branch:

```
execPlan
 ├─ source, target, repo, authorityRef, platforms …   the request, resolved
 └─ steps []*execStep            a SEQUENCE — every required one must succeed, in order
     ├─ dst, ref, platforms, verbatim, referrers, fills, optional
     └─ attempts []*execAttempt  ALTERNATIVES — the first that succeeds ends the step
         └─ src, ref, pullRef, why, needs, waitFill
```

- A route prunes itself with `needs` — a static predicate over which earlier steps
  *delivered*, evaluated at run time. The plan is never rewritten, which is what lets `Plan`
  report the whole route before a byte moves.
- `optional` marks a step gantry added for itself (a cache fill). Its failure is recorded and
  tolerated; §3.3 falls out with no branch.
- `why` (`planned` / `route` / `origin`) is authored on the attempt, so "left the caller's
  source for the origin" and "abandoned a route gantry chose for itself" are different facts
  in the metric and the audit event.
- Today's single-hop job is one step with one attempt; the landed source fallback is one step
  with two. Neither changes.

The rejected alternative modelled a job as *alternative routes*, each a full sequence of
hops. It loses on cost — a 3-hop route with a fallback per hop is 2³ duplicated hop lists —
and on accounting: it resets the byte count at a route boundary, so a job that filled the
cache and then fell back would report one image's worth of bytes when it moved two,
including the cloud egress the whole feature exists to avoid.

Grafted from it: a `mover` interface per (step, attempt) so `runStep` needs no kind switch
and `pullHook` is structurally unreachable from a registry step; rows published when a step
starts rather than all at admission (which removes the winner's one new liability, an
insert-in-the-middle that could silently mis-order); breaking the `errors.Is` chain on a
self-inflicted abort so it can never reach `finish()` as a cancellation; and a checked bound
on total attempts per job, because each attempt can move a whole image.

### Decisions the two designs left open

1. **§3.5 was internally contradictory and is corrected.** "The same platform set at each
   hop" cannot hold: a verbatim commit writes every child manifest, and a non-verbatim one
   rebuilds the index so `A'` never holds the authority digest — which breaks both §2.4's
   anchoring and §2.3's probe. So a **fill step is always `verbatim` and always copies every
   platform**, whatever the job asked for. A narrowed routed copy therefore still fills the
   cache completely; that is a cost, and for a shared site cache it is the desirable one. The
   delivery hop keeps today's rule (verbatim iff its own target ref is a digest, or
   `copy_referrers` forced it).
2. **The fill step's target ref is the TAG form**, committed verbatim so the authority digest
   resolves from it too. Deriving it from the pinned ref instead would leave `A'` holding an
   untagged manifest — invisible to retention there and a probe miss for the next job.
3. **`Transfer.step` is the plan index**, and rows are published when a step starts. The
   documented contract is "in execution order, non-decreasing" — not `0..n-1`, because a
   planned step that never runs leaves no row. (No shape in this plan does that: a warm cache
   yields a one-step plan rather than a skipped step.)
4. **A proxy-mode cache collapses, it is not rejected.** Reading through a pull-through cache
   is what fills it, so routing becomes a single delivery step sourced at `A'` — no fill
   step, no probe, no write access needed. The hard guard is the inverse: **never plan a fill
   step whose target is proxy-mode**, because `proxySource.Fill` reads the whole image into
   `io.Discard` and commits nothing.
5. **The verification-vs-proxy refusal needs no extension.** It exists because a proxy
   resolves a *tag* itself and could serve an image the verifier never saw. A routed read of
   a verified job is digest-anchored by construction, and a proxy asked for `repo@digest` can
   only answer with that digest. An unpinned job made no claim to protect.
6. **`gantry.job.fallback`'s reason comes from the attempt's own `why`**, not from counting
   rows: a wait-for-fill retry adds a row without consuming an attempt, so row-counting
   desynchronises and would report "fallback" where the truth is "route".
7. **`Job.Fills` stays job-level** (a list, released when the job ends) rather than
   per-step-with-its-own-note. A waiter on `A'` is therefore released one hop later than
   strictly necessary — bounded by the job it is already waiting for, and bounded again by
   `source_wait`. Per-step notes are a follow-up, not a correctness matter.
8. **`LAYER_STATE_COPIED` is added.** `source_copy.go` writes `"copied"` for every copied
   blob and `layerStateToPB` has no entry, so **every registry copy currently reports
   `LAYER_STATE_UNSPECIFIED` per layer** — a live bug, found by the design pass, fixed in the
   one proto-regeneration window this work already needs.
9. **The two reference-parsing paths stay distinct.** A source ref is parsed under its store's
   options plus the Copier's test options; `dstRef` parses under the target store's own. They
   are not unified: the only coverage that http-vs-https does not leak between stores depends
   on that separation, and a third store makes it matter more, not less.
10. **An abandoned route's already-pushed blobs are not cleaned up.** They are
    content-addressed, so the next attempt and the direct route both reuse them; deleting
    them would risk deleting content another job is using. A registry with aggressive
    unreferenced-blob GC turns an abandonment into a re-transfer, which is documented rather
    than defended against.
11. **`Plan`'s route is advisory.** Coalescing is request-level (correctly: every route
    delivers the same image to the same store, so the route is provably not identity-bearing),
    so a submit can be served by an existing job that probed differently. Documented as a
    caveat on `Plan`.

### Two behaviours changed while landing Phase 1, both deliberate

The refactor was meant to be behaviour-free and was, except for two things the old
structure had been hiding:

- **The planned attempt's pull ref.** The old code derived it before verification pinned the
  source, which kept the tag by accident of ordering. Building attempts *after* pinning made
  that ordering explicit — and revealed that the tag form is a requirement, not a
  coincidence: the daemon is told to pull the tag and the digest anchors it separately. It is
  now derived from the tag deliberately, in the same place the fill-wait reference is.
- **A source that reports a cancellation.** Classification happens on the error as it came,
  so "the source said it was cancelled" still means *do not try elsewhere*. But the error is
  unwrapped where it leaves the job, so a job nobody withdrew is recorded as **failed**
  rather than canceled. Previously it read as canceled, which was the old code's accident
  and the wrong answer: `finish()` maps a cancellation to `JobCanceled`, and a self-inflicted
  abort is a failure.

### Mutation coverage after Phase 1

`worthAnotherSource` is pinned. Three mutations survive — pruning by `needs`, tolerating an
`optional` step's failure, and correcting a seeded row's source — because **no plan yet
contains any of them**: they are exactly what Phase 2 introduces, and its tests are what will
pin them. Noted here so a green suite is not mistaken for coverage of unbuilt behaviour.

### Log

- The queue/worker dispatch (a channel plus `MaxConcurrentJobs` goroutines running a job to
  completion) is **not** what this feature strains, so it is not being rewritten. What is
  strained is the execution *plan*: `jobExec` is a flat bag of single-valued fields that has
  already been partially generalised once (`bindings` for attempts) and would need a second,
  overlapping generalisation for steps. Two overlapping generalisations of the same idea is the
  actual smell. A design pass is running to settle the replacement before any of it is written.
