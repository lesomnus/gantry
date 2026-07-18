# Runtime signature enforcement (quarantine) — implementation plan

Status: **in progress** (branch `feat/verify-enforce`). This document is both the
build plan and the living feature description — each section is kept in sync with
the implementation as PRs land.

## Progress

| PR | Workstream | Status |
|----|-----------|--------|
| PR1 | Duration `w`/`d` parsing | ✅ landed |
| PR2 | Config structs + Evaluate | ✅ landed |
| PR3 | bbolt verdict cache | ✅ landed |
| PR3b | Local OCI-layout verify source | ✅ landed |
| PR4 | Caching verifier decorator | ⬜ pending |
| PR5 | down.Enforcer + docker methods | ⬜ pending |
| PR6 | Enforce manager | ⬜ pending |
| PR7 | App wiring + refresh sweeper + docker E2E | ⬜ pending |

Deviations from the original plan are noted inline in each PR section as
**(landed: …)** annotations.

## Goal

Kill containers that run an image **not signed by a trusted Root CA**. gantry
already verifies signatures at *copy/job admission* (`internal/verify` +
`internal/cpx` `SetVerifier`). This adds a *runtime* gate: gantry watches each
docker engine's container-start events and, when a started container's image is
unsigned/untrusted, force-removes the container (`docker rm -f`) and then removes
the image.

This is **post-hoc quarantine**, not admission control — the `start` event fires
*after* the container is running, so a short-lived container can complete before
it is killed. True pre-start blocking would need a docker authorization plugin;
that is explicitly out of scope. A verdict **cache** lets enforcement keep
working when the source registries are unreachable.

Scope for v1: **docker engines only** (the lab runs docker with the containerd
image store). Pure-containerd/k3s enforcement is a follow-up — the kill mechanic
differs (task `Kill`+`Delete`, not `docker rm -f`), exactly like `Reconciler`
being docker-only today.

## Design at a glance

Three independent, composable pieces plus config:

1. **Verdict cache** (`internal/verify/cache.go`) — a bbolt store keyed by
   **content digest**, value `{Trusted, VerifiedAt, RefreshAfter, ExpiresAt,
   SourceRef, Mode}`. Modeled on `internal/retention/index.go` (bbolt `Open`) and
   the flat bucket in `internal/event/log.go`.
2. **Caching verifier** (`internal/verify/caching.go`) — a decorator that embeds
   the existing `verify.Service`. It answers digest hits offline from the cache
   and, on a live verify, writes the verdict. Only **definitive** answers are
   cached (see the "never cache unreachable" rule below).
3. **Local OCI-layout verify source** (`internal/verify/notation.go`) — an
   offline, file-based signature source for bootstrap/air-gapped images, verified
   against the **same** trust store. Uses notation-go's native
   `NewOCIRepository(path)` — no new dependency. Content/crypto-based, so it
   cannot be spoofed by naming (this replaces the role a name-based `exempt` would
   have played, without its bypass).
4. **Enforce manager** (`internal/enforce/`) — one goroutine per docker engine
   that subscribes to its own container-start stream, resolves each container to
   a content digest, checks the verdict in precedence (cache → local layout →
   live registry, all under the same Root CA) with `grace` on outage, and kills +
   removes on an untrusted verdict. Never touches gantry's own container
   (self-identity guard).

Plus: `serve.enforce`, `serve.verify.cache`, and `serve.verify.local_layout`
config; and `w`/`d` suffixes in the `Duration` type.

### Why a new capability, not a wider interface

`down.Engine` (`internal/down/down.go:55-88`) stays untouched. Enforcement is an
**optional capability discovered by type assertion**, exactly like `Verifier`,
`Collector`, `Reconciler` (`down.go:90-134`) with a matching `Caps.Enforce` entry
in `Capabilities()` (`down.go:145-150`). The existing `UsageSink func(ref, at)`
(`down.go:26-32`) deliberately drops the container ID, so enforcement cannot
reuse retention's `WatchUsage` stream — it needs a richer event carrying the
container ID, hence its own subscription.

### Data flow (docker)

