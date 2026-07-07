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
JobService.Add {ref, from, to}
   from (registry) ──copy──▶ to (registry/cache)     # oci: gantry copies blobs
   from (registry) ──pull──▶ to (docker/containerd)  # engine: the daemon pulls
        the transfer reports per-layer byte progress
JobService.Get  ·  JobService.Watch (server stream)
```

- The `from`→`to` copy is gantry-driven: it pulls each blob and pushes it,
  skipping blobs the destination already has, so progress reflects bytes actually
  moved (**copy** mode). **proxy** mode reads through `to` so a pull-through cache
  self-fills instead.
- An engine `to` pulls exactly one platform: the request's, or the daemon
  host's when omitted. The daemon's progress is folded into the same transfer
  (totals estimated upfront from the source manifest, refined as the daemon
  reports). `StoreService.Pull` remains for ad-hoc, job-less pulls.
- Optionally, gantry reclaims space on the engines it feeds: it tracks each
  image's last-used time from the daemon's container events and runs an adaptive,
  policy-driven GC (per-store `retention`). See [Configuration](#configuration).
- Optionally, gantry verifies the `from` image's signature (Notary Project /
  notation) at job creation and rejects the job on failure — pinning the verified
  digest so it moves exactly what was verified (`serve.verify`). The signature can
  travel with the image into the cache (`copy_referrers`), so it still verifies
  there.
- Optionally, gantry keeps a durable audit log of what it did — jobs, GC,
  pins, manual ops — that survives restarts (`serve.events`), and exports metrics
  and traces over OTLP (`otel`).

The cache-side reference is derived from the destination store's `rewrite` rules
(ordered `{glob: template}`, first match wins). `from`/`to` may be a declared
store name or a bare registry host (when `allow_unknown_stores` is set).

## Quick start

Server reflection is on (and public), so `grpcurl` works without proto files:

```sh
$ gantry serve --addr :8080

# See the configured stores and their capabilities.
$ grpcurl -plaintext localhost:8080 gantry.StoreService/List

# Copy redis from docker.io into the cache.
$ grpcurl -plaintext -d '{
    "ref": "library/redis:7",
    "from": {"name": "docker.io"}, "to": {"name": "cache"},
    "platforms": ["linux/amd64"]
  }' localhost:8080 gantry.JobService/Add

# Move the cached image onto the k3s node — same job shape, engine destination.
# The daemon pulls it; platform defaults to the node's own.
$ grpcurl -plaintext -d '{
    "ref": "library/redis:7",
    "from": {"name": "cache"}, "to": {"name": "k3s"}
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
CRUD shape — `Add` / `Get` / `Patch` / `Erase` returning the entity, `Ref`
messages addressing it by key or unique index — with `List` and the custom
actions merged on top. Write RPCs without a domain operation (stores are
declared in configuration; image records and audit events are written
internally) answer `UNIMPLEMENTED`.

| Service | RPCs | Notes |
|---|---|---|
| `JobService` | `Add` · `Get` · `List` · `Erase` · `Watch` · `Plan` · `Cancel` · `Retry` | `Add` coalesces onto an identical in-flight move (the `gantry-coalesced` response trailer tells which) and honors an `idempotency-key` request metadata; `Watch` streams job snapshots until terminal; `Plan` is dry-run admission; `Erase` evicts (cancels first when running). Verification rejections are `FAILED_PRECONDITION`, a full queue is `RESOURCE_EXHAUSTED`. |
| `StoreService` | `Get` · `List` · `Pull` · `Remove` · `Health` · `GcStatus` · `GcPlan` · `GcApply` | Stores are declared in config. `Pull`/`Remove` drive one engine daemon (and keep the retention index in sync). The GC RPCs need the store's `retention`; `GcPlan` dry-runs, `GcApply` executes, both take a one-shot policy override. `GcStatus` includes the usage-watcher health. |
| `ImageService` | `Get` · `List` · `Erase` | The retention inventory. `List` filters by `repo`/`ref`/`pinned`/`in_use` (the live daemon set) and carries the untagged reap clocks on unfiltered lists; `Erase` purges an orphan record without touching the engine. |
| `PinService` | `Add` · `Get` · `List` · `Erase` | GC exemptions: an exact ref or a doublestar `pattern`. `Add` upserts; `Erase` is idempotent. |
| `EventService` | `Get` · `List` | The audit log (requires `serve.events`); newest-first with `type`/`store`/`ref`/`state`/`since` filters. |
| `VerifyService` | `Describe` · `Check` · `Reload` | Trust introspection (never key material), preflight ("would this gantry accept the image"), and truststore hot-reload for CA rotation. |
| `grpc.health.v1.Health` | `Check` · `Watch` | Liveness and readiness: the overall status follows aggregate health over the gated stores (`serve.health.ready_stores`). Public. |

