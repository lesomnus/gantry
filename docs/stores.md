# Stores: registries and engines

A **store** is where images live or land. gantry moves an image from a source
registry into a target store; the target is either another registry (gantry
copies the blobs in) or an engine daemon (the daemon is told to pull). This doc
covers the store kinds, the two OCI fill modes, reference rewriting, the
`downstream_host`/`pull_host` host substitution, outbound TLS (private-CA
verification and TPM-sealed client mTLS), caller-chosen `as` names, digest
pinning, verbatim registry commits, and the full per-store configuration
reference.

Stores are declared under the top-level `stores` map in `../gantry.yaml`, keyed
by name. See `retention.md` for per-store image GC, `verification.md` for
signature verification, `api.md` for the move/pull RPCs, and `observability.md`
for the health probe that reports store reachability.

## Store kinds

Every store sets `kind`. There are three:

- **`oci`** — an OCI distribution registry gantry reads blobs from and writes
  blobs to (`StoreConfig.IsRegistry`). It can be a job **source** (gantry pulls
  it), a copy **target** (gantry pushes into it), or both. Reported capabilities
  are `read` + `write`.
- **`docker`** — a docker daemon gantry triggers to pull (`IsEngine`). Target
  only. Reported capabilities are `pull`, `gc`, and `reconcile` (inventory scans
  / untagged reaping).
- **`containerd`** — a containerd daemon gantry triggers to pull. Target only.
  Reported capabilities are `pull` and `gc`; it has no `reconcile` capability —
  gantry drops the pull-created digest record after retagging, so containerd's
  own GC reclaims replaced content (see `retention.md`).

`docker` and `containerd` are collectively **engine** stores. A job's `source`
must resolve to a registry; its `target` may be any declared store. The move
dispatches on what the target *can do* — a registry is a "pusher", an engine is
a "puller" — not on its declared kind, so the same job shape (`{ref, source,
target}`) works for both.

The store map's order is not significant; gantry sorts names for stable display.
For an `oci` store, `host` defaults to the store name when omitted (a store named
`docker.io` reaches `docker.io`), and `mode` defaults to `copy`.

### Resolving a store name

`source`/`target` may name a declared store, or — when
`serve.allow_unknown_stores` is set — a bare registry host. A bare host that
matches a declared registry's `host` resolves to that store even when
`allow_unknown_stores` is `false`. An undeclared bare host (with the flag on) is
synthesized as an `oci` copy-mode store. **Engine
stores must always be declared** — a bare host never resolves to a docker /
containerd daemon. Naming an engine where a registry is required (or vice versa)
is an error (`store %q is a %s, not a registry`).

Registry readiness is reported from config (always `ready`, never probed — a
remote like `docker.io` need not be reachable *from* gantry). Engine readiness
is a live daemon probe. See `observability.md` for the cached `StoreService.Health`
probe.

## OCI fill modes: copy vs proxy

An `oci` copy target sets `mode` to one of two values (`copy` is the default):

- **`copy`** — gantry pulls each blob from the source registry and pushes it into
  the target, **skipping blobs the target already has** (content-addressed, so
  progress reflects bytes actually moved). It then commits the manifest: a single
  image is pushed byte-for-byte; a multi-platform index is rebuilt to reference
  **only the copied platforms**, so unwanted architectures are not pulled into
  the cache — unless the commit is `verbatim` (see below).
- **`proxy`** — gantry reads the image *through* the target so a pull-through
  cache fetches and persists it from upstream itself. The manifest is resolved
  against the cache and every blob is read to EOF (a `HEAD` or partial read would
  leave the cache cold); the commit is a no-op and the committed digest is
  unknown (the cache resolves the tag itself).

`proxy` mode carries two hard constraints the plan enforces at admission:

- A **verifying** job refuses a `proxy` destination: the proxy reads through by
  tag and ignores the pinned source digest, so it could fill from a different
  (unverified) image if the tag moves after verification (`signature
  verification requires a copy-mode destination`). See `verification.md`.
