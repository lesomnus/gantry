# gantry

Move container images between **stores** — registries and daemon engines — and
watch the per-layer progress over a gRPC API. A job copies an image from one
registry into another (e.g. a remote into a local cache) or straight onto a
docker / containerd engine, which is told to pull it.

## How it works

A **store** is a registry (gantry reads/writes blobs) or an engine — docker or
containerd (a daemon that pulls). A **job** moves an image from a registry into
any store:

```
JobService.Add {ref, source, target}
   source (registry) ──copy──▶ target (registry/cache)     # oci: gantry copies blobs
   source (registry) ──pull──▶ target (docker/containerd)  # engine: the daemon pulls
        the transfer reports per-layer byte progress
JobService.Get  ·  JobService.Watch (server stream)
```

- The `source`→`target` copy is gantry-driven: it pulls each blob and pushes it,
  skipping blobs the target already has, so progress reflects bytes actually
  moved (**copy** mode). **proxy** mode reads through `target` so a pull-through
  cache self-fills instead.
- An engine `target` pulls exactly one platform: the request's, or the daemon
  host's when omitted. The daemon's progress is folded into the same transfer
  (totals estimated upfront from the source manifest, refined as the daemon
  reports). `StoreService.Pull` remains for ad-hoc, job-less pulls.
- `as` records the pulled image on the engine under caller-chosen names instead
  of the pull reference, so a cache-fed node keeps the upstream name
  (`docker.io/library/redis:7`). For a **digest-pinned** job (a digest ref, or a
  verified source) the names may be digest references carrying the pinned digest
  (`cr.example.com/app@sha256:…`): gantry registers them over the pulled content
  — the anchor manifest's bytes come from the cache, the origin registry is
  never contacted — so a jobspec pinned to `repo@sha256:INDEX` resolves locally
  (`docker image inspect` hits, `force_pull=false` runs pull nothing). Digest
  names need an engine whose docker uses the containerd image store; a classic
  graph store skips them with a warning (tags still apply). A digest-ref copy
  into a registry commits the source index **verbatim**, so the same digest
  resolves from the cache.
- Optionally, gantry reclaims space on the engines it feeds: it tracks each
  image's last-used time from the daemon's container events and runs an adaptive,
  policy-driven GC (per-store `retention`). See [Configuration](#configuration).
- Optionally, gantry verifies the `source` image's signature (Notary Project /
  notation) at job creation and rejects the job on failure — pinning the verified
  digest so it moves exactly what was verified (`serve.verify`). The signature can
  travel with the image into the cache (`copy_referrers`), so it still verifies
  there.
- Optionally, gantry keeps a durable audit log of what it did — jobs, GC,
  pins, manual ops — that survives restarts (`serve.events`), and exports metrics
  and traces over OTLP (`otel`).

The cache-side reference is derived from the target store's `rewrite` rules
(ordered `{glob: template}`, first match wins). `source`/`target` may be a
declared store name or a bare registry host (when `allow_unknown_stores` is set).

## Install

Prebuilt multi-arch image (amd64/arm64), published to GHCR by CI:

```sh
docker pull ghcr.io/lesomnus/gantry:edge
```

Or build from source. The binary is static (CGO disabled), so the runtime image is
`FROM scratch` (runs as UID 65532, entrypoint `/gantry`):

```sh
docker buildx bake build app   # cross-build binaries into ./dist, then the image
go build ./...                 # or a plain host build
```

`gantry version` prints the build stamp (`GANTRY_VERSION` / `GANTRY_GIT_REV` /
`GANTRY_GIT_DIRTY`).

## Quick start

Server reflection is on (and public), so `grpcurl` works without proto files:

```sh
$ gantry serve --addr :8080

# See the configured stores and their capabilities.
$ grpcurl -plaintext localhost:8080 gantry.StoreService/List

# Copy redis from docker.io into the cache.
$ grpcurl -plaintext -d '{
    "ref": "library/redis:7",
    "source": {"name": "docker.io"}, "target": {"name": "cache"},
    "platforms": ["linux/amd64"]
  }' localhost:8080 gantry.JobService/Add

# Move the cached image onto the k3s node — same job shape, engine target.
# The daemon pulls it; platform defaults to the node's own.
$ grpcurl -plaintext -d '{
    "ref": "library/redis:7",
    "source": {"name": "cache"}, "target": {"name": "k3s"}
  }' localhost:8080 gantry.JobService/Add

# Watch progress (every transfer carries per-layer bytes); the stream ends
# after the snapshot that carries a terminal state.
$ grpcurl -plaintext -d '{"id": "<id>"}' localhost:8080 gantry.JobService/Watch
```

