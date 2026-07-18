# Development

A guide to getting a gantry development environment running, building and
testing it, and regenerating the gRPC contract. It targets a new contributor:
by the end you can build the binary, run the unit and live integration tests,
drive a full copy→pull loop against real daemons, and regenerate the API.

For what each subsystem *does*, read the topic guides — [stores.md](stores.md),
[retention.md](retention.md), [verification.md](verification.md),
[observability.md](observability.md), [api.md](api.md); this doc is about
*working on* the code.

## Getting started (the devcontainer)

The repository ships a [devcontainer](../.devcontainer/) that is the supported
way to develop gantry. Open the repo in a devcontainer-aware editor (VS Code:
*Reopen in Container*) or any tool that reads `.devcontainer/devcontainer.json`.
It brings up three services wired together so the live integration tests and the
full loop work with no extra setup:

- **`dev`** — the development container (Go 1.26, `/workspace` = the repo, runs as
  uid 1000). You run `gantry` and `go test` here. Module and build caches are
  volume-mounted so rebuilds stay fast.
- **`docker`** — a Docker-in-Docker daemon (`docker:dind`) reachable at
  `tcp://docker:2375` (exported as `DOCKER_HOST`).
- **`containerd`** — a **dedicated containerd sidecar**, independent of docker.

Working without the devcontainer is possible but unsupported: you need a Go
toolchain matching [`go.mod`](../go.mod) (1.26.x) and, for the live tests, a
docker daemon (and optionally a containerd socket) reachable at the addresses
below.

### Topology

The three services from
[`.devcontainer/docker-compose.yaml`](../.devcontainer/docker-compose.yaml):

```
┌───────────────────────────────────────┐         ┌─────────────────────────────────────┐
│ dev (uid 1000)                        │         │ docker  (image: docker:dind)        │
│  - runs gantry / go test              │         │  - dockerd  (tcp://0.0.0.0:2375)    │
│  - DOCKER_HOST=tcp://docker:2375 ─────┼──────▶ │  [manual e2e] registry:2 → :5000   │
│  - CONTAINERD_ADDRESS=                │         │  127.0.0.1:5000 (dind loopback)     │
│      /run/containerd/…sock  ◀━━┐    │         └─────────────────────────────────────┘
│  - CONTAINERD_NAMESPACE=gantry   │    │         ┌─────────────────────────────────────┐
│  127.0.0.1:5000 ─(local fwd)─────┼────┼──────▶ │ containerd  (image: docker:dind,    │
│                          shared  │    │         │   command: containerd)              │
│                          volume  └────┼━━━━━━━━━┥  - dedicated containerd             │
└───────────────────────────────────────┘         │      /run/containerd/…sock         │
                                                  └─────────────────────────────────────┘
```

The `docker:dind` image already bundles a standalone `containerd` binary (plus
`runc`/shim), so the sidecar runs `command: containerd` on the same image rather
than needing a custom one. The pull-and-unpack that the retention and
anchored-pull tests exercise needs only the daemon and the overlayfs snapshotter
— no CNI, no `runc` tasks — so a plain containerd is enough.

Historically the sidecar was docker's own bundled containerd socket (namespace
`moby`, shared via a `chmod` hack); splitting it into a dedicated service removed
that coupling.

### Two daemon endpoints

| Daemon     | Address from `dev`                                        | Notes                                        |
| ---------- | --------------------------------------------------------- | -------------------------------------------- |
| docker     | `tcp://docker:2375` (`DOCKER_HOST`)                       | the hostname `docker` resolves to the dind container |
| containerd | `/run/containerd/containerd.sock` (`CONTAINERD_ADDRESS`)  | the dedicated sidecar, exposed via the socket share below |