```
enforce.Manager (1 goroutine per engine, mirrors retention unit.watch
                 manager.go:431-479)
  on (re)connect: ListRunning()  ── cold-reconcile already-running containers
                                     (needs container IDs; InUse drops them)
  WatchStarts()  ── second, independent docker /events stream
                    (clone of WatchUsage docker.go:347-366, but reads m.Actor.ID)
  per StartEvent:
    1. self-protect: is this gantry's OWN container? (self-inspect by id/hostname,
       NOT image name) -> always allow
    2. ResolveImage(id) -> ContainerInspect: .Config.Image, .Image,
                           .ImageManifestDescriptor.Digest, RepoDigests fallback
    3. reduce to a CONTENT DIGEST (top-level index digest — see risk below)
    4. verdict, in precedence order (all use the SAME trust store + Root CA):
         a. cache.Get(digest)          -> trusted & within hard TTL -> allow
         b. local OCI layout (offline) -> notation verify against on-disk layout
         c. live registry              -> referrer verify via caching Service
       result:
         trusted                       -> allow (+ write cache)
         untrusted                     -> RemoveContainer(force) + Engine.Remove(image)
         all sources unreachable       -> on_unavailable (grace|kill|allow)
```

There is **no image-name allowlist**. A name-based `exempt` is not a security
boundary — anyone who can start a container can `docker tag evil <exempt-pattern>`
and match it. Bootstrap / offline images are handled by the **local OCI layout**
(signed content, verified against the Root CA — unspoofable), and gantry protects
itself with a **self-identity interlock** (it never kills the container it runs
in, identified by self-inspection, not by image name — an attacker cannot make
their container *be* gantry's container).

Independent docker `/events` streams are fine — the daemon fans out to any number
of readers (`docker.go:353`); keeping enforcement's stream separate isolates its
failure domain from retention's.

## The two decided policies

- **`on_unavailable: grace`** (default): when no live verdict is obtainable and
  the cache has an **expired-but-known trusted** entry, honor it (allow). Only
  when there is *no* usable knowledge does it fall back (allow-and-log). `kill`
  = fail closed; `allow` = always allow on doubt.
- **`Duration` learns `w`/`d`**: so `4w`/`2w` are writable (today
  `time.ParseDuration` caps at `h`).
- **Event set = `start` + `restart`** (drop `unpause` — an unpaused container was
  already checked at start).
- **`ErrNotFound` is transient** — a vanished source reference (e.g. a deleted
  tag) is *not* cached as untrusted; it is re-probed next time. A deleted tag is
  not a bad signature, and caching it would risk a permanent kill from a
  transient registry state.

## Config schema

```yaml
serve:
  verify:
    mode: require                 # existing
    trust_store: /etc/gantry/ca   # existing; REQUIRED when enforce is on, even if mode: off
    local_layout: /etc/gantry/sig # OCI layout dir of pre-signed bootstrap images (offline
                                  # verify source); Enabled() == path != ""; verified against
                                  # trust_store, so unspoofable. Thin bundle: subject manifests
                                  # + signature manifests + signature blobs (no image layers).
    cache:
      path: /var/lib/gantry/verify-cache.db  # bbolt file; Enabled() == path != ""
                                             # must differ from every retention.path and events.path
      ttl: 4w                     # hard TTL (default 4w = 28*24h); unusable once now > VerifiedAt+ttl
      refresh: 2w                 # soft refresh (default 2w = 14*24h); sweeper revalidates past this
  enforce:
    mode: quarantine              # off | quarantine (default off); Enabled() == mode == quarantine
    stores: ["rt", "hday"]        # engine store names; each must exist AND IsEngine(); empty+quarantine = error
    on_unavailable: grace         # grace | kill | allow (default grace)
    self_container: ""            # optional: gantry's own container id/name (or via
                                  # GANTRY_SELF_ID env). Confirmed by ContainerInspect;
                                  # empty falls back to hostname, then /proc/self/cgroup.
    # No image-name allowlist by design (spoofable). gantry self-protects by
    # identity; bootstrap/offline images go in serve.verify.local_layout.
```

Cross-field invariants enforced in `Config.Evaluate` (`config.go:91-206`):
`refresh <= ttl`; `enforce: quarantine` requires `verify.cache.path` **and**
verify trust material (offline verdicts are the whole point); `cache.path` must
not collide with any retention/events bbolt path (extend the guard at
`config.go:147-156`).

## Workstreams / PR breakdown

| PR | Workstream | Depends | Files |
|----|-----------|---------|-------|
| PR1 | **Duration w/d parsing** | — | `cmd/config/duration.go` (+test) |
| PR2 | **Config structs + Evaluate** (`EnforceConfig`, `VerifyCacheConfig`, `local_layout`, `NeedVerifier()` gate) | PR1 | `cmd/config/serve.go`, `config.go` (+test) |
| PR3 | **bbolt verdict cache** | PR2 | `internal/verify/cache.go` (+test) |
| PR3b | **Local OCI-layout verify source** (offline notation verify) | PR2 | `internal/verify/notation.go` (+test) |
| PR4 | **Caching verifier decorator** | PR3 | `internal/verify/caching.go` (+test) |
| PR5 | **down.Enforcer capability + docker methods** | — (parallel to PR3/4) | `internal/down/down.go`, `docker.go` (+test) |
| PR6 | **Enforce manager** (`manager`/`decision`/`selfguard`) | PR4, PR3b, PR5 | `internal/enforce/*` (+test) |
| PR7 | **App wiring + refresh sweeper** | PR2,3,3b,4,6 | `internal/app/app.go`, `internal/verify/refresh.go` |

### PR1 — Duration `w`/`d` — ✅ landed
`duration.go` `UnmarshalText` runs `expandWeeksDays` before delegating to
`time.ParseDuration`: a byte-scanner walks number+unit tokens, rewriting `w`→168h
and `d`→24h (whole numbers scale as integers to stay exact; fractions via float),
and leaves every other token — standard units and a single leading sign —
verbatim. `time.ParseDuration` then sums the (possibly repeated) `h` tokens
(verified: `336h72h12h` → 420h). A string with no `w`/`d` is passed through
untouched so standard-input errors are unchanged; an unrecognized token defers to
`time.ParseDuration`'s error. `MarshalText` re-serializes as hours (unchanged).
Covers compounds (`2w3d12h`), decimals (`1.5w`), sign (`-2w`).
**(landed: added `EnforceConfig`/cache fields will use `4w`/`2w` defaults in PR2.)**

### PR2 — Config — ✅ landed
`EnforceConfig{Mode, Stores []string, OnUnavailable, SelfContainer string}` +
`Enabled()` (== `mode == "quarantine"`), added to `ServeConfig`. **No `Exempt`
field** — a name allowlist is not a security boundary (spoofable by `docker tag`);
bootstrap/offline images use `verify.local_layout` and gantry self-protects by
identity (`self_container`). `VerifyConfig` gains `LocalLayout string`
(`yaml:"local_layout"`) + `LocalLayoutEnabled()`, and `Cache VerifyCacheConfig`.
`VerifyCacheConfig{Path, TTL, Refresh Duration}` + `Enabled()`. `Config`
`NeedVerifier() = VerifyEnabled() || Serve.Enforce.Enabled()` gates the verifier
defaults/build (the `if c.VerifyEnabled()` block in `Evaluate` now keys off
`NeedVerifier`), so enforcement gets the verifier + trust store even when
admission `mode: off`. Validation is in `evaluateVerifyCache` (defaults 4w/2w;
`refresh <= ttl`; positive) and `evaluateEnforce` (mode ∈ off/quarantine always
validated; when on: `on_unavailable` ∈ grace/kill/allow default grace; ≥1 store,
each declared + `IsEngine()`; requires `verify.cache.path` and
`verify.trust_store`). The retention path-collision guard was generalized to a
**`claimBbolt` helper** covering every bbolt file (per-store retention indexes,
`serve.events.path`, `serve.verify.cache.path`) — error text is now `"… share
bbolt path …"`.
**(landed: `local_layout` existence/OCI-layout validation deferred to PR3b, where
the layout is actually opened; keeping fail-fast at the layer that reads it.)**

### PR3 — Cache — ✅ landed
`internal/verify/cache.go`: `Verdict{Digest, Trusted, Mode, SourceRef,
VerifiedAt, RefreshAfter, ExpiresAt}` (json tags) with `Expired(now)` /
`StaleForRefresh(now)` helpers. `Cache{db, ttl, refresh, now}` opened with
`OpenCache(path, ttl, refresh, ...CacheOption)` mirroring `retention.Open`
(`bolt.Open(path,0o600,{Timeout:3s})`, one flat bucket `verdict`). `Get` returns
`(Verdict, found, err)` and does **not** filter by expiry — callers apply policy
(grace honors an expired trusted verdict). `Put(digest, trusted, mode,
sourceRef)` stamps `VerifiedAt=now`, `RefreshAfter=now+refresh`,
`ExpiresAt=now+ttl` (overwrite renews — the refresh path). `ForEach`, `Count`
(`Stats().KeyN`), `Delete` (missing ok), `Close`, plus `TTL()`/`Refresh()`
accessors for the sweeper. `WithNow` for deterministic tests.
**(landed: named the constructor `OpenCache` — `verify.Open` would be ambiguous
in this package.)**

### PR3b — Local OCI-layout verify source — ✅ landed
notation-go supports this natively, **no new dependency**:
`notationregistry.NewOCIRepository(path, opts)` builds the same `Repository`
`notation.Verify` consumes, backed by an on-disk OCI layout
(`oras.land/oras-go/v2/content/oci.Store`). `notaryVerifier` gained a
`localLayout` field; the resolve→signature-gate→`notation.Verify` core was
extracted into `verifyAgainst(ctx, repo, mode, src)` so it runs against either a
live registry or the layout. `Verify` now tries `verifyLocalLayout` **first**: it
returns `(res, true)` **only** on a clean verified-trusted result — absent,
unsigned, unverifiable, or a broken layout all return `false` and fall through to
the registry, so the layout is strictly **additive** (it can grant trust, never
deny it). `newNotary` fails fast when `local_layout` lacks the `oci-layout`
marker (oras would silently create an empty layout for a wrong path).
**Verified empirically**: a signature referrer written to a layout is found by a
freshly-opened `NewOCIRepository`, and an offline verify succeeds with the source
registry host set unroutable (no network touched). Bundle is thin — subject
manifest + signature manifest + signature blob (no image layers needed).

### PR4 — Caching decorator
`type Caching struct{ verify.Service; cache *Cache; now }` (embeds `Service`, so
`Describe`/`Reload` delegate). Override `Verify(ctx, from, src)`:
- `src` is a `name.Digest` and a **hit within hard TTL** → return a reconstructed
  `Result{Mode: EffectiveMode(from), Digest: key}` **without** touching the
  registry (offline path).
- else call the embedded `Service.Verify` and **classify**:
  - `err==nil && res.Verified()` → `Put(res.Digest, Trusted=true, SourceRef=src)`.
  - `errors.Is(err, ErrUnsigned|ErrUntrusted)` **and** `src` is a digest →
    `Put(digest, Trusted=false)`.
  - anything else (`ErrNotFound`, unreachable, timeout, tag-src) → **pass through,
    do not cache** (the "never cache unreachable" rule — the `verify.Verifier`
    contract guarantees unreachable is a non-sentinel error).

The decorator's `Verify` stays **fail-closed** (preserving the copy-path and RPC
semantics). `grace` lives only in the enforce read path (PR6), which reads the
cache directly.

