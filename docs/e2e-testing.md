# End-to-end testing

A full-stack, feature-oriented test plan. It stands up a realistic
**remote → cache → engine** pipeline — the shape of a real gantry deployment — and
exercises each feature a user actually drives, giving the exact request and how to
confirm the result.

This complements [development.md](development.md), which covers the Go unit and
package-level integration tests. Those verify the mechanics in isolation; the
tests here drive a **running gantry** against **real registries and a real
daemon**, so they validate the whole system and double as reproductions of
user-facing behavior. Every scenario is manual and observable — submit a job over
gRPC, then inspect the registries and the daemon.

For the design behind each feature, follow the topic links: [stores.md](stores.md),
[retention.md](retention.md), [verification.md](verification.md),
[observability.md](observability.md), [api.md](api.md).

## The standard environment

```
┌───────────┐   gantry pulls     ┌───────────────┐   gantry pushes    ┌───────────┐
│  remote   │ ────────────────▶ │    gantry     │ ────────────────▶ │   cache   │
│ registry  │   (copy source)    │  (dev, :18080)│   (copy target)    │ registry  │
└───────────┘                    └───────────────┘                    └─────┬─────┘
 127.0.0.1:5001                                                             │ engine pulls
 (upstream images)                                                          ▼
                                                                      ┌───────────┐
                                                                      │   edge    │
                                                                      │  (docker) │
                                                                      └───────────┘
                                                                    tcp://docker:2375
```

| Store    | Kind      | Role |
|----------|-----------|------|
| `remote` | `oci`     | Upstream registry holding the source images. gantry reads (pulls) from it — stands in for docker.io or a private registry. |
| `cache`  | `oci`     | The cache registry gantry copies into and the engine pulls from (`mode: copy`). |
| `edge`   | `docker`  | A downstream engine (a fleet node). gantry tells it to pull from `cache`. |

Adding a `containerd` engine is the same shape; a few features (digest `as` names)
are containerd-image-store specific and are called out where they apply. The
[development.md](development.md) devcontainer already provides the docker daemon
and a containerd sidecar.

### Bring-up (in the devcontainer)