## gRPC API

The contract lives in [proto/gantry](proto/gantry) (entities plus the merged
service definitions) with generated Go stubs, ergonomic request constructors,
and client/server wiring in the [pb](pb) package
(`github.com/lesomnus/gantry/pb`). Entity services follow a resource-oriented
CRUD shape — `Add` / `Get` / `Patch` return the entity and `Erase` returns
`Empty`, `Ref` messages addressing it by key or unique index — with `List` and
the custom actions merged on top. Write RPCs without a domain operation (stores are
declared in configuration; image records and audit events are written
internally) answer `UNIMPLEMENTED`.

| Service | RPCs | Notes |
|---|---|---|
| `JobService` | `Add` · `Get` · `List` · `Erase` · `Watch` · `Plan` · `Cancel` · `Retry` | `Add` coalesces onto an identical in-flight move (a `gantry-coalesced` response trailer flags **whether** the submit joined one, not which) and honors an `idempotency-key` request metadata (a repeat with the same key replays the remembered job even if the body differs, until the record is swept); `Watch` streams job snapshots until terminal; `Plan` is dry-run admission; `Erase` evicts (cancels first when running). Jobs carry a free-form `labels` map (filterable on `List`). Verification rejections are `FAILED_PRECONDITION`, a full queue is `RESOURCE_EXHAUSTED`. |
| `StoreService` | `Get` · `List` · `Pull` · `Remove` · `Health` · `GcStatus` · `GcPlan` · `GcApply` | Stores are declared in config. `Pull`/`Remove` drive one engine daemon (and keep the retention index in sync). The GC RPCs need the store's `retention`; `GcPlan` dry-runs, `GcApply` executes, both take a one-shot policy override. `GcStatus` includes the usage-watcher health. |
| `ImageService` | `Get` · `List` · `Erase` | The retention inventory. `List` filters by `repo`/`ref`/`pinned`/`in_use` (the live daemon set) and carries the untagged reap clocks on unfiltered lists; `Erase` purges an orphan record without touching the engine. |
| `PinService` | `Add` · `Get` · `List` · `Erase` | GC exemptions: an exact ref or a doublestar `pattern`. `Add` upserts and echoes the pin's current blast radius as `gantry-pin-matched-count` / `gantry-pin-matched` response trailers (the index records it protects), so a careless broad pattern is visible; `Erase` is idempotent. |
| `EventService` | `Get` · `List` | The audit log (requires `serve.events`); newest-first with `type`/`store`/`ref`/`state`/`since` filters. |
| `VerifyService` | `Describe` · `Check` · `Reload` | Trust introspection (never key material), preflight ("would this gantry accept the image"), and truststore hot-reload for CA rotation. |
| `grpc.health.v1.Health` | `Check` · `Watch` | Liveness and readiness: the overall status follows aggregate health over the gated stores (`serve.health.ready_stores`). Public. |

Every RPC is guarded by a bearer token (`authorization: Bearer <token>` request
metadata) when `serve.auth.tokens` is set; with none set, auth is disabled
(intended to sit behind a trusted network). The standard health and reflection
services are always exempt — they expose liveness and the schema, not the data.
Serve TLS with `serve.auth.tls_cert`/`tls_key`, or terminate TLS/mTLS at a
reverse proxy.

A few behaviors worth knowing:

- **Verification & proxy** — a verifying job refuses a `proxy`-mode destination
  (the proxy cache never learns the digest to anchor).
- **Verbatim commits** — a digest-ref registry copy, and any job with
  `copy_referrers`, commit the source index byte-for-byte, so they refuse platform
  narrowing (`platforms` must be empty). `copy_referrers` is a per-job `Add` flag,
  on by default when the job verified a signature and platforms weren't narrowed.
- **Dedup & mutable tags** — coalescing keys on `(ref, platforms, source, target,
  as)` and treats a tag as stable for the life of an active job: a tag re-pushed
  mid-job does not start a second copy until the first finishes, and with digest
  pinning the first job carries the digest resolved at admission (the pre-repush
  image).
- **Listing** — `List` RPCs paginate with `page_size` / `page_token`;
  `EventService.List` returns at most 1000 events (default 100), older ones
  reachable only by `Get`.
