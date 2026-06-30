# gantry

Warm a local pull-through **cache container registry**, expose an HTTP API to
see which images were pulled and how much, and trigger downstream docker /
containerd daemons to pull the warmed images from the cache.

## How it works

```
POST /v1/job ──▶ worker warms the cache ──▶ GET /v1/job/{id} (live progress)
                   (copy: upstream→cache push,         │
                    or proxy: read-through self-fill)   ▼
                                              fan-out to downstream targets
                                              (docker / containerd pull from cache)
```

- **copy mode** (default): gantry pulls each blob from upstream and pushes it
  into the cache, skipping blobs already present, so progress reflects bytes
  actually moved into the cache. The cache must be a writable registry.
- **proxy mode**: gantry reads each blob through the cache so a pull-through
  cache (zot / distribution proxy) self-fills from upstream.

A request always names the **canonical upstream reference**; the cache-side
reference is derived from `registry.rewrite` (ordered `{glob: template}` rules,
first match wins) and is used for the push/pull and the downstream trigger.

## Quick start

```sh
# Edit gantry.yaml: set registry.host, rewrite rules, and downstream targets.
$ gantry serve --addr :8080

# Warm an image (canonical upstream ref) and trigger the configured targets.
$ curl -X POST localhost:8080/v1/job \
    -d '{"ref":"docker.io/library/redis:7","platforms":["linux/amd64"]}'

# Watch progress.
$ curl localhost:8080/v1/job/<id>
$ curl -N localhost:8080/v1/job/<id>/progress   # SSE stream
```

## HTTP API

| Method | Path | Purpose |
|---|---|---|
| `POST` | `/v1/job` | Submit a warm job (idempotent per ref + platform set). |
| `GET` | `/v1/job` | List jobs; filter with `?state=` and `?ref=`. |
| `GET` | `/v1/job/{id}` | Job status with per-layer and per-target progress. |
| `GET` | `/v1/job/{id}/progress` | SSE progress stream, or `?wait=<dur>` long-poll. |
| `DELETE` | `/v1/job/{id}` | Cancel an in-flight job / evict a finished one. |
| `GET` | `/v1/target` | Configured downstream targets and their capabilities. |
| `POST` | `/v1/target/{name}/pull` | Trigger one target to pull a reference. |
| `GET` | `/healthz` | Liveness (exempt from auth). |

`/v1/*` is guarded by bearer token and/or mTLS when configured (see
`serve.auth`); with neither set, auth is disabled (intended to sit behind a
trusted reverse proxy).

## Configuration

See [gantry.yaml](gantry.yaml) for the full annotated example. Key blocks:

- `serve.registry` — cache `host`, `mode` (`copy`/`proxy`), `insecure`, and the
  `rewrite` rules (`{{.CacheHost}} {{.Registry}} {{.Repo}} {{.Tag}} ...`).
- `serve.warm` — `platforms` fallback, `max_concurrent_jobs`/`max_concurrent_layers` pool sizes.
- `serve.targets` — downstream `docker`/`containerd` daemons (`address`, and
  `namespace` for containerd, e.g. `k8s.io` for k3s).
- `serve.auth` — `tokens` (env-expanded), `client_ca`, and server `tls_cert`/`tls_key`.

## Development

The devcontainer runs gantry against a Docker-in-Docker daemon (and its bundled
containerd). Test layout, the live integration tests, the full warm→distribute
loop, and the insecure-registry constraints are documented in
[docs/test-environment.md](docs/test-environment.md).

```sh
go test -race ./...   # unit tests always; live docker/containerd tests self-skip without a daemon
```

## Status

Phase 1 (warm + status API + downstream trigger seam) is implemented and
verified end-to-end against real docker and containerd daemons. Planned next:
image signature verification and downstream image GC, which bolt onto the target
seam as optional capabilities. See [plan.md](plan.md).
