# Runtime signature enforcement (quarantine)

Where [signature verification](verification.md) gates what gantry **copies**,
runtime enforcement gates what an engine **runs**: gantry watches each configured
docker engine's container-start events and force-removes any container whose
image is not signed by a trusted Root CA, then removes the image. This doc covers
how a running container is judged, the durable verdict cache, the offline
signature layout, the `on_unavailable` policy, gantry's self-protection, and what
gets removed. The full annotated config lives in [../gantry.yaml](../gantry.yaml);
related topics are [verification.md](verification.md), [stores.md](stores.md),
[retention.md](retention.md), and [observability.md](observability.md).

## Overview

Enforcement is **post-hoc quarantine**. The signal is the daemon's container
**start** (and restart) event, which fires *after* the container is already
running — so a very short-lived container can finish before it is killed. This is
**not** admission control: gantry cannot prevent a container from starting, only
detect and remove one that should not have. For pre-start blocking you need a
docker authorization plugin, which is out of scope.

When enabled (`serve.enforce.mode: quarantine`), gantry runs one watcher per
listed engine store. For each started container it resolves the image's content
digest, decides a verdict (trusted or not), and — when untrusted — force-removes
the container (`docker rm -f`) and then removes the image. The trust anchor is the
**same `serve.verify.trust_store`** used for admission; enabling enforcement
builds the verifier even when `serve.verify.mode` is `off`.

