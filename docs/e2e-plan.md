# gantry E2E automation plan

A plan to automate the [end-to-end test matrix](e2e-testing.md) — today a manual
`grpcurl` runbook — into a layered, mostly-hermetic suite that also runs against
**real daemons** and **major registry implementations** in CI, with an
external-automation tier (Ansible) for the multi-host / non-loopback truth the
loopback shortcut cannot reach, and convenient local entry points.

Guiding principles:

- **The fast path stays fast.** The default `go test -race ./...` gains broad E2E
  coverage with **no new infrastructure**; heavier tiers are opt-in (`-tags e2e`,
  labels, nightly).
- **Hermetic first, real where it matters.** Most features are provable
  in-process against in-memory registries and a fake engine; a real docker /
  containerd daemon and real registries are layered on for the daemon-facing and
  cross-implementation truth.
- **Pure Go, CGO disabled.** Every tier uses libraries already in `go.mod`
  (`go-containerregistry`, `notation-go`, `oras`, `docker/docker`, `containerd/v2`,
  `bbolt`, `grpc`). No cgo, no new heavyweight deps.
- **Everything, eventually.** All layers ship; this doc is the sequence.

## Layer model

| Layer | What it is | Runs where | Gates |
|---|---|---|---|
| **L0** | Enabling refactors (`buildServer()`, `WithNow` clock seam) | — | prerequisite |
| **L1** | Hermetic in-process: real server + in-memory registries + fake engine + injected clock | **every** `go test -race ./...`, CI + local | fast path |
| **L2** | Real-daemon: real docker/containerd + real registry containers, matrix over registry impls | `-tags e2e`, CI opt-in jobs + local (docker present) | pre-push |
| **L3** | Black-box: the shipped `gantry serve` binary against real infra (graceful shutdown, restart persistence) | `-tags e2e`, CI job + local | pre-push |
| **L3-infra** | Ansible-provisioned full environment: registry matrix + real DNS/TLS + non-loopback + Harbor/cloud | nightly / self-hosted / VM, local `make e2e-vm` | never gates |

L1→L2→L3 **share one harness** — they differ only in what backs the source/cache
(in-memory vs real registry) and the engine (fake vs real daemon vs the real
binary). L3-infra provisions the world Go tests then target.

## Implementation status

