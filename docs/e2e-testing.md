# End-to-end testing

gantry is tested end-to-end two complementary ways, both covered here:

- **The [automated suite](#the-automated-suite)** (`internal/e2e`) — a layered Go
  suite that stands the real server up and drives it over gRPC, from a fully
  hermetic in-process tier that runs on every `go test` to black-box tiers that
  run the shipped binary and the shipped container image. This is how E2E runs in
  CI.
- **The [manual runbook](#the-manual-runbook)** — a `grpcurl` walkthrough of each
  user-facing feature on a standard remote→cache→engine environment, for hands-on
  exploration and reproducing behavior against real infrastructure.

For each feature's design see the topic guides — [stores.md](stores.md),
[retention.md](retention.md), [verification.md](verification.md),
[observability.md](observability.md), [api.md](api.md). For the devcontainer, the
unit/integration tests, and the loopback-insecure constraint, see
[development.md](development.md).

## The automated suite

`internal/e2e` builds the real gantry server through
[`internal/app.Build`](../internal/app) — the same wiring `gantry serve` uses, so
tests exercise production assembly rather than a copy — and drives it with the
generated `pb` client. Five tiers share one harness, differing only in what backs
the stores and the engine:

| Tier | Backing | Runs | Command |
|---|---|---|---|
| **L1 hermetic** | in-memory registries + a fake engine + an injected clock | **every** `go test -race ./...` (CI + local), seconds, no infra | `make e2e` |
| **L2 real daemon** | real `registry:2`/`registry:3` containers + the real docker daemon | opt-in; self-skips without docker | `make e2e-daemon` |
| **L3 black-box** | the shipped `gantry serve` binary + a real registry | opt-in | `make e2e-blackbox` |
| **L3 image** | the shipped **container image** (`FROM scratch`, non-root) on a user network + real registries | CI `build` job; opt-in local via `GANTRY_E2E_IMAGE` | `make e2e-image` |
| **L3-infra** | an Ansible-provisioned matrix (plain + TLS + zot + proxy) on a self-hosted host | manual / on-demand | `make e2e-infra` |

L2/L3 are behind `//go:build e2e` and L3-infra behind `//go:build e2e_infra`, so
the default `go test ./...` compiles and runs **L1 only**. Every tier is pure Go
with no new dependencies (`go-containerregistry`, `notation-go`, `oras`,
`docker/docker`, `bbolt`, `grpc`); CGO stays disabled.

### Running it

| Command | Tier | Needs |
|---|---|---|
| `make e2e` (`go test ./internal/e2e/...`) | L1 | Go only |
| `make e2e-daemon` (`go test -tags e2e -run TestL2 ./internal/e2e/...`) | L2 | a docker daemon |
| `make e2e-registries REG=registry:3` | L2 | docker; selects the registry image (`GANTRY_E2E_REGISTRY`) |
| `make e2e-blackbox` (`go test -tags e2e -run TestL3BlackBox ./internal/e2e/...`) | L3 | docker (reuses a prebuilt binary when `GANTRY_E2E_BIN` is set) |
| `make e2e-image` (`go test -tags e2e -run TestL3Image ./internal/e2e/...`) | L3 image | docker **and** a built image named in `GANTRY_E2E_IMAGE` (self-skips otherwise) |
| `make e2e-up` / `make e2e-down` | — | docker; the persistent `deploy/compose/e2e.compose.yaml` env |
| `make e2e-infra` | L3-infra | a self-hosted host + Ansible |

A single test: `go test -tags e2e -run TestL2CopyAndEnginePull ./internal/e2e/...`.

In the **devcontainer** the test process and the docker daemon sit in separate
network namespaces, so the L2/L3 harness starts a same-address forwarder
(`127.0.0.1:<port>` → `docker:<port>`) automatically when `DOCKER_HOST` is a
remote tcp endpoint. On a single-netns host (CI, or a laptop with a local daemon)
no forwarder is needed.

### Feature coverage

Which tier automates each feature (with the test that proves it):

| # | Feature | Where |
|---|---|---|
| 1 | Registry→registry copy + incremental blob skip | L1 `TestCopyRemoteToCache`, L2 `TestL2CopyAndEnginePull` |
| 2 | Engine pull (cache→daemon) | L1 `TestEnginePull` (fake), **L2 `TestL2CopyAndEnginePull` (real daemon)** |
| 3 | Proxy-mode pull-through | L3-infra (compose `cache-proxy`) |
| 4 | Platform selection | L1 `TestPlatformSelection` |
| 5 | Caller-chosen `as` names | L1 `TestAsNames` |
| 6 | Digest pin + verbatim commit | L1 `TestDigestPin` |
| 7 | Host substitution (`downstream_host`/`pull_host`) | L1 `TestPlanResolves`; real DNS in L3-infra |
| 8 | Signature verification (notation, in-process) | L1 `TestVerification` |
| 9 | Retention / GC (injected clock) | L1 `TestRetentionGC`; **L2 `TestL2RetentionCoversWhatARoutedPullLeftBehind` (a rule written for the origin covering what a routed pull left under the cache's host)** |
| 10 | Dedup & `Idempotency-Key` | L1 `TestIdempotencyKey` |
| 11 | Cancel & Retry | L1 `TestCancelRetry` |
| 12 | Audit log | L1 `TestAuditLog`; **real restart in L3 `TestL3BlackBox`** |
| 13 | Health & readiness | L1 `TestHealth` |
| 14 | Runtime enforcement (quarantine) | L1 `TestEnforcement`; **real docker in `internal/enforce` (`TestEnforceDockerE2E`) + `internal/down` (`TestDockerEnforcerLive`)** |
| 15 | Source fallback + `worker.source_wait` | L1 `TestEnginePullFallsBackToOrigin`, `TestEnginePullWithoutFallbackDoesNotTouchTheOrigin`, `TestFailedFallbackReportsTheRequestedSource`; **L2 `TestL2EnginePullFallsBackToOrigin`, `TestL2EnginePullWithoutFallbackDoesNotTouchTheOrigin`, `TestL2FallbackReportsBothAttemptsWhenNothingServes`, `TestL2SourceWaitJoinsAnInFlightFill`** |
| 16 | Routed copy through a store's declared `cache:` | L1 `TestRoutedCopyThroughACache`, `TestPlanReportsTheRoute`; **L2 `TestL2RoutedCopy*`, `TestL2RoutedFill*`, `TestL2SecondJobJoinsAnInFlightRoutedFill`, `TestL2ScopedRouteOnlyAppliesToMatchingRepos`, `TestL2WarmCacheServesARegistryTargetWhileTheOriginIsDown`**; L3 `TestL3RoutedCopyFromYAMLConfig` |
| 17 | `require_authority` on a routed job | **L2 `TestL2RequireAuthorityRejectsAnUnconfirmedRoute`**, paired with `TestL2WarmCacheServesARegistryTargetWhileTheOriginIsDown` for the default |
| — | Private-CA TLS (`ca_cert`) | L1 `TestTLSCache`; real registry in L3-infra |
| — | Graceful shutdown | L3 `TestL3BlackBox` |
| — | Route/fallback observability (`gantry.job.route`, `gantry.job.fallback`, `EVENT_TYPE_JOB_FALLBACK`) | L3 `TestL3RoutingObservability` |
| — | Shipped image runs (scratch base; non-root user writes the audit db) | L3 image `TestL3Image` |

#### What the live tiers stage that a fake cannot

The routing features are decisions gantry makes about failures, so the L2 tier
stages the failures for real rather than injecting them:

- **A miss is the registry's own 404**, arriving through the daemon's pull
  stream. That is the error `worthAnotherSource` actually has to classify; a
  fake engine can only return whatever the test invented.
- **A read-only cache** is a real registry whose `storage.maintenance.readonly`
  is on, so a push is refused by a registry that answers every read. Two traps,
  both of which stage *nothing* while looking like they worked, so the harness
  probes the cache with a write and fails loudly unless it is refused:
  - It has to be configured with a **file**, not `REGISTRY_*` env: distribution
    env overrides *replace* the `storage` map rather than merging into it, so
    the env spelling leaves the registry with no driver and it exits at startup
    — a registry that is gone, not one that refuses writes.
  - The file's **path differs by major** (2.x `/etc/docker/registry/config.yml`,
    3.x `/etc/distribution/config.yml`), and the CI matrix runs both. Writing to
    the wrong one is silent — the registry just starts on its own writable
    default. The harness derives the path from the image's own `cmd` instead of
    hardcoding it.
- **An outage** is the origin container removed, with the harness waiting until
  the published port stops answering.
- **A fill in flight** is a bandwidth-throttled proxy in front of the origin, so
  the window a second job has to observe the fill is a few seconds by
  construction rather than by luck.
- **`gantry.job.route`** is read off a real OTLP/gRPC export into a receiver the
  test runs, because a declined route is invisible in the job snapshot: it looks
  exactly like a job that was never eligible for one.

### CI

`.github/workflows/ci.yaml` runs the **source tests** and compiles the release
binaries **once** in parallel, **tests the real artifact** per architecture once
both are green, and only then tags anything:

- **`test`** — `go test -race ./...`: unit tests **and the L1 suite** (no infra).
- **`e2e-docker`** — the **L2** tests against the runner's docker daemon, matrixed
  over `registry:2` (referrer tag-fallback) and `registry:3` (native referrers).
- **`e2e-containerd`** — provisions containerd and runs the `internal/down`
  integration tests (digest `as`, anchored pull).
- **`dist`** (needs nothing — runs immediately, in parallel with the tests) — bakes
  the `build` stage **once**. That stage cross-compiles both `amd64` and `arm64` in
  a single run, so `./dist` (both binaries + the CA bundle) is uploaded as an
  artifact rather than recompiled per arch.
- **`verify`** (matrix: `amd64` on `ubuntu-latest`, `arm64` on `ubuntu-24.04-arm`;
  needs `dist` **and** the whole source gate) — downloads `./dist` and assembles the
  `FROM scratch` image for its arch — a COPY-only stage, **no recompile, no qemu** —
  then black-boxes **the real artifact** on native hardware: **L3** against the
  `./dist/<arch>` binary (`GANTRY_E2E_BIN`) and **L3 image** against the loaded
  image (`GANTRY_E2E_IMAGE`). It is a **pure test job** — it never touches the
  registry; it only gates `promote`.
- **`promote`** (needs both `verify` legs, `main` push only) — builds the shippable
  multi-arch image from the same `./dist` and pushes it with every tag in one step.
  Because `app` is a COPY-only scratch stage, both arches assemble on a single
  `amd64` runner with **no qemu**, and the pushed binaries are **byte-for-byte the
  ones `verify` black-boxed** (same `./dist`, deterministic COPY). It is the only
  job that writes to the registry, and it runs only once both `verify` legs pass, so
  nothing is published until the real artifact is green. `edge` always tracks the
  latest promoted build.

Compiling once in `dist` and fanning out to native runners means the Go build is
not repeated, no qemu is involved, and the arm64 image is still exercised on real
arm64 hardware. The source gate already runs the unit and L1 tiers, so `verify`
does **not** rebake the Dockerfile's in-image `test` stage — that stage stays for
anyone who builds the image directly with `docker buildx bake`.

> **arm64 runner** — `ubuntu-24.04-arm` is a GitHub-hosted runner (free for public
> repositories). A private repo needs arm runners enabled, otherwise drop the
> `arm64` matrix leg and promote an amd64-only manifest.

The **L3-infra** (Ansible) tier is not wired to CI: run it on demand with
`make e2e-infra` on a host with docker + Ansible. Add a self-hosted scheduled
job later if a standing runner is available.

On the runner the test process shares its network namespace with the docker
daemon, so a registry on `127.0.0.1:<port>` is auto-trusted as insecure by both
gantry and the daemon — no `daemon.json`, no forwarder. Payload images are
synthesized in-process, so a run makes **no Docker Hub pulls**; only the registry
daemon image is fetched.

### Registry implementations

gantry (via `oras`) handles **both** OCI signature-referrer schemes — the native
`/v2/.../referrers/` API and the tag-schema fallback — so the matrix deliberately
covers both branches:

| Registry | Referrers API | Tier | Exercises |
|---|---|---|---|
| `registry:2` (Distribution v2) | tag-fallback (404s native) | per-PR CI | copy, proxy, the **fallback** referrer path |
| `registry:3` (Distribution v3) | native | per-PR CI | native referrers in the codebase users deploy |
| `zot` | native (OCI 1.1.1) | infra (compose) | the **native** path + signature travel, a 2nd impl |
| Harbor, ECR/ACR/GCR | native (edges vary) | future / opt-in | robot-auth / cloud auth, production-grade |

### How it works

- **`internal/app.Build`** assembles the whole server from config — the same path
  `gantry serve` uses. `app.WithStoreSet` injects the fake engine (since
  `store.NewSet` dials real daemons); `app.WithNow` injects a clock.
- **Artifact reuse** — `GANTRY_E2E_BIN` lets the binary tier skip its own `go
  build` and drive a prebuilt binary; `GANTRY_E2E_IMAGE` points the image tier at
  an already-built image (it self-skips without one). CI sets both to `bake`'s
  outputs, so the shipping artifact is compiled once and is exactly what L3
  exercises. The image tier injects config and a non-root-owned `/data` with the
  container copy API rather than a bind mount, so it is correct against a remote
  daemon too.
- **Injected clock** (`retention.WithNow` / `event.WithNow`) makes time-dependent
  GC (grace, `max_age`, `max_idle`, untagged reap) deterministic — no `time.Sleep`.
- **In-process notation** — verification is tested without the `notation` CLI:
  `notation-go` signs an image (pushed as a referrer) and gantry verifies against a
  temp CA trust store, exactly as `internal/verify/notation_integration_test.go`.
- **`tools/e2e-seed`** — a pure-Go seeder that pushes synthetic single- or
  multi-platform images egress-free, optionally notation-signing them and exporting
  the CA, for the L2/L3/infra tiers and the manual runbook.
- **`ansible/`** (self-hosted, no vault) provisions the L3-infra environment: a
  private TLS CA the docker daemon trusts, the registry matrix from
  `deploy/compose/e2e.compose.yaml`, a signed seed image, and a `gantry-e2e.json`
  discovery file the `e2e_infra` tests read (`GANTRY_E2E_CONFIG`; self-skips
  without it).

## The manual runbook

A hands-on walkthrough: stand up a realistic remote→cache→engine pipeline and
exercise each feature with the exact request and how to confirm it. Every scenario
goes through a running gantry — submit over gRPC, then inspect the registries and
the daemon.

### The standard environment

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
| `remote` | `oci`     | Upstream registry holding the source images (stands in for docker.io / a private registry). |
| `cache`  | `oci`     | The cache registry gantry copies into and the engine pulls from (`mode: copy`). |
| `edge`   | `docker`  | A downstream engine (a fleet node). gantry tells it to pull from `cache`. |

#### Bring-up (in the devcontainer)

Both registries run on the DinD daemon; the docker engine is that same daemon.
Because a daemon only pulls an insecure (plain-HTTP) registry from its **own
loopback** without extra config (see [development.md](development.md#insecure-registry-constraint-loopback-only)),
publish the registries on the dind host and forward the same `127.0.0.1` ports
inside `dev` so gantry uses the identical references.

```sh
# Two registries on the dind daemon: remote (upstream) and cache.
docker run -d -p 5001:5000 --name remote registry:2
docker run -d -p 5000:5000 --name cache  registry:2

# Forward both ports into dev (see the forwarder in development.md), one per port:
# 127.0.0.1:5000→docker:5000 and 127.0.0.1:5001→docker:5001.
curl -s -o /dev/null -w "cache %{http_code}\n"  http://127.0.0.1:5000/v2/   # 200
curl -s -o /dev/null -w "remote %{http_code}\n" http://127.0.0.1:5001/v2/   # 200

# Seed the remote with an image, egress-free, using the in-repo seeder.
mkdir -p /tmp/gantry-e2e
go run ./tools/e2e-seed --to 127.0.0.1:5001 --repo library/busybox --tag 1.36 --insecure
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
> `edge` — a `retention` block on an `oci` store like `cache` is rejected at
> startup. See [retention.md](retention.md).

Start gantry and smoke-test the surface:

```sh
go run . --config gantry-e2e.yaml serve &

grpcurl -plaintext 127.0.0.1:18080 gantry.StoreService/List
# expect: remote / cache / edge, with capabilities and ready=true
```

#### Test helpers

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

### Feature test matrix

| # | Feature | Doc |
|---|---------|-----|
| 1 | Registry→registry copy (remote → cache) | [stores.md](stores.md) |
| 2 | Engine pull (cache → edge) | [stores.md](stores.md) |
| 3 | Proxy-mode pull-through cache | [stores.md](stores.md) |
| 4 | Platform selection | [stores.md](stores.md) |
| 5 | Caller-chosen `as` names | [stores.md](stores.md) |
| 6 | Digest-pinned job (verbatim, local resolve) | [stores.md](stores.md) |
| 7 | Host substitution (`downstream_host`/`pull_host`) | [stores.md](stores.md) |
| 8 | Signature verification | [verification.md](verification.md) |
| 9 | Retention / GC | [retention.md](retention.md) |
| 10 | Dedup & `Idempotency-Key` | [api.md](api.md) |
| 11 | Cancel & Retry | [api.md](api.md) |
| 12 | Audit log | [observability.md](observability.md) |
| 13 | Health & readiness | [observability.md](observability.md) |
| 14 | Runtime enforcement (quarantine) | [enforcement.md](enforcement.md) |

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
engine; a digest `as` name against a classic graph store is rejected before the pull (tags still apply). See
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

## 7. Host substitution (`downstream_host` / `pull_host`)

**What** — the cache-side ref is the source repo/tag under the target store's
own host; `downstream_host`/`pull_host` decouple the address gantry pushes to
from the one the engine is told to pull from.

**Run** — add `downstream_host` to the `cache` store, then:
```sh
plan '{"ref":"library/busybox:1.36","source":{"name":"remote"},"target":{"name":"cache"}}'
```

**Expect** — `Plan`'s `target_ref` shows the cache ref (the source repo/tag under
the cache `host`); with `downstream_host` set, an engine job's `transfers[].ref`
(from `Get`) shows the substituted pull host while gantry still pushes to the
store's real `host`. See [stores.md](stores.md#downstream_host-and-pull_host).

## 8. Signature verification

**What** — with `serve.verify` enabled, gantry verifies the source signature at
admission and fails closed; the verified digest is pinned; `copy_referrers` carries
the signature into the cache.

**Setup** — seed a signed image into `remote` and trust its CA, using the pure-Go
seeder (no `notation` CLI needed):
```sh
mkdir -p /tmp/gantry-e2e/trust
go run ./tools/e2e-seed \
  --to 127.0.0.1:5001 --repo library/signed --tag 1 --insecure \
  --sign --ca-out /tmp/gantry-e2e/trust/ca.crt
# point serve.verify at that trust store, e.g.:
#   serve:
#     verify: { mode: "require", provider: "notation", trust_store: "/tmp/gantry-e2e/trust", level: "permissive" }
```

**Run**
```sh
# preflight without creating a job
$G -d '{"ref":"library/signed:1","source":{"name":"remote"}}' gantry.VerifyService/Check
# a real move, carrying the signature into the cache
add '{"ref":"library/signed:1","source":{"name":"remote"},"target":{"name":"cache"},"copy_referrers":true}'
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

## 14. Runtime enforcement (quarantine)

**What** — with `serve.enforce` on an engine store, gantry watches that daemon's
container starts and force-removes any container whose image is not signed by a
trusted Root CA — then removes the image. Enforcement verifies in `require`
semantics regardless of `serve.verify.mode`. See [enforcement.md](enforcement.md).

**Setup** — reuse the signed image and trust store from test 8, and add a verdict
cache plus enforcement on the `edge` engine store (the admission `mode` may stay
`off` — enforcement forces `require` itself):
```yaml
serve:
  verify:
    trust_store: "/tmp/gantry-e2e/trust"          # from test 8
    cache: { path: "/tmp/gantry-e2e/verify.db" }
  enforce:
    mode: "quarantine"
    stores: ["edge"]
    on_unavailable: "grace"
```
Load a signed and an unsigned image onto the `edge` daemon first (a gantry job or
`docker pull` from the cache), so each is present with a repo digest.

**Run** — start a container from each image on the `edge` daemon:
```sh
docker run -d --name ok  <cache-host>/library/signed:1   sleep 300
docker run -d --name bad <cache-host>/library/unsigned:1 sleep 300
```

**Expect** — within a moment gantry force-removes `bad` and removes its image,
while `ok` keeps running (`docker ps` shows only `ok`). The gantry log carries a
`quarantining untrusted container` warning naming `bad`'s digest; gantry never
removes its own container. Setting `serve.verify.mode` to `off` does not change
this — enforcement is independent of the admission mode.

## Teardown

```sh
kill %1 2>/dev/null                       # gantry (and any forwarders/watchers)
docker rm -f remote cache
rm -rf /tmp/gantry-e2e
```
