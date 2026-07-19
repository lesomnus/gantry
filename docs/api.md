# gRPC API behaviors

gantry serves its entire surface over gRPC. This doc covers the contract and its
code generation, the per-service behaviors, and the cross-cutting mechanics a
client needs to use it correctly: response trailers, the `Idempotency-Key`
metadata, coalescing and its dedup key, dry-run `Plan`, the live-vs-durable job
model, pagination, stable resource ids, and authentication. For what a job
actually does once admitted see the sibling docs — `stores.md` (the store
model), `verification.md` (signature verification), `retention.md` (GC), and
`observability.md` (metrics, the audit log, and health). The full annotated
config lives in [`../gantry.yaml`](../gantry.yaml).

## Contract & code generation

The contract lives in [`proto/gantry`](../proto/gantry): the entity messages
(`Job`, `Store`, `Image`, `Pin`, `Event`) plus the merged service definitions,
compiled to Go under the [`pb`](../pb) package
(`github.com/lesomnus/gantry/pb`). The package ships generated Go stubs,
ergonomic request constructors (the edition-2023 opaque-message builder API,
e.g. `pb.JobListResponse_builder{…}.Build()`), and client/server wiring
(`pb.RegisterServer` registers `JobService` / `StoreService` / `ImageService` /
`PinService` / `EventService` in one call; `VerifyService` is registered
separately).

Codegen runs `scripts/gen-proto.sh`: `protoc-gen-orm-service` emits the CRUD
services from the orm-annotated entities, `protobuf-merge` overlays the
hand-written RPCs (`proto.svc/`), then `protoc-gen-go` / `protoc-gen-go-grpc` /
`protoc-gen-orm-go` compile everything into `pb/`. `VerifyService` is
hand-written (`verify_svc.proto`), not orm-generated, which is why it is
registered alongside rather than by `pb.RegisterServer`.

### Resource-oriented CRUD

Entity services follow a resource-oriented CRUD shape: `Add` / `Get` / `Patch`
return the entity, `Erase` returns `google.protobuf.Empty`, and a per-entity
`Ref` message addresses one instance by key or unique index (`JobRef{id}`,
`StoreRef{name}`, `EventRef{seq}`, and `ImageRef` / `PinRef`, each a oneof of a
raw `id` or a locator — `ImageRefByLocator{store, ref}`,
`PinRefByValue{store, value}`). On top of that base, `List` and the custom
actions are merged in: `JobService` adds `Watch` / `Plan` / `Cancel` / `Retry`,
`StoreService` adds `Pull` / `Remove` / `Health` / `GcStatus` / `GcPlan` /
`GcApply`, and `VerifyService` is entirely custom (`Describe` / `Check` /
`Reload`).

### Writes without a domain operation answer `UNIMPLEMENTED`

The generated CRUD includes write RPCs gantry has no domain operation for, and
those answer `codes.Unimplemented`:

- **Stores are declared in configuration.** `StoreService.Add` / `Patch` /
  `Erase` return `UNIMPLEMENTED` with an explicit message — *"stores are
  declared in gantry.yaml; the API cannot create, modify, or delete them."*
- **Image records and audit events are written internally.**
  `ImageService.Add` / `Patch`, `EventService.Add` / `Patch` / `Erase`, and
  `PinService.Patch` are left on the embedded `Unimplemented…Server` default (a
  generic `UNIMPLEMENTED`). What *is* implemented is the operational subset —
  `ImageService.Erase` purges an orphan index record, `PinService.Add` upserts a
  pin — so the table below is the source of truth for which RPCs act.
- **Jobs are immutable after submission.** `JobService.Patch` is deliberately
  left `UNIMPLEMENTED`: a client cannot rewrite a submitted job (labels
  included — they are fixed at submission). `Patch` remains in the generated API
  only as a test affordance.

### Addressing: declared stores only

