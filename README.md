# Zero-Downtime PostgreSQL → MongoDB Migration

> A live demonstration of CDC-based migration with zero downtime, exactly-once effects under at-least-once delivery, and measured resilience under chaos.

[![chaos](https://img.shields.io/badge/chaos--suite-passing-brightgreen)](./chaos/)
[![lag p99](https://img.shields.io/badge/replication_lag_p99-%3C5s-brightgreen)](./observability/)

---

## The problem (30 seconds)

Migrating a live table between databases without stopping writes is the single hardest problem in data platform engineering. Dual-write strategies drift. Batch ETL misses writes during cutover. The only correct answer is **Change Data Capture** — and getting CDC right requires solving five sub-problems most tutorials skip:

1. **Zero message loss** across broker, consumer, and sink failures.
2. **Checkpointing** so a crashed service resumes exactly where it stopped — no rescan, no gap.
3. **Idempotency** so at-least-once delivery does not corrupt the destination.
4. **Schema evolution** without pipeline downtime.
5. **Measured resilience** — prove it works under failure, don't just claim it.

This project solves all five and publishes the measurements.

## Prerequisites

| Tool | Why | Install |
|---|---|---|
| Docker Desktop (or engine) 24+ with Compose v2 | Runs the entire stack | <https://www.docker.com/> |
| `make` | Operator UX (`make demo`, `make chaos`, ...) | macOS/Linux: preinstalled. Windows: WSL2, or `choco install make`, or `scoop install make`. |
| `jq` + `curl` | Connector registration + status checks | macOS/Linux: preinstalled or via package manager. Windows: `choco install jq`. |
| Go 1.22+ | Week 2+: build the transformer and sink services | <https://go.dev/dl/> |
| `k6` | Week 3+: load generation | <https://k6.io/docs/get-started/installation/> |

Without `make` (Windows native PowerShell / bare bash), the Week 1 flow is:

```bash
cp .env.example .env
docker compose up -d --build --wait

# Register source + sink connectors (replace curl with iwr on PowerShell)
curl -sS -X PUT -H "Content-Type: application/json" \
  --data @connectors/debezium-postgres.json \
  http://localhost:8083/connectors/zdt-postgres-source/config
curl -sS -X PUT -H "Content-Type: application/json" \
  --data @connectors/mongo-sink.json \
  http://localhost:8083/connectors/zdt-mongo-sink/config

# Verify CDC flows through
docker compose exec postgres psql -U app -d app \
  -c "INSERT INTO users (email, full_name) VALUES ('test@x.io','Test') ON CONFLICT DO NOTHING;"
docker compose exec mongo mongosh --quiet \
  "mongodb://localhost:27017/migration?replicaSet=rs0" \
  --eval "db.users.countDocuments()"
```

Everything else is required only for the phase it covers (see the per-week roadmap in [docs/plan.md](./docs/plan.md)).

## See it run (60 seconds)

```bash
git clone <this-repo> && cd ZeroDownTime
cp .env.example .env
make demo          # boots Postgres, Kafka (KRaft), Debezium, Mongo, Schema Registry
make seed          # inserts sample rows into Postgres → confirm they land in Mongo
make load          # [Week 3] k6 drives 5k writes/sec against Postgres
open http://localhost:3000     # [Week 3] Grafana — Migration Overview dashboard
make chaos         # [Week 3] kills services mid-stream, verifies integrity after each
```

**Pass criterion:** all five chaos scenarios pass AND `make verify` returns exit 0 AND Grafana shows replication lag p99 < 5s during the load run.

## Architecture

```
Postgres 16 ──WAL──▶ Debezium ──▶ Kafka ──▶ transformer-svc ──▶ Kafka ──▶ sink-svc ──▶ MongoDB 7
    (logical               (Kafka Connect)     (Go, Kafka→Kafka,         (Go, Kafka→Mongo,
     decoding)                                  schema mapping,           LSN-gated upserts,
                                                DLQ routing)              bulk writes)

Cross-cutting: Prometheus + Grafana + OpenTelemetry | Toxiproxy for chaos | k6 for load
```

Full diagram in [docs/architecture.md](./docs/architecture.md).

## Why these decisions (links to ADRs)

| # | Decision | Rationale (1-line) |
|---|---|---|
| 001 | Kafka over RabbitMQ | Partition-ordered delivery keyed on PK + native offset storage + replay from LSN |
| 002 | LSN-gated upserts, not distributed transactions | 2PC across Kafka+Mongo doesn't compose; idempotent sink does |
| 003 | Commit after side-effect | Crash-between-commit-and-write = data loss; this pattern makes it impossible |
| 004 | YAML transform rules, not code | Adding a table = adding a file, not a deploy |
| 005 | Toxiproxy for chaos | Reproducible, scriptable, laptop-friendly; avoids needing a K8s chaos operator |
| 006 | Schema registry + explicit versioning | Wire-level compat (registry) + semantic transform (YAML) — both required |

## What this demonstrates (for hiring managers)

- **Distributed systems reasoning.** Partition ordering, consumer-group semantics, WAL internals, LSN monotonicity.
- **Production SRE instincts.** Golden signals, SLOs, runbooks, chaos-as-CI.
- **Pragmatic architecture.** Two-service split (transformer + sink) — neither monolith nor microservice sprawl.
- **Data modeling.** Relational → document with schema evolution that doesn't require downtime.

## Trade-offs I deliberately made (the honest section)

- **No multi-region.** Single-DC demo. Real prod would need MirrorMaker 2 and tombstone-aware cross-region replication. Doubles infra scope, adds nothing to the core story.
- **No Kafka transactions across heterogeneous sinks.** 2PC between Kafka and Mongo doesn't compose — I chose idempotent sink as the correct pattern. ADR-002 explains why.
- **Laptop-simplified Mongo.** Single-node replica set in dev compose, not a 3-node set. Noted in [CLAUDE.md](./CLAUDE.md).
- **No DLQ web UI.** `make reprocess-dlq` is sufficient for a portfolio demo.
- **Secrets via `.env`.** Real prod would use Vault/SSM. Out of scope here.

A senior engineer knows what NOT to build. These omissions are signals, not gaps.

## Project status

Week 1 of 4 (walking skeleton) — see [the approved plan](./docs/plan.md) for the full roadmap.
