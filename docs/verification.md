# Source image signature verification

gantry can verify a `source` image's signature before it admits a job, and
reject the job when the image is unsigned or its signature does not check out.
This doc covers the trust model, the enforcement modes, how a verified image is
digest-pinned into the move, how the signature travels into the cache
(`copy_referrers`), the destination and per-store restrictions, and the
`VerifyService` introspection/reload surface. The full annotated config lives in
[../gantry.yaml](../gantry.yaml); related topics are [stores.md](stores.md),
[enforcement.md](enforcement.md) (verifying what an engine *runs*, not just what
gantry copies), [retention.md](retention.md), [observability.md](observability.md),
and [api.md](api.md).

## Overview

Verification is **fail-closed at job admission**. When it is enabled, gantry
resolves the `source` reference on its origin registry, verifies the signature
per policy, and — on success — pins the verified digest so the move covers
exactly what was verified. Any failure rejects the job synchronously: the job
never enters the queue and nothing is created. It runs inside `JobService.Add`
(and `Plan`), after the target ref is derived from the source tag but before the
job is admitted.

The one provider is **Notary Project / notation** (`provider: notation`, the
only accepted value). Signatures are OCI-native: they live as **referrer
artifacts** alongside the image manifest and are verified against a trust store
(CA certificates) and a trust policy. gantry lists the source image's referrers
to decide whether it is signed, then runs notation's verification over the
resolved manifest.

Verification uses the **same outbound wiring as the copy path** for the source
store — basic auth when the store sets `username`/`password`, otherwise the
docker keychain (`~/.docker/config.json`), plus the store's transport (TPM mTLS,
private-CA verification, or the plain-HTTP/self-signed skip from `insecure`). A
source fronted by mTLS or a private CA must therefore be reachable for
verification too, or admission would reject every job from it. See
[stores.md](stores.md) for the transport details.

A rejected job surfaces as gRPC `FAILED_PRECONDITION` (both an unsigned image
under `require` and a signature that fails verification). A source that cannot be
resolved is `NOT_FOUND`; any other verification error (unreachable registry,
etc.) still rejects the job — every non-nil error fails closed.

## Modes

The enforcement level is `serve.verify.mode`, one of:

- **`off`** (or unset) — no verification. Nothing is resolved or pinned.
- **`verify-if-present`** — verify when the source image carries a signature;
  allow it through unsigned. A present-but-invalid signature is still rejected.
- **`require`** — a signature is mandatory. Reject both an unsigned image
  (`image is not signed`) and an invalid one (`image signature verification
  failed`).

gantry checks signature presence uniformly for both enabled modes so it can
report a missing signature distinctly from a bad one. Under `verify-if-present`
an unsigned image is admitted with nothing pinned; a signed image is verified
and pinned exactly as under `require`. An unrecognized `mode` is a config error
that fails startup.