Every RPC is guarded by bearer token (`authorization: Bearer <token>` request
metadata) and/or mTLS when configured (see `serve.auth`); with neither set,
auth is disabled (intended to sit behind a trusted network). The standard
health and reflection services are always exempt — they expose liveness and
the schema, not the data.

## Configuration

See [gantry.yaml](gantry.yaml) for the full annotated example. Key blocks:

- `stores` (top-level) — the unified store map (keyed by name). `kind: oci` (`host`,
  `mode`, `insecure`, `rewrite`, `downstream_host`) or `kind: docker`/`containerd`
  (`address`, `namespace`, `pull_host`).
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
  refuses to start), an optional notation `trust_policy`, and `level`. A verified
  image is pinned to its digest for the copy/pull. Per-source-registry `verify.mode`
  overrides the global default.
- `stores.<name>.retention` — image GC, configured **per engine store** (there is
  no global policy). Each store has its own `path` (usage index), scheduler
  cadence, `grace`, and per-repo `rules`. gantry tracks last-used time from the
  engine's container events, then keeps in-use, pinned, the `keep_n` most-recent
  tags per repo, and anything newer than `max_age`; `max_n` caps the tags kept per
  repo (oldest beyond the cap deleted even before `max_age`). Each rule's `repo`
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
- `serve.auth` — `tokens` (env-expanded), `client_ca`, and server `tls_cert`/`tls_key`.
- `otel` (top-level, not under `serve`) — the OpenTelemetry pipeline (mkot-style).
  Instruments are a no-op until an exporter is wired to a provider; the `otlp`
  exporter pushes metrics/traces/logs over OTLP/gRPC.

## Development

The devcontainer runs gantry against a Docker-in-Docker daemon (and its bundled
containerd). Test layout, the live integration tests, the full job loop, and the
insecure-registry constraints are documented in
[docs/test-environment.md](docs/test-environment.md).

```sh
go test -race ./...     # unit tests always; live docker/containerd tests self-skip without a daemon
scripts/gen-proto.sh    # regenerate the gRPC contract: protoc-gen-orm-service emits the CRUD
                        # services from the orm-annotated entities, protobuf-merge overlays the
                        # hand-written RPCs (proto.svc/), then protoc-gen-go(-grpc) and
                        # protoc-gen-orm-go compile everything into pb/
```

## Status

Implemented and tested (unit + live docker/containerd integration; `go test -race`):

- **Image movement** — the store/transfer model, `copy`/`proxy` fill, the engine
  pull seam, streaming progress. Verified end-to-end against real daemons.
- **Retention / GC** (per-store `retention`) — usage tracking from container events,
  keep-N-per-repo, exact and pattern pins, the adaptive scheduler, and the
  inventory / status / watcher APIs. On docker stores the GC pass also reconciles
  against a full daemon inventory: unknown tagged images join the index and
  untagged leftovers are reaped after `untagged_after`.
- **Signature verification** (`serve.verify`, Notary Project / notation) — verified
  at admission with digest pinning; engine pulls are digest-anchored and signatures
  can be copied into the cache (`copy_referrers`). Preflight, introspection, and
  hot-reload APIs. Tested with in-process notation signing.
- **Job lifecycle** — dry-run plan, retry, idempotency, cancel-vs-evict.
- **Observability** — OTLP metrics/traces (`otel`), a durable audit log
  (`serve.events`), and health/readiness over `grpc.health.v1.Health`.

The design docs — [plan.md](plan.md) (movement), [plan-gc.md](plan-gc.md) (retention),
and [plan-api.md](plan-api.md) (the API expansion) — record the rationale and
point-in-time decisions.