Tracks what has landed against the [sequencing](#sequencing) below.

- **L0 refactors — ✅ done.** `internal/app.Build(ctx, *config.Config, …app.Option)`
  extracts the whole server-construction path out of `cmd/serve.go`'s command
  closure, so `serve` and the test harness build identical production wiring;
  `app.WithNow` threads a clock. `retention.WithNow` / `event.WithNow` add the
  clock seam. Behavior-preserving (all existing tests green).
- **L1 hermetic suite — ✅ done.** `internal/e2e` harness (`app.Build` over
  bufconn + ggcr in-memory registries + fake engine + injected clock, driven by
  `pb.NewClient`; `app.WithStoreSet` injects the fake daemon). Feature tests, all
  under plain `go test -race`: registry→registry copy + incremental blob skip (1),
  engine pull (2, fake), platform selection (4), `as` names (5), digest pinning +
  verbatim commit (6), Plan (7), signature verification incl. copy_referrers
  travel (8, in-process notation), retention/GC via the injected clock (9),
  Idempotency-Key (10), cancel/retry (11), audit log (12), health/readiness (13).
  Proxy-mode (3) is deferred to L2 — the in-memory registry has no pull-through
  mode.
- **L2 real daemon + registry matrix — ⏳ in progress.** Build-tagged (`e2e`)
  `internal/e2e/l2_*_test.go`: real `registry:2` containers (via the docker
  client) + the real docker daemon, self-skipping when no daemon is reachable.
  Handles the loopback-insecure model and the devcontainer's separate-netns case
  with a same-address forwarder. `TestL2CopyAndEnginePull` (feature 2, engine
  pull) passes against a real docker daemon. Remaining: the registry-impl matrix
  (`zot`/`registry:3`), proxy mode (3), containerd sub-tier, and the CI jobs.
- **L3 black-box — ☐ not started.**
- **L3-infra (Ansible) + nightly CI — ☐ not started.**

## Feature × layer coverage

The 13 features from [e2e-testing.md](e2e-testing.md), mapped to where each is
automated:

| # | Feature | L1 hermetic | L2 real daemon | L3 / L3-infra |
|---|---|:--:|:--:|:--:|
| 1 | Registry→registry copy (+ incremental skip) | ✅ | ✅ (matrix) | ✅ |
| 2 | Engine pull (cache→daemon) | ⚠️ fake engine | ✅ **headline** | ✅ |
| 3 | Proxy-mode pull-through | ⚠️ partial | ✅ (registry proxy) | ✅ |
| 4 | Platform selection | ✅ | ✅ | ✅ |
| 5 | `as` names (tag) | ✅ registry-side | ✅ daemon RepoTag | ✅ |
| 5/6 | digest `as` names (containerd store) | ❌ | ✅ containerd tier | ✅ (devcontainer/VM) |
| 6 | Digest-pinned verbatim commit | ✅ admission | ✅ | ✅ |
| 7 | Rewrite & `downstream_host` | ✅ (Plan) | ✅ | ✅ **real DNS** (L3-infra) |
| 8 | Signature verification (notation) | ✅ **in-process** | ✅ native+fallback refs | ✅ |
| 9 | Retention / GC (count/age/idle/untagged/pins) | ✅ (injected clock) | ✅ daemon removal | ✅ |
| 10 | Dedup & `Idempotency-Key` | ✅ | ✅ | ✅ |
| 11 | Cancel & Retry | ✅ | ✅ | ✅ |
| 12 | Audit log | ✅ (reopen) | ✅ | ✅ **real restart** (L3) |
| 13 | Health & readiness | ✅ | ✅ | ✅ |
| — | non-loopback insecure / TLS / robot-auth | ❌ | ❌ | ✅ **L3-infra only** |
| — | graceful shutdown / drain | ❌ | ❌ | ✅ **L3 only** |

Key correction to the current [e2e-testing.md](e2e-testing.md): **signature
verification is fully automatable in-process** (the `notation-go` signer +
`notation-core-go/testhelper` certs, as `internal/verify/notation_integration_test.go`
already does) — it does **not** need the `notation` CLI. That doc's verification
section should be updated when L1 lands.

## L0 — enabling refactors

Two small, independently valuable refactors unblock deterministic, drift-free E2E:

- **R1 · `buildServer()`** — `cmd/serve.go`'s wiring (stores → retention `[]Store`,
  the Copier + pull-hook + recorders, health, events, grpc registration) lives
  inside one `xli.OnRun` closure (`cmd/serve.go:37-221`). Extract its body into
  `func buildServer(ctx, *config.Config) (*grpc.Server, cleanup func(), error)`
  that both `cmd/serve` and the E2E harness call, so the harness exercises
  **production wiring** instead of a parallel copy that can drift.
- **R2 · `WithNow` clock seam** — `retention.Manager.now` and `event.Log.now` are
  unexported `time.Now` fields with no setter (`internal/retention/manager.go:57`,
  `internal/event/log.go:60`). Add `retention.WithNow(func() time.Time)` /
  `event.WithNow(...)` options and thread a manually-advanced fake clock. This
  makes the adaptive scheduler, grace window, heartbeat cadence, and untagged-reap
  delay deterministic — and removes the `time.Sleep`-based flakiness in the
  existing scheduler tests.
- **R3 · (note, not a blocker)** — the Copier has no synchronous run-to-completion
  hook (`internal/cpx/cpx.go:549`); completion is observed via `Job.Watch` / poll.
  A shared `waitTerminal(client, id, timeout)` helper in the harness absorbs this;
  a future `Copier.RunInline` hook could remove the residual poll.

## L1 — hermetic in-process suite

The backbone. A new **`internal/e2e`** package (must be `internal/` — it calls
`rpc.New`, `cpx.NewCopier`, `retention.NewManager`, `store.NewSet`, `event.Open`,
`health.NewChecker`, all internal).