Every store reference on the gRPC surface is a `StoreRef{name}`, so the gRPC API
addresses **declared stores only**. Unlike the REST surface, a bare registry
host allowed by `serve.allow_unknown_stores` is *not* expressible through a
`StoreRef` — name the store in `stores` to move through it over gRPC. An engine
GC RPC additionally rejects a non-engine store with `NOT_FOUND` and, if the
store is an engine but has no `retention` configured, `FAILED_PRECONDITION`.

## Services at a glance

| Service | RPCs | Notes |
|---|---|---|
| `JobService` | `Add` · `Get` · `List` · `Erase` · `Watch` · `Plan` · `Cancel` · `Retry` | `Add` submits a move and coalesces onto an identical in-flight one (a `gantry-coalesced` trailer flags **whether** the submit joined one, not which) and honors `idempotency-key` request metadata. `Watch` streams job snapshots until terminal; `Plan` is dry-run admission; `Cancel` detaches this caller's handle; `Erase` evicts (cancels first when running); `Retry` re-submits a terminal job. Jobs carry a free-form `labels` map (filterable on `List`). `Patch` is `UNIMPLEMENTED`. |
| `StoreService` | `Get` · `List` · `Pull` · `Remove` · `Health` · `GcStatus` · `GcPlan` · `GcApply` | Stores are declared in config; `Add` / `Patch` / `Erase` answer `UNIMPLEMENTED`. `Pull` / `Remove` drive one engine daemon (and keep the retention index in sync). The GC RPCs need the store's `retention`; `GcPlan` dry-runs, `GcApply` executes, both take a one-shot policy override. `GcStatus` includes the usage-watcher health. See `retention.md`. |
| `ImageService` | `Get` · `List` · `Erase` | The retention inventory. `List` filters by `repo` / `ref` / `pinned` / `in_use` (the live daemon set) and carries the untagged reap clocks on unfiltered lists; `Erase` purges an orphan record without touching the engine. `Add` / `Patch` answer `UNIMPLEMENTED`. |
| `PinService` | `Add` · `Get` · `List` · `Erase` | GC exemptions: an exact ref or a doublestar `pattern`. `Add` upserts and echoes the pin's blast radius as `gantry-pin-matched-count` / `gantry-pin-matched` trailers; `Erase` is idempotent. `Patch` answers `UNIMPLEMENTED`. |
| `EventService` | `Get` · `List` | The audit log (requires `serve.events`, else `FAILED_PRECONDITION`); newest-first with `type` / `store` / `ref` / `state` / `since` filters. `Add` / `Patch` / `Erase` answer `UNIMPLEMENTED`. See `observability.md`. |
| `VerifyService` | `Describe` · `Check` · `Reload` | Trust introspection (never key material), preflight ("would this gantry accept the image"), and truststore hot-reload for CA rotation. See `verification.md`. |
| `grpc.health.v1.Health` | `Check` · `Watch` | Liveness and readiness: the overall status follows aggregate health over the gated stores (`serve.health.ready_stores`; empty = every engine store). Public. See `observability.md`. |

### Status-code mapping

The service layer maps domain errors onto gRPC codes consistently:

- `INVALID_ARGUMENT` — missing/blank required field (`ref`, `id`, store name,
  pin value), a bad page token, a negative `page_size`, an invalid doublestar
  pattern, an unknown enum value, or an otherwise-unclassified submit error.
- `FAILED_PRECONDITION` — a verification rejection (unsigned / untrusted
  source), `Retry`/`Cancel` on a job that is already terminal or active,
  retention/GC not enabled for the store, or the audit log disabled.
- `RESOURCE_EXHAUSTED` — the job queue is full.
- `NOT_FOUND` — no such job, image record, pin, store, or event; also an unknown
  store name or a non-engine store on an engine-only RPC.
- `UNAVAILABLE` — the engine daemon failed a `Pull` / `Remove`, a GC
  `Plan` / `Apply`, or an in-use lookup.