Enforcement **requires** a verdict cache (`serve.verify.cache`) and a trust store
(`serve.verify.trust_store`); startup fails without them. It is **docker-only** in
this release — a store of another kind in `serve.enforce.stores` is rejected at
startup (containerd's kill mechanic differs and is a later addition).

## How a container is judged

The verdict is keyed on the image's **top-level content digest** — the digest a
notation signature is over. gantry reads it from the running container's
`RepoDigests` (the `repo@sha256:…` references the daemon recorded when it pulled
the image). It deliberately does **not** use the platform-specific manifest
digest, which carries no signature — keying on that would make every multi-arch
image look unsigned.

For each start event, in order:

1. **Self-check.** If the container is gantry's own, it is always allowed (see
   [Self-protection](#self-protection)).
2. **Resolve the digest.** If the image has no `RepoDigests` (locally built,
   `docker load`, or a bare image id), there is no content digest to key on and
   the [`on_unavailable`](#when-a-verdict-cant-be-obtained-on_unavailable) policy
   decides.
3. **Verdict, in precedence:**
   - **Cache** — a fresh (within hard TTL) cached verdict is decisive and
     offline: trusted ⇒ allow, untrusted ⇒ quarantine.
   - **Live verification** — on a cache miss or a soft-expired entry, gantry
     verifies the digest against the source registry it was pulled from (matched
     by the `RepoDigests` host to a configured `oci` store), which also consults
     the [local layout](#offline-signatures-the-local-layout) and writes the
     result back to the cache. A verified signature ⇒ allow; an unsigned or
     untrusted image ⇒ quarantine.
   - **`on_unavailable`** — if no live answer is obtainable (registry
     unreachable, no matching source store), the policy decides.

A "could not reach the registry" outcome is **never** treated as untrusted and
**never** kills — a transient failure is a non-sentinel error, distinct from a
signature that genuinely fails to verify. Likewise an inspect failure on the
container never triggers a kill.

## The verdict cache

`serve.verify.cache` is a durable [bbolt](https://github.com/etcd-io/bbolt) store
keyed by content digest. It is **shared**: admission verification writes a verdict
as a side effect of every check, and enforcement reads it. This is what lets
enforcement decide **offline** — once an image's digest has a verdict, gantry can
quarantine (or allow) a container for it without touching the registry, so a
registry outage does not blind or disarm enforcement.

Each verdict carries two ages:

- **`ttl`** (hard, default `4w`) — past it the verdict is unusable and the image
  must be re-verified.
- **`refresh`** (soft, default `2w`) — past it a background sweeper re-verifies
  the entry and re-stamps it, so a revoked signature (or a since-signed image) is
  reflected before the hard TTL rather than only at it. The sweeper uses the raw
  verifier (reaching the registry/layout, not the cache), renews a still-valid
  verdict, flips a revoked one, and leaves an entry untouched when the registry is
  unreachable. `refresh` must be `<= ttl` (it defaults down to a shorter `ttl`).

Durations accept **`w`** (weeks) and **`d`** (days) in addition to the usual
`h`/`m`/`s`, so a four-week TTL is `4w` rather than `672h`; the two forms are
interchangeable.

The cache is invalidated on **trust rotation**: reloading the trust store (via
`VerifyService.Reload`, see [verification.md](verification.md)) clears every
verdict, so a decision made against the old CA is never served against the new
one.

The cache path must be distinct from every retention index and the audit log —
each bbolt file takes an exclusive lock, and a collision is a startup error.

## Offline signatures: the local layout

`serve.verify.local_layout` points at an on-disk **OCI image layout** of
pre-signed images — a bundle you place on the node for bootstrap or air-gapped
operation. gantry consults it **before** the live registry and verifies against
the **same trust store**, so it is content/crypto-based and cannot be spoofed by
naming. A digest present and signed there verifies fully offline; a digest absent
from it (or present but unsigned) falls through to the registry. The layout is
strictly **additive** — it can grant trust, never deny it, so a partial or stale
bundle can never turn a good image into a quarantined one.

The bundle is thin: it needs each image's **subject manifest plus its signature
referrer** — the image layers are not required to verify a signature. Build it
with your signing tooling (e.g. `oras cp` the signed images and their signatures
into an OCI layout directory). gantry fails startup if `local_layout` is set but
the directory is not an OCI layout.

The same offline source backs [admission verification](verification.md): a job
whose source is unreachable still verifies if the image is in the layout.

## When a verdict can't be obtained: `on_unavailable`

`serve.enforce.on_unavailable` decides what happens when there is **no fresh
cached verdict and no live answer** (registry down, or an image with no registry
provenance):

- **`grace`** (default) — honor a **known-but-expired trusted** verdict from the
  cache if one exists; otherwise allow the container and log it. This biases
  toward availability: it keeps known-good workloads running through a registry
  outage, and does not kill something it has never been able to judge.
- **`kill`** — fail closed: quarantine anything that cannot be affirmatively
  verified. Use this when running an unverifiable image is worse than a false
  positive.
- **`allow`** — always allow on doubt (only affirmatively-untrusted images are
  quarantined).

`grace` is meaningless without a cache to draw on, which is why enforcement
requires `serve.verify.cache`.

## Self-protection

There is deliberately **no image-name allowlist**. An image name is
attacker-chosen — anyone who can start a container can `docker tag` a malicious
image to match any pattern — so a name-based exemption is not a security boundary.
Two mechanisms replace it:

- **Sign what must run.** An image that has to run offline (an infra agent, a
  bootstrap component) should be signed and placed in
  [`local_layout`](#offline-signatures-the-local-layout). Then it verifies on its
  own merits, no exemption required.
- **Self-identity guard.** gantry never removes the container it runs in,
  identified by **container identity, not image name** — an attacker cannot make
  their container *be* gantry's. Identity is resolved, in order: the explicit
  `serve.enforce.self_container` (or the `GANTRY_SELF_ID` environment variable);
  else the hostname, which docker defaults to the short container id (used only
  when it looks like an id, so a custom `--hostname` is ignored); else
  `/proc/self/cgroup`. Note the last yields nothing under cgroup v2 with a private
  cgroup namespace (the modern default), where the file reads `0::/` — so on such
  hosts set `self_container` or rely on the hostname. This is a **safety
  interlock, not a security control**: gantry's own image should itself be signed
  and trusted, in which case it passes verification regardless.

## What is removed

On an untrusted verdict gantry **removes the container first** (`docker rm -f`,
which is also the enforcement action) and **then removes the image**. The image
removal is best-effort cleanup that prevents an immediate restart from the same
on-disk content; it is not forced, so an image still referenced by another
(legitimate) container is left in place (a benign skip). A container that is
already gone is treated as success, so replayed or duplicated events are
idempotent.

An image with **no registry provenance** (no `RepoDigests`) has no content digest
to key a verdict on, so it is routed to `on_unavailable` rather than killed
outright — under the default `grace` it is allowed and logged. There is no way to
distinguish an image deleted out-of-band from one whose provenance was stripped;
if you must reject such images, use `on_unavailable: kill` or sign and bundle them
in `local_layout`.

## Reliability

- **Reconnect gap.** If the event stream drops, a container started during the
  gap would be missed. gantry closes this by **cold-reconciling** the running
  containers on every (re)connect — listing what is running and judging each — so
  a container that slipped through the gap is still caught. The event stream is
  independent of the [retention](retention.md) usage watcher; the daemon fans out
  events to both.
- **Shutdown.** The watchers stop promptly on shutdown: `Stop` cancels them and
  joins before the shared cache file is closed, so no kill is issued against a
  half-torn-down server.

## Limitations

- **Docker only.** containerd/k3s enforcement is a later addition (its kill
  mechanic — task kill + delete — differs from `docker rm -f`).
- **Post-hoc.** A container runs until the start event is processed; a very
  short-lived one may complete first. This is quarantine, not prevention.
- **Admission-mode interplay.** Enforcement treats a cached untrusted verdict as
  kill-worthy regardless of a later relaxation of the source store's admission
  mode to `verify-if-present`. Enforcement plus `verify-if-present` on the same
  source is a contradictory posture; a relaxed mode takes full effect only after
  the verdict expires or trust is reloaded (which clears the cache).

## Configuration reference

`serve.enforce` (defaults applied only when `mode` is `quarantine`):

| Key | Type | Default | Meaning |
|---|---|---|---|
| `mode` | `off` \| `quarantine` | `off` | Whether runtime enforcement runs. |
| `stores` | list of store names | — (required when on) | Engine stores to police. Each must be a declared `docker` store. |
| `on_unavailable` | `grace` \| `kill` \| `allow` | `grace` | Decision when no verdict is obtainable and none is cached. |
| `self_container` | string | — | gantry's own container id/name, so it never removes itself. Empty falls back to the hostname, then `/proc/self/cgroup`. Also settable via `GANTRY_SELF_ID`. |

`serve.verify.cache` (required by `serve.enforce`; also usable on its own to
accelerate admission):

| Key | Type | Default | Meaning |
|---|---|---|---|
| `path` | path (bbolt file) | — (enables the cache) | Durable verdict store. Must differ from every retention/events bbolt file. |
| `ttl` | duration | `4w` | Hard max-age of a verdict; unusable past it. `w`/`d` units accepted. |
| `refresh` | duration | `2w` (capped at `ttl`) | Soft revalidation age; re-verified past it, usable until `ttl`. Must be `<= ttl`. |

`serve.verify.local_layout`:

| Key | Type | Default | Meaning |
|---|---|---|---|
| `local_layout` | path (dir) | — | On-disk OCI image layout of pre-signed images, consulted before the registry and verified against `trust_store`. Offline, additive, unspoofable. |
