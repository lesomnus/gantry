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
  (`docker.io/library/redis:7`). For a **digest-pinned** job the names may be
  digest references (`cr.example.com/app@sha256:…`) that gantry registers over
  the pulled content — resolved locally from the cache, the origin registry never
  contacted. Digest names need a containerd-backed engine; see
  [docs/stores.md](docs/stores.md).
- Optionally, gantry reclaims space on the engines it feeds with an adaptive,
  policy-driven GC ([docs/retention.md](docs/retention.md)); verifies the
  `source` image's signature at job creation and pins the verified digest
  ([docs/verification.md](docs/verification.md)); and keeps a durable audit log
  plus OTLP metrics and traces ([docs/observability.md](docs/observability.md)).

The cache-side reference is derived from the target store's `rewrite` rules
(ordered `{glob: template}`, first match wins). `source`/`target` may be a
declared store name or a bare registry host (when `allow_unknown_stores` is set).

## Documentation

The guides below live in [docs/](docs/) ([index](docs/README.md)):

- **[docs/stores.md](docs/stores.md)** — store kinds, copy/proxy fill, `rewrite`,
  outbound TLS (private-CA and TPM-sealed mTLS), `as` names, digest pinning.
- **[docs/retention.md](docs/retention.md)** — per-store image GC: policy
  cascade, digest counting, pins, the untagged reaper, the adaptive scheduler.
- **[docs/verification.md](docs/verification.md)** — source-image signature
  verification (Notary Project / notation): modes, trust store/policy,
  `copy_referrers`, `VerifyService`.
- **[docs/observability.md](docs/observability.md)** — the OTLP metric catalog,
  the audit log (`EventService`), and health/readiness.
- **[docs/api.md](docs/api.md)** — gRPC behaviors: coalescing and trailers,
  `Idempotency-Key`, the dedup key, `Plan`, the live-vs-durable job model.

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

The whole surface is gRPC; the contract lives in [proto/gantry](proto/gantry)
with generated Go stubs and client/server wiring in the [pb](pb) package
(`github.com/lesomnus/gantry/pb`). Server reflection is on, so `grpcurl` and
similar tools work without proto files.

Day to day you drive **`JobService`**. `Add {ref, source, target}` submits a
move — optionally narrowing `platforms` or recording the pulled image under
caller-chosen `as` names — and returns the job right away; an identical in-flight
move is coalesced onto rather than run twice. `Watch` streams snapshots with
per-layer byte progress until the job reaches a terminal state, and `Get` fetches
one snapshot. `Plan` runs the same admission **without submitting** — a preflight
that reports the resolved source digest, the chosen platforms, and whether an
identical move is already running — while `Cancel` and `Retry` stop a job or
re-submit a finished one's request as a fresh job.

Everything else is operational and mostly out of the hot path: **`StoreService`**
lists stores and their health and drives ad-hoc engine `Pull`/`Remove` and
per-store GC ([docs/retention.md](docs/retention.md)); **`PinService`** exempts
images from GC; **`ImageService`** is the retention inventory; **`EventService`**
is the durable audit log ([docs/observability.md](docs/observability.md));
**`VerifyService`** introspects the trust configuration
([docs/verification.md](docs/verification.md)); and liveness/readiness is served
over the standard `grpc.health.v1.Health`.

Every RPC is guarded by a bearer token (`authorization: Bearer <token>` request
metadata) when `serve.auth.tokens` is set; with none set, auth is disabled
(intended to sit behind a trusted network). Health and reflection are always
exempt. Serve TLS with `serve.auth.tls_cert`/`tls_key`, or terminate TLS/mTLS at
a reverse proxy.

The complete service and RPC catalog — every method, the status-code mapping,
response trailers, the `Idempotency-Key`, the live-vs-durable job model, and
pagination — is in **[docs/api.md](docs/api.md)**.

## Configuration

