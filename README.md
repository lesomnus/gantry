# gantry

Move container images between **stores** — registries and daemon engines — and
watch the per-layer progress over an HTTP API. A job copies an image from one
registry into another (e.g. a remote into a local cache), then triggers docker /
containerd engines to pull it.

## How it works

A **store** is a registry (gantry reads/writes blobs) or an engine — docker or
containerd (gantry triggers a pull). A **job** moves an image:

```
POST /v1/job {ref, from, to, distribute}
   from (registry) ──copy──▶ to (registry/cache) ──pull──▶ distribute (engines)
        every step is a Transfer with the same per-layer byte progress
GET /v1/job/{id}  ·  GET /v1/job/{id}/progress (SSE)
```

- The `from`→`to` copy is gantry-driven: it pulls each blob and pushes it,
  skipping blobs the destination already has, so progress reflects bytes actually
  moved (**copy** mode). **proxy** mode reads through `to` so a pull-through cache
  self-fills instead.
- `distribute` engines pull `to` (or `from`, when there is no `to`). Their
  per-layer progress is reported best-effort from the daemon.
- Optionally, gantry reclaims space on the engines it feeds: it tracks each
  image's last-used time from the daemon's container events and runs an adaptive,
  policy-driven GC (`serve.retention`). See [Configuration](#configuration).
- Optionally, gantry verifies the `from` image's signature (Notary Project /
  notation) at job creation and rejects the job on failure — pinning the verified
  digest so it moves exactly what was verified (`serve.verify`). Engines pull that
  digest (not the tag), and the signature can travel with the image into the cache
  (`copy_referrers`), so it still verifies there.
- Optionally, gantry keeps a durable audit log of what it did — jobs, GC,
  pins, manual ops — that survives restarts (`serve.events`), and exports metrics
  and traces over OTLP (`otel`).

The cache-side reference is derived from the destination store's `rewrite` rules
(ordered `{glob: template}`, first match wins). `from`/`to` may be a declared
store name or a bare registry host (when `allow_unknown_stores` is set).

## Quick start

```sh
$ gantry serve --addr :8080

# See the configured stores and their capabilities.
$ curl localhost:8080/v1/store

# Copy redis from docker.io into the cache, then have k3s pull it.
$ curl -X POST localhost:8080/v1/job -d '{
    "ref": "library/redis:7", "from": "docker.io", "to": "cache",
    "distribute": ["k3s"], "platforms": ["linux/amd64"]
  }'

# Watch progress (every transfer carries per-layer bytes).
$ curl localhost:8080/v1/job/<id>
$ curl -N localhost:8080/v1/job/<id>/progress   # SSE stream
```

## HTTP API

**Jobs**

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/job` | Submit a job (`ref`, `from`, `to`, `distribute`, `platforms`, `copy_referrers`). `202` created / `200` coalesced onto an identical in-flight move; an `Idempotency-Key` header maps a retry back to the same job. `422` if signature verification is enabled and fails. |
| `POST` | `/v1/job/plan` | Dry-run admission: the resolved store bindings, rewritten cache ref, engine pull refs, and verification outcome — without moving bytes or creating a job. |
| `GET` | `/v1/job` | List jobs; filter with `?state=` (validated), `?ref=`, `?since=`, `?limit=`. |
| `GET` | `/v1/job/{id}` | Job status: a `transfers[]` array (each with per-layer progress) plus the `verification` outcome. |
| `GET` | `/v1/job/{id}/progress` | SSE progress stream, or `?wait=<dur>` long-poll. |
| `POST` | `/v1/job/{id}/retry` | Re-submit a terminal job's original request (fresh resolution + verification). |
| `POST` | `/v1/job/{id}/cancel` | Cancel a running job but keep its record (the terminal state stays inspectable). |
| `DELETE` | `/v1/job/{id}` | Evict a job record (canceling it first if still running). |

**Stores**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/store` | Configured stores with their kind, capabilities, and readiness. |
| `GET` | `/v1/store/{name}/health` | Probe one store's reachability (engine ready-check / registry `/v2/` ping). Cached ~5s; `200` healthy, `503` unhealthy. |
| `GET` | `/v1/store/{name}/inuse` | References and image IDs live containers currently hold on an engine. |
| `POST` | `/v1/store/{name}/pull` | Trigger one engine store to pull a reference (stamps the retention index). |
| `POST` | `/v1/store/{name}/remove` | Delete one image from an engine store (syncs the retention index). |

**Retention** (require `serve.retention`)

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/gc` | GC scheduler status: last run, next wake, grace window, effective policy, per-engine index counts. |
| `GET` · `POST` | `/v1/store/{name}/gc` | `GET` dry-runs the retention policy (keep/delete decision); `POST` applies it. Optional body overrides `max_age`/`keep_n`/`pins`. |
| `GET` · `POST` · `DELETE` | `/v1/store/{name}/pin` | List / add / remove pins (an exact `ref` or a doublestar `pattern`), exempt from GC. |
| `GET` · `DELETE` | `/v1/store/{name}/image` | List the retention inventory (filters: `?repo=`, `?ref=`, `?pinned=`); `DELETE` purges one orphan record without touching the engine. |
| `GET` | `/v1/store/{name}/watcher` | Usage-event stream liveness (a dead stream silently degrades age GC). |

**Verification** (require `serve.verify`)

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/verify` | Verify a reference without moving it — CI can gate a rollout on "this gantry will accept the image". `422` unsigned/untrusted, `404` not found. |
| `GET` | `/v1/verify` | The effective trust configuration: provider, global and per-store modes, policy scopes, and each anchor's subject/fingerprint/expiry (never key material). |
| `POST` | `/v1/verify/reload` | Re-read the CA dir and policy from disk and swap the verifier on success (CA rotation without a restart); the old verifier is retained on failure. |

