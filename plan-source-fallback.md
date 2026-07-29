# Plan — engine-pull source fallback (cache miss → origin)

Status: **Phases 1–3 landed and reviewed** · see [Progress](#progress) at the bottom.

## 1. Problem

The `remote → cache → engine` flow is two independently submitted jobs:

| hop | request | what runs |
| --- | --- | --- |
| hop1 | `ref=<origin>/repo:tag`, `source=<origin>`, `target=cache` | gantry copies blobs into the cache (`runCopy`) |
| hop2 | `ref=<origin>/repo:tag`, `source=cache`, `target=engine` | the daemon pulls from the cache (`runPull`) |

Nothing links them. There is no dependency edge, no cancellation propagation, and
**no ordering guarantee** — `MaxConcurrentJobs` defaults to 2, so a hop2 submitted
alongside hop1 can run first. Today the client is responsible for watching hop1 to
`done` before submitting hop2.

Consequences we want to fix, for the hop2 job only:

1. **A cache that cannot serve the image fails the whole job**, even though the origin
   is reachable and holds exactly the same content. The operator's intent — "the cache
   is an optimization, not a dependency" — is not expressible.
2. Whether the cache is empty *because hop1 failed*, *because hop1 has not run yet*, or
   *because the cache host is down* is indistinguishable from hop2's vantage point, and
   does not need to be distinguished: the observable fact is "I could not get it from my
   source".
3. When the cache is merely *not filled yet* (hop1 in flight), failing — or bypassing the
   cache — is worse than waiting a moment.

## 2. Design

### 2.1 An attempt loop, not a dependency graph

`runPull` becomes a loop over **source bindings** instead of a single pull:

```
attempt(planned source = cache)
  ├─ success ────────────────────────────────────────────────► job DONE
  └─ failure
       ├─ fatal (canceled / engine cannot do this at all) ────► job FAILED
       └─ retryable
            ├─ an in-flight job is filling this exact cache ref?
            │     yes → wait for it (bounded), then attempt(cache) once more ──► DONE / continue
            └─ fallback enabled?
                  yes → attempt(origin = the registry named in the job's own ref) ─► DONE / FAILED
                  no  → job FAILED
```

Why this shape rather than an admission-time dependency graph:

- **A warm cache is never delayed.** The wait only happens after a real miss, so a
  concurrent re-warm hop1 (which in `proxy` mode re-reads every blob to EOF, and in
  `copy` mode still round-trips the origin manifest) cannot block node pulls that the
  cache could already serve. An admission-time edge would block them and would re-import
  hop1's failure modes — including its origin dependency — into a pull that did not need
  either.
- **No new scheduler.** Pre-planned blocking would need a `BLOCKED` job state and a
  dispatcher that only hands runnable jobs to workers; otherwise `MaxConcurrentJobs`
  blocked jobs deadlock the pool against their own dependencies sitting behind them in
  the FIFO queue. Waiting *inside* a running attempt cannot deadlock the queue as long as
  the wait is bounded and capped (see §2.5).
- **No admission-time probe.** The first attempt *is* the probe.

### 2.2 The origin is already known

`plan()` resolves the source as `req.Source`, falling back to the ref's own registry
(`cpx.go:374-377`). So the origin is always recoverable from `req.Ref` — the fallback
binding is not new information, it is the binding the job would have had with `source`
unset. No new proto field is needed to *name* the fallback target.

### 2.3 Deriving the fallback binding

After admission (including verification's digest pinning), for a puller destination:

```go
originCfg   = stores.Registry(refRegistryHost)          // same call plan() already makes
originRef   = rewriteHost(ex.srcRef, originCfg.Host)    // preserves repo + tag/digest
originPull  = d.pullRef(originRef, originCfg)           // engine pull_host / origin downstream_host
```

Deriving `originRef` from `ex.srcRef` **after** pinning means the fallback inherits the
pinned digest automatically: a verified job pulls `origin/repo@sha256:…`, the same bytes
the cache attempt would have produced. See §3.2.

### 2.4 Finding an in-flight fill

The join key already exists and is exact — no heuristics:

- hop1 (pusher) **provides** `ex.cacheRef.Name()` — also stored in `Transfer.Ref`
  (`cpx.go:429`).
- hop2 (puller) **wants** `ex.srcRef.Name()` *before* verification pinning (hop1's cache
  ref is derived from the tag before pinning too, `cpx.go:396-398`).

Both are produced by `name.ParseReference`, so `.Name()` is canonical on both sides and
directly comparable. Each job records the ref it fills (`Job.Fills`, empty for puller
jobs); the store gains a lookup over active, non-`enqueuing` jobs.

### 2.5 Waiting safely

- `Job` gains a `done chan struct{}` closed exactly once by `run()` via `sync.Once`
  (in `run`, not `finish`, so an erased/evicted job still releases its waiters).
- Waiters `select` on `done`, `ctx.Done()`, and a timer — a lost close can only cost the
  timeout, never a hang.
- **Wait admission cap**: at most `max(1, MaxConcurrentJobs-1)` jobs may be waiting at
  once (a counting semaphore on the `Copier`). A job that cannot take a wait slot skips
  straight to the fallback/failure branch. This guarantees at least one worker keeps
  draining the queue, so a waited-for hop1 that is still `PENDING` can always be reached.
- The wait is bounded by `worker.source_wait` (default `0` = disabled).

### 2.6 One transfer row per attempt

`Job.Transfers` is already `repeated` and documented as "the job's steps, in execution
order"; today `plan()` emits exactly one and the executor hardcodes `Transfers[0]`.
Each attempt now appends its own row, so a job that fell back reads:

| # | store | source | ref | state | error |
| - | ----- | ------ | --- | ----- | ----- |
| 0 | node-a | cache | `cache:5000/lib/redis:7` | failed | `… 404 …` |
| 1 | node-a | remote | `docker.io/library/redis:7` | done | |

and the **job** is `done`. That is the "a cache miss is not a job failure" semantic,
expressed in the existing data model, with full per-layer progress on the winning
attempt. Attempts are capped at 3 (cache, cache-after-wait, origin).

## 3. Decisions

### 3.1 Scope: engine targets only

A registry target's source is normally already the origin; a fallback there has no
meaning and only complicates the admission rules. `plan()` rejects the flag on a pusher
destination.

### 3.2 Verification

The pinned digest is what makes the fallback safe: `runPull` anchors the daemon pull to
the binding's digest, and the fallback binding is derived from the *same* pinned ref, so
a verified job pulls byte-identical content from the origin. Rules:

- Verification runs once, against the **planned** source, exactly as today.
- No second verification is performed against the origin. Recon confirmed this is the
  right call for three independent reasons: it buys nothing when the job is pinned
  (content is digest-identical); with the verdict cache wired in (`app.go` → the
  `Caching` decorator, keyed by digest alone) a re-verify would usually be a **no-op**
  that answers from the first verdict; and it can *lose* the pin outright if the origin
  store is declared `verify: {mode: off}`, since both the notation verifier and the
  caching decorator return a zero-digest result for an off mode. A fourth hazard is
  avoided by not doing it at all: `Verify(from, src)` derives the registry from `src`
  but credentials/TLS from `from`, so passing a mismatched pair would send the cache's
  credentials to the origin.
- An **unpinned tag** job is a genuinely different trust decision, not a retry: the
  origin resolves the tag itself. That is the tag's authority, and the job never made a
  signature claim — but it is called out in the docs rather than left implicit.
- A `Verification` record with a digest continues to describe the job accurately: the
  digest is what was verified *and* what was pulled, regardless of which host served it.
- The fallback does **not** relax the existing proxy-mode refusal, which is an admission
  rule about the *destination*, not the source. Recon confirmed a proxy-mode **source**
  was never covered by that rule and is unaffected.

### 3.3 Digest `as` anchors

`fetchAnchor` currently reads the anchor manifest from `ex.source` and `docs/stores.md`
promises the origin is *never* contacted. With a fallback attempt the anchor must come
from the binding that is actually serving the pull. The promise becomes conditional:
**the origin is never contacted unless the fallback fires**, which is exactly what the
operator asked for by enabling it. Doc text is updated accordingly; the digest is still
re-hashed locally (`source.go:129`), so the anchor cannot be substituted.

### 3.4 `pull_host` makes a fallback inexpressible

`engineDest.pullRef` prefers `d.cfg.PullHost` over the source's host
(`dest.go:108-117`). With `pull_host` set, both bindings produce the *same* pull ref, so
a "fallback" would silently re-pull from the same place. `plan()` rejects the
combination rather than no-op'ing.

### 3.5 Dedup key

`dedupKey` must include the effective flag. Without it, a `fallback=false` submit could
coalesce onto an active `fallback=true` job and be served content from the origin it
explicitly refused. Same for the reverse direction.

### 3.6 Image naming is attempt-dependent (documented, not fixed)

Without `as`, the daemon records the image under the winning attempt's pull ref — the
cache host, or the origin host if the fallback fired — and `pullHook` stamps retention
with whatever the daemon reports (already reality-based). Jobs that need a stable local
name should use `as`, which is unaffected by which attempt won.

Recon surfaced the operational consequence, which is sharper than "the name differs":
retention rules are doublestar patterns over **host-qualified** repositories, so a rule
written for the cache host does not match an image the fallback delivered — it lands as
`unmanaged` and is never collected. Runtime enforcement is similarly host-keyed and may
fall through to its `on_unavailable` policy for such an image. Both are documented in
`docs/stores.md`, and `as` is the recommended fix.

### 3.7 Error classification

`down.Engine.Pull` returns opaque errors — recon confirmed there is **no** typed error or
sentinel on the pull path today (the daemon's in-band error becomes
`fmt.Errorf("pull: %s", …)`, discarding the JSON error code). Two classes must not
trigger a fallback:

1. **Cancellation** — `ctx.Err() != nil` / `errors.Is(err, context.Canceled)`: the job is
   being canceled or the server is shutting down.
2. **Engine-side failures** — the daemon could not do this at all, or the content already
   arrived and *naming* it failed. Recon found the second group is larger than expected
   and matters more: the classic-store digest-`as` rejection and the store probe, plus
   every post-pull step (`tag`, `loadDigestNames`, `untag`; containerd's `tag`, `retag`,
   `untrack`, and its digest-name mismatch). Retrying those against another source
   re-downloads the whole image and fails identically.

A single `down.ErrEngine` sentinel is introduced and wrapped at each of those sites, so
`cpx` classifies without string matching.

Everything else falls back, including "platform not available": a cache filled by a
platform-narrowed hop1 legitimately lacks a platform the origin has. A wrong fallback
costs one extra failed attempt; the final error joins both attempts' errors so nothing
is lost.
### 3.8 An unavailable fallback: error when asked for, no-op when defaulted

Recon caught a regression this would otherwise ship: a repository-only `ref`
(`team/app:1`) resolves its origin to `index.docker.io`, which is not a declared store,
so with `allow_unknown_stores` off a *server-wide default* would start failing every such
job at admission. The rule is therefore asymmetric:

- The request set `fallback_to_origin` explicitly → a fallback that cannot be expressed
  is an **error** (the caller asked for something unavailable).
- The value came from `worker.fallback_to_origin` → the fallback simply **does not apply**
  to this job; it is logged and `Plan` reports no fallback ref. Never silent.

This covers the undeclared origin, the `pull_host` collision (§3.4), and any ref-parsing
failure on the origin side.

## 4. Surface

### 4.1 Per-job flag

`proto/gantry/job.proto`, `Job` field 21:

```proto
  // Engine target only: when the job's source cannot serve the image, pull from
  // the registry named in `ref` (the origin) instead of failing — a cache is
  // then an optimization, not a dependency. Absent = server default
  // (worker.fallback_to_origin). Rejected for registry targets and for engine
  // stores with pull_host set.
  bool fallback_to_origin = 21 [
    features.field_presence = EXPLICIT,
    (orm.field) = {immutable: true}
  ];
```

Mirrors `copy_referrers` (field 20) exactly: EXPLICIT presence, absent = server default,
immutable. ORM generation propagates it into `JobAddRequest`/`JobSelect`;
`JobPlanRequest` (field 7) and `JobPlanResponse` (field 10) are hand-written in
`proto.svc/gantry/job_svc.proto`.

`JobPlanResponse` also gains `fallback_ref` (field 11) — the ref the engine would be told
to pull on a fallback — so `Plan` stays a complete preflight.

### 4.2 Server config

```yaml
worker:
  # Default for a job that does not set fallback_to_origin.
  fallback_to_origin: false
  # How long an engine pull waits for an in-flight job that is filling its
  # source with exactly this image before giving up on the cache. 0 disables.
  source_wait: 0s
```

Both default to today's behavior, so nothing changes for an existing deployment until it
opts in.

### 4.3 Observability

- `gantry.job.fallback` (counter, attrs `from`, `to`) — incremented when an origin
  attempt starts. This is the anti-silent-bypass signal: a cache quietly not being used
  shows up here.
- `gantry.job.source_wait` (histogram, seconds, attr `outcome` =
  `served|timeout|canceled|skipped`) — whether waiting for a fill is worth its bound is
  an operational question, and this answers it per deployment.
- Audit: a `job_fallback` event type (durable, unlike the in-memory job record) carrying
  the job id, ref, from/to store and the cause string.
- The failed attempt stays visible as a failed `Transfer` on a `done` job.

## 5. Change list

| File | Change |
| ---- | ------ |
| `proto/gantry/job.proto` | `Job.fallback_to_origin = 21` |
| `proto.svc/gantry/job_svc.proto` | `JobPlanRequest.fallback_to_origin = 7`, `JobPlanResponse.fallback_to_origin = 10`, `.fallback_ref = 11` |
| `proto/gantry/event.proto` | `EVENT_TYPE_JOB_FALLBACK` |
| `pb/**`, `proto/gantry/*_svc.g.proto` | regenerated (`scripts/gen-proto.sh`, `ORM_ROOT=/workspaces/github.com/protobuf-orm`) |
| `cmd/config/serve.go`, `config.go` | `WorkerConfig.FallbackToOrigin`, `WorkerConfig.SourceWait` + defaults |
| `internal/cpx/cpx.go` | `Request.FallbackToOrigin *bool`; `jobExec` bindings; `plan()` admission; `runPull` attempt loop; wait semaphore; metrics |
| `internal/cpx/job.go` | `Job.Fills`, `Job.done`/`markDone`, `dedupKey` gains the flag, transfer append helper |
| `internal/cpx/store.go` | `Filling(ref string)` lookup over active jobs |
| `internal/cpx/source.go` | `fetchAnchor` called per binding (signature already takes the store) |
| `internal/down/down.go`, `docker.go` | `ErrUnsupported` sentinel + wrap |
| `internal/rpc/job.go` | plumb the flag through `Add`/`Plan` |
| `internal/rpc/convert.go` | job `Source`/`Target` from the **last** transfer; emit `fallback_to_origin`; event type map |
| `internal/event/log.go`, `recorder.go` | `JobFallback` type + recorder method |
| `docs/stores.md` | fallback section; amend the "origin never contacted" invariant |
| `docs/api.md` | job fields, dedup key, Plan output |
| `README.md` | one bullet |

## 6. Phases

Each phase is independently shippable and leaves the tree green.

- **Phase 1 — fallback.** Attempt loop with two bindings, the flag, config default,
  per-attempt transfers, dedup key, the `convert.go` served-attempt fix, metrics, docs,
  tests. No waiting, no store lookup. Delivers the actual ask.
- **Phase 2 — wait for an in-flight fill.** `Job.Fills` + `done` channel + `Filling`
  lookup + wait semaphore + `source_wait` config + tests.
- **Phase 3 — audit event.** `job_fallback` durable event (needs the event enum regen).

## 7. Tests

| Level | Case |
| ----- | ---- |
| unit | cache attempt fails → origin attempt runs → job `done`, 2 transfers, `[0]` failed |
| unit | cache attempt fails, flag off → job `failed`, 1 transfer (today's behavior) |
| unit | cancellation during attempt 1 → job `canceled`, **no** origin attempt |
| unit | engine-capability rejection → job `failed`, no origin attempt |
| unit | both attempts fail → job `failed`, error mentions both |
| unit | digest-pinned job → origin attempt is digest-anchored to the same digest |
| unit | digest `as` → anchor is fetched from the winning binding |
| unit | admission: flag + registry target → rejected; flag + `pull_host` → rejected |
| unit | dedup: flag on/off do not coalesce onto each other |
| unit | `Plan` reports the effective flag and the fallback ref |
| unit (P2) | in-flight fill present → waits, retries the cache, succeeds without touching origin |
| unit (P2) | wait times out → falls back |
| unit (P2) | wait slots exhausted → skips the wait, falls back immediately |
| e2e | L1 hermetic: cache registry stopped mid-flow, engine still gets the image |

## Progress

Legend: ☐ todo · ◐ in progress · ☑ done · ✗ dropped (with reason)

| # | Item | State | Notes |
| - | ---- | ----- | ----- |
| 0 | Recon of affected subsystems | ☑ | 5 parallel readers (job store / verify / engine pull / codegen / tests); findings folded into §3 |
| 1 | This plan | ☑ | revised after recon |
| — | **Phase 1 — fallback** | ☑ | `go build`, `go vet`, `go test -race ./...` all green |
| 1.1 | `down.ErrEngine` sentinel + wrap at every engine-side site | ☑ | `down.go`, `docker.go` (probe, classic-store, tag, loadDigestNames, untag), `containerd.go` (digest mismatch, tag, retag, untrack) |
| 1.2 | `sourceBinding` + `bindSources` admission | ☑ | origin re-parsed under its OWN store options (TLS scheme), `pull_host` collision rejected, explicit-vs-default asymmetry (§3.8) |
| 1.3 | `runPull` attempt loop + `pullFrom` + `sourceFallbackWorthy` | ☑ | one `Transfer` row per attempt; anchor + size estimate now per binding |
| 1.4 | `dedupKey` gains the flag | ☑ | plus the two existing test call sites |
| 1.5 | `Job`/`JobSnapshot`/`PlanResult` carry the effective decision | ☑ | `PlanResult` also reports `FallbackRef` |
| 1.6 | `convert.go`: job source/target from the LAST transfer | ☑ | single-transfer jobs unaffected |
| 1.7 | `worker.fallback_to_origin` config + `gantry.yaml` sample | ☑ | default `false` |
| 1.8 | `gantry.job.fallback` metric | ☑ | attrs `from`/`to` |
| 1.9 | proto field + regeneration | ☑ | `Job.fallback_to_origin=21`, `JobPlanRequest`=7, `JobPlanResponse`=10/11; see the regen note below |
| 1.10 | RPC plumbing (`Add`, `Plan`, `jobToPB`, `planToPB`) | ☑ | tri-state preserved end to end |
| 1.11 | Unit tests (`internal/cpx/fallback_test.go`) | ☑ | 10 cases; `fakePullEngine` gained per-ref failure + full attempt history |
| 1.12 | RPC test (`internal/rpc/job_test.go`) | ☑ | absent / true / false all forwarded correctly |
| 1.13 | L1 e2e tests (`internal/e2e/fallback_test.go`) | ☑ | full gRPC path; asserts the origin is NOT contacted without the flag |
| 1.14 | Docs | ☑ | `stores.md` (new section + amended anchor invariant), `api.md` (dedup key, Plan), `observability.md`, `README.md`, `gantry.yaml` |
| — | **Phase 2 — wait for an in-flight fill** | ☑ | `go test -race -count=2 ./internal/... ./cmd/...` green |
| 2.1 | `Job.Fills` + `done` channel + `markDone` released by `run()` | ☑ | `sync.Once`; deferred in `run`, not `finish`, so an erased record still frees waiters |
| 2.2 | `Store.Filling(ref)` | ☑ | mirrors coalescing's terminal/canceled/enqueuing exclusions; also skips records with no `done` channel |
| 2.3 | `jobExec.fillWant` / `.fills` | ☑ | `fillWant` captured pre-pinning, so it matches the tag-derived ref a registry target advertises |
| 2.4 | `waitForFill` + wait-slot semaphore | ☑ | `cap = max(1, MaxConcurrentJobs-1)`; no slot → straight to the fallback |
| 2.5 | `worker.source_wait` config + sample | ☑ | default `0` = off |
| 2.6 | Tests | ☑ | 5 cases incl. the join-key test; the *capture point* (pre-pinning) is pinned separately by `TestFillWantIsCapturedBeforePinning` |
| 2.7 | Docs | ☑ | `stores.md` "Waiting out a fill that is still running" |
| — | **Phase 3 — audit event** | ☑ | |
| 3.1 | `EVENT_TYPE_JOB_FALLBACK = 8` + regen | ☑ | strictly additive; `EventSelect`/`EventAddRequest` reference the enum by name so no service-proto churn |
| 3.2 | `event.JobFallback` + `Recorder.JobFellBack` | ☑ | reuses `Event.Store`/`Error` + `detail.{job,source}`; `admittedDetail` renamed `jobDetail` |
| 3.3 | `cpx.Recorder` gains `JobFellBack`, emitted from the attempt loop | ☑ | emission pinned by `TestFallbackIsAudited` (fires once on a fallback, never otherwise) |
| 3.4 | convert maps + docs + test | ☑ | `observability.md` event table |
| — | **Adversarial review** | ☑ | 2 real defects found and fixed, 1 out-of-scope; see the log below |
| 4.1 | Effective-vs-requested flag (dedup key + report) | ☑ | + test `TestFallbackWithNowhereToFallBackIsNotReported` |
| 4.2 | Job source/target reported from the attempt that served it | ☑ | + e2e test `TestFailedFallbackReportsTheRequestedSource` |
| 4.3 | Canceled-context branch of `sourceFallbackWorthy` covered | ☑ | `TestEnginePullDoesNotFallBackWhenTheJobIsCanceled`; the fake engine now calls `failFor` outside its lock so a test can hold a pull open |
| 4.4 | Wait-slot cap documented precisely (floor of one at `max_concurrent_jobs: 1`) | ☑ | from a refuted-but-accurate review observation |
| — | **Test/doc audit** | ☑ | mutation-tested; 8/8 mutations now caught |
| 5.1 | The cancel test was **vacuous** — fixed | ☑ | it waited on the *handle* snapshot, which reads terminal the instant a handle is canceled, so it returned before the worker could make a second attempt. It now waits on the execution's own completion, and a mutation removing the `ctx.Err()` guard fails it |
| 5.2 | Audit emission + `gantry.job.fallback` counter pinned | ☑ | fake `Recorder` + a manual OTel reader |
| 5.3 | Anchor-per-binding pinned | ☑ | the cache store is given an unbuildable transport, so an origin attempt carrying the wrong store config fails outright |
| 5.4 | `Store.Filling` exclusions pinned | ☑ | terminal / canceled / enqueuing / no completion channel |
| 5.5 | Origin binding parsed under its OWN store options pinned | ☑ | named hosts, not loopback — ggcr treats `127.0.0.1` as insecure on its own and would have masked it |
| 5.6 | `anchoredRef` failures wrapped in `ErrEngine` | ☑ | a malformed anchored ref fails identically at any source |
| 5.7 | `gantry.job.source_wait` histogram implemented | ☑ | §4.3 had designed it; the plan now matches the code |
| 5.8 | e2e fake engine records every pull it was ASKED for | ☑ | it recorded only successful ones, so `pullRecords`' own comment was false and any "attempts == 2" assertion would have been wrong |
| 5.9 | Four wrong doc statements corrected | ☑ | see the log |

### Notes for whoever runs the generator next

`scripts/gen-proto.sh` **overwrites** `proto/gantry/job_svc.g.proto` and drops
hand-written comments that were edited directly into that generated file. Verified by
running the generator on an unmodified copy of the tree: `pb/` regenerates
**byte-identical** (no `features.field_presence` drift, contrary to the earlier note in
this repo's lore), and the only diff is lost comments. After this change:

- The `List` RPC comment was **moved into the overlay** (`proto.svc/gantry/job_svc.proto`)
  so it now survives regeneration.
- The `Get` RPC comment and the `JobAddRequest` leading comment come from the generator's
  own templates and must still be re-applied by hand after each run. Both are re-applied
  in this change (and the `JobAddRequest` one updated for the new dedup key).

Caveat: the `protobuf-orm` generator repos on this machine have uncommitted local
changes, so byte-identical regeneration is guaranteed here but not from a clean clone of
their pushed HEADs.

### Log

- Baseline verified: `go build ./...` clean; `ORM_ROOT=/workspaces/github.com/protobuf-orm`
  present with all four generator repos; module proxy reachable (buf fetchable).
- Recon revised four design points: the verifier must **not** be re-run against the origin
  (verdict cache no-op + pin loss + credential mismatch); engine-side failures are a
  larger class than "capability gap" and must not be retried; an undeclared origin would
  have made a server-wide default fail jobs (hence §3.8); retention rules are host-keyed,
  so a fallback-delivered image can become uncollectable without `as`.
- Phase 1 landed green. `jobBytes` deliberately still sums every transfer, so a failed
  attempt's partial bytes count toward `gantry.bytes` and the audit record — "bytes moved"
  is honest, but worth knowing when reading a fallback job's numbers.
- Phase 2 landed green. The wait turned out cheaper than the plan estimated: because it
  runs *inside* a running attempt, no `BLOCKED` state, no scheduler change and no
  admission-time probe were needed. The slot semaphore is what makes it safe — without it,
  `MaxConcurrentJobs` waiters could park on fills still sitting in the queue behind them.
- Phase 3 landed green. Regenerating for the enum also re-dropped the two hand-written
  comments in `job_svc.g.proto`; re-applied again. That is now a standing cost of every
  regeneration — see the note above.
- Adversarial review (4 lenses × independent refuters) found two real defects, both fixed:
  1. **The reported flag was the requested value, not the effective one.** A job whose
     source already *is* the origin has no second binding, yet reported
     `fallback_to_origin: true` — and, worse, carried that into the dedup key, so two jobs
     that provably behave identically stopped coalescing. `plan()` now narrows
     `ex.fallback` to `len(bindings) > 1`, which is the honest definition, and both the
     report and the key follow from it.
  2. **`jobToPB` named the last attempt's store even when nothing succeeded.** A job that
     failed every attempt reported `source: origin` — a store that never served it and
     that the caller never asked for. It now reports the attempt that actually served the
     job, falling back to the first (i.e. the requested source) when none did.
  A third finding — the idempotency key replays a remembered job without comparing the
  request, so it can bypass the dedup key — is **pre-existing and documented behavior**
  ("the key alone wins, even if the request body differs", `docs/api.md`). Not changed.

  Three further findings were raised and did not survive verification, but two left
  something worth recording:
  - *A store declared literally as `docker.io` never matches the ref's canonical
    `index.docker.io`, so the origin will not resolve.* True, but pre-existing: the
    unchanged `source`-defaulting path at the top of `plan()` already fails the same way
    for the same config, and `allow_unknown_stores` (or declaring `host: index.docker.io`)
    fixes both. This change only adds one more caller of the existing resolver rule.
  - *The wait-slot cap floors at one, so `max_concurrent_jobs: 1` lets the only worker
    park on a fill that can never start.* The arithmetic is right and it is what the spec
    says, but at one worker the pipeline is serial by construction — nothing can be
    filling anything while the pull runs — so the wait only spends its bound and then
    behaves exactly as it would with `source_wait: 0`. Latency, not wrong behavior, in a
    config the operator degraded themselves. **The docs were loose about this and are now
    corrected** (`docs/stores.md`, `WorkerConfig.SourceWait`): the cap is
    `max_concurrent_jobs - 1` but never below one, and `source_wait` should stay `0` on a
    single-worker server.
- Also hardened while reading the diff: `runPull` now rejects an empty binding list
  explicitly. It is unreachable through `plan()`, but the loop would otherwise fall
  through with no error and mark a job DONE without pulling anything.
- A follow-up audit mutation-tested the suite (flip the classification, disable the wait,
  drop the narrowing, revert the reported source, …). It found one test that could not
  fail — see 5.1 — plus six behaviors with no test at all, now covered. Every mutation is
  caught.
- It also found four doc statements that were wrong rather than merely loose, all fixed:
  1. "The last three are errors only when the request set `fallback_to_origin`
     explicitly" — a source that already *is* the origin is never an error, explicit or
     not; it returns before the error path.
  2. "The job's reported `source` is the attempt that actually served it" — only when
     something served it; a wholly failed job reports the source it was pointed at.
  3. The "origin never contacted" invariant survived unamended in `README.md`,
     `fetchAnchor`'s own doc comment, `down.AnchorBlob`, and the `as` field in
     `job.proto` — the one place it had been amended was `docs/stores.md`.
  4. `event.proto` said `detail.source` on `job_admitted` is "the one that served the
     job"; it is the source the job was *admitted with*, which on a job that later fell
     back is precisely the one that did not serve it.