- **Stable ids** — `Image` and `Pin` ids are deterministic UUIDs derived from
  `(store, ref)` / `(store, value)`, so they survive restarts.
- **Plan** — `JobService.Plan` returns the resolved plan: the rewritten target ref,
  chosen platforms, `as` names, the `copy_referrers` default, the verification
  outcome, and which in-flight job an identical `Add` would coalesce onto.

## Configuration

gantry reads `--config <file>` (a root flag, so it precedes the subcommand),
defaulting to `./gantry.yaml` then `./gantry.yml`, and falling back to built-in
defaults when none exists. `gantry config` prints the effective configuration with
defaults applied. Example deployments: [gantry.nomad.hcl](gantry.nomad.hcl) (Nomad)
and [gantry.hday.yaml](gantry.hday.yaml) (a lab config); [docker-compose.yaml](docker-compose.yaml)
is a smoke test.

See [gantry.yaml](gantry.yaml) for the full annotated example. Key blocks:

- `stores` (top-level) — the unified store map (keyed by name). `kind: oci` (`host`,
  `mode`, `insecure`, `rewrite`, `downstream_host`, `username`/`password`) or
  `kind: docker`/`containerd` (`address`, `namespace`, `pull_host`). Any store may
  also carry outbound TLS: `ca_cert` (verify a private-CA server, usable on its own)
  and TPM-sealed client mTLS (`tpm`/`tpm_handle`/`tpm_cert`, **ECC keys only**) —
  gantry presents the client cert and signs the handshake with the TPM-held key,
  which never leaves the device. See [gantry.yaml](gantry.yaml) for the annotated forms.
- `worker` (top-level) — `max_concurrent_jobs`/`max_concurrent_layers` pool sizes.
- `serve.addr` — the gRPC listen address.
- `serve.allow_unknown_stores` — let a job name a bare registry host not declared
  as a store (default false).
