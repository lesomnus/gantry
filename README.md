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

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/job` | Submit a job (`ref`, `from`, `to`, `distribute`, `platforms`). Idempotent per identical move. |
| `GET` | `/v1/job` | List jobs; filter with `?state=` and `?ref=`. |
| `GET` | `/v1/job/{id}` | Job status: a `transfers[]` array, each with per-layer progress. |
| `GET` | `/v1/job/{id}/progress` | SSE progress stream, or `?wait=<dur>` long-poll. |
| `DELETE` | `/v1/job/{id}` | Cancel an in-flight job / evict a finished one. |
| `GET` | `/v1/store` | Configured stores with their kind, capabilities, and readiness. |
| `POST` | `/v1/store/{name}/pull` | Trigger one engine store to pull a reference. |
| `POST` | `/v1/store/{name}/remove` | Delete one image from an engine store (syncs the retention index). |
| `GET` · `POST` | `/v1/store/{name}/gc` | `GET` dry-runs the retention policy (keep/delete decision); `POST` applies it. Optional body overrides `max_age`/`keep_n`/`pins`. |
| `GET` · `POST` · `DELETE` | `/v1/store/{name}/pin` | List / add / remove pinned references (exempt from GC). |
| `GET` | `/openapi.json` · `/openapi.yaml` | The OpenAPI 3.1 schema (exempt from auth). Point any viewer at it. |
| `GET` | `/healthz` | Liveness (exempt from auth). |

`/v1/*` is guarded by bearer token and/or mTLS when configured (see
`serve.auth`); with neither set, auth is disabled (intended to sit behind a
trusted reverse proxy).

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
- `serve.retention` — image GC on engine stores (disabled unless `path` is set).
  gantry tracks last-used time from the engine's container events, then keeps
  in-use, `pins`, the `keep_n` most-recent tags per repo, and anything newer than
  `max_age`; the rest is reclaimed. The scheduler is adaptive — it idles up to
  `interval` and wakes only when a record is about to age out or usage changes.
- `serve.auth` — `tokens` (env-expanded), `client_ca`, and server `tls_cert`/`tls_key`.

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

Phase 1 (move images between stores + status API + engine pull seam) is
implemented and verified end-to-end against real docker and containerd daemons.
Image retention/GC (`serve.retention`: usage tracking, keep-N-per-repo, pins,
adaptive scheduler, `/gc` · `/pin` · `/remove` APIs) is implemented and tested
against a live docker daemon. Planned next: image signature verification, which
bolts onto the same engine seam as an optional capability. See [plan.md](plan.md).