- `UNIMPLEMENTED` — a write RPC with no domain operation (see above).

## Coalescing and the `gantry-coalesced` trailer

`JobService.Add` collapses identical in-flight moves: if an active job already
matches the submit's dedup key, `Add` attaches a fresh caller handle to that job
rather than starting a second copy, and returns that job's snapshot. Because
`Add` returns the bare `Job`, the "did I join one?" bit rides a **response
trailer**: `gantry-coalesced: true|false`. It flags *whether* the submit joined
an existing move, not *which* — read `Plan`'s `coalesces` first if you need the
target job's id. Each coalesced caller keeps its own distinct handle
server-side, so their `labels` and `Cancel` are independent (`Cancel` detaches
only the calling handle; the shared move stops once its last caller cancels).
`Retry` emits the same `gantry-coalesced` trailer, since a retry can likewise
land on an already-active identical move.

## `Idempotency-Key` metadata

`Add` honors an `idempotency-key` request-metadata entry. On a repeat with a
remembered key, gantry **replays the remembered job** — returns its current
snapshot with `gantry-coalesced: true` — and does *not* re-run the move, **even
if the request body differs**: the key alone wins. gantry remembers the
key→job-id mapping only while the job record lives; the mapping is swept
together with the finished job record after `worker.job_ttl` (default `30m`),
after which the same key is a miss and submits a fresh move. Use it to make a
client retry (network blip, restart) safe without double-moving an image.

## Dedup key and mutable-tag stability

