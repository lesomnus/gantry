# Plan — routing a copy through a store's cache (A → A' → B)

Status: **all phases landed and reviewed** · follows `plan-source-fallback.md` (already landed on `feat/source-fallback`).

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
| 0.2 | Execution-model redesign: recon + two independent designs + judge | ☑ | recon in; two designs and the judge still running |
| 0.3 | `Job.Source`/`Target` carried on the record, not derived from a transfer (§3.6) | ☑ | + `docs/api.md` section and an rpc test; one e2e assertion updated |
| 0.4 | `StoreConfig.Cache` + cross-store validation | ☑ | declared, a registry, not itself, and not on an engine store; a store that is someone's cache may declare its own (routing is one level) |
| 0.5 | Recon bugs in the prerequisite: seal on every exit, `Filling` skips sealed | ☑ | both pinned by tests |
| 0.6 | Execution-model verdict + 11 open decisions settled | ☑ | `execPlan` (steps × attempts) wins; 9 ideas grafted from the runner-up |
| — | **Phase 1 — steps** | ☑ | behaviour-free: `-685/+310` lines, whole suite green unchanged apart from two documented semantics |
| 1.0 | Free `copyLayers` from `jobExec` | ☑ | takes the two repositories; the last `jobExec` literal outside `plan()` is gone |
| 1.1 | `Transfer.Step` + `LAYER_STATE_COPIED` (one proto regen) | ☑ | the enum gap was live: every registry copy reported `LAYER_STATE_UNSPECIFIED` per layer |
| 1.2 | `execPlan`/`execStep`/`execAttempt` + `mover` | ☑ | `plan.go` / `move.go` / `run.go`; `jobExec`, `sourceBinding`, `runCopy`, `runPull`, `pullFrom`, `bindSources`, `sourceFallbackWorthy` all deleted |
| 1.3 | `plan.validate()` + tests | ☑ | delivery-only-last, attempt bound, indices, runner present |
| — | **Phase 2 — the route** | ☑ | |
| 2.1 | `resolveDigest` / `holdsDigest` source helpers | ☑ | one manifest request at the authority, one digest probe at the cache |
| 2.2 | `route()`: probe, fill step, route attempt, degenerate rules | ☑ | 14 unit cases + 2 L1 e2e |
| 2.3 | `PlanResult.Steps` + `JobPlanResponse.steps` | ☑ | the resolved route, visible before submitting |
| 2.4 | Docs | ☑ | `stores.md` (new section), `api.md` (transfers-as-hops contract, Plan), `observability.md`, `README.md`, `gantry.yaml` |
| — | **Phase 3 — `require_authority`** | ☑ | |
| — | **Adversarial review** | ☑ | 5 lenses × refuters; 2 serious defects and 8 more, all fixed and mutation-checked |
| 3.1 | `Job.require_authority` = 22 + `worker.require_authority` | ☑ | one regen with the Plan route messages |
| 3.2 | Enforcement + RPC plumbing + tests | ☑ | explicit request beats the server default; no-op without a route |

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

### Mutation coverage after Phases 2–3

Everything Phase 1 left unpinned is now caught: pruning by `needs`, tolerating an `optional`
hop's failure, correcting a seeded row's source, and collapsing a proxy cache to one hop. The
fill hop's own settings (`verbatim`, un-narrowed `platforms`, `optional`, the tag it publishes)
are pinned on the plan.

Two gaps stated plainly rather than papered over:

- **`plan.validate()`'s call site is unpinned.** The function is unit-tested directly, but no
  plan `plan()` currently builds violates an invariant, so deleting the call changes nothing.
  It is a construction guard; it earns its keep the next time a step is added.
- **The fill's `verbatim` cannot be caught end to end with an in-memory registry.** Both of
  its reasons are properties of a real one: a rebuilt index differs only when the source index
  has something the rebuild drops (annotations, attestation children) — a plain two-platform
  index round-trips byte-identically — and "a registry rejects an index whose children are
  missing" is a validation the fake does not perform. Hence the plan-level assertion, with the
  reasoning written next to it.

### The adversarial review, and what it found

Five lenses × independent refuters, 79 agents. Several lenses found the same defects
independently. Everything below is fixed, with a test that a mutation reverting the fix fails.

**Two of them were serious.**

