# gantry documentation

Topic guides for [gantry](../README.md). The [README](../README.md) covers what
gantry is, how to install it, and a quick start; these docs go deep on each
subsystem. The full annotated configuration lives in
[../gantry.yaml](../gantry.yaml).

| Guide | Covers |
|---|---|
| [stores.md](stores.md) | Store kinds (oci / docker / containerd), copy vs proxy fill, the cache-side reference, `downstream_host`/`pull_host`, outbound TLS (private-CA and TPM-sealed mTLS), caller-chosen `as` names, and digest pinning. |
| [retention.md](retention.md) | Per-engine-store image GC: usage tracking, the policy cascade and evaluation order, digest counting, pins, the untagged reaper, inventory reconciliation, and the adaptive scheduler. |
| [verification.md](verification.md) | Source-image signature verification (Notary Project / notation): enforcement modes, the trust store and policy, digest pinning, `copy_referrers`, and the `VerifyService` surface. |
| [enforcement.md](enforcement.md) | Runtime signature enforcement (quarantine): force-removing running containers whose image is unsigned, the durable verdict cache, the offline local signature layout, the `on_unavailable` policy, and gantry's self-protection. |
| [observability.md](observability.md) | The OpenTelemetry metrics/traces pipeline and instrument catalog, the durable audit log (`EventService`), and health/readiness. |
| [api.md](api.md) | gRPC contract and behaviors: coalescing and response trailers, `Idempotency-Key`, the dedup key, dry-run `Plan`, the live-vs-durable job model, pagination, stable ids, and auth. |
| [development.md](development.md) | Contributor onboarding: the devcontainer, repository layout, building, unit and live integration tests, the full copy→pull loop, insecure-registry constraints, and regenerating the gRPC contract. |
| [e2e-testing.md](e2e-testing.md) | End-to-end testing: the layered automated suite (`internal/e2e` — hermetic in-process → real daemon → black-box binary & scratch image → Ansible infra), the build-once/test-the-artifact/promote-by-digest CI, plus the manual `grpcurl` runbook for each user-facing feature. |

Project status, the pre-v1 maintainer to-dos, and the non-goals are in the
[README](../README.md#status); the full design history lives in the git log.
