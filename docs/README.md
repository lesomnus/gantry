# gantry documentation

Topic guides for [gantry](../README.md). The [README](../README.md) covers what
gantry is, how to install it, and a quick start; these docs go deep on each
subsystem. The full annotated configuration lives in
[../gantry.yaml](../gantry.yaml).

| Guide | Covers |
|---|---|
| [stores.md](stores.md) | Store kinds (oci / docker / containerd), copy vs proxy fill, `rewrite` rules, `downstream_host`/`pull_host`, outbound TLS (private-CA and TPM-sealed mTLS), caller-chosen `as` names, and digest pinning. |
| [retention.md](retention.md) | Per-engine-store image GC: usage tracking, the policy cascade and evaluation order, digest counting, pins, the untagged reaper, inventory reconciliation, and the adaptive scheduler. |
| [verification.md](verification.md) | Source-image signature verification (Notary Project / notation): enforcement modes, the trust store and policy, digest pinning, `copy_referrers`, and the `VerifyService` surface. |
| [observability.md](observability.md) | The OpenTelemetry metrics/traces pipeline and instrument catalog, the durable audit log (`EventService`), and health/readiness. |
| [api.md](api.md) | gRPC contract and behaviors: coalescing and response trailers, `Idempotency-Key`, the dedup key, dry-run `Plan`, the live-vs-durable job model, pagination, stable ids, and auth. |
| [test-environment.md](test-environment.md) | The devcontainer, the live docker/containerd integration tests, the full job loop, and the insecure-registry constraints. |

Design decisions, remaining work, and the release checklist are tracked in
[../ROADMAP.md](../ROADMAP.md).