**Harness** (`internal/e2e/harness.go`), extending the proven `newEnv` in
`internal/rpc/rpc_test.go`:

1. Two in-memory OCI registries — `httptest.NewServer(registry.New(registry.WithReferrersSupport(true)))`
   for `remote` and `cache`; seed images with `random.Image` / `mutate` + `remote.Write`.
2. `config.Config` as a struct literal → `Evaluate()` for defaults.
3. `store.NewSet(cfg.Stores, false)`; `stores.PutEngine(engCfg, fakeEngine)`.
4. `cpx.NewCopier(...)` with **real workers** started via `Start(ctx)`.
5. `retention.NewManager(..., retention.WithNow(clk))` + `event.Open(tmp, cap, event.WithNow(clk))`.
6. `srv := rpc.New(...)` (ideally via **R1** `buildServer`); `grpc.NewServer` + `srv.Register(g)`.
7. `bufconn.Listen` + `pb.NewClient(conn)` — the exact recipe from `rpc_test.go` / `grpc_smoke_test.go`.

**Assertion styles:** (a) *synchronous admission* — `Plan`/`Add` return the
resolved plan (rewritten target ref, verbatim digest pin, `downstream_host`
substitution, coalescing / `Idempotency-Key` returning the same id, queue-full
rejection); (b) *completion* — `Submit` then `waitTerminal`, then read blobs back
from the cache registry with `remote.Get`. Incremental skip (feature 1) is proven
by counting blob uploads (wrap the registry handler) and asserting **zero** on a
second copy. Time-based GC uses backdated records (`ix.Touch`) with `now=time.Now`;
scheduler/grace/reap timing uses the **R2** injected clock.

Files: `harness.go`, `registry.go`, `engine.go` (consolidate the three existing
engine fakes), and `feature_NN_*_test.go` per feature. **No build tag** — L1 runs
under the standard `go test -race ./...`, in seconds, everywhere.

## L2 — real-daemon tier

`//go:build e2e`, self-skipping on missing infra exactly like
`internal/down/*_integration_test.go` (env probe + `t.Skipf`). Same harness, but:

- **Engine** = a real `docker` (and, in the containerd tier, real `containerd`)
  `down.Engine`, driven at the addresses the existing integration tests use
  (`DOCKER_HOST`, `CONTAINERD_ADDRESS`/`NAMESPACE`).
- **Registries** = real registry **containers** brought up via the `docker/docker`
  client already in `go.mod` (ephemeral host port, labelled, `t.Cleanup` removes
  them). No `testcontainers-go` (avoids a large dep tree for no gain).