gantry reads `--config <file>` (a root flag, so it precedes the subcommand),
defaulting to `./gantry.yaml` then `./gantry.yml`, and falling back to built-in
defaults when none exists. Unknown keys are **rejected**, so a typo in a
security-sensitive block fails loudly instead of silently disabling a control.
`gantry config` prints the effective configuration with defaults applied and
secrets (`serve.auth.tokens`, store passwords) redacted. Example deployments:
[gantry.nomad.hcl](gantry.nomad.hcl) (Nomad)
and [gantry.hday.yaml](gantry.hday.yaml) (a lab config); [docker-compose.yaml](docker-compose.yaml)
is a smoke test.

See [gantry.yaml](gantry.yaml) for the full annotated example. Top-level blocks:

- `stores` — the unified store map (keyed by name): `kind: oci` registries
  (`host`, `mode`, `insecure`, `rewrite`, `downstream_host`, credentials) or
  `kind: docker`/`containerd` engines (`address`, `namespace`, `pull_host`). Any
  store may carry outbound TLS (`ca_cert` and TPM-sealed client mTLS). See
  [docs/stores.md](docs/stores.md).
- `worker` — `max_concurrent_jobs` / `max_concurrent_layers` pool sizes.
- `serve.addr` — the gRPC listen address.
- `serve.allow_unknown_stores` — let a job name a bare registry host not declared
  as a store (default false).
- `serve.health` — per-store health-probe cache (`cache_ttl`, `probe_timeout`)
  and `ready_stores` (which stores readiness gates on). See
  [docs/observability.md](docs/observability.md).
- `serve.verify` — source-image signature verification (Notary Project /
  notation) at job creation. See [docs/verification.md](docs/verification.md).
- `stores.<name>.retention` — image GC, configured **per engine store** (there is
  no global policy). See [docs/retention.md](docs/retention.md).
- `serve.events` — the durable audit log (a bounded bbolt ring), queryable via
  `EventService`. See [docs/observability.md](docs/observability.md).
- `serve.auth` — `tokens` (env-expanded) and server `tls_cert`/`tls_key` for TLS.
- `otel` — the OpenTelemetry pipeline (metrics/traces/logs over OTLP). See
  [docs/observability.md](docs/observability.md).

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

Feature-complete for v1 and tested — `go build`, `go vet`, and `go test -race
./...` all pass, with the live docker/containerd integration tests exercised
against real daemons (they self-skip when no daemon is present). The
store/transfer model, caller-chosen `as` names, per-store retention/GC, signature
verification, the resource-oriented gRPC contract, and OTLP/audit-log
observability are all implemented; each is covered under [docs/](docs/).

A few things are left to the maintainer before tagging v1, none of them baked
into the code as blockers:

- **License** — to be added by the maintainer; until then no license file ships.
- **Release tagging / versioning** — the maintainer cuts version tags; CI already
  publishes the `edge` image on every push to `main`.
- **No compatibility guarantees yet** — pre-v1, the on-disk bbolt formats (the
  retention index and the audit log) and the proto contract may change without a
  migration path. A future incompatible API would ship under a new proto package
  rather than mutating the current one.

## Not planned

Proposals weighed and intentionally left out, recorded so they are not
re-litigated:

- **`GET /metrics` (Prometheus scrape)** — metrics push over OTLP instead; a
  scrape endpoint would duplicate an OTel→collector pipeline that already exists.
- **Declarative store reconcile** (a desired-image-set endpoint) — the inventory
  plus ordinary jobs/removes let a client diff for itself; putting desired state
  on the server muddies ownership.
- **Job-completion webhooks** — the `Watch` stream covers it; outbound callbacks
  from the edge only add reachability failure modes.
- **A token-management API** — the static-token allowlist (plus mTLS at a proxy)
  is the trust model; minting tokens over the API doesn't fit it.
- **A retry `force` flag that bypasses dedup** — it would put two writers on one
  destination tag; cancel-then-retry is the supported path.
- **A persistent job store** — job history is already durable in the audit log
  (`EventService`); `JobService.Get`/`List` are the live registry by design and
  gantry does not auto-resume interrupted jobs (`Retry` re-submits). See
  [docs/api.md](docs/api.md).

The full design history — the rationale behind each decision here and the former
consolidated plan / GC / API / digest-`as` design docs — lives in the git log.