- `copy_referrers` requires `copy` mode — a proxy cache never learns the digest
  to anchor the referrer artifacts to.

A `mode` other than `copy`/`proxy` is rejected at config load.

### Credentials

Registry credentials are **per store**: `username`/`password` (env-expanded, so
`${VAR}` works) authenticate operations against *that* store — a copy-mode push
into it, or a proxy-mode read-through pull. Set them on the source store to
authenticate the upstream pull, and on the target store to authenticate the
push. When a store sets no `username`, its requests fall back to the docker
keychain (`~/.docker/config.json`). The cache store's own credentials therefore
do **not** authenticate the upstream `source` pull in copy mode — that pull uses
the source store's credentials (or the keychain).

## Cache-side reference

When a registry is a copy **target**, gantry derives the cache-side reference
from the source reference by substituting **only the host**: the source
repository path and tag/digest are preserved under the target store's own
`host`. So `docker.io/library/redis:7` copied into a store whose `host` is
`cache.local:5000` lands as `cache.local:5000/library/redis:7`; a digest
reference keeps its `@sha256:…`.

A copy destination is, by construction, this store's own host, so the mapping is
fixed — there is nothing to configure. The repository path is never rewritten,
which keeps `source → cache` a deterministic, one-to-one mapping (so a later
pull resolves to the same place and duplicate copies coalesce).

The same host-only substitution applies to an **engine** target, except the
engine is *told* to pull the reference rather than having blobs pushed into it;
which host it reaches is set by `downstream_host`/`pull_host` (below).

## downstream_host and pull_host

An engine is told to pull the source registry's reference by host. Two knobs
override which host it reaches, since gantry may push to one name (e.g. an IP)
while daemons must pull another (a trusted DNS name):

- **`downstream_host`** — set on a **registry** store (the cache); overrides the
  host engines are told to pull from when pulling *out of* this registry.
- **`pull_host`** — set on an **engine** store; overrides the source registry's
  host for that engine, and **takes precedence over the source registry's
  `downstream_host`**.

Resolution order for the pull reference's host: the engine's `pull_host`, else
the source registry's `downstream_host`, else the source registry's own `host`
(no substitution). Only the host is rewritten; the repository path and tag/digest
are preserved.

## Outbound TLS: private-CA verification and client mTLS

Any store — registry or engine — may carry outbound TLS settings. These build a
single per-store transport (`internal/xport`), memoized so the same store config
yields one pooled connection set (and one open TPM device) across every job,
layer, verification, referrer copy, and health probe; only successful builds are
cached, so a transient failure (device busy, cert mid-rotation) is retried on the
next call rather than poisoning the store until restart.

### ca_cert (server verification)

`ca_cert` is a PEM file of CA certificate(s) used to verify the registry / token
server (a private CA). It is **usable on its own**, with or without a client
certificate. Empty falls back to the system roots — or is skipped entirely when
`insecure` is set. `insecure` allows plain-HTTP or self-signed registries (skip
verification); plain HTTP itself is driven by `name.Insecure` on the reference.

### Client mTLS (`cred`)

`cred` is the client credential gantry presents to the store. Like a store it
carries a `kind`, which selects where the private key lives; `cred.cert` — the
client certificate (leaf + chain), PEM — is common to every kind, and its public
key **must match** the private key, or the transport build fails with a clear
config error rather than an opaque handshake failure at pull time. Unlike a
store, a field belonging to another kind is **rejected** rather than ignored: a
stray `key` or `handle` usually means the wrong kind was picked.

**kind `tpm`** — the key is sealed in a TPM and never leaves the device; gantry
signs the TLS handshake inside it:

```yaml
cred:
  kind: "tpm"
  handle: "0x81000001"            # persistent handle of the client key
  cert: "/etc/gantry/device.crt"  # client cert (leaf + chain), PEM
  # device: "/dev/tpmrm0"         # default
```