1. **A routed copy silently dropped referrer propagation** — reproduced twice, independently.
   The fill hop never set `referrers`, so it copied the image without the signatures over it;
   the delivery hop then asked the *cache* for referrers it had never been given, found none —
   an empty list is not an error — and the job completed `done` having dropped exactly what
   `copy_referrers` exists to propagate. It needed no flag: for a verified source
   `copy_referrers` defaults on, so declaring `cache` on a verified store silently stopped
   signatures reaching every target. It also outlived the fill: a cache warmed by an earlier
   job that did not need referrers stays referrer-less, so every later job inherited the drop.
   §3.4 asserted the opposite behaviour and §9 listed a test for it that was never written.

   Fixed on both halves: the fill carries the job's `copy_referrers`, and a warm cache that
   holds the image but *not* its referrers is declined rather than read — the image is already
   cached, so declining costs only the bytes the job was going to move anyway.

2. **`resolveDigest` read a definitive answer as silence.** A 404 from the authority — "I do
   not have this reference" — was treated the same as an unreachable authority, so the job fell
   into the read-the-cache-by-tag branch and served content the authority never had, under a
   tag the caller believes points at the authority's image. `holdsDigest` was worse: its
   predicate accepted *any* error carrying an OCI error envelope, so 401/403/429/5xx all read
   as "the cache is cold" and triggered a pointless full fill into a store that already had it
   and would refuse the push for the same reason it refused the probe.

   Fixed by naming the distinction: `ErrNoSuchImage` for a registry's own definitive no (404,
   and 403 because some registries hide existence behind it), everything else an error. A
   definite no now means the job is simply not routed; only genuine silence reaches
   `require_authority`.

**And these:**

- **`require_authority` was missing from the dedup key**, so a strict submit could be served by
  an active job that read a cache unconfirmed — the same class of bug already fixed for
  `fallback_to_origin`. The key is now about what a caller may be *served*, and the doc says so.
- **A routed job waited out `source_wait` on its own fill.** `Filling` had no self-exclusion,
  and a routed job publishes the very reference its own later hop reads.
- **`pull_host` left an orphan fill hop**: the collapse check ran when the route *attempt* was
  added, after the fill step had already been inserted, so gantry copied a whole image into the
  cache and then never read it. Reachability is now settled before anything is filled — which
  also covers a proxy-mode *target*, which ignores whatever source it is handed and fetches
  from its own upstream.
- **An abandoned route announced nothing.** `recordLeaving` only fired between two attempts
  that both ran, so a route attempt *pruned* by `needs` — the common case — emitted no counter
  and no audit event, and the reason came from the attempt taken up rather than the one given
  up, making the documented `reason=route` unreachable. Now `recordGivingUp` fires for a
  skipped attempt too and names what was given up.
- **A layer failure sealed a job permanently.** The seal is meant to stop new callers joining a
  move that is winding down; a job that recovers via another attempt or hop is not winding down,
  so it is now lifted when an attempt succeeds.
- **Admission had unbounded I/O.** The two new registry requests are bounded together by
  `worker.admission_timeout` (default 10s), so an unresponsive registry costs one submit its
  timeout instead of holding the caller open.
- **`Job.require_authority` was never populated**, and a refusal returned `INVALID_ARGUMENT`
  where the documented class for "the environment could not satisfy a precondition" is
  `FAILED_PRECONDITION`. Both fixed.
- **Docs**: field 21's entire doc comment had been absorbed into field 22, leaving
  `fallback_to_origin` undocumented in the proto; `Transfer.step`'s "a step gantry planned and
  then did not need leaves no row" was false, since one row is seeded per planned step;
  `docs/stores.md` still carried the pre-`cdfb30c` rule about a job's reported source; the
  README described a `transfers` entry as a hop when it is an attempt; and the routing section
  omitted the retention consequence — a routed engine job leaves the node holding the *cache's*
  host name, so host-keyed rules written for the origin stop matching.

### Log

- The queue/worker dispatch (a channel plus `MaxConcurrentJobs` goroutines running a job to
  completion) is **not** what this feature strains, so it is not being rewritten. What is
  strained is the execution *plan*: `jobExec` is a flat bag of single-valued fields that has
  already been partially generalised once (`bindings` for attempts) and would need a second,
  overlapping generalisation for steps. Two overlapping generalisations of the same idea is the
  actual smell. A design pass is running to settle the replacement before any of it is written.

### Second adversarial review, and what it found

Six lenses (routing, execution, identity, concurrency, contract, design) × independent
refuters, over the whole branch. Everything below is fixed, and every fix has a test
that a mutation reverting it fails.

The shape of what it found is worth stating, because it is not what the first round
found. The first round found *missing* behaviour. This one found **guards wired onto
one branch of three**: `cacheHasReferrers` and `unreadableCache` were both added late,
both onto the confirmed-authority path only, so `route()` ran on the silent path a
route the same function refuses to build one branch over.