### PR5 — down.Enforcer
Add to `down.go` (next to the other capabilities): `StartEvent{ContainerID,
Image string; At time.Time}`, `ContainerImage{ConfigImage, ImageID,
ManifestDigest string; RepoDigests []string}`, and
```go
type Enforcer interface {
    WatchStarts(ctx, sink func(StartEvent)) error
    ListRunning(ctx) ([]StartEvent, error)
    ResolveImage(ctx, containerID string) (ContainerImage, error)
    RemoveContainer(ctx, containerID string, force bool) error
}
```
Add `Caps.Enforce` + `_, enforce := e.(Enforcer)`. Implement on `*dockerEngine`:
`WatchStarts` clones `WatchUsage` but reads `m.Actor.ID` (filter start+restart,
drop unpause); `ListRunning` = `ContainerList(All:false)` keeping `c.ID`;
`ResolveImage` = `ContainerInspect` (net-new client call) + `ImageInspect`
`RepoDigests` fallback; `RemoveContainer` = `ContainerRemove{Force:true}`,
`IsErrNotFound`→converged. Image removal reuses `Engine.Remove`
(`docker.go:368-396`) **without image force** (a shared image → `IsConflict`,
benign skip). containerd: unimplemented in v1 (documented follow-up).

### PR6 — Enforce manager
New package `internal/enforce`. `manager.go`: `Manager` holds `[]unit`, the
`*verify.Cache`, the caching `verify.Service` (for on-miss live verify — which
also consults the local layout via PR3b), the `on_unavailable` policy, the
self-identity guard, `now`, and a **`sync.WaitGroup`**. `StartWatchers(ctx)`
launches one **joined** goroutine per store mirroring `retention unit.watch`
(`manager.go:431-479`): cold-reconcile via `ListRunning` on connect,
`WatchStarts`, fixed-2s backoff + re-seed on stream end. The WaitGroup lets
`Stop()` **join before shutdown** so kills cease promptly on ctx-cancel (unlike
retention's fire-and-forget). Optional periodic reconcile tick (like retention
heartbeat `manager.go:360-385`) re-lists running containers so a soft-refresh
flip (trusted→untrusted) is acted on.
`decision.go`: self-guard → `ResolveImage` → pick content digest → verdict in
precedence (cache → local layout → live verify) → allow / kill+remove /
`applyOnUnavailable`. `selfguard.go`: identify gantry's OWN container so it is
never killed — resolve gantry's container id at startup by **self-inspection,
never image name**, and short-circuit any event whose container id matches.
Identity resolution, in priority order:
1. **Explicit** — `serve.enforce.self_container` (or a `GANTRY_SELF_ID` env the
   deployment injects); `ContainerInspect` confirms it. Most robust, no heuristics.