- **`handle`** — the persistent handle of the client signing key, as hex
  (`0x81000001`) or decimal; the value must fit in a uint32. gantry does **not**
  create keys — the handle must reference a key already provisioned in the TPM.
- **`device`** — the TPM device path. Defaults to `/dev/tpmrm0` (the
  resource-manager device, which multiplexes access) when omitted.

**ECC keys only** — the signer supports NIST P-256 / P-384 / P-521; any other TPM
key type is rejected. Signing runs `TPM2_Sign` inside the device and returns the
ASN.1 DER ECDSA signature `crypto/tls` expects, so the private key material is
never present in process memory. Access to a single TPM connection is serialized
(one file descriptor, no internal locking), so parallel TLS handshakes signing
with the same key are safe.

**kind `file`** — an ordinary PEM key pair on disk:

```yaml
cred:
  kind: "file"
  cert: "/etc/gantry/client.crt"  # client cert (leaf + chain), PEM
  key: "/etc/gantry/client.key"   # private key, PEM
```

- **`key`** — the private key, PEM. PKCS#8 (`PRIVATE KEY`), SEC1
  (`EC PRIVATE KEY`), and PKCS#1 (`RSA PRIVATE KEY`) blocks are accepted;
  encrypted keys are not. A combined file (certificate + key blocks in one PEM)
  works as `key` — non-key blocks are skipped.

Both kinds share the exact transport wiring (same chain assembly, key-match
check, `ca_cert` verification, memoization) — the only difference is where the
private key lives and who signs.

For an `oci` registry, a cred applies to **every** outbound direction — pull,
push, referrer copy — including the bearer-token endpoint. For a **docker**
engine it is the client certificate presented to the daemon's TLS port (tcp
mTLS); the docker client detects the transport's TLS config and dials the daemon
over `https`. (A containerd engine reaches its daemon over a local socket and
does not use this.)

Validation and lifecycle:

- `cred.kind` and `cred.cert` are always required; `kind: tpm` additionally
  requires `handle`, `kind: file` requires `key`. `ca_cert` is intentionally a
  sibling of `cred`, not part of it: it configures server verification and is
  valid on its own.
- `insecure` **cannot** be combined with a cred: `insecure` enables plain HTTP
  on the oras path, which would silently drop the client certificate. To trust
  a self-signed mTLS server, set `ca_cert` instead.
- The device, certificate, and key files are validated lazily on first use, so a
  missing TPM or key file does not block startup for stores that do not use it.
  Devices are released at server shutdown.

## Caller-chosen `as` names

