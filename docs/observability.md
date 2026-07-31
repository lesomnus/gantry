# Observability: metrics, audit log, and health

gantry exposes three independent observability surfaces: an OpenTelemetry
pipeline (`otel`) that exports metrics and traces over OTLP, a durable audit log
(`serve.events`) queryable through `EventService`, and the standard
`grpc.health.v1.Health` service for liveness and readiness. Each is opt-in and
works on its own — metrics need an exporter wired, the audit log needs a `path`,
and health is always on. This doc covers the mechanics of all three; the
JobService job-records-vs-durable-history contract lives in `api.md`.

## OTLP pipeline (`otel`)

`otel` is a **top-level** config block (a sibling of `stores` and `worker`, not
nested under `serve`), shared by every subcommand. It is an
[mkot](https://github.com/lesomnus/mkot)-style pipeline: `exporters`,
`processors`, and `providers` (one each for `meter`, `tracer`, `logger`) wired by
name. The full annotated form is in [../gantry.yaml](../gantry.yaml).

Every metric instrument and trace span records to a **no-op** unless a provider
with an exporter is wired here — exporting is a deployment opt-in, so an
unconfigured gantry carries the instruments at zero cost. The `otlp` exporter
(`github.com/lesomnus/mkot/otlp`, registered by import) pushes metrics/traces
over OTLP/gRPC, e.g. to a node-local collector:

```yaml
otel:
  exporters:
    otlp:
      endpoint: "127.0.0.1:4317"
      tls: { insecure: true }        # plaintext; omit for TLS with system roots
      # headers: [ { name: authorization, value: "Bearer ..." } ]
      # interval: 60s                 # metric push period
      # temporality: delta            # cumulative (default) | delta
      # retry_on_failure: { max_elapsed_time: 1m }
  providers:
    meter:  { exporters: [otlp] }
    tracer: { exporters: [otlp] }
```

Regardless of what you wire, gantry always:

- **Stamps the service resource** on every provider — `service.name` = `gantry`
  and `service.version` = the build stamp (`gantry version`). It is prepended, so
  a resource processor you define yourself still wins, and a config that wires its
  own providers never silently loses `service.name`.
- **Registers default providers** `meter`, `tracer`, and `logger` even when the
  block is absent. The `meter` and `tracer` providers carry the service resource
  but have no exporter until you add one (hence no-op). The `logger` provider is
  wired to a built-in `pretty` console exporter, which renders gantry's structured
  application logs — this is why logs appear on stdout with no `otel` block at all.

### Traces

Each job runs under a `job` span carrying a `gantry.ref` attribute; a failing job
records the error on its span (`span.RecordError`). Cancellation is a normal
terminal state and is **not** recorded as a span error. Spans export only when the
`tracer` provider has an exporter wired.

## Metric instruments

Every instrument is namespaced `gantry.*`. Observable gauges are sampled at each
collection cycle; counters and histograms record inline as work happens. All are
no-ops until the `meter` provider has an exporter.

### Job pipeline

| Instrument | Kind | Unit | Attributes | Meaning |
|---|---|---|---|---|
| `gantry.bytes` | counter | `By` | — | Bytes moved between stores, summed once per finished job — across every hop and every attempt, so a routed job reports roughly twice its image size and an abandoned attempt's partial transfer counts too. |
| `gantry.job.duration` | histogram | `s` | `state` (`done`/`failed`/`canceled`) | Wall-clock duration of each job, labelled by its terminal state. |
| `gantry.jobs.active` | up-down counter | — | — | Jobs currently executing (in flight). |
| `gantry.job.route` | counter | — | `decision` (`filled`/`warm`/`proxy`/`joined`/`declined`/`rejected`), `source`, `cache`, `reason` (present on a decline or a rejection) | What gantry decided about routing a job through its source's declared `cache`, counted once at admission for every job whose source declares one — and only those, so `sum by (decision)` is a complete breakdown of the routed population. `filled`/`warm`/`proxy`/`joined` are the route being used; their ratio is whether the cache is earning its keep. `declined` is gantry choosing not to route, and it is the metric's reason for existing: a declined route is otherwise invisible — the job is `done`, the target has the image, and the only difference is how many times the origin was read — so a permanently dead route looks exactly like a healthy one everywhere else. Alert on a sustained non-zero `declined`, then read `reason`: `cache_undeclared`/`cache_unresolved`/`cache_not_a_registry` are configuration, `cache_unreadable` is a target that can never read the cache (`pull_host`, a proxy target), `probe_failed` is the cache not answering, `referrers_incomplete`/`referrers_unverifiable` are a cache that cannot supply the signatures the job needs, `no_such_image` is the authority saying it does not have the reference, and `target_is_cache`/`source_is_cache` are ordinary jobs that name one end of the route. `rejected` is the job being refused outright (`require_authority`, or a planning error). See [stores.md](stores.md#routing-a-copy-through-a-cache). |
| `gantry.job.fallback` | counter | — | `from`, `to` (store names; `to` may be `(none)`), `reason` (`planned`/`route`) | gantry gave up on a source it meant to use. The `reason` names what was GIVEN UP, never what was taken up: `route` is a cache gantry chose for itself being abandoned — the signal that it is not earning its keep — and `planned` is the source the caller named being left for another, which is what a fallback to the origin reports. (`origin` is never emitted: the origin attempt is always last, so it is never the one given up.) It fires when an attempt fails and when one is skipped because the hop that would have filled it did not deliver, since a route that never ran is as unused as one that failed. `to` is the source taken up instead, or `(none)` when there was none. Non-zero means images are reaching nodes from somewhere other than where the operator pointed them — a cache quietly not being used looks like success everywhere else. See [stores.md](stores.md#falling-back-to-the-origin). |
| `gantry.job.source_wait` | histogram | `s` | `outcome` (`served`/`timeout`/`canceled`/`skipped`) | Time an engine pull spent waiting for an in-flight job filling its source (`worker.source_wait`). `served` means the wait paid off; a lot of `timeout` means the bound is too short or the fills are too slow; `skipped` means no wait slot was free. |
| `gantry.jobs` | observable gauge | — | `state` (`pending`/`running`/`done`/`failed`/`canceled`) | Count of in-memory job records by state — the live registry the `job_ttl` sweeper trims. |
| `gantry.queue.depth` | observable gauge | — | — | Jobs waiting in the pending-job queue (visible before a full queue starts rejecting with `RESOURCE_EXHAUSTED`). |
| `gantry.queue.capacity` | observable gauge | — | — | The queue buffer size (`worker.queue_size`). |

### Health

| Instrument | Kind | Unit | Attributes | Meaning |
|---|---|---|---|---|
| `gantry.health.probe.duration` | histogram | `s` | `store`, `kind`, `healthy` | Duration of one store health probe, labelled by store name, kind, and probe outcome. Created lazily on the first probe. |

### Retention / GC (per engine store)

Every retention instrument carries a `store` attribute (the engine store name), so
each store's GC is tracked independently — there is no global retention policy.
See `retention.md` for what these numbers mean.

| Instrument | Kind | Attributes | Meaning |
|---|---|---|---|
| `gantry.retention.records` | observable gauge | `store` | Retention index records (tracked images) in the store's index. |
| `gantry.retention.pins` | observable gauge | `store` | Pinned references in the store's index. |
| `gantry.retention.untagged` | observable gauge | `store` | Tracked untagged images with a reap clock running. |
| `gantry.retention.watcher.connected` | observable gauge | `store` | `1` when the usage watcher's event stream is connected, `0` otherwise — alert on `0`, since a dead stream freezes last-used time and silently degrades age GC. |
| `gantry.retention.gc.deleted` | counter | `store` | Images whose content a GC pass freed. |
| `gantry.retention.gc.untagged` | counter | `store` | Refs a GC pass untagged (content may remain). |
| `gantry.retention.gc.reaped` | counter | `store` | Untagged images a GC pass reaped. |
| `gantry.retention.gc.errors` | counter | `store` | Per-ref removal failures during a GC pass. |

The four GC counters increment only when a pass actually removes something; the
gauges are re-observed from each store's index on every collection cycle.

## Audit log (`serve.events`)

The audit log is an append-only record of the operationally significant things
gantry did — the durable history the in-memory job registry cannot provide, since
that registry is lost on restart. It is **independent of retention**, so it works
even when GC is disabled, and enabled only when a `path` is set:

```yaml
serve:
  events:
    path: "/var/lib/gantry/events.db"   # bbolt file; empty disables the log
    cap: 10000                          # max entries retained (default 10000)
```

### Bounded bbolt ring

The log is a bbolt file holding a monotonically-sequenced ring of at most `cap`
entries (default `10000` when `cap` ≤ 0). Each `Append` assigns the next sequence
number and a UTC timestamp, then evicts everything at or before `newest − cap`, so
the file stays bounded — lowering `cap` on a restart shrinks it on the next write.
Sequence numbers are never reused, so an evicted entry's `seq` becomes permanently
`NotFound`.

Writing an audit event must never break the operation it records, so an `Append`
failure (full disk, locked/corrupt db) is swallowed rather than propagated — but
it is surfaced: each drop is logged at `WARN` with a running `dropped_total`, so a
silently failing log is observable.

### Event types

Seven event types are recorded, each carrying best-effort context relevant to its
type:

| Type | Emitted when | Carries |
|---|---|---|
| `job_admitted` | a job is accepted into the pipeline | target `store`, source `ref`, pinned `digest`, `detail.job` (id) + `detail.source` |
| `job_done` | a job reaches a terminal state | `ref`, `state` (`done`/`failed`/`canceled`), `error`, `detail.job` + `detail.bytes` |
| `job_fallback` | a job's source could not serve it and another was tried | `ref`, `store` (the source tried instead), `error` (why the first one failed), `detail.job` + `detail.source` (the source that failed) |
| `gc_applied` | a GC pass removes something | `store`, `detail.{deleted,untagged,reaped,errors}` |
| `image_pulled` | a manual `StoreService.Pull` completes | `store`, `ref` |
| `image_removed` | a GC deletion or `StoreService.Remove` | `store`, `ref` |
| `pinned` | `PinService.Add` | `store`, `ref` (the pin value) |
| `unpinned` | `PinService.Erase` | `store`, `ref` (the pin value) |

`job_fallback` is the durable counterpart of the failed transfer row on a job
that nevertheless completed: the in-memory job record is emptied on restart, but
a cache quietly falling out of use is worth still knowing tomorrow. See
[stores.md](stores.md#falling-back-to-the-origin).

A job's full durable lifecycle is the `job_admitted` → `job_done` pair correlated
by `detail.job` (the job id): the admitted event names the source and pinned
digest, the done event carries the final state, error, and bytes moved. This is
how you reconstruct a job after a restart — see the job-records-vs-history
contract in `api.md`.

### EventService: Get, List, filters, pagination

`EventService` gates on the log being enabled: every RPC answers
`FAILED_PRECONDITION` ("the audit log is not enabled") when `serve.events.path` is
unset. It is a read-only, resource-oriented view — writing events is internal, so
there is no `Add`.

- **`Get`** takes an event `seq` (required; `0` is `INVALID_ARGUMENT`) and returns
  that one event, or `NotFound` if it was never written or has been evicted by the
  ring. `Get` reaches events older than a `List` page can.
- **`List`** returns matching events **newest-first**, with optional filters
  `type`, `store`, `ref`, `state`, and `since` (an unknown `type` or `state` is
  `INVALID_ARGUMENT`). It paginates with `page_size` / `page_token`: `page_size`
  defaults to 100 and is hard-capped at **1000** — older events beyond the last
  page are reachable only by `Get`. The page token is a decimal offset;
  `page_size` must not be negative.

## Health and readiness

### Per-store health probe (`StoreService.Health`)

`StoreService.Health` probes whether a single configured store is reachable. An
**engine** store (docker/containerd) is probed with its daemon ready-check; a
**registry** store with a `GET /v2/` ping, where a `200` or a `401` auth challenge
both count as reachable (bearer auth is intentionally not attempted — reachability
is the signal). The probe uses the store's real outbound transport (TPM mTLS,
private CA, or insecure skip), so an mTLS/private-CA registry is not falsely
reported unhealthy. An unhealthy store is a **report** (`healthy: false` with the
error), not an RPC failure; only an unknown store name is a `NotFound` error.

Probes are governed by `serve.health`:

```yaml
serve:
  health:
    cache_ttl: "5s"        # how long a probe result is reused (default 5s)
    probe_timeout: "3s"    # per-probe deadline; a hung backend fails here (default 3s)
    # ready_stores: ["cache"]
```

Results are cached for `cache_ttl` so frequent polling (load balancers,
dashboards) does not hammer backends, and concurrent callers for the same store
coalesce onto a single probe (singleflight). Each probe runs under a
`probe_timeout` deadline detached from the caller's context, so one caller
disconnecting cannot abort a probe others are waiting on. Every probe also records
`gantry.health.probe.duration`. A report flags `cached: true` when served from the
TTL cache rather than freshly probed.

### `grpc.health.v1.Health` (liveness and readiness)

gantry registers the standard `grpc.health.v1.Health` service (`Check` and
`Watch`). It is always registered and, like server reflection, **exempt from
bearer-token auth** — it exposes liveness and readiness, not data. The overall
serving status (the empty `""` service name) tracks **readiness**: a background
loop re-evaluates it every 5s and flips it between `SERVING` and `NOT_SERVING`.

Readiness gates on the stores named in `serve.health.ready_stores`. When that list
is **empty** it defaults to **every engine store** — a flaky remote upstream
registry must not flap the node's readiness, so registries join the gate only by
being listed explicitly (e.g. the local cache). The status is `SERVING` only when
every gated store's health probe currently passes; the probes reuse the same
TTL-cached checker, so readiness polling is cheap.

### Watcher health via `StoreService.GcStatus`

The retention usage-watcher's health is reported through `StoreService.GcStatus`
(not the health service): its `watcher` field carries `connected`, `watching_since`,
`date_last_event`, `date_last_seed`, `reconnects`, and `last_error`. A watcher with
`connected: false` or a stale `date_last_event` means the engine's event stream
died and last-used time is frozen — the same condition the
`gantry.retention.watcher.connected` gauge exposes for alerting. See
`retention.md` for the full GC status surface and `stores.md` for store
configuration.