These are only the **control channel** — how gantry tells a daemon *"pull this"*.
Any address works; they are unrelated to the [insecure-registry
constraint](#insecure-registry-constraint-loopback-only), which is about the
*registry* address a daemon pulls from.

### Sharing the containerd socket

The dedicated sidecar's socket lives inside its own container, invisible to
`dev`. Compose exposes it the same way it does docker's:

1. **Shared volume** `containerd.run` is mounted at `/run/containerd` in both the
   `containerd` and `dev` services. (A unix socket is the same inode in the
   shared volume, so the kernel routes the cross-container connection.)
2. **Permissions** — the socket is `root:root 0660` with a restrictive parent, so
   `dev`'s uid 1000 cannot use it as-is. The `containerd` service's `command`
   runs a background loop that keeps the directory `0711` and the socket `0666`
   (`exec containerd` stays PID 1; the loop re-applies every 3s), mirroring the
   docker service.
3. `dev` gets `CONTAINERD_ADDRESS=/run/containerd/containerd.sock`.

This takes effect on a **devcontainer rebuild**. Verify:

```sh
ls -l /run/containerd/containerd.sock   # expect srw-rw-rw-
```

The sidecar keeps its own data store (`containerd.data`); its image namespace is
`CONTAINERD_NAMESPACE` (default `gantry`). A namespace is created lazily on the
first pull, so any name works — set `namespace: "gantry"` on a containerd target.

## Repository layout

```
main.go            process entrypoint
cmd/               CLI (root + serve / config / version subcommands)
internal/
  cpx/             registry source (copy/proxy) + the Copier that drives a job   → stores.md
  down/            engine drivers (docker, containerd) + capability interfaces   → stores.md
  retention/       usage watcher, policy cascade, GC, the bbolt usage index      → retention.md
  verify/          Notary Project / notation signature verification              → verification.md
  event/           the bbolt audit log and its recorder                          → observability.md
  rpc/             the gRPC service implementations                              → api.md
  store/           the store set: config resolution, capabilities
  health/          store health probes + readiness
  tpm/             TPM-sealed client-mTLS signer
  xport/           outbound transport (auth / TLS / TPM), memoized per store
proto/gantry/      the API contract (entity protos + merged service protos)
proto.svc/gantry/  hand-written RPC overlays merged onto the generated services
pb/                generated Go stubs, request builders, client/server wiring
docs/              these topic guides
gantry.yaml        the full annotated config; gantry.nomad.hcl / gantry.hday.yaml are examples
```

New engine kind = one file in `internal/down` plus a factory case; verification
and GC light up automatically for an engine that satisfies the optional
capability interfaces.

## Building and running

```sh
go build ./...                          # host build
go run . serve --config gantry.yaml     # run the server
gantry version                          # build stamp (version / git rev / dirty)
```

The release binary is built with **CGO disabled** and runs `FROM scratch`, so
every dependency must be pure Go — do not introduce a cgo dependency (this is why
the sqlite-free bbolt index and the WASM-free stack matter). The container image
is built with buildx bake:

```sh
docker buildx bake build app   # cross-build binaries into ./dist, then the image
```

The [`docker-bake.hcl`](../docker-bake.hcl) targets are `test` (the test stage),
`build` (compiles binaries into `./dist`), and `app` (assembles the runtime image
from `./dist`).

## Testing

```sh
go test -race ./...
```

- **Unit tests** need no daemon: config round-trip, `rewrite` rules, `Store`
  concurrency, the copy/proxy `Source` against an in-memory registry, the
  `Copier`, the engine destination against a fake target, the retention policy
  evaluator, and so on.
- **Live integration tests** self-skip when their daemon is absent, so the suite
  is safe to run anywhere:
  - `internal/down/docker_integration_test.go` — pings `tcp://docker:2375`, then
    really pulls `alpine:latest` (a different image from the containerd test's
    `busybox`, so they don't collide in docker 29's shared containerd content
    store).
  - `internal/down/containerd_integration_test.go` — if `CONTAINERD_ADDRESS` is
    present, pulls under `CONTAINERD_NAMESPACE` (default `gantry`): a plain pull, a
    digest-anchored pull (asserting no **unrequested** digest-named record lingers
    after a retag), and digest `as` names (a requested digest name is recorded
    over the pulled content, while a name claiming a *different* digest is
    rejected). Skips immediately if the socket is missing.

CI (`.github/workflows/ci.yaml`) runs `go test -race ./...` on the runner — its
docker daemon drives the docker integration tests; the containerd-only tests
self-skip — then builds the image via bake and, on a push to `main`, publishes
`ghcr.io/lesomnus/gantry:edge`. Keep the build green: `go test -race ./...`, and
`gofmt`/`go vet` clean.

## End-to-end manual verification (the full loop)

This exercises the whole path in one go: gantry copies an image into a cache
registry, then both daemons pull it back out of that cache. For a feature-by-feature
end-to-end test plan on a standard remote→cache→engine environment — copy, pull,
proxy, `as` names, digest pinning, verification, GC, dedup, and more — see
[e2e-testing.md](e2e-testing.md).

### 1. Cache registry + loopback forward

Run the cache registry on the dind daemon, published on the dind host at `:5000`.
So that `dev` and the daemons use the **same reference** `127.0.0.1:5000/…`, add a
`127.0.0.1:5000 → docker:5000` TCP forward inside `dev` (the reason is the
[insecure constraint](#insecure-registry-constraint-loopback-only)):

```sh
docker run -d -p 5000:5000 --name cache registry:2

# A minimal dependency-free forwarder (drop it in the scratchpad and run it).
cat > /tmp/fwd.go <<'EOF'
package main
import ("io";"net")
func main(){ l,_:=net.Listen("tcp","127.0.0.1:5000"); for{ c,e:=l.Accept(); if e!=nil{continue};
  go func(c net.Conn){defer c.Close(); u,e:=net.Dial("tcp","docker:5000"); if e!=nil{return};
  defer u.Close(); go io.Copy(u,c); io.Copy(c,u)}(c) } }
EOF
go run /tmp/fwd.go &
curl -s -o /dev/null -w "%{http_code}\n" http://127.0.0.1:5000/v2/   # 200
```

### 2. gantry config

```yaml
serve:
  addr: "127.0.0.1:18080"
  allow_unknown_stores: true   # let `source: docker.io` resolve to a bare host
stores:
  cache:       { kind: "oci", host: "127.0.0.1:5000", insecure: true, mode: "copy" }
  dind-docker: { kind: "docker",     address: "tcp://docker:2375" }
  dind-ctr:    { kind: "containerd", address: "/run/containerd/containerd.sock", namespace: "gantry" }
```

> Platform is chosen per job (the `platforms` field of the `Add` below) — there is
> no global worker setting. Omitted, a registry copy takes every platform of the
> source and an engine target takes the daemon host's platform.

### 3. Submit jobs and check

```sh
go run . --config gantry-e2e.yaml serve &

grpcurl -plaintext 127.0.0.1:18080 gantry.StoreService/List   # 3 stores, capabilities, ready

add() { grpcurl -plaintext -d "$1" 127.0.0.1:18080 gantry.JobService/Add \
          | sed -n 's/.*"id": "\([^"]*\)".*/\1/p'; }
get() { grpcurl -plaintext -d "{\"ref\":{\"id\":\"$1\"}}" 127.0.0.1:18080 gantry.JobService/Get; }

# 1) Copy into the cache (registry copy). Use an image present nowhere yet, to
#    avoid content-store confusion.
CP=$(add '{"ref":"busybox:latest","source":{"name":"docker.io"},"target":{"name":"cache"},"platforms":["linux/amd64"]}')
get "$CP"   # transfers[]: the cache copy

# 2) Pull from the cache to each engine. One job = one target, so one job per engine.
D=$(add '{"ref":"busybox:latest","source":{"name":"cache"},"target":{"name":"dind-docker"}}')
C=$(add '{"ref":"busybox:latest","source":{"name":"cache"},"target":{"name":"dind-ctr"}}')
get "$D"; get "$C"
```

Expected: all three jobs reach `state=done`, each job's transfer ends
`TRANSFER_STATE_DONE` (the cache copy with `bytes_done == bytes_total`). Confirm
the registry (`curl http://127.0.0.1:5000/v2/library/busybox/tags/list`) and
`docker images` hold the image.

### 4. Clean up

```sh
kill %1 %2 2>/dev/null
docker rm -f cache
docker rmi 127.0.0.1:5000/library/busybox:latest busybox:latest 2>/dev/null
```

## Insecure-registry constraint (loopback only)

When the cache is plain-HTTP, **the downstream daemon pulling from the cache** has
to trust that registry as insecure. This constraint is on the **cache registry
address**, not on the gantry↔daemon control socket.

- **docker daemon**: automatically trusts `127.0.0.0/8` and `::1` as insecure, so
  `127.0.0.1:5000` works. A non-loopback address (`registry.cache.local:5000`
  etc.) needs `insecure-registries` in `daemon.json`.
- **containerd**: its default resolver also treats loopback as plain-HTTP, so
  `127.0.0.1:5000` works. A non-loopback address needs
  `/etc/containerd/certs.d/<host>/hosts.toml`.

That is why this setup uses `127.0.0.1:5000` and the forward — in DinD, the daemon's
own loopback is the only address it will pull an insecure cache from **without
extra config**. gantry does not enforce the downstream's insecure policy
(split-brain: gantry's own copy follows the store's `insecure`, but a daemon's
cache-pull follows the daemon's config). To use a non-loopback insecure cache in a
real fleet, configure each daemon as above or put TLS in front of the cache.

### Downstream host override

You can decouple the address gantry **pushes to** from the address a daemon
**pulls from**: push to the real location via the cache store's `host`, but have
the daemon pull under a different name it has been configured to trust.

```yaml
stores:
  cache:
    kind: "oci"
    host: "192.168.0.22:5000"        # where gantry pushes/reads
    downstream_host: "cache.cr.com"  # where a daemon pulls (this registry's default)
  k3s:
    kind: "containerd"
    address: "..."
    pull_host: "cache.cr.com:5000"   # per-store override (beats downstream_host)
```

gantry then pushes to `192.168.0.22:5000/library/redis:7` but tells the daemon to
pull `cache.cr.com/library/redis:7` (repo and tag/digest unchanged, host
substituted). Have the daemon trust `cache.cr.com` (TLS or `insecure-registries`)
and point DNS/hosts at `192.168.0.22`, and you sidestep the insecure-trust problem
off loopback. Each target's pull ref is visible in `JobService.Get`'s
`transfers[].ref`. See [stores.md](stores.md) for `downstream_host`/`pull_host`.

## Regenerating the gRPC contract

The generated service protos (`proto/gantry/*_svc.g.proto`) and the Go stubs
(`pb/`) are **committed**, so you only regenerate after changing an entity proto
(`proto/gantry/*.proto`) or a hand-written RPC overlay (`proto.svc/gantry/`).

```sh
scripts/gen-proto.sh
```

The pipeline (see the script header for detail):

1. `protoc-gen-orm-service` emits the CRUD services from the orm-annotated entity
   protos.
2. `protobuf-merge` overlays the hand-written RPCs (`List`, custom actions from
   `proto.svc/gantry/`) onto them.
3. `protoc-gen-go` / `protoc-gen-go-grpc` / `protoc-gen-orm-go` compile everything
   under `proto/gantry` into `pb/`.

The `protobuf-orm` tools are built from a local checkout (`protobuf-merge` is not
fetchable as a Go module); point `ORM_ROOT` at the directory holding the
`github.com/protobuf-orm/{protobuf-orm,protoc-gen-orm-service,protoc-gen-orm-go,protobuf-merge}`
repositories (default `/workspaces/github.com/protobuf-orm`, which the devcontainer
mounts). `VerifyService` is hand-written (`proto.svc`/`verify_svc.proto`), not
orm-generated, which is why it is registered separately from `pb.RegisterServer`
— see [api.md](api.md).

## Conventions

- Write code that reads like its surroundings — match the naming, comment density,
  and idioms of the package you are in.
- `gofmt` and `go vet ./...` must be clean; add or adjust tests for behavior
  changes (`go test -race ./...`).
- Keep the pure-Go / CGO-disabled constraint (the binary must still link `FROM
  scratch`).
- When you change user-facing behavior or config, update the relevant `docs/`
  guide and [`gantry.yaml`](../gantry.yaml); the [README](../README.md) stays an
  overview that links here.