For an **engine** target, `as` records the pulled image under caller-chosen names
instead of the pull reference — so a cache-fed node keeps the upstream name
(`docker.io/library/redis:7`) even though gantry had it pull from the cache.
`as` is **engine targets only**; supplying it for a registry target is an error
(``as` names the image on an engine; store %q is a registry``).

`as` strings are kept **verbatim** — containerd resolves image names by exact
match, so normalizing (`docker.io` → `index.docker.io`) would break kubelet
lookups. `as` participates in the coalescing key `(ref, platforms, source,
target, as, fallback_to_origin)`, so two submits differing only in `as` are
distinct moves.

An `as` entry is a tag reference on any engine. A **digest** reference
(`repo@sha256:…`) is also allowed, under the conditions below.

## Digest-pinned jobs and digest `as` names

A job is **digest-pinned** when the job reference is a digest ref, or when
signature verification pinned the source to a digest (see `verification.md`). A
digest-pinned engine pull is **anchored**: the daemon pulls `repo@digest` and
then records the tag/`as` names over that exact content, so a mutable tag
re-resolved by a pull-through cache cannot substitute different bytes.

A digest `as` name must **carry the job's pinned digest** and requires a
digest-pinned job — anything else would register a reference to content that is
not that digest. The plan rejects a mismatched or unpinned digest `as` name
(``as` name %q does not carry the job's pinned digest``, and `digest `as` names
require a digest-pinned job`).

The anchor manifest's raw bytes back the digest name. gantry fetches them from
the store the attempt pulls from — normally the job's **source** (the cache), so
the origin registry is **never contacted**; the one exception is a
`fallback_to_origin` attempt, which fetches the anchor from the origin along with
the content (see [Falling back to the origin](#falling-back-to-the-origin)).
Either way the bytes are hashed against the reference's digest (sha256 only)
rather than trusting the transport, because they are about to be registered on a
node under that digest's name. The fetch happens **before** the pull, so a source
that cannot resolve the digest fails the attempt before any bytes move; the
engine registers the names only **after** its pull succeeds, so a name never
resolves to absent content.

Digest names require the **containerd image store**:

- On a **`containerd`** engine, the name is created natively via the image
  service.
- On a **`docker`** engine, gantry forges the `RepoDigest` by streaming a thin
  OCI archive (the anchor manifest under the requested names) into the daemon's
  `/images/load` over the content the pull just placed — no registry contact.
  This works only when the daemon runs the containerd image store
  (`driver-type = io.containerd.snapshotter.v1`). A **classic graph store cannot
  represent a digest reference** without a real registry pull, so a digest `as`
  name against a classic-store docker is **rejected before the pull** (the job
  fails with a clear error) rather than silently dropped — a caller that asked
  for a digest name would otherwise have a node quietly pull through to the
  origin later. Tag `as` names are unaffected and work on either store.

The retention index is stamped with the names the daemon **actually** holds (as
reported back by the engine), never with a name it does not resolve (see
`retention.md`).

The net effect: a jobspec pinned to `repo@sha256:INDEX` resolves locally after
the move — `docker image inspect` hits, and a `force_pull=false` deployment pulls
nothing.

## Routing a copy through a cache

A registry can declare that another one holds copies of its content:

```yaml
stores:
  "cr.example.com":
    kind: oci
    cache: "site"        # reading from me may go through this store

  site:
    kind: oci
    host: registry.corp.internal
```

gantry may then satisfy `local ◀── cr.example.com` by going through `site`, so the
cloud registry is read **once** rather than once per destination. The caller neither
names `site` nor sees a different result — this is gantry's own cost optimization,
not a change to what the job means.

`cache` is declared on the store being cached: once per origin rather than repeated
per destination, and where the cost being avoided actually lives. It must name a
declared registry, cannot name the store itself, and is rejected on an engine store
(an engine is never a job's source).

### One job, two hops

A routed move is **one job** whose `transfers` are its hops, distinguished by
`Transfer.step`:

```
Add {ref: "cr.example.com/app:1", source: "cr.example.com", target: local}

  transfers[0]  step 0   site  ◀── cr.example.com   done      ← the fill
  transfers[1]  step 1   local ◀── site             done      ← the delivery
  job           done     source: cr.example.com  target: local
```

One job id, one `Watch` stream carrying both hops' per-layer progress, ordering
guaranteed by construction. The job's own `source`/`target` stay what the caller
asked for; the hops say what happened.

### The authority settles the tag

Before routing, gantry asks the **source** — the authority — what the reference
means right now, and anchors every hop to the digest it answers with. That is one
manifest request against bytes that would otherwise move in full, and it is what
makes a nearer copy provably the *same content* rather than merely the same tag.

The cache is then probed for that digest, which decides the shape:

| probe | plan |
| ----- | ---- |
| the cache holds it | one hop: `target ◀── [cache, source]` |
| it does not | two hops: `cache ◀── source`, then `target ◀── [cache, source]` |

Either way the source the caller named stays in the list, so **a route that does
not work is not a failure** — it costs one abandoned attempt and the direct copy
runs. That is why there is no switch for "may gantry write to the cache": the
answer is whether it works.

### The fill copies everything, verbatim

The fill hop commits the authority's manifest **byte for byte** and copies **every
platform**, whatever the job asked for. Both are required rather than chosen:

- A rebuilt (platform-filtered) index is a *different digest for the same tag*, so
  the cache would never satisfy the probe and would never be read.
- A verbatim commit references every child manifest, and a registry rejects an
  index whose children are missing.

So a narrowed routed copy still fills the cache completely. The caller's own hop
keeps the narrowing; only the shared cache is filled whole, which for a shared
cache is the desirable trade.

The fill lands under the **tag**, so both the tag and the authority's digest resolve
from the cache afterwards — the digest is what the next job probes for.

### When it does not route

None of these is a misconfiguration; an operator declares the cache once and jobs
that touch either end of the route are ordinary:

- The job's **target is the cache** — filling it already *is* the requested copy.
- The job's **source is the cache** — nothing to route through.
- The cache is **`mode: proxy`** — reading a pull-through cache is what fills it, so
  routing collapses to a single hop sourced at the cache: no fill hop, no probe, no
  write access needed. (gantry never *pushes* into a proxy store: that would read
  the whole image and commit nothing.)
- The cache **cannot be probed** — without an answer there is nothing to decide
  with, so the job runs unrouted.
- An engine target whose **`pull_host`** collapses every source onto one host, so
  the daemon would read the same place twice.

A hop gantry generates is never itself routed, even if its own source declares a
cache. One level, so no graph is walked and no cycle can form.

### When the authority cannot be reached

Then there is no digest and no probe, and gantry reads the cache **by tag** — the
site registry keeps working while the cloud one does not. It is also the only case
where a caller can receive content the authority never confirmed, so it is opt-out:

```
require_authority: bool     # per job; server default worker.require_authority
```

`true` refuses such a job instead. It is a no-op for a job that is not routed, since
there the source the caller named *is* the authority.

### Consequences worth knowing

- **A routed job moves the image twice**, so `gantry.bytes` and the `job_done`
  audit record report roughly twice its size. That is honest — the bytes did move —
  but worth knowing when reading a routed job's numbers.
- **`gantry.job.fallback` counts a route being abandoned as well as a source
  fallback**, distinguished by its `reason` attribute, which names what was GIVEN
  UP: `route` is gantry's own cache being abandoned, `planned` is the source the
  caller named being left (i.e. a fallback to the origin). A route that fails at
  run time shows up there rather than as failing jobs. A route gantry declines at
  admission does **not** — it is logged, and visible in `JobService.Plan`, but
  there is no counter for it.
- **An abandoned route's already-pushed blobs are left in the cache.** They are
  content-addressed, so the next attempt and the direct copy both reuse them; a
  registry with aggressive unreferenced-blob GC turns an abandonment into a
  re-transfer.
- **`JobService.Plan` reports the resolved route** (`steps`), so the shape is
  visible before submitting. It is advisory: coalescing is request-level, so a
  submit can be served by an active job that probed differently.
- **A routed engine job leaves the node holding the CACHE's host name**, because
  the daemon was told to pull from there and retention is stamped with what the
  daemon actually holds. A job that named the origin as its source and expected the
  origin's name on the node will not get it, and host-qualified retention rules
  written for the origin will not match the image. Use `as` to give it a name
  independent of which store served it — the same remedy as for
  [the source fallback](#falling-back-to-the-origin).
- **Referrers travel on every hop, and a fill always carries them.** A fill hop
  copies the authority's referrer artifacts whatever the job asked for — the same
  rule as `verbatim` and `platforms`, and for the same reason: what gantry puts in
  a shared cache is read later by jobs that asked for something else. An engine
  target cannot ask for referrer propagation at all (a daemon pull has no referrer
  transport) and needs it anyway, because `serve.enforce` re-verifies what a node
  holds against the store whose host the daemon recorded — which, on a routed job,
  is the cache.
- **A warm cache is judged against the authority, not against zero.** Before
  reading a cache instead of the authority, gantry checks that it holds at least as
  many referrers as the authority does for that digest; a deficient one is declined
  and the authority is read, so a signature is never silently dropped. Asking
  "does it have any" would get this wrong in both directions — a cache with one of
  three signatures would read as complete, and an image that legitimately has none
  would read as deficient, which would leave routing permanently inert for every
  unsigned image. The check costs one referrer listing at the authority, plus one
  at the cache when the authority actually has some. It runs for a job that
  propagates referrers, and for every engine-target job.
- **An unconfirmed cache is never read by a job that needs referrers.** With no
  digest there is nothing to check the cache against, so a job propagating
  referrers — or any engine-target job — is simply not routed when the authority
  cannot confirm the reference, rather than reading the cache on faith.
- **A pull-through cache is not routed through by a job that needs referrers.**
  Reading a proxy is what fills it with the *image*; whether it also proxies the
  referrers API is the upstream product's business, and there is no fill hop to
  carry them instead.
- **Admission does two registry requests** (settle the tag at the authority, probe
  the cache), bounded together by `worker.admission_timeout` (default `10s`), plus
  the referrer listings above when the job needs them.
- **A fill already in flight is not duplicated.** The probe cannot see one — a fill
  that has not committed yet has published nothing — so gantry also asks its own
  job store. A second cold job for the same image therefore plans no fill of its
  own and reads the cache instead. Whether it *waits* for that fill is
  `worker.source_wait`: at the default `0` it reads the cache, misses, and falls
  through to the source the caller named, which costs what not routing would have
  cost. **Set `worker.source_wait` if you submit many destinations for a cold image
  at once** — that is what collapses the burst onto one authority read.
- **Routing does not re-anchor the origin fallback.** A job that falls back to the
  registry named in its own `ref` resolves the tag *there*, at the tag's own
  authority, and declaring a `cache:` on its source does not change that. Otherwise
  a source holding a rebuilt (platform-narrowed) index would pin the fallback to a
  digest the origin never had, and the fallback would fail on exactly the jobs it
  exists to rescue.
- **A target that refuses the write is not a source fault.** When the caller's own
  target rejects the image, gantry does not answer by re-reading it from another
  source: the second attempt would move every byte again and be refused
  identically. Such a failure is not counted against the cache in
  `gantry.job.fallback`.

## Falling back to the origin

The `remote → cache → engine` flow is two jobs, and nothing links them: there is
no dependency edge and **no ordering guarantee** between a cache-fill job and an
engine pull that reads from that cache. From the pull's side, a cache that is
empty because its fill job failed, has not run yet, or is unreachable is one
indistinguishable fact — *this source cannot serve the image* — and by default
that fails the job.

`fallback_to_origin` (engine targets only) makes the pull re-attempt against the
registry named in the job's own `ref` instead:

```
JobService.Add {ref: "cr.example.com/app:1", source: cache, target: node,
                fallback_to_origin: true}

   attempt 1  node ◀── cache            failed   transfers[0]
   attempt 2  node ◀── cr.example.com   done     transfers[1]     job: DONE
```

`source` is only ever an *override* of the ref's own registry, so the fallback
binding needs no new input — it is the binding the job would have had with
`source` unset. Each attempt is its own `transfers` entry, so the failed one
stays on the record (with its error) while the job itself completes: **a cache
miss reads as a miss, not an outage.** The job's own `source`/`target` stay what
was asked for — see [api.md](api.md#what-a-jobs-source--target-mean) — and the
attempt rows say where the bytes came from.

Absent from the request, the value is the server default
`worker.fallback_to_origin` (default `false` — a deployment that has not opted in
behaves exactly as before). What a job *reports* is the **effective** decision:
false when it has no second source to reach, whatever the request said. That is
also what enters the coalescing key, so two jobs that provably behave the same
still collapse onto one move.

**What the fallback does and does not guarantee**

- A **digest-pinned** job (a digest ref, or a verified source) falls back to that
  same digest: the daemon is asked for `origin/repo@sha256:…`, so the origin
  cannot serve different bytes than the cache would have. The admission-time
  `verification` record still describes exactly what was pulled.
- An **unpinned tag** job resolves its tag at the origin, which is that tag's
  authority — if the result differs from what the cache held, the cache was
  stale. No signature claim is made either way, because the job never made one.
- Verification is **not** re-run against the origin. It would add nothing to a
  pinned job (the content is digest-identical) and cannot apply to an unpinned
  one.

**When it does not apply**

- A **registry** target — its `source` is normally the origin already
  (`fallback_to_origin applies to an engine target`).
- An engine with **`pull_host`** set, or a `downstream_host` shared by both
  stores: both sources then resolve to the same pull ref, so the second attempt
  would re-pull from the same place (`fallback to origin %q is not addressable
  from engine %q`).
- A job whose `source` already **is** the origin — a normal shape, not an error.
- An origin that cannot be resolved as a store (a bare host with
  `allow_unknown_stores` off — note that a repository-only `ref` resolves its
  origin to `index.docker.io`).

A job whose `source` already is the origin is never an error — a client that
always sets the flag hits that shape on every direct job. The `pull_host`
collision and the unresolvable origin are errors only when the request set
`fallback_to_origin` **explicitly**; inherited from the server default they
simply do not apply to that job, because a blanket default must not start
failing every job whose ref names an origin nobody declared. `JobService.Plan` reports the effective decision and the
`fallback_ref` the engine would be told to pull, so the difference is visible
before submitting.

**Waiting out a fill that is still running**

A cache that is empty *because its fill job has not finished yet* is a different
situation from one that cannot serve the image at all — but the pull cannot tell
them apart from a single failed attempt. `worker.source_wait` (default `0` =
off) lets a missed pull wait for an active job that is putting **exactly this
image** into **exactly this store**, then try that source once more:

```
attempt 1  node ◀── cache    failed        transfers[0]
           (a cache-fill job for this image is running — wait for it)
attempt 2  node ◀── cache    done          transfers[1]     job: DONE
```

The two jobs are joined on an exact string: the ref a registry-target job puts
into its store is the same ref an engine pull reads out of it. No heuristics, no
dependency graph, and **nothing is declared up front** — the wait happens only
after a real miss, so a cache that can already serve the image is never delayed
by an unrelated re-warm job running beside it.

The wait is bounded by `source_wait` and takes one of a limited number of slots
— `max_concurrent_jobs - 1`, but never fewer than one — so a pool of two or more
always keeps a worker free to run the fills themselves. A pull that cannot take a
slot, or whose wait expires, goes straight on to the fallback. (With
`max_concurrent_jobs: 1` the pipeline is serial by construction: nothing can be
filling the cache while the pull runs, so a wait there only spends its bound
before falling back. Leave `source_wait` at `0` on a single-worker server.) If the fill *failed*, the retry costs one cheap
miss and the fallback follows — only the source itself can say whether it now
holds the image.

Waiting is independent of `fallback_to_origin`: with `source_wait` set and the
fallback off, a missed pull still waits for the fill and then either succeeds or
fails, never leaving the source it was given.

**Operational consequences worth knowing**

- A pull that failed over is **not retried** for anything the *engine* itself
  could not do — a capability the daemon lacks, or a step after the content
  already arrived. Those fail identically wherever the bytes come from. Every
  other failure, including a platform this cache's copy of the index lacks, is
  treated as a property of the source.
- **The node ends up holding the origin-host name**, and the retention index is
  stamped with what the daemon actually holds. Retention rules are patterns over
  host-qualified repositories, so a rule written for the cache host will not
  match an image the fallback delivered — it lands as `unmanaged` and is never
  collected. Use `as` to give the image a stable name independent of which
  attempt won.
- The daemon, not gantry, authenticates the pull. A node with no credentials for
  the origin fails the fallback attempt; the job's error carries both attempts'
  errors.
- Falling back does **not** fill the cache. The next pull misses again. Watch
  `gantry.job.fallback` (labelled `from`/`to`): a cache quietly not being used
  looks like success everywhere else.

## Verbatim digest-ref registry copies

When a **registry** target's rewritten cache reference is itself a **digest**
reference, the cache ref keeps the source digest — so a copy-mode commit must
preserve the source manifest / index **byte-for-byte**. A rebuilt
(platform-filtered) index would have a different digest and the registry would
reject the put. gantry marks such a copy `verbatim`: it pushes every child
manifest first (a registry rejects an index whose children are missing —
children outside the copied platforms, e.g. attestation entries, upload their
blobs here), then the raw index bytes under the cache tag, preserving the source
digest. The same digest then resolves from the cache.

A verbatim commit copies the whole image, so it **refuses platform narrowing** —
`platforms` must be empty (`a digest-pinned copy preserves the source image
verbatim (all platforms); omit platforms`). `copy_referrers` also forces a
verbatim commit for the same reason (the source digest must survive so the
signatures still verify); see `verification.md`. Proxy mode is exempt from the
digest-preservation rule — it commits nothing, resolving the digest through the
cache itself.

## Configuration reference

Full annotated example: `../gantry.yaml`. Stores are the top-level `stores` map,
keyed by name; the key is the store name (and, for `oci`, the default `host`).

### Common

| Key | Applies to | Meaning |
|---|---|---|
| `kind` | all | `oci` \| `docker` \| `containerd`. Required. |
| `cred` | all | Client-mTLS credential block; omit for no client certificate. |
| `cred.kind` | all | `tpm` (key sealed in a TPM) \| `file` (PEM key pair on disk). Required. |
| `cred.cert` | all | Client certificate (leaf + chain), PEM; public key must match the private key. Required. |
| `cred.handle` | all | `tpm` — persistent handle of the client key (hex or decimal, uint32). Required. |
| `cred.device` | all | `tpm` — TPM device path. Default `/dev/tpmrm0`. |
| `cred.key` | all | `file` — private key, PEM (PKCS#8 / SEC1 EC / PKCS#1 RSA; unencrypted). Required. |
| `ca_cert` | all | PEM CA(s) to verify the server. Usable on its own. Empty = system roots (skipped when `insecure`). |

### `oci` registry

| Key | Meaning |
|---|---|
| `host` | Registry host. Copy target: the cache-side ref is the source repo/tag under this host. Defaults to the store name. |
| `mode` | `copy` (default; push blobs) \| `proxy` (read-through self-fill). |
| `insecure` | Allow plain-HTTP / self-signed (skip TLS verification). |
| `username` / `password` | Credentials for operations against **this** store (env-expanded). Empty = docker keychain. |
| `downstream_host` | Host engines are told to pull from when pulling out of this registry (overridden per-engine by `pull_host`). |
| `verify` | Per-source-registry override of `serve.verify.mode` (see `verification.md`). |

### `docker` / `containerd` engine

| Key | Meaning |
|---|---|
| `address` | Daemon endpoint. docker: a socket path or `proto://host` (default `unix:///var/run/docker.sock`). containerd: the socket, e.g. `/run/k3s/containerd/containerd.sock`. |
| `namespace` | **containerd only** — the containerd namespace (e.g. `k8s.io` for k3s). Defaults to `default` when omitted. The docker engine ignores this field. |
| `pull_host` | Override the registry host this engine is told to pull from; takes precedence over the source registry's `downstream_host`. |
| `retention` | Per-store image GC (engine stores only). See `retention.md`. |

Two structural constraints checked at config load: two stores must not share a
`retention.path` (bbolt takes an exclusive lock), and two `docker` stores must
not run untagged reaping against the same daemon address (their reap clocks would
fight). Both surface as clear startup errors.
