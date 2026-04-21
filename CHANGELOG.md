# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `LICENSE` (MIT).
- `SECURITY.md` with vulnerability disclosure process and response timelines.
- `CHANGELOG.md` (this file).
- `golangci-lint` configuration and CI step.
- Container image scan (trivy) in CI for all service Dockerfiles.
- Go source security scan (gosec) in CI.

### Fixed
- `chaos/run-all.sh` now iterates scenario paths via `mapfile` array expansion,
  allowing the script to run from a workspace whose absolute path contains
  spaces (commit `1975862`).
- `chaos/verify-integrity.sh` now polls for drain convergence (2s interval,
  60s default timeout) instead of a static 10s `sleep`, eliminating false
  `INTEGRITY FAILED` reports under moderate post-load drain (commit `810539c`).
- **Transformer indefinite hang on first record after `down -v + up`**:
  the kgo client did not request auto-topic-creation from the broker, so
  ProduceSync to a not-yet-existent `transformed.<table>` topic would hang
  forever even though `auto.create.topics.enable=true` was set on the
  broker. KRaft-mode brokers (cp-kafka 7.6.1+) only auto-create when the
  client request explicitly asks for it. Added `kgo.AllowAutoTopicCreation()`
  to the transformer's client config. Verified by `down -v + up + insert + verify`
  end-to-end against a clean stack.

### Known Issues
- Under scenario 01 reruns (consecutive sink SIGKILLs during loadgen write
  traffic), Postgres↔MongoDB row counts can drift by a small number of rows
  in a Mongo-has-more-than-Postgres direction. Root cause under investigation;
  initial reordering hypothesis was ruled out by an integration-level test.
  See the GitHub issue tracker for reproducer and current status.

---

## [1.0.0] — Pre-release (pending bug closure)

Initial shipping milestone: a Postgres→MongoDB CDC pipeline with
idempotent LSN-gated sink, YAML-driven schema transformation, a 5-scenario
chaos suite, and GitHub Actions CI. See README for the measured results
table.

### Added

- **Core pipeline.** Postgres (logical replication) → Debezium → Kafka →
  Go `transformer` → Kafka → Go `sink` → MongoDB replica set.
- **LSN-gated upserts and deletes in the sink** (ADR-002). Integration
  test proves idempotency across 6 replay-ordering cases against a live
  Mongo.
- **Commit-after-side-effect consume loop** (ADR-003). Offsets are marked
  only after `BulkWrite` succeeds; at-least-once delivery is absorbed by
  the LSN gate on replay.
- **Batched BulkWrite** in the sink (~30× faster drain after post-burst
  recovery: 240 w/s → ≥7,300 w/s; see `docs/chaos-findings.md`).
- **YAML-driven transformer** (ADR-004). New tables are added by dropping
  a YAML file under `schema/transforms/`, not by code changes.
- **Chaos scenario suite** (`chaos/scenarios/`): kill sink, Kafka network
  partition, Mongo primary stepdown, Postgres WAL pressure, poison event
  routing. Each scenario carries an explicit `# PASS:` criterion.
- **k6 write-mix load generator** with measured 3.7k RPS sustained,
  0 failed requests.
- **GitHub Actions CI**: unit tests, `go vet`, integration tests against
  live Mongo, end-to-end stack boot with seed + integrity verification.
- **Prometheus metrics** from the sink (`migration_events_processed_total`,
  error counters). Grafana dashboard JSON under `observability/grafana/`.
- **Toxiproxy wiring** for reproducible Kafka network chaos (ADR-005).
- **Architecture Decision Records** under `docs/decisions/` — six ADRs
  covering Kafka choice, LSN gating, commit ordering, YAML transforms,
  Toxiproxy, and schema registry strategy.

### Architecture Invariants

Documented in [`CLAUDE.md`](./CLAUDE.md):

1. Partition key = source primary key.
2. Commit-after-side-effect.
3. LSN-gated writes.
4. Producer `acks=all`, `enable.idempotence=true`.
5. `min.insync.replicas=2`, RF=3 (production compose only; dev compose
   relaxes this and states so).
6. DLQ is write-only from services.

[Unreleased]: https://github.com/NicolasDeNigris91/Pg2MongoCdC/compare/v1.0.0...HEAD
[1.0.0]: https://github.com/NicolasDeNigris91/Pg2MongoCdC/releases/tag/v1.0.0