1. **Routing silently re-anchored the origin fallback.** `repin` rewrote *every*
   delivery attempt to the digest the SOURCE reported, including the `whyOrigin` one —
   which is not a nearer read of the authority's content but the fallback's own,
   different trust decision (§3.2 of the fallback plan: the origin resolves the tag
   itself). Declaring `cache:` on a store therefore converted an unpinned tag fallback
   into a digest fallback, and failed it outright whenever the source held a manifest
   the origin never had — a platform-narrowed copy rebuilds the index, so that is
   ordinary rather than exotic. `repin` now skips the origin attempt.

2. **The referrer guard asked the wrong question.** `count > 0` at the cache gets both
   directions wrong: a cache with one of three signatures reads as complete, and an
   image that legitimately has none reads as deficient — so a warm cache was declined
   for **every unsigned image**, permanently, while every job still reported `done`.
   The optimization was inert for a broad class of images and nothing said so. It now
   compares against the authority (`have >= want`), with `want == 0` short-circuiting
   before the cache is listed at all.

3. **The silent-authority branch skipped both guards.** A `copy_referrers` job read a
   referrer-less cache unprobed and completed `done` with the signature dropped — the
   same request without the `cache:` line *fails*. And `require_authority` refused jobs
   that provably could not be routed (a `pull_host`-collapsed engine, a proxy target),
   contradicting "a no-op for a job that is not routed". Both guards now run on that
   branch, ahead of `require_authority`, because a job gantry was never going to route
   is exactly the job the flag has nothing to say about.

4. **A fill did not carry referrers for an engine target, and could not be asked to.**
   `p.copyReferrers` is assigned only inside `if isRegistry`, and an explicit
   `copy_referrers` on an engine target is rejected — so a routed engine job filled the
   cache without signatures, and `serve.enforce`, which resolves what a node holds by
   HOST, then found an unsigned image at the cache and killed the container. The fill
   now carries referrers **unconditionally**, which is the same rule §Decision 1
   already reached for `verbatim` and `platforms` and for the same reason: what gantry
   puts in a shared cache is read by later jobs that asked for something else.

5. **`commitVerbatim` lost the destination's scheme.** `name.NewDigest(dst.Context().Name() + "@" + …)`
   re-parses without options; http-vs-https lives in the parsed `Registry`, not in the
   string. So the children went over https while the index went over http, and the
   verbatim commit could never succeed against a plain-HTTP cache addressed by a DNS
   name. Making the routed fill unconditionally verbatim turned that latent opt-in bug
   into a hard blocker for the whole feature — and every `httptest` registry in the
   suite masked it, because ggcr auto-detects `127.0.0.1` as http on both sides. The
   child is now derived from `dst.Context()`, and the test binds **127.0.0.2**, which
   ggcr does not special-case.

6. **`copy_referrers` was missing from the dedup key.** The branch rewrote the key's
   contract to "what a caller may be SERVED" and widened it twice on that rule, leaving
   out the strongest case: the image is byte-identical and the signature simply absent,
   which is the hardest of the three to notice.

7. **Nothing coordinated cold fills.** The probe cannot see a fill in flight — it has
   published nothing yet — so N concurrent submits each planned their own fill and each
   streamed the image out of the authority, which is the egress the feature exists to
   spend once. `route()` now asks the job store as well, and plans no fill when one is
   already running. Collapsing the burst onto a single authority read still requires
   `worker.source_wait`; that is now documented where it matters rather than left to be
   discovered.

8. **A target that refused the write was blamed on the source.** `worthAnotherSource`
   excluded only cancellation and `down.ErrEngine`, so a destination-side rejection on a
   registry copy was answered by re-reading the whole image from the origin — refused
   identically at the end — and reported as `gantry.job.fallback{reason=route}`, polluting
   the one signal an operator has for cache health. The recon note that said this
   classifier "cannot be reused verbatim on the copy path" had been honoured for the fill
   hop only. There is now an `ErrDestination` sentinel, applied by attributing the error
   to whichever registry actually answered it (`transport.Error` carries the request), so
   a mid-copy failure at the *source* still falls through as before.

9. **`reason=origin` is unreachable** — the origin attempt is always last, so it is never
   the attempt given up. The mechanism is right (the reason names what was given up); the
   docs promised a value that cannot occur. Documentation fixed, in `stores.md` and
   `observability.md`.