Coalescing (and idempotency's "identical move" notion) keys on the tuple
**(`ref`, `platforms`, `source`, `target`, `as`)**. Two submits that differ in
any of those five run as separate jobs; two that match an active job coalesce.

A **tag is treated as stable for the life of an active job**: a tag re-pushed
mid-job does *not* start a second copy until the first finishes — the second
`Add` coalesces onto the first. With digest pinning (a digest ref, or a verified
source, see `verification.md`) the first job carries the digest resolved at
**admission**, i.e. the pre-repush image; the re-pushed content moves only once
that job ends and a new submit re-resolves the tag.

## `Plan` — dry-run admission

`JobService.Plan` runs the full admission path for a would-be job — store
binding, platform selection, `as` normalization, signature
verification, and coalescing — **without submitting**. `JobPlanResponse`
returns the resolved plan:

- `source` / `target` — the resolved store names.
- `source_ref` — the source-side ref, digest-pinned when the source verified.
- `target_ref` — the target-side ref: the rewritten cache ref for a registry
  target, or the ref the engine is told to pull.
- `platforms` — the chosen platforms (empty = all, for a registry target; the
  single platform for an engine target).
- `as` — the normalized names the engine would record the image under.
- `copy_referrers` — the effective value after the server default is applied
  (on by default when the job verified a signature and platforms weren't
  narrowed).
- `verification` — the admission verification outcome, when verification ran.
- `coalesces` — the id of the active job an identical `Add` would coalesce onto,
  if any (empty otherwise).

`Plan` surfaces a verification rejection as `FAILED_PRECONDITION` and any other
resolution error as `INVALID_ARGUMENT`, so it doubles as a preflight for job
admissibility.

## Job records vs. history

`JobService.Get` and `List` read the **live, in-memory** job registry: the
snapshots that carry real-time per-layer byte progress and the cancel handles.
That registry holds only jobs seen **since the process started** and is
**emptied on restart**. `List` returns newest-first and filters by `state`,
`ref` (substring), `since`, and `labels` (subset match — a job matches when it
carries every requested key with the given value; an empty map matches all).

A job's **durable** lifecycle lives in the audit log instead: a `job_admitted`
event at submission and a `job_done` event at completion, correlated by job
**id** and carrying source/target, final state, error text, and bytes moved.
After a restart, reconstruct history through `EventService` (requires
`serve.events`; see `observability.md` for the audit-log mechanics). gantry does
**not** resurrect or resume interrupted jobs on restart — re-submit to continue,
or `Retry` a terminal job's original request as a fresh job with new resolution
and verification. Because copies are content-addressed, blobs already at the
target are skipped, so a re-submit picks up cheaply.

`Watch` is the streaming counterpart of `Get`: it sends a `Job` snapshot roughly
every 250 ms and **ends after the snapshot that carries a terminal state**
(`DONE` / `FAILED` / `CANCELED`); it also ends cleanly if the record is evicted
mid-stream. `Erase` evicts the record and cancels a still-running job first;
`Cancel` stops the job but keeps the record for inspection (returning
`FAILED_PRECONDITION` if it was already terminal).

## Pagination

Every `List` RPC paginates with `page_size` and `page_token`. The token is an
opaque **decimal offset** into the stable-ordered full result; the response's
`next_page_token` is empty once the list is exhausted. `page_size` of `0` means
no limit; a negative `page_size` is `INVALID_ARGUMENT`, as is a non-numeric or
negative token.

`EventService.List` is the one bounded list: the underlying audit log serves
newest-first with a **default of 100** events and a **hard cap of 1000** — so a
`page_size` above 1000, or `0` on the event log, still yields at most that many
per query. Events older than the served window are reachable only by
`EventService.Get` on their `seq`.

## Stable resource ids

`Job` ids are server-generated opaque strings (e.g. `job_1f2e…`). `Event`s are
keyed by a monotonic `seq` (uint64). `Image` and `Pin` ids are **deterministic
UUIDv5** synthesized over a fixed gantry namespace from the resource's composite
identity — `(store, ref)` for an image, `(store, value)` for a pin — so a given
image/pin keeps the same id across process restarts and index rebuilds. Because
the id is derived (not stored as the primary key), a `Get`/`Erase` by raw `id`
scans the retention-managed stores' records for the match; addressing by locator
(`{store, ref}` / `{store, value}`) skips the scan.

## Pin match trailers

`PinService.Add` creates or refreshes a pin (an upsert: pinning an existing
value refreshes `date_pinned` and may flip its `pattern` flag) and echoes the
pin's **current blast radius** as non-blocking response trailers:

- `gantry-pin-matched-count` — how many index records the pin currently
  protects.
- `gantry-pin-matched` — the matching refs themselves, capped at 50 entries
  (present only when there is at least one).

This makes a careless broad `pattern` — one that would neutralize GC across a
whole repo — visible at the moment it is set. A `pattern` pin whose value fails
`doublestar` validation is rejected `INVALID_ARGUMENT` (fail-closed: a malformed
pattern would never match and would silently let its images GC). `Erase` is
idempotent — unpinning a value that isn't pinned still succeeds. See
`retention.md` for how pins participate in the GC decision.

## Authentication and transport security

Every RPC is guarded by a **bearer token** when `serve.auth.tokens` is set:
supply `authorization: Bearer <token>` request metadata. Tokens are
env-expanded (empty results dropped) and compared in constant time; any one
match authorizes the call. With **no** tokens configured, auth is **disabled** —
every RPC, including destructive ones (`StoreService.Remove`, `GcApply`, the
`PinService` writes), is open, so this mode is intended to sit behind a trusted
network or an authenticating proxy (gantry logs a startup warning when auth is
off).

Two services are **always exempt**, even with tokens set:
`grpc.health.v1.Health` (the standard health/readiness service) and
`grpc.reflection.*` (server reflection). They expose liveness and the schema,
not the data — server reflection is on and public, which is why `grpcurl` works
without proto files.

For transport security, serve TLS directly with `serve.auth.tls_cert` /
`serve.auth.tls_key`, or terminate TLS (or mTLS) at a reverse proxy in front of
gantry. (Outbound TLS/mTLS to the stores gantry *talks to* is configured
per-store; see `stores.md`.)