- **Loopback-insecure**, solved for free in CI: the runner's test process and its
  docker daemon share the **host network namespace**, so a registry on
  `127.0.0.1:<port>` is the same auto-insecure (`127.0.0.0/8`) reference for both
  gantry and the daemon — **no `daemon.json`, no forwarder**. In the devcontainer
  (separate netns) the same-address forwarder from
  [development.md](development.md#end-to-end-manual-verification-the-full-loop) is
  needed; the harness/Makefile starts it.
- **Egress-free seeding** — synthesize images in-process with `go-containerregistry`
  (`random.Image`/`random.Index`) and `remote.Write` them; only the registry
  daemon image itself needs a pull (digest-pinned + cached in CI).
- **Assertions** — registries via `go-containerregistry` (`remote.Get`/`Head`,
  referrers), daemon via the docker client (`ImageInspect` RepoTags/RepoDigests) or
  gantry's own `ImageService`/`PinService` RPCs.

The **containerd sub-tier** (its own job / build path) covers the
containerd-image-store features (digest `as` names, anchored pull, no-unrequested-
record) that the docker daemon can't guarantee.

### Registry implementation matrix

gantry (via `oras`) must handle **both** signature-referrer schemes, and the
current suite only ever sees one. The matrix deliberately covers both branches:

| Registry | Referrers API | Insecure/loopback | Proxy mode | Tier | Exercises |
|---|---|---|---|---|---|
| `registry:2` (Distribution v2.8) | **tag-fallback** (404s native) | ✅ plain-HTTP | ✅ `proxy.remoteurl` | **default CI** | copy, proxy (3), the **fallback** referrer path |
| `zot` | **native** (OCI 1.1.1) | ✅ plain-HTTP (static binary) | ✅ `sync` on-demand | **default CI** | the **native** referrer path, signature travel, 2nd impl |
| `registry:3` (Distribution v3) | **native** | ✅ plain-HTTP | ✅ | default CI (optional 3rd) | native referrers in the Distribution codebase users deploy |
| Harbor | native (v2.10+; replication gaps) | ✗ HTTPS-oriented, heavy | ✅ proxy-cache **project** | **nightly** | robot-account/RBAC auth, proxy-cache, production-grade |
| GHCR | tag-fallback (recheck) | ✗ (TLS + token) | ✗ | auth-real smoke (nightly / trusted) | real TLS + bearer keychain auth |
| ECR / ACR / GCR | native | ✗ (cloud auth) | ✅ (managed) | nightly, creds-gated | real cloud auth, native referrers |

Default per-PR set: **`registry:2` + `zot`** (both referrer branches), optionally
`+ registry:3`. Keep it to 2–3. Harbor and cloud are nightly. *(OSS facts high
confidence as of mid-2026; GHCR native-referrer status and cloud specifics:
recheck vendor docs at implementation time.)*

## L3 — black-box tier

`//go:build e2e`. Builds the actual binary (`CGO_ENABLED=0 go build -o gantry .`,
or consumes the `build` job's artifact / `GANTRY_BIN`) and runs `gantry serve`
against a real config file and real (loopback or provisioned) infra. It uniquely
proves what no in-process tier can: **graceful shutdown / drain** on `SIGTERM`, and
**audit-log persistence across a real process restart** (relaunch on the same
`events.db`, assert `EventService/List` survives while `JobService/List` is empty).
Because it runs the shipped binary, it needs **no `serve.go` refactor** and cannot
drift from production wiring — a useful complement to L1's `buildServer` path.

## L3-infra — external automation (Ansible)

For the truth the loopback shortcut structurally cannot reach — a **real
non-loopback registry** the daemons must be configured to trust
(`insecure-registries` / containerd `certs.d/hosts.toml` / a TLS CA), **real DNS**
so `downstream_host`/`pull_host` resolve, a distributed **TLS + notation CA**, and
a **fresh docker/containerd host** on a VM — Ansible is the spine (docker-compose
cannot express host config or multi-host).

Layout:

```
ansible/
  ansible.cfg  requirements.yml           # community.docker/general/crypto, ansible.posix
  inventories/{localhost,multipass,ci,cloud}/   # swap inventory → localhost | VM | runner | cloud
  group_vars/all/{main.yml,vault.yml}      # the declarative env: registries[], seed_images[], engines[], domain, tls
  roles/
    ca/                # one root CA → registry TLS certs + notation signing leaf, fanned to every trust store
    dns_hosts/         # /etc/hosts or dnsmasq so downstream_host/pull_host names resolve (routable only)
    registry/          # stand up each registries[] entry (distribution/zot as containers, harbor via installer)
    docker_engine/     # provision + trust-config a real dockerd (insecure-registries or certs.d + CA)
    containerd_engine/ # provision + config_path=/etc/containerd/certs.d + hosts.toml
    seed_images/       # run tools/e2e-seed (pure-Go: ggcr copy + notation-go sign via oras); CLI fallback
    gantry/            # place the shipped binary + render gantry.yaml.j2 from resolved facts
    testenv/           # emit ansible/.out/gantry-e2e.json (+ .env): discovery for the Go tests
  playbooks/{site,provision,seed,emit-config,run-l3,teardown}.yml
deploy/compose/e2e.compose.yaml            # the lighter single-host loopback driver (L2 + quick L3)
tools/e2e-seed/                            # pure-Go seeder: upstream→remote copy + notation-signed referrers
```

- **One orchestration path, two weights.** For a single loopback host (a dev's
  quick L2/L3), the shipped `deploy/compose/e2e.compose.yaml` suffices, and the
  `localhost` inventory simply drives that compose file
  (`community.docker.docker_compose_v2`) then runs only `role:testenv`. For
  multi-host / routable / TLS / fresh-VM, the full roles run — same playbooks, swap
  the inventory (`multipass`, `cloud`, `ci`).
- **Discovery contract.** `role:testenv` renders `gantry-e2e.json` (gantry
  binary/config/addr; stores with names + addresses + impl + `caps`; docker/
  containerd endpoints; trust-store dir; signed digest; `network_mode`). The Go L3
  tests read `GANTRY_E2E_CONFIG` and **self-skip when unset** — so the default
  `go test` is untouched. `caps` lets a matrix-aware test skip a scenario when the
  target registry lacks a capability (proxy, native referrers).
- **Idempotent teardown** that **reverses host config** (critical on a persistent
  self-hosted runner: a leftover `insecure-registries` entry silently weakens the
  host) — prefer ephemeral VMs where teardown is `destroy`.
- **Seeding without the notation CLI** — `tools/e2e-seed` (pure-Go: `go-containerregistry`
  copy/retag + `notation-go` signer with a `role:ca` cert, pushed as a referrer via
  `oras`); `use_notation_cli: true` is the escape hatch.

## CI pipeline

Layered onto the existing `ci.yaml` (`test` = `go test -race`; `build` = bake +
push `edge` on `main`). New jobs run **in parallel**; only `build` fans in.

| Job | Trigger | Runner | Covers | Gates push |
|---|---|---|---|---|
| `test` (L1) | every push/PR | ubuntu-latest | L1 hermetic (no build tag, in the race run) | ✅ |
| `e2e-docker` (L2) | every push/PR* | ubuntu-latest (host docker) | copy/pull/etc. **matrix over `registry:2`, `zot`** (+`registry:3`) | ✅ |
| `e2e-containerd` (L2) | every push/PR* | ubuntu-latest (`apt install containerd`) | containerd image-store: digest `as`, anchored pull | ✅ |
| `e2e-blackbox` (L3) | every push/PR* | ubuntu-latest | shipped binary: graceful shutdown, restart persistence | ✅ |
| `build` | push/PR | ubuntu-latest | bake + `edge` push (on `main`) | — |
| `nightly` | cron + dispatch | self-hosted / cloud VM | Harbor, cloud registries (creds-gated), **Ansible full-provision + L3-infra** | ✗ never |

\* optionally skip the L2/L3 tiers on **draft** PRs; L1 always runs.

- **`build` re-gated**: `needs: [test, e2e-docker, e2e-containerd, e2e-blackbox]`,
  so a broken E2E blocks the `edge` publish. `nightly` is deliberately **not** in
  `needs` — a flaky Harbor/cloud run never blocks a push.
- **`e2e-docker` matrix** (`fail-fast: false`): `registry: [ {registry2, registry@sha256:…},
  {zot, ghcr.io/project-zot/zot-linux-amd64@sha256:…, serve /etc/zot/config.json} ]`,
  optional `+ registry:3`; optional `auth: [anonymous, htpasswd]` follow-up axis.
  Each: `docker run -d -p 127.0.0.1:$PORT:5000 $IMAGE@$DIGEST` → wait `/v2/` →
  `go run ./internal/e2e/cmd/seed --to 127.0.0.1:$PORT` → `GANTRY_E2E_REGISTRY=127.0.0.1:$PORT
  go test -tags e2e -race ./internal/e2e/...`.
- **Egress minimization**: payload images synthesized by the ggcr seeder (zero
  Docker Hub pulls); registry daemon images **digest-pinned** and cached
  (`docker save`/`load` keyed on the digest, or mirrored into org GHCR).
- **Ergonomics**: `concurrency: {group: ci-<ref>, cancel-in-progress: true}`; go
  module + build caches; failure artifacts (serve stdout/stderr, registry logs).
- **`nightly.yaml`**: `heavy-registries` matrix (Harbor via compose; ECR/GCR/ACR
  each `if: secrets.<P> != ''`) + `ansible-full-provision` on `[self-hosted,
  gantry-e2e]` or an ephemeral cloud VM, running L3-infra; `if: always()` teardown.

## Local developer workflow

The bar: **not necessarily trivial, but never impossible.** A `Makefile` front door
(pure-Go where it can be):

| Command | Tier | Needs | Does |
|---|---|---|---|
| `make e2e` | L1 | Go only | `go test -race ./internal/e2e/...` — hermetic, seconds |
| `make e2e-daemon` | L2 | docker | spin `registry:2` on loopback, seed, `go test -tags e2e ./internal/e2e/...` |
| `make e2e-registries REG=zot` | L2 | docker | same, against a chosen registry impl |
| `make e2e-containerd` | L2 | containerd sock | containerd image-store features |
| `make e2e-blackbox` | L3 | docker | build the binary, run it, assert shutdown/restart |
| `make e2e-up` / `make e2e-down` | L2/L3 | docker | bring up / tear down the persistent `deploy/compose/e2e.compose.yaml` env for iterating |
| `make e2e-seed` | — | Go | run `tools/e2e-seed`: notation CA + signed images into the running env |
| `make e2e-fwd` | L2/L3 in devcontainer | — | start the same-address `127.0.0.1:{5000,5001}` forwarder (separate-netns fix) |
| `make e2e-vm` | L3-infra | multipass/ansible | provision a routable VM and run the full non-loopback suite |

- **Single feature / layer**: `go test -tags e2e -run TestE2E_EnginePull ./internal/e2e/...`.
- **Devcontainer note**: docker + a containerd sidecar are already provided; L2/L3
  locally need `make e2e-fwd` first (separate netns). On a single-netns host (VM,
  laptop with local docker) no forwarder is needed.
- **Minimum per tier** — L1: nothing but Go. L2: a reachable docker daemon
  (`DOCKER_HOST`). L3: docker + the binary build. L3-infra: `multipass` + the
  Ansible collections (`ansible-galaxy collection install -r ansible/requirements.yml`).

## Sequencing

1. **Phase 1 — foundation.** R1 `buildServer()` + R2 `WithNow` seam; the
   `internal/e2e` harness; L1 tests for the ~11 hermetic-coverable features
   (incl. in-process signature verification). *Lands the bulk of the value; runs on
   every CI push.*
2. **Phase 2 — real daemon + registry matrix.** L2 docker tier + the ggcr seeder +
   the `e2e-docker` CI job with the `registry:2`/`zot` matrix. *Closes feature 2
   (engine pull) and the native-vs-fallback referrer branches.*
3. **Phase 3 — containerd + black-box.** `e2e-containerd` job (digest `as`,
   anchored pull) and the L3 `e2e-blackbox` job (shutdown, restart). Re-gate the
   `build` push.
4. **Phase 4 — external automation + nightly.** The `ansible/` tree +
   `deploy/compose/e2e.compose.yaml` + `tools/e2e-seed`; `nightly.yaml` with Harbor,
   cloud registries, and the routable/non-loopback L3-infra run.

## Open decisions

These need a maintainer call before/at implementation:

- **L3 subject** — build-local (freshest, needs Go on target) vs the `edge` GHCR
  image (matches the shipped artifact, lags a PR) as the canonical black-box binary.
- **gantry process in L3-infra** — a subprocess the Go test spawns (owns lifecycle,
  tests `SIGTERM`/restart directly — recommended) vs a systemd unit Ansible manages.
- **CI registry matrix** — confirm `{registry:2, zot}` default; include `registry:3`?
- **Routable driver** — `multipass`, a specific cloud provider, or self-hosted only
  (decides the inventory plugin and vault contents).
- **`tools/e2e-seed` in-repo** — acceptable to add the small pure-Go seeder binary?
- **TLS registry in the routable matrix** — exercise `ca_cert`/mTLS/TPM store
  fields, or is plain-HTTP + `insecure-registries` enough for the first cut?

## See also

- [e2e-testing.md](e2e-testing.md) — the manual feature matrix this automates.
- [development.md](development.md) — the devcontainer, unit/integration tests, the
  loopback-insecure constraint, proto regen.
- [stores.md](stores.md) · [retention.md](retention.md) · [verification.md](verification.md)
  · [observability.md](observability.md) · [api.md](api.md) — feature design.