2. **Hostname → confirm** — `os.Hostname()` is the 12-char short id by default
   under docker; `ContainerInspect(hostname)` confirms. Works on any cgroup
   version, no mounts. Fails under `--hostname`/`--network=host` → fall back.
3. **`/proc/self/cgroup`** — best-effort only. No mount needed (docker mounts
   `/proc` in every container; `/proc/self` is the process reading itself), but on
   **cgroup v2 + cgroupns=private** (docker's default on modern hosts, i.e. the
   lab's v28+containerd runtime) the file is just `0::/` and carries no id. Useful
   only on cgroup v1 / cgroupns=host.

This is a **safety interlock, not a security boundary** — gantry's own image
should be signed and present in `local_layout`, so it passes verification anyway;
the guard only covers the misconfig case. If identity cannot be resolved, log
loudly and degrade conservatively (e.g. do not enforce on the store gantry runs
on) rather than risk self-termination. There is no image-name matcher.

### PR7 — App wiring + sweeper
In `app.go Build`: (1) change the verifier gate (`app.go:186`) from
`VerifyEnabled()` to `NeedVerifier()`. (2) if `verify.cache` enabled: `Open` the
cache next to `retention.Open` (`app.go:116`), append `Close` to closers
(`app.go:120`), wrap `v` with `verify.NewCaching(v, cache)` and pass the wrapper
to `wmr.SetVerifier` (`app.go:192`) so writer and reader **share one file**. (3)
if `enforce` enabled: iterate `Serve.Enforce.Stores`, resolve `stores.Engine`,
type-assert `down.Enforcer` (fail-fast if absent), build `enforce.Manager`,
append `Stop` to closers, `StartWatchers(ctx)` next to `gc.StartWatchers`
(`app.go:204-207`). (4) start the single refresh sweeper. Shutdown: enforce
`Stop` **joins its WaitGroup before** the cache bbolt file closes.

`refresh.go` (single goroutine — one shared cache, mirrors retention
`runScheduler`/`nextWake`/`poke` `manager.go:735-826`): periodically `ForEach`
the bucket, re-verify **only trusted** entries past `RefreshAfter` through the
caching `Service`; on a definitive answer `Put` a fresh verdict; on unreachable,
leave it untouched (grace). Next-wake = soonest `RefreshAfter`.

## Risks & mitigations

- **Digest-keying mismatch (highest)**: notation signs the **top-level
  index/manifest** digest, but `ContainerInspect.ImageManifestDescriptor.Digest`
  is a *platform-specific* manifest digest with no signature — keying on it makes
  every multi-arch image look unsigned. → Key on the top-level digest: prefer
  `ImageInspect(.Image).RepoDigests` (on containerd-store docker this is the
  pulled/index digest verify pinned at copy time); use `ManifestDigest` only as a
  cross-check. **Validate against a real signed multi-arch image in L3 before
  finalizing** `ResolveImage`.
- **Reconnect gap**: a container started during the 2s disconnect escapes the
  event kill. → cold-reconcile `ListRunning` on every (re)connect + optional
  periodic reconcile tick.
- **Unmappable images** (locally built, `docker load`, bare ID): no RepoDigest →
  guaranteed miss. → route to `on_unavailable` deliberately, never crash; under
  `grace`, allow-and-log. The *correct* fix for an image that must run offline is
  to sign it (even with the local CA) and drop it in `verify.local_layout` — not a
  name allowlist.
- **Name-based bypass** (why there is no `exempt`): an image ref is
  attacker-chosen, so any name allowlist is defeated by `docker tag evil
  <pattern>`. → enforcement is keyed on **content** (digest + signature + Root CA)
  only; the one name-independent exemption is gantry's **self-identity guard**
  (an attacker cannot make their container be gantry's own container).
- **Shared-image deletion**: force-removing the image could evict content a
  legit sibling uses. → remove the *container* first (frees the ref), then
  `Engine.Remove` without image-force (`IsConflict` → skip).
- **Trust rotation staleness**: a cached `trusted` verdict is digest-only, so a
  CA revocation via `Swappable.Reload` (`swappable.go:106-115`) isn't reflected
  until soft-refresh (2w) / hard TTL (4w). → stamp a trust-material fingerprint
  (`Description.Anchors`) into the verdict and/or bump a cache epoch on `Reload`;
  document the window. Post-v1 refinement.
- **Grace without a cache** is meaningless. → `Evaluate` rejects
  `quarantine` without `verify.cache.path`.
- **Shutdown kills**: fire-and-forget watchers would keep killing during
  shutdown. → WaitGroup + `Stop()` joins before closing the cache.

## Test plan

- **Unit**: duration parsing (`4w`,`2w`,`1d`,`2w3d12h`,`1.5w`, standard strings
  unchanged, malformed error); `Evaluate` rejections (mode/on_unavailable typos,
  non-engine/unknown store, missing `local_layout` dir, `refresh>ttl`, path
  collision) +
  defaults + `NeedVerifier`; cache round-trip + expiry boundaries (`WithNow`);
  decorator (cached hit skips inner verifier via spy, caches trusted, caches
  untrusted only for digest-src reject, never caches `ErrNotFound`/unreachable);
  **local-layout verify** (a digest present+signed in an on-disk OCI layout
  verifies offline with no registry; absent digest falls through to remote);
  docker Enforcer methods via fake client; **self-guard** (gantry's own container
  id is never killed); manager decisions (trusted→allow, untrusted→remove+
  image-remove in order, grace honors expired-trusted / kill fails closed,
  idempotent replays, cold-reconcile).
- **E2E image tier** (`internal/e2e`): extend the fake engine
  (`internal/e2e/engine.go`) to optionally implement `down.Enforcer`; drive
  start→quarantine deterministically; assert unsigned removed, trusted survives,
  grace keeps a known-trusted image alive when the source is stubbed unreachable.
- **E2E L3-infra tier** (real docker + containerd image store, TLS): warm a
  signed and an unsigned image through gantry, `docker run` both, assert the
  unsigned is `rm -f`'d + pruned while the signed survives; kill the source
  registry and assert grace keeps the signed container alive on a cache hit. **This
  tier confirms the digest-keying end-to-end.**

## Open decisions (surface before/while implementing)

1. **Which digest does notation sign** in this deployment (index vs platform),
   and does `RepoDigests` on the lab's containerd-store docker v28 return that
   same top-level digest a `docker run` reports? The whole cache key rests on
   this — validate against a real signed multi-arch image.
2. **Cache miss with no provenance** (image gantry never moved / locally built /
   `as`-warmed): allow-through under grace, live-probe every oci store, or require
   the daemon ref to name a configured store? Recommended: derive the oci store
   from the RepoDigest host and live-verify there, else `on_unavailable`.
3. **Verdict bucket flat vs per-store**: can a digest's trust answer differ across
   source registries? Recommended flat + `SourceRef` in the value for v1.
4. **Soft-refresh flip → immediate re-quarantine** of running containers, or only
   on next start / periodic reconcile? Recommended: periodic reconcile tick
   (keeps the refresh loop decoupled).
5. ~~**Event set**~~ — **DECIDED: `start` + `restart`** (drop unpause).
6. ~~**`ErrNotFound` caching**~~ — **DECIDED: transient** (do not cache).
7. ~~**Bootstrap / exemption**~~ — **DECIDED: no image-name `exempt`.** Offline
   images use `serve.verify.local_layout` (signed content, unspoofable); gantry
   self-protects by container identity resolved as **explicit `self_container`/
   `GANTRY_SELF_ID` → hostname→`ContainerInspect` → `/proc/self/cgroup`
   (best-effort)**. Note `/proc/self/cgroup` needs no mount but yields only `0::/`
   under cgroup v2 + cgroupns (the lab runtime), so explicit/hostname carry it;
   the guard is safety-only (gantry's own image should be signed + in
   `local_layout`).
8. **containerd/k3s parity** deferred (task `Kill`+`Delete`, digest from
   `ImageService().Get().Target`) — docker-only first.
