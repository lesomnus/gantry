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
synthesized as an `oci` copy-mode store with the default rewrite rule. **Engine
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

## Rewrite rules

When a registry is a copy **target**, gantry derives the cache-side reference
from the store's `rewrite` rules — an **ordered** list of single-key
`{glob: template}` mappings, **first match wins**. Each rule is written as a
one-entry YAML mapping so the surrounding sequence preserves priority; a mapping
with more than one entry is rejected at load.

The glob (a `doublestar` pattern) is matched against the source reference's
fully-qualified name. The template (Go `text/template`) renders the destination
reference from these variables:

| Variable | Meaning |
|---|---|
| `.Ref` | the parsed reference as a string |
| `.Full` | fully-qualified name (`registry/repo:tag`) |
| `.CacheHost` | the target store's `host` |
| `.Registry` | source registry, e.g. `index.docker.io` |
| `.Repo` | repository path, e.g. `library/redis` |
| `.Tag` | set for tag references |
| `.Digest` | set for digest references |
| `.Identifier` | the tag or digest |

If the rendered reference omits a tag/digest, the source identifier is carried
over — so `{{.CacheHost}}/{{.Repo}}` keeps the original `:7`. (A `:` is only
treated as a tag when it is in the last path segment; otherwise it is a registry
port.) The default rule, applied to any `oci` store that declares no `rewrite`
(and to a synthesized bare-host store), is:

```yaml
rewrite:
  - { "**": "{{.CacheHost}}/{{.Repo}}" }
```

Two matching notes:

- ggcr normalizes Docker Hub to `index.docker.io`, so `.Registry` is
  `index.docker.io` — match it with `index.docker.io/**` or just `**`.
- If no rule matches, the job fails with `no rewrite rule matched %q` listing the
  patterns tried.

Rewrite applies only to a **registry** target. An **engine** target is told to
pull the source reference with only its *host* substituted (below), never a
rewritten repo path.

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

## Outbound TLS: private-CA verification and TPM-sealed client mTLS

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

### TPM-sealed client mTLS

For mutual TLS whose client key never leaves the device, gantry signs the TLS
handshake with a key held in a TPM and addressed by its persistent handle:

- **`tpm`** — the TPM device path. Defaults to `/dev/tpmrm0` (the resource-manager
  device, which multiplexes access) when omitted.
- **`tpm_handle`** — the persistent handle of the client signing key, as hex
  (`0x81000001`) or decimal; the value must fit in a uint32. gantry does **not**
  create keys — the handle must reference a key already provisioned in the TPM.
- **`tpm_cert`** — the client certificate (leaf + chain), PEM. Its public key
  **must match** the key at the handle, or the transport build fails with a clear
  config error rather than an opaque handshake failure at pull time.

**ECC keys only** — the signer supports NIST P-256 / P-384 / P-521; any other TPM
key type is rejected. Signing runs `TPM2_Sign` inside the device and returns the
ASN.1 DER ECDSA signature `crypto/tls` expects, so the private key material is
never present in process memory. Access to a single TPM connection is serialized
(one file descriptor, no internal locking), so parallel TLS handshakes signing
with the same key are safe.

For an `oci` registry, TPM mTLS applies to **every** outbound direction — pull,
push, referrer copy — including the bearer-token endpoint. For a **docker**
engine it is the client certificate presented to the daemon's TLS port (tcp
mTLS); the docker client detects the transport's TLS config and dials the daemon
over `https`. (A containerd engine reaches its daemon over a local socket and
does not use this.)

Validation and lifecycle:

- `tpm_handle` and `tpm_cert` are **both required** once any TPM field is set;
  `ca_cert` is intentionally excluded from that trigger (it configures server
  verification and is valid on its own).
- `insecure` **cannot** be combined with TPM mTLS: `insecure` enables plain HTTP
  on the oras path, which would silently drop the client certificate. To trust a
  self-signed mTLS server, set `ca_cert` instead.
- The device and certificate files are validated lazily on first use, so a
  missing TPM does not block startup for stores that do not use it. Devices are
  released at server shutdown.

## Caller-chosen `as` names

For an **engine** target, `as` records the pulled image under caller-chosen names
instead of the pull reference — so a cache-fed node keeps the upstream name
(`docker.io/library/redis:7`) even though gantry had it pull from the cache.
`as` is **engine targets only**; supplying it for a registry target is an error
(``as` names the image on an engine; store %q is a registry``).

`as` strings are kept **verbatim** — containerd resolves image names by exact
match, so normalizing (`docker.io` → `index.docker.io`) would break kubelet
lookups. `as` participates in the coalescing key `(ref, platforms, source,
target, as)`, so two submits differing only in `as` are distinct moves.

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
the job's **source** (the cache) — the origin registry is **never contacted** —
and hashes them against the reference's digest (sha256 only) rather than trusting
the transport, because they are about to be registered on a node under that
digest's name. The fetch happens **before** the pull, so a cache that cannot
resolve the digest fails the job before any bytes move; the engine registers the
names only **after** its pull succeeds, so a name never resolves to absent
content.

Digest names require the **containerd image store**:

- On a **`containerd`** engine, the name is created natively via the image
  service.
- On a **`docker`** engine, gantry forges the `RepoDigest` by streaming a thin
  OCI archive (the anchor manifest under the requested names) into the daemon's
  `/images/load` over the content the pull just placed — no registry contact.
  This works only when the daemon runs the containerd image store
  (`driver-type = io.containerd.snapshotter.v1`). A **classic graph store cannot
  represent a digest reference** without a real registry pull, so gantry **skips**
  the digest names and logs a warning; tag `as` names still apply, and the image
  resolves by its pull reference.

The retention index is stamped with the names the daemon **actually** holds (as
reported back by the engine), never with a skipped name — so a classic-store skip
leaves the image tracked under its pull reference, not a phantom digest name (see
`retention.md`).

The net effect: a jobspec pinned to `repo@sha256:INDEX` resolves locally after
the move — `docker image inspect` hits, and a `force_pull=false` deployment pulls
nothing.

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
| `tpm` | all | TPM device path for client mTLS. Default `/dev/tpmrm0` when any TPM field is set. |
| `tpm_handle` | all | Persistent handle of the TPM client key (hex or decimal, uint32). Required with `tpm_cert`. |
| `tpm_cert` | all | Client certificate (leaf + chain), PEM; public key must match the TPM key. |
| `ca_cert` | all | PEM CA(s) to verify the server. Usable on its own. Empty = system roots (skipped when `insecure`). |

### `oci` registry

| Key | Meaning |
|---|---|
| `host` | Registry host, exposed to rewrite templates as `.CacheHost`. Defaults to the store name. |
| `mode` | `copy` (default; push blobs) \| `proxy` (read-through self-fill). |
| `insecure` | Allow plain-HTTP / self-signed (skip TLS verification). |
| `username` / `password` | Credentials for operations against **this** store (env-expanded). Empty = docker keychain. |
| `rewrite` | Ordered `{glob: template}` rules for the cache-side ref (copy target). First match wins; default `{"**": "{{.CacheHost}}/{{.Repo}}"}`. |
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
