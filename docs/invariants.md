# Pipeline invariants

Read this before changing anything in the pipeline. These are the constraints that, if violated, silently break correctness in ways that are hard to detect without load + chaos tests.

## Hard invariants (NEVER violate)

1. **Partition key = source PK.** Every Debezium/transformer/sink topic is partitioned by the source table's primary key. An UPDATE must never overtake an INSERT for the same row. Do not change partition keys without a full topic rebuild.

2. **Commit-after-side-effect.** Consumers MUST manually commit offsets AFTER the downstream write succeeds. Never set `enable.auto.commit=true`. Crash between write and commit is fine, idempotency absorbs it. Crash between commit and write is data loss.

3. **LSN-gated writes.** Every Mongo upsert/delete carries the Postgres LSN and filters `sourceLsn < event.lsn`. Never remove this filter. It's the only defense against out-of-order replay overwriting newer data with older.

4. **Producer settings.** `acks=all`, `enable.idempotence=true`, `max.in.flight.requests.per.connection=5`. Do not relax for throughput without an ADR.

5. **`min.insync.replicas=2`, RF=3.** In production compose. The local dev compose (see `docker-compose.override.yml` if present) may relax this for laptop resource reasons. If so, it must be called out and never shipped as "the demo".

6. **DLQ is write-only from services.** Services produce to DLQ, they never consume from it. Humans reprocess via `make reprocess-dlq`. This prevents poison-loop storms.

## Conventions

- **Go:** `gofmt`, `golangci-lint run`, errors wrapped with `fmt.Errorf("...: %w", err)`, no `panic()` in service code (only in `main()` boot failures).
- **Metrics:** All prefixed `migration_`. Follow Prometheus naming: counters end `_total`, durations end `_seconds`, bytes end `_bytes`.
- **Tests:** Every transform rule gets a test in `services/transformer/internal/mapper/mapper_test.go`. Every chaos scenario has a `# PASS:` comment stating its criterion.
- **Schema registry:** All topics use Avro with BACKWARD compatibility. Breaking changes require the dual-write pattern (see `docs/decisions/006-schema-evolution.md`).

## Verification before claiming "done"

1. `make demo` boots the stack cleanly (all healthchecks pass).
2. `make seed` inserts test rows; they appear in Mongo within 2s.
3. `make load` runs k6 for 60s at target RPS without error.
4. `make chaos` passes all 5 scenarios.
5. `make verify` (a.k.a. `chaos/verify-integrity.sh`) returns exit 0.
6. Grafana "Migration Overview" shows replication lag p99 < 5s during load.

Tests and types pass != feature works. If you didn't run the chaos suite, the feature isn't done.

## What this project is NOT

- Not a general-purpose ETL tool. **Postgres 16 -> MongoDB 7 only.** Other sources/sinks are explicitly out of scope.
- Not production-hardened. Secrets live in `.env` and compose env vars. Real prod would use Vault/SSM.
- Not a replacement for Debezium. It's the idempotent-sink + observability + chaos layer around Debezium.
- Not multi-region. Single-DC demo. Multi-region would need MirrorMaker 2 and is out of scope.

## Directory map (fast orientation)

- `services/transformer/` - Go, Kafka->Kafka. Stateless. Schema mapping + DLQ routing.
- `services/sink/` - Go, Kafka->Mongo. Idempotent upserts. LSN-gated.
- `connectors/` - Debezium + Mongo Kafka Connector JSON configs.
- `schema/postgres/` - source DDL. `schema/transforms/` - YAML SQL->Mongo rules.
- `observability/` - Prometheus config, Grafana dashboards (JSON), alert rules.
- `chaos/` - scenario scripts + `verify-integrity.sh`.
- `load/k6/` - write-mix load generator.
- `docs/decisions/` - ADRs, one per architectural pillar.