Both registries run on the DinD daemon; the docker engine is that same daemon.
Because a daemon only pulls an insecure (plain-HTTP) registry from its **own
loopback** without extra config (see [Insecure
constraint](development.md#insecure-registry-constraint-loopback-only)), publish
the registries on the dind host and forward the same `127.0.0.1` ports inside
`dev` so gantry uses the identical references.

```sh
# Two registries on the dind daemon: remote (upstream) and cache.
docker run -d -p 5001:5000 --name remote registry:2
docker run -d -p 5000:5000 --name cache  registry:2

# Forward both ports into dev so gantry reaches them at the same 127.0.0.1 the
# daemon uses. Run the tiny forwarder from development.md once per port,
# 127.0.0.1:5000→docker:5000 and 127.0.0.1:5001→docker:5001.
curl -s -o /dev/null -w "cache %{http_code}\n"  http://127.0.0.1:5000/v2/   # 200
curl -s -o /dev/null -w "remote %{http_code}\n" http://127.0.0.1:5001/v2/   # 200

# Seed the remote with an upstream image (the daemon pushes to its own loopback).
docker pull busybox:1.36
docker tag  busybox:1.36 127.0.0.1:5001/library/busybox:1.36
docker push 127.0.0.1:5001/library/busybox:1.36
```

`gantry-e2e.yaml`:

```yaml
serve:
  addr: "127.0.0.1:18080"
  events: { path: "/tmp/gantry-e2e/events.db" }   # audit log, for the EventService tests
stores:
  remote: { kind: "oci", host: "127.0.0.1:5001", insecure: true }
  cache:  { kind: "oci", host: "127.0.0.1:5000", insecure: true, mode: "copy" }
  edge:
    kind: "docker"
    address: "tcp://docker:2375"
    retention:                                    # for the GC tests (test 9)
      path: "/tmp/gantry-e2e/edge.db"
      rules:
        - repo: "**"
          keep_n: 2
```

> Retention is configured **per engine store**, so the `retention` block sits on
> `edge` (the daemon) — a `retention` block on an `oci` store like `cache` is
> rejected at startup. See [retention.md](retention.md).

Start gantry and smoke-test the surface:

```sh
mkdir -p /tmp/gantry-e2e
go run . --config gantry-e2e.yaml serve &

grpcurl -plaintext 127.0.0.1:18080 gantry.StoreService/List
# expect: remote / cache / edge, with capabilities and ready=true
```

### Test helpers

```sh
G="grpcurl -plaintext 127.0.0.1:18080"

add()   { $G -d "$1" gantry.JobService/Add | sed -n 's/.*"id": "\([^"]*\)".*/\1/p'; }
get()   { $G -d "{\"ref\":{\"id\":\"$1\"}}" gantry.JobService/Get; }
watch() { $G -d "{\"id\":\"$1\"}" gantry.JobService/Watch; }   # streams to terminal state
plan()  { $G -d "$1" gantry.JobService/Plan; }

# Inspect the stores directly:
cache_tags()  { curl -s "http://127.0.0.1:5000/v2/$1/tags/list"; }   # e.g. cache_tags library/busybox
edge_images() { docker images; }                                    # the dind daemon
```

Store references are always `{"name": "<store>"}`. A `Get`/`Watch` shows the job's
`state` and each `transfers[]` entry's `state` and `bytes_done`/`bytes_total`.

## Feature test matrix

| # | Feature | Doc |
|---|---------|-----|
| 1 | Registry→registry copy (remote → cache) | [stores.md](stores.md) |
| 2 | Engine pull (cache → edge) | [stores.md](stores.md) |
| 3 | Proxy-mode pull-through cache | [stores.md](stores.md) |
| 4 | Platform selection | [stores.md](stores.md) |
| 5 | Caller-chosen `as` names | [stores.md](stores.md) |
| 6 | Digest-pinned job (verbatim, local resolve) | [stores.md](stores.md) |
| 7 | Rewrite & downstream-host override | [stores.md](stores.md) |
| 8 | Signature verification | [verification.md](verification.md) |
| 9 | Retention / GC | [retention.md](retention.md) |
| 10 | Dedup & `Idempotency-Key` | [api.md](api.md) |
| 11 | Cancel & Retry | [api.md](api.md) |
| 12 | Audit log | [observability.md](observability.md) |
| 13 | Health & readiness | [observability.md](observability.md) |

Each test below is **What → Run → Expect**.

## 1. Registry → registry copy

**What** — gantry pulls each blob from `remote` and pushes it to `cache`, skipping
blobs the cache already has, reporting bytes actually moved.

**Run**
```sh
CP=$(add '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"},"platforms":["linux/amd64"]}')
watch "$CP"
```

**Expect** — `state=done`; the transfer ends `TRANSFER_STATE_DONE` with
`bytes_done == bytes_total`. `cache_tags library/busybox` lists `1.36`. Re-submit
the same job: the second run copies **zero bytes** (every blob is skipped), proving
incremental copy.

## 2. Engine pull

**What** — gantry tells the `edge` daemon to pull the image from `cache`; the
daemon's own pull progress folds into the transfer.

**Run**
```sh
P=$(add '{"ref":"library/busybox:1.36","source":{"name":"cache"},"target":{"name":"edge"}}')
watch "$P"
```

**Expect** — `state=done`; `docker images` shows `127.0.0.1:5000/library/busybox:1.36`
(or the recorded name, see test 5). Platform defaults to the daemon host's when
`platforms` is omitted.

## 3. Proxy-mode pull-through cache

**What** — a `proxy` cache fills itself by reading through on the tag instead of
gantry pushing blobs. Point a second cache store at a pull-through registry and set
`mode: "proxy"`; the job reads the manifest through to trigger the fill.

**Run** — add a `cache-proxy` store (`mode: "proxy"`) fronting a registry
configured as a pull-through mirror of `remote`, then:
```sh
add '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache-proxy"}}'
```

**Expect** — `state=done` with no per-blob push progress (the cache self-fills); a
subsequent read from `cache-proxy` serves the image. A **verifying** job refuses a
proxy target (test 8) — the proxy never learns the digest to anchor.

## 4. Platform selection

**What** — a registry copy takes every platform by default; a narrowed `platforms`
copies only those; an engine target pulls exactly one (the request's, or the host's).

**Run**
```sh
plan '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}'                       # platforms: all
plan '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"},"platforms":["linux/arm64"]}'  # narrowed
```

**Expect** — `Plan`'s `platforms` reflects the choice without moving anything.
Narrowing to a platform the source lacks fails admission (`INVALID_ARGUMENT`).

## 5. Caller-chosen `as` names

**What** — `as` records the pulled image on the engine under names of your choosing
instead of the cache pull ref, so a cache-fed node keeps the upstream name.

**Run**
```sh
add '{"ref":"library/busybox:1.36","source":{"name":"cache"},"target":{"name":"edge"},
      "as":["docker.io/library/busybox:1.36"]}'
```

**Expect** — `docker images` lists `docker.io/library/busybox:1.36` (the `as` name)
rather than the `127.0.0.1:5000/...` pull ref. **Digest** `as` names
(`repo@sha256:…`) require a digest-pinned job (test 6) and a containerd-image-store
engine; a classic graph store skips them with a warning (tags still apply). See
[stores.md](stores.md#caller-chosen-as-names).

## 6. Digest-pinned job (verbatim, local resolve)

**What** — a job whose `ref` is a digest (`repo@sha256:…`) commits the source index
byte-for-byte, so the same digest resolves from the cache; on a containerd-store
engine a digest `as` name then resolves **locally** with no origin contact.

**Run**
```sh
# resolve a digest from the remote first
DGST=$(crane digest 127.0.0.1:5001/library/busybox:1.36 2>/dev/null || echo "<sha256:...>")
add "{\"ref\":\"library/busybox@${DGST}\",\"source\":{\"name\":\"remote\"},\"target\":{\"name\":\"cache\"}}"
```

**Expect** — `state=done`; `cache_tags library/busybox` plus a manifest fetch by
digest returns the **same** digest (verbatim commit). Pulling that digest to a
containerd-store `edge` with a matching digest `as` name resolves locally
(`docker image inspect` hits; a `force_pull=false` pull moves nothing). Verify the
verbatim guarantee: a digest-ref copy refuses `platforms` narrowing.

## 7. Rewrite & downstream-host override

**What** — the cache-side ref comes from the target store's `rewrite` rules, and
`downstream_host`/`pull_host` decouple the address gantry pushes to from the one the
engine pulls from.

**Run** — add `rewrite`/`downstream_host` to the `cache` store, then:
```sh
plan '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}'
```

**Expect** — `Plan`'s `target_ref` shows the rewritten cache ref; with
`downstream_host` set, an engine job's `transfers[].ref` (from `Get`) shows the
substituted pull host while gantry still pushes to the store's real `host`. See
[stores.md](stores.md#rewrite-rules).

## 8. Signature verification

**What** — with `serve.verify` enabled, gantry verifies the source signature at
admission and fails closed; the verified digest is pinned; `copy_referrers` carries
the signature into the cache. Requires the [notation](https://notaryproject.dev)
CLI and a signing CA.

**Setup** — sign an image into `remote` and trust its CA:
```sh
notation cert generate-test "gantry-e2e"                         # a test CA + key
notation sign --insecure-registry 127.0.0.1:5001/library/busybox:1.36
# point serve.verify at the CA dir (notation's local trust store), e.g.:
#   serve: { verify: { mode: "require", trust_store: "/home/hypnos/.config/notation/truststore/x509/ca/gantry-e2e" } }
```

**Run**
```sh
# preflight without creating a job
$G -d '{"ref":"library/busybox:1.36","source":{"name":"remote"}}' gantry.VerifyService/Check
# a real move, carrying the signature into the cache
add '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"},"copy_referrers":true}'
```

**Expect** — under `require`: the signed image is admitted (`Check` reports the
pinned digest) and an **unsigned** ref is rejected `FAILED_PRECONDITION`. With
`copy_referrers`, a `VerifyService/Check` against the **cache** copy also verifies
(the signature travelled). A verifying job aimed at a `proxy` target is rejected.
`VerifyService/Describe` (empty body) lists the loaded anchors and effective modes.
See [verification.md](verification.md).

## 9. Retention / GC

**What** — per-engine-store GC keeps in-use/pinned/recent images and reaps the rest
by policy; `GcPlan` dry-runs, `GcApply` executes, both take a one-shot override so
you don't wait for the scheduler.

**Setup** — the base config already gives `edge` a `retention` block; pull a few
tags to it (test 2) so there are records to evaluate.

**Run**
```sh
# what the current policy would do (no deletion)
$G -d '{"store":{"name":"edge"}}' gantry.StoreService/GcStatus
$G -d '{"store":{"name":"edge"}}' gantry.StoreService/GcPlan

# force a blanket policy for one pass: keep only the newest, ignore age
$G -d '{"store":{"name":"edge"},"override":{"keep_n":1}}' gantry.StoreService/GcApply

# inventory + a pin
$G -d '{"store":{"name":"edge"}}' gantry.ImageService/List
$G -d '{"store":{"name":"edge"},"value":"*:1.36","pattern":true}' gantry.PinService/Add
```

**Expect** — `GcPlan` lists a decision per record with a reason (`keep_n_recent`,
`age_exceeded`, `pinned`, …); `GcApply` with `keep_n:1` removes all but the newest
digest-group and reports `deleted`/`untagged`. After the pin, `GcPlan` marks the
matching images `pinned` (kept), and `PinService/Add` echoes the
`gantry-pin-matched-count` trailer. To exercise the docker-only untagged reaper,
re-push a tag to a new image so the previous one goes untagged, then run a pass with
`untagged_after` short. See [retention.md](retention.md).

## 10. Dedup & `Idempotency-Key`

**What** — `Add` coalesces onto an identical in-flight move rather than running it
twice; an `Idempotency-Key` replays a remembered job even if the body differs.

**Run**
```sh
# fire two identical Adds at once; the second should coalesce
add '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}' &
add '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}' &
wait

# idempotency: same key twice returns the same job
$G -H 'idempotency-key: e2e-1' -d '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}' gantry.JobService/Add
$G -H 'idempotency-key: e2e-1' -d '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}' gantry.JobService/Add
```

**Expect** — the two concurrent submits resolve to **one** job id (watch the
`gantry-coalesced: true` response trailer with `grpcurl -v`); the repeated
idempotency key returns the **same** job id both times. See [api.md](api.md).

## 11. Cancel & Retry

**What** — `Cancel` stops a running job but keeps its record; `Retry` re-submits a
terminal job's original request as a fresh job.

**Run**
```sh
BIG=$(add '{"ref":"library/<large-image>:latest","source":{"name":"remote"},"target":{"name":"cache"}}')
$G -d "{\"id\":\"$BIG\"}" gantry.JobService/Cancel
$G -d "{\"id\":\"$BIG\"}" gantry.JobService/Retry
```

**Expect** — after `Cancel`, `Get` shows `state=canceled` and the record survives;
`Retry` returns a **new** id that runs to `done`. Because copies are
content-addressed, a retry after a partial run skips blobs already in the cache.

## 12. Audit log

**What** — with `serve.events` enabled, gantry durably records what it did;
`EventService` queries it and survives a restart (unlike `JobService.Get`/`List`).

**Run**
```sh
$G -d '{}' gantry.EventService/List
$G -d '{"type":"EVENT_TYPE_JOB_DONE"}' gantry.EventService/List
```

**Expect** — a `job_admitted` and a `job_done` per job run above, correlated by
`detail.job` (the job id); `gc_applied`, `pinned`, `image_removed` events from the
GC tests. Restart gantry and re-run `List`: the history is still there, while a
`JobService/List` is empty. See [observability.md](observability.md).

## 13. Health & readiness

**What** — `StoreService.Health` probes one store; the standard
`grpc.health.v1.Health` reports readiness aggregated over the gated stores.

**Run**
```sh
$G -d '{"name":"cache"}' gantry.StoreService/Health
$G -d '{"service":""}'   grpc.health.v1.Health/Check
```

**Expect** — `Health` reports `healthy:true` for a reachable store (and
`healthy:false` with the error, not an RPC failure, for an unreachable one — try
after `docker stop cache`). The overall `Health/Check` is `SERVING` while every
gated store (default: every engine store) probes healthy; stopping the `edge`
daemon flips it to `NOT_SERVING` within the readiness loop. See
[observability.md](observability.md).

## Teardown

```sh
kill %1 2>/dev/null                       # gantry (and any forwarders/watchers)
docker rm -f remote cache
rm -rf /tmp/gantry-e2e
```