**Meta**

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v1/event` | The audit log (jobs, GC, pins, manual ops); survives restart. Filters: `?type=`, `?store=`, `?ref=`, `?state=`, `?since=`, `?limit=`. Requires `serve.events`. |
| `GET` | `/v1/version` | Build info (`version`, `git_rev`, `git_dirty`). |
| `GET` | `/openapi.json` · `/openapi.yaml` | The OpenAPI 3.1 schema (exempt from auth). Point any viewer at it. |
| `GET` | `/healthz` | Liveness (exempt from auth). |
| `GET` | `/readyz` | Readiness: aggregate health over the gated stores (`serve.health.ready_stores`), `200`/`503` (exempt from auth; the per-store breakdown is authenticated-only). |

`/v1/*` is guarded by bearer token and/or mTLS when configured (see
`serve.auth`); with neither set, auth is disabled (intended to sit behind a
trusted reverse proxy). `/healthz`, `/readyz`, and `/openapi.*` are always exempt.

A rendered, human-readable reference (every endpoint with parameters, schemas,
and a `curl` sample) is checked in at [docs/api.md](docs/api.md). For an
interactive view, point [Scalar](https://github.com/scalar/scalar),
[Redoc](https://github.com/Redocly/redoc), or
[Stoplight Elements](https://github.com/stoplightio/elements) at the live
`/openapi.json` — the server only ships the contract, not a bundled viewer.

## Configuration

See [gantry.yaml](gantry.yaml) for the full annotated example. Key blocks:

- `serve.stores` — the unified store map (keyed by name). `kind: oci` (`host`,
  `mode`, `insecure`, `rewrite`, `downstream_host`) or `kind: docker`/`containerd`
  (`address`, `namespace`, `pull_host`).
- `serve.allow_unknown_stores` — let a job name a bare registry host not declared
  as a store (default false).
- `serve.warm` — `platforms` fallback, `max_concurrent_jobs`/`max_concurrent_layers`
  pool sizes, `distribute_by_default`.
- `serve.health` — per-store health probe cache: `cache_ttl` (default 5s),
  `probe_timeout` (default 3s), and `ready_stores` (which stores `GET /readyz`
  gates on; empty = every engine store, so a flaky upstream can't flap the node).
- `serve.verify` — source-image signature verification (Notary Project / notation)
  at job creation. `mode` (`off` | `verify-if-present` | `require`), a `trust_store`
  of CA certs (required when enabled — no OS-root fallback; missing ⇒ the server
  refuses to start), an optional notation `trust_policy`, and `level`. A verified
  image is pinned to its digest for the copy/pull. Per-source-registry `verify.mode`
  overrides the global default.
- `serve.retention` — image GC on engine stores (disabled unless `path` is set).
  gantry tracks last-used time from the engine's container events, then keeps
  in-use, `pins` (exact refs or doublestar patterns), the `keep_n` most-recent tags
  per repo, and anything newer than `max_age`; the rest is reclaimed. The scheduler
  is adaptive — it idles up to `interval` and wakes only when a record is about to
  age out or usage changes.
- `serve.events` — the audit log (disabled unless `path` is set): a bounded bbolt
  ring (`cap` entries) of jobs, GC, pins, and manual ops, queryable at `/v1/event`.
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
go test -race ./...   # unit tests always; live docker/containerd tests self-skip without a daemon
go generate ./...     # regenerate the OpenAPI 3.1 spec (swaggo/swag v2) from handler comments,
                      # then render docs/api.md from it (widdershins via scripts/gen-api-docs.sh; needs Node)
```

## Status

Implemented and tested (unit + live docker/containerd integration; `go test -race`):

- **Image movement** — the store/transfer model, `copy`/`proxy` fill, the engine
  pull seam, SSE/long-poll progress. Verified end-to-end against real daemons.
- **Retention / GC** (`serve.retention`) — usage tracking from container events,
  keep-N-per-repo, exact and pattern pins, the adaptive scheduler, and the
  inventory / status / watcher APIs.
- **Signature verification** (`serve.verify`, Notary Project / notation) — verified
  at admission with digest pinning; engine pulls are digest-anchored and signatures
  can be copied into the cache (`copy_referrers`). Preflight, introspection, and
  hot-reload APIs. Tested with in-process notation signing.
- **Job lifecycle** — dry-run plan, retry, idempotency, cancel-vs-evict.
- **Observability** — OTLP metrics/traces (`otel`), a durable audit log
  (`serve.events`), and `/readyz`.

The design docs — [plan.md](plan.md) (movement), [plan-gc.md](plan-gc.md) (retention),
and [plan-api.md](plan-api.md) (the API expansion) — record the rationale and
point-in-time decisions.