10. **A cache that stopped being declared** produced a zero `StoreConfig` (the error from
    `stores.Config` was discarded), which would have routed the job at whatever
    `/repo:tag` parses to. Now declines to route.

Not fixed, deliberately: `require_authority` enters the dedup key un-narrowed while
`fallback` is narrowed to "an origin attempt was actually bound". Narrowing it would make
a coalesced strict caller read `require_authority: false` from the shared snapshot, and
`route_test.go` deliberately pins the split. The asymmetry is real and is a design
question, not a defect.

Mutation coverage: 9 of the 10 fixes fail a test when reverted. The tenth — the
`want == 0` short-circuit in `cacheServesReferrers` — cannot, because removing it is a
cost difference (one fewer listing at the cache) and not a behaviour: with no referrers
anywhere, `have >= want` answers the same. It is noted rather than papered over.

### Acting on the design-level concerns

The second review left six things that were not defects — gaps in what the design
could express, what it defaulted to, and what an operator could see. All six are
now closed. Each has a test that a mutation reverting it fails (14 mutations, all
caught).

1. **The cost model needed a second knob to hold, and nobody was told.** The
   promise is that the origin is read once rather than once per destination, and
   the burst the feature exists for — N destinations submitted together for a cold
   image — was the case where it did not. `worker.source_wait` is what collapses
   it, and it defaulted to off.

   It now defaults to **30s**. The argument that made the wait safe in the first
   place (§2.1: the wait happens only after a real miss, so a warm source is never
   delayed) is the argument for defaulting it on, and the slot semaphore already
   bounds the cost. Zero is a meaningful value here — waiting off — so the field
   became a pointer: unset is 30s, explicit `0s` is off, and one worker forces 0
   because a serial pipeline can have nothing filling anything while the move that
   would wait for it runs.

2. **A waiter was released a hop late.** `Job.Fills` released everyone when the JOB
   ended (Decision 7), so a reader of an intermediate waited out the delivery hop
   as well — a whole image copy it has no stake in. With waiting on by default that
   stops being theoretical: waiters would routinely burn their whole bound on a
   fill that landed minutes earlier. Each published reference now has its own gate,
   opened by the step that publishes it; the job's end opens whatever is left, so a
   waiter still can never outlive the job it waits on.

3. **A declined route was invisible.** There are nine ways `route()` can decline,
   and all of them logged and returned. `gantry.job.fallback` cannot cover them —
   it counts a route abandoned at RUN time — so a permanently dead route looked
   exactly like a healthy fleet: same job outcomes, same states, a different bill.
   `gantry.job.route` now records every decision at admission
   (`filled`/`warm`/`proxy`/`joined`/`declined`/`rejected`, plus a closed set of
   reasons), counted only for jobs whose source declares a route, so the metric is
   a complete breakdown of the routed population rather than of every job on the
   server. `route()` became a wrapper over `routeDecision` so there is exactly one
   place that records.

4. **`require_authority` split the dedup key when it could not apply.** Its twin
   `fallback` is narrowed to "an origin attempt was actually bound"; this one was
   the raw effective value, so a strict and a lenient submit for a source with no
   route ran the same image copy twice. It is now narrowed to sources that declare
   a route — the flag is only ever consulted for a routed job, and only such a
   source can be routed — and field 22's proto comment says "effective" in the same
   sense field 21 does, which it did not before.

5. **The route could not express a topology.** One cache per source, for every job.
   A per-rack or per-repository layout was inexpressible, and the natural attempt —
   declaring a cache on the cache — silently produces two independent routes rather
   than a chain, which the config validator accepted and a test appeared to bless.
   `caches:` is now an ordered list of routes scoped by `for_targets` (store names)
   and `for_repos` (doublestar over the repository PATH); first match wins, `cache:
   x` is the shorthand for a single unscoped route, and the loader rejects both
   spellings together, a host-qualified repo pattern (which could never match), and
   a route made unreachable by an unscoped one ahead of it. Routing stays one level
   deep — the docs now say so plainly.

6. **The host a routed job leaves on a node was nobody else's business.** Retention
   rules and `serve.enforce` both key off it, and both were written for the origin.
   A rule now expands across the caches its registry declares (for the targets those
   routes can reach), so it is stated once; and enforcement asks the cache first and
   then the registries that declare it as their cache, acting on a refusal only once
   every one of them has been asked. The signature is over the digest, so proof from
   any of them is proof about the same bytes — and a cache whose referrers were
   collected no longer costs a running container.

Also: `maxAttempts` is headroom, not a live constraint, and the comment now records
that the largest plan `plan()` can build makes four attempts, so the gap is visible.