Whether verification runs at all is decided by the **effective** mode: a
non-empty per-store override wins over the global default (see
[Per-source-store override](#per-source-store-override)). Verification is
"enabled" — and the verifier is built at startup — when the global default is a
non-off value **or** any store overrides its mode to a non-off value.

## Trust store and trust policy

### Trust store — no OS-root fallback

`serve.verify.trust_store` is a directory of trusted CA certificate files, PEM
encoded, with extension `.crt`, `.pem`, `.cert`, or `.cer` (case-insensitive;
subdirectories and other files are ignored, and every `CERTIFICATE` PEM block in
a matching file is loaded). It is **required** whenever verification is enabled —
there is deliberately **no OS/system-root fallback**. At startup gantry:

- refuses to start if `trust_store` is empty (`serve.verify.trust_store is
  required when verification is enabled`);
- refuses the literal value `system` (OS roots), directing you to supply a CA
  directory instead — trusting the OS roots would accept any publicly-chained
  signature;
- refuses if the directory yields **no** parseable CA certificates, or if a
  matching file contains an unparseable certificate.

This is fail-fast by design: a misconfiguration stops the server rather than
silently accepting attacker signatures or failing every job at runtime.

gantry's trust store is a single **flat CA bundle** — all loaded certificates
are treated as one `ca`-type store. Only notation `ca:`-type trust stores resolve
against it; any other store type (e.g. `signingAuthority`) receives no
certificates and therefore fails closed rather than silently borrowing the CA
bundle.

### Trust policy — synthesized or supplied

`serve.verify.trust_policy` is an optional path to a notation trust policy JSON.

When it is **omitted**, gantry synthesizes a permissive-scope policy that trusts
any signature chaining to the configured CA(s), under any identity:

```json
{
  "version": "1.0",
  "trustPolicies": [{
    "name": "gantry-default",
    "registryScopes": ["*"],
    "signatureVerification": { "level": "<level>" },
    "trustStores": ["ca:gantry"],
    "trustedIdentities": ["*"]
  }]
}
```

`<level>` is `serve.verify.level` (below). Supply your own `trust_policy` to pin
`registryScopes` and `trustedIdentities` (e.g. restrict which registries or
signing identities are trusted).

A supplied policy is loaded, JSON-parsed, and validated by notation, and gantry
additionally enforces that it references **exactly one `ca:<name>` trust store**:

- a policy referencing any non-`ca:` store is rejected (gantry cannot
  distinguish store types behind its flat bundle);
- a policy referencing **more than one distinct** store name is rejected —
  every referenced store would resolve to the same bundle, silently
  over-trusting relative to intent (a team-B CA signature would satisfy a
  team-A-scoped policy).

Any policy/store validation failure is an "invalid trust material" error that
stops startup (and is reported distinctly from transient I/O by
`VerifyService.Reload`).

### Verification level

`serve.verify.level` sets the synthesized policy's verification level and is
**ignored when `trust_policy` is set** (the supplied policy carries its own):

- **`strict`** (default) — enforce certificate expiry and revocation.
- **`permissive`** — downgrade expiry/revocation to warnings; the trust-anchor
  (authenticity) check still holds. gantry logs a warning at startup when the
  level is not `strict`, since expiry and revocation are then not enforced.
- **`audit`** is **rejected** — both by config validation and by policy
  synthesis — because it downgrades authenticity to log-only, defeating the
  trust anchor. This is defense in depth: a job would otherwise be admitted for
  a signature that does not chain to the trust store at all.

## Digest pinning of the verified image

When a signature verifies, the move is **pinned to the digest that was
verified**. gantry resolves the source tag (or digest) to its manifest
descriptor, verifies that exact descriptor, and rewrites the job's source
reference to `repo@sha256:…` so the copy/pull moves precisely those bytes — not
whatever the tag points at by the time the transfer runs. The pinned digest is
recorded on the job's verification snapshot (observable via `JobService.Plan`
and the audit log) alongside the effective mode and whether a signature was
actually verified.

Digest pinning is also what makes digest-form `as` names honest: a digest `as`
name (`repo@sha256:…`) requires a digest-pinned job — either a digest source ref
or a verified source — and must carry that same anchor digest, so the engine
records a reference to exactly the pulled content. See [api.md](api.md) for `as`
semantics.

## `copy_referrers` — the signature travels into the cache

`copy_referrers` is a per-job `Add` flag. When set, after committing the image
into a registry destination gantry copies **every referrer artifact** of the
source manifest (the notation signature among them) into the cache repository,
with the subject digest preserved — so a downstream verifier can re-verify the
image **from the cache**. oras handles whichever discovery scheme the registry
supports (the referrers API or the fallback tag). Referrer copy is **fail-closed**:
if it errors the transfer fails, since propagating the signatures is the point.

Behavior and constraints:

- **Default on** only when the job actually verified a signature, the request
  did not narrow `platforms`, and the destination is a copy-mode registry.
  Otherwise it defaults off. Pass the flag explicitly to force it either way.
- **Registry destination only** — an engine pull has no referrer transport, so
  `copy_referrers` on an engine target is rejected.
- **Copy-mode only** — a `proxy` destination is rejected (it never holds the
  bytes to attach referrers to).
- **Verbatim / all platforms** — preserving the source digest requires
  committing the source index byte-for-byte, so `copy_referrers` forces every
  child manifest to be present and **refuses platform narrowing** (`platforms`
  must be empty). This is the same verbatim-commit rule a digest-ref registry
  copy follows.

### Multi-arch images

A notation signature on a multi-arch image is a referrer of the **top-level
index**, not of the per-platform child manifests — so the verified/pinned digest
is the **index digest**, and `copy_referrers` copies that one index signature
(all child manifests are committed verbatim so the index digest is preserved).
There is no per-platform signature to copy.

This is why `copy_referrers` refuses platform narrowing: a platform-filtered copy
would rebuild a smaller index with a **different digest**, to which the original
signature no longer applies. Copy the full multi-arch image (with its signature)
into the cache, then narrow the platform on the downstream engine pull — the
engine still anchors to the index digest, so the index signature covers it. See
[enforcement.md](enforcement.md#multi-arch-images-and-platform-narrowing) for how
a platform-narrowed running container is verified against the index signature.

## Proxy-destination restriction

A job that **pins a verified digest refuses a `proxy`-mode destination**. A
proxy cache fills itself by reading through on the tag and ignores the pinned
source digest — so if the tag moved after verification, the cache could
self-fill from a different, unverified image. Rather than move unverified bytes,
gantry rejects the job (`signature verification requires a copy-mode
destination; store … is proxy`). This applies whenever a signature was verified
(the digest was pinned); an unsigned image admitted under `verify-if-present`
pins nothing and is unaffected. See [stores.md](stores.md) for `copy` vs `proxy`
fill.

## Per-source-store override

A source registry store may override the global mode with
`stores.<name>.verify.mode` (`off` | `verify-if-present` | `require`). The
effective mode for a job's source store is the per-store override when set,
otherwise the global `serve.verify.mode` (unset ⇒ `off`). A per-store override to
a non-off value **enables verification even when the global default is unset** —
the verifier is still built at startup.

This lets you require signing on some registries and relax it on others:

```yaml
stores:
  dockerhub:   { kind: "oci", verify: { mode: "require" } }
  internal-ci: { kind: "oci", verify: { mode: "off" } }
```

Overrides apply only to `oci` (registry) source stores. An invalid override mode
is a config error that fails startup.

## `VerifyService`

The gRPC `VerifyService` exposes trust introspection, preflight checks, and
trust-store hot-reload. It never returns private key material. See
[api.md](api.md) for the service catalog.

- **`Describe`** reports the **effective** trust configuration: whether
  verification is enabled, the provider, the global default mode and level, the
  trust-policy statements (name, `registryScopes`, `trustedIdentities`,
  verification level), the loaded **anchors** (each certificate's subject, its
  SHA-256 fingerprint over the DER encoding, and its `not_after` expiry), and a
  per-registry-store map of the effective mode. It answers even when
  verification is disabled (`enabled=false`, mode `off`). Anchor expiries are
  meant to drive alerting — in `require` mode an expired CA fails every job.
- **`Check`** is preflight — "would this gantry accept this image?" — resolving
  the source store and reference the same way admission does and running the
  full verification without creating a job. It returns whether a signature was
  verified, the effective mode, and (when verified) the pinned digest.
  Rejections map to gRPC codes exactly as admission does: unsigned/untrusted ⇒
  `FAILED_PRECONDITION`, missing reference ⇒ `NOT_FOUND`, other errors ⇒
  `UNAVAILABLE`. It requires verification to be enabled, else
  `FAILED_PRECONDITION`.
- **`Reload`** re-reads the trust store and policy from disk with the same
  fail-fast validation as startup — this is **CA rotation without a restart**.
  The rebuilt verifier is swapped in **only on success**; on failure the
  previous verifier stays active and in-flight jobs are untouched. Invalid trust
  material returns `FAILED_PRECONDITION` (`reload rejected: …`); other failures
  return `INTERNAL`. Concurrent reloads are serialized so a race with a disk
  rotation cannot leave the older material active; `Verify`/`Describe` stay
  lock-free. The new policy is immediately observable via `Describe`.

## Configuration reference

The `serve.verify` block (defaults are applied only when verification is
enabled):

| Key | Type | Default | Meaning |
|---|---|---|---|
| `mode` | `off` \| `verify-if-present` \| `require` | `off` | Global default enforcement level. |
| `provider` | string | `notation` | Signature scheme; only `notation` is accepted. |
| `trust_store` | path (dir) | — (required when enabled) | Directory of trusted CA certificate files (`.crt`/`.pem`/`.cert`/`.cer`, PEM). No OS-root fallback; `system` is rejected. |
| `trust_policy` | path (JSON) | synthesized | notation trust policy. When omitted, gantry synthesizes one (`registryScopes ["*"]`, `trustedIdentities ["*"]`, `ca:gantry`, `level`). A supplied policy must reference exactly one `ca:<name>` store. |
| `level` | `strict` \| `permissive` | `strict` | Synthesized-policy level; ignored when `trust_policy` is set. `audit` is rejected. |
| `timeout` | duration | `15s` | Bounds one verification (registry resolve + signature fetch + verify). |

Per-source-store override, in the top-level `stores` map:

| Key | Type | Meaning |
|---|---|---|
| `stores.<name>.verify.mode` | `off` \| `verify-if-present` \| `require` | Overrides the global mode for images pulled from this `oci` source registry (unset ⇒ inherit the global default; a non-off value enables verification even when the global default is unset). |

Per-job (`JobService.Add` / `Plan`):

| Field | Type | Meaning |
|---|---|---|
| `copy_referrers` | bool (optional) | Copy the source's referrer artifacts (signatures) into a registry destination. Defaults on when the job verified a signature and did not narrow `platforms`; requires a copy-mode registry destination and forces a verbatim, all-platforms commit. |