- `serve.health` — per-store health probe cache: `cache_ttl` (default 5s),
  `probe_timeout` (default 3s), and `ready_stores` (which stores the health
  service's readiness gates on; empty = every engine store, so a flaky upstream
  can't flap the node).
- `serve.verify` — source-image signature verification (Notary Project / notation)
  at job creation. `mode` (`off` | `verify-if-present` | `require`), a `trust_store`
  of CA certs (required when enabled — no OS-root fallback; missing ⇒ the server
  refuses to start), an optional notation `trust_policy` (must reference exactly one
  `ca:<name>` trust store, else startup fails), and `level`. A verified image is
  pinned to its digest for the copy/pull. Setting `verify.mode` on a source `oci`
  store enables verification for that registry even when the global mode is unset.
- `stores.<name>.retention` — image GC, configured **per engine store** (there is
  no global policy). Each store has its own `path` (usage index), scheduler
  cadence, `grace`, and per-repo `rules`. gantry tracks last-used time from the
  engine's container events, then keeps in-use, pinned, the `keep_n` most-recent
  images per repo, and anything newer than `max_age`; `max_n` caps the images kept
  per repo (oldest beyond the cap deleted even before `max_age`). `keep_n`/`max_n`
  count by digest, so tags sharing an image count once. `max_idle` is a hard
  cap — an image unused longer than it is deleted regardless of `keep_n`/`max_n`
  (only in-use and pins protect it), so a settled-but-ancient tag doesn't linger.
  Each rule's `repo`
  is a doublestar pattern; for a repo the matching rules cascade field-by-field
  (the longest-prefix match wins each field, pins are unioned), and a repo that
  matches no rule is left untouched. The scheduler is adaptive — it idles up to
  `interval` and wakes only when a record is about to age out or usage changes.
- `stores.<name>.retention.untagged_after` (docker stores; **default `1h`, on**) —
  reap an image this long after gantry first observes it with **no tags** — e.g.
  the previous image of a tag that was re-pulled, which docker otherwise keeps on
  disk forever. Every GC pass also takes a full inventory scan of the daemon, so
  images pulled or untagged while gantry was down (or by a human) still converge
  on the configured rules: unknown *tagged* refs are seeded into the index
  (age clock starts at observation; repos matching no rule stay untouched — note
  a `max_n` cap applies to seeded refs on the next pass once the startup grace
  ends), and untagged images start a reap clock. Untagged images bypass the per-repo rules
  (there is no tag to match); running containers, digest-pinned index records
  (e.g. a digest-ref job), and `repo@digest`/image-ID pins still protect them —
  tag-form pins cannot, since the tag is gone. Set `"0s"` to turn the reaper off.
  containerd needs none of this (gantry untracks the digest record after a pull,
  so containerd's own GC reclaims replaced content) and rejects the knob.
- `serve.events` — the audit log (disabled unless `path` is set): a bounded bbolt
  ring (`cap` entries) of jobs, GC, pins, and manual ops, queryable via
  `EventService`.
- `serve.auth` — `tokens` (env-expanded) and server `tls_cert`/`tls_key` for TLS.
- `otel` (top-level, not under `serve`) — the OpenTelemetry pipeline (mkot-style).
  Instruments are a no-op until an exporter is wired to a provider; the `otlp`
  exporter pushes metrics/traces/logs over OTLP/gRPC. Metric instruments:
  `gantry.bytes`, `gantry.job.duration`, `gantry.jobs` (by state),
  `gantry.jobs.active`, `gantry.queue.depth` / `gantry.queue.capacity`,
  `gantry.health.probe.duration`, and per-engine `gantry.retention.records` /
  `gantry.retention.pins` / `gantry.retention.untagged`.

## Development

The devcontainer runs gantry against a Docker-in-Docker daemon and a dedicated
containerd sidecar. Test layout, the live integration tests, the full job loop, and
the insecure-registry constraints are documented in
[docs/test-environment.md](docs/test-environment.md).

```sh
go test -race ./...     # unit tests always; live docker/containerd tests self-skip without a daemon
scripts/gen-proto.sh    # regenerate the gRPC contract: protoc-gen-orm-service emits the CRUD
                        # services from the orm-annotated entities, protobuf-merge overlays the
                        # hand-written RPCs (proto.svc/), then protoc-gen-go(-grpc) and
                        # protoc-gen-orm-go compile everything into pb/
```

## Status

Feature-complete and tested — `go build`, `go vet`, and `go test -race ./...` all
pass, with the live docker/containerd integration tests exercised against real
daemons (they self-skip when no daemon is present).

Implemented:

- **Image movement** — the store/transfer model, `copy`/`proxy` fill, the engine
  pull seam, per-layer streaming progress. Verified end-to-end against real daemons.
- **Caller-chosen `as` names** — records the pulled image on the engine under
  upstream names instead of the cache pull ref. Tag names on any engine; **digest
  names** (`repo@sha256:…`) on an engine whose docker uses the containerd image
  store, registered over cache-fed content via a thin OCI `docker load` — no origin
  registry contact — so a digest-pinned jobspec resolves locally. Classic graph
  stores skip digest names with a warning (tags still apply). A digest-ref registry
  copy commits the source index verbatim so the same digest resolves from the cache.
  Verified end-to-end on real containerd-store nodes.
- **Retention / GC** (per-store `retention`) — usage tracking from container events,
  the per-repo rule cascade (keep-N, `max_n` cap, `max_age`), exact and pattern
  pins, the adaptive scheduler, and the inventory / status / watcher APIs. Each GC
  pass reconciles against a full daemon inventory: unknown tagged images join the
  index and (on docker) untagged leftovers are reaped after `untagged_after`.
- **Signature verification** (`serve.verify`, Notary Project / notation) — verified
  fail-closed at admission with digest pinning and a fail-fast trust store; engine
  pulls are digest-anchored (`repo@digest`) and signatures can travel into the cache
  (`copy_referrers`). Preflight, introspection, and truststore hot-reload APIs.
  Tested with in-process notation signing.
- **Job lifecycle & API** — the resource-oriented gRPC contract (`JobService` /
  `StoreService` / `ImageService` / `PinService` / `EventService` / `VerifyService`
  plus `grpc.health.v1.Health`) with dry-run `Plan`, `Retry`, an `Idempotency-Key`,
  coalescing onto an identical in-flight move (a `gantry-coalesced` trailer flags
  whether the submit joined one), job `labels`, and cancel-vs-evict. Bearer-token
  auth and server reflection.
- **Observability** — OTLP metrics/traces (`otel`), a durable audit log
  (`serve.events`), and health/readiness over `grpc.health.v1.Health`.

Remaining work, documentation gaps, and the v1-release checklist are tracked in
[ROADMAP.md](ROADMAP.md), which consolidates the former design docs (their full
rationale remains in git history).
