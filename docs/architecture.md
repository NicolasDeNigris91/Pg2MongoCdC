# Architecture

This document is the deep-dive companion to the top-level [README](../README.md) and the full [approved plan](./plan.md).

## Data flow (detailed)

```
┌─────────────────────┐      WAL (pgoutput)     ┌─────────────────────────┐
│   PostgreSQL 16     │◀────── logical ─────────│ Debezium PG Connector   │
│ publication:        │      replication slot   │ (Kafka Connect worker)  │
│   zdt_publication   │      zdt_slot           │ snapshot.mode=initial   │
│ REPLICA IDENTITY=   │                         │ heartbeat.interval=1s   │
│   FULL (all tables) │                         │ publication.autocreate  │
└─────────────────────┘                         │   =disabled             │
                                                └──────────┬──────────────┘
                                                           │ Avro-encoded,
                                                           │ keyed by PK,
                                                           │ via Schema Registry
                                                           ▼
              ┌───────────────────────────────────────────────────────────┐
              │  Kafka (KRaft)                                            │
              │  topics: cdc.users | cdc.orders | cdc.order_items         │
              │          dlq.source (Debezium failures)                   │
              │          dlq.sink   (Mongo sink failures)                 │
              │          transformed.<table>  (Week 2, post transformer)  │
              │          _migration_checkpoints (Week 2, sink-owned)      │
              └────────┬────────────────────────────────────┬─────────────┘
                       │                                    │
          (Week 2+)    ▼                                    ▼ (Week 1 only:
              ┌────────────────────┐              direct Mongo sink
              │  transformer-svc   │              via MongoSinkConnector
              │  Go, franz-go      │              with PostgresHandler)
              │  stateless, HPA    │                        │
              │  by partition cnt  │                        │
              └────────┬───────────┘                        │
                       │                                    │
                       ▼                                    ▼
              ┌─────────────────────────────────────────────────┐
              │  sink-svc (Week 2) │   MongoDB Kafka Connector   │
              │  Go, Mongo driver  │        (Week 1 only)        │
              │  idempotent upsert │                             │
              │  LSN-gated         │                             │
              └─────────────────────────────────────────────────┘
                                    │
                                    ▼
                          ┌─────────────────────┐
                          │     MongoDB 7       │
                          │  migration DB:      │
                          │    users            │
                          │    orders           │
                          │    order_items      │
                          │    _migration_*     │  (checkpoint docs, Week 2)
                          └─────────────────────┘

Cross-cutting (Week 3):
   Prometheus  ── scrapes ──▶ [kafka-exporter, connect /metrics,
                                transformer /metrics, sink /metrics,
                                mongo-exporter, postgres-exporter]
                            ──▶ Grafana dashboards + Alertmanager
   Toxiproxy   ── proxies ──▶ kafka ⇆ transformer,
                              transformer ⇆ mongo   (chaos injection)
   k6          ── load ─────▶ Postgres (INSERT/UPDATE/DELETE generator)
   OpenTelemetry ── traces ─▶ Jaeger (optional), traceparent in Kafka headers
```

## Topic-level invariants

| Topic | Partitions | RF (dev) | RF (prod) | Retention | Notes |
|---|---|---|---|---|---|
| `cdc.users` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `cdc.orders` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `cdc.order_items` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `transformed.*` | 6 | 1 | 3 | 3d | Week 2+ — written by transformer |
| `dlq.source` | 3 | 1 | 3 | ∞ | Debezium failures |
| `dlq.sink` | 3 | 1 | 3 | ∞ | Sink failures |

**Why PK partitioning, not random.** Same-row events must stay in order. A later UPDATE cannot overtake an earlier INSERT for the same row, or the sink writes stale data. Co-partitioning by PK guarantees a single consumer processes all events for a given row in order. See [CLAUDE.md invariant #1](../CLAUDE.md).

## Replication slot / WAL safety

- Postgres: `wal_level=logical`, `max_replication_slots=10`, `max_wal_senders=10`.
- Debezium uses the `pgoutput` plugin (built into Postgres, no extension required).
- Slot name `zdt_slot` persists across Debezium restarts → restart is crash-safe.
- Heartbeat interval 1s prevents WAL from being recycled when the target tables are idle but the broader DB is busy (the #1 cause of silent data loss in real deployments).
- `REPLICA IDENTITY FULL` on all source tables so DELETE events carry the full before-image — crucial for correct tombstone handling downstream.

## Week 1 vs. the final architecture

**Today (Week 1 walking skeleton):** The MongoDB Kafka Connector sits where the Go `sink-svc` will eventually sit. It consumes `cdc.*` topics using Debezium's `PostgresHandler` and writes upserts/deletes to Mongo. This proves the Postgres → Kafka → Mongo path end-to-end with zero custom code.

**Week 2 onward:** The off-the-shelf Mongo sink is replaced by:

1. **`transformer-svc`** — a Go service that consumes `cdc.*`, applies declarative YAML schema rules, produces to `transformed.*`, and routes malformed events to `dlq.source` with full error context.

2. **`sink-svc`** — a Go service that consumes `transformed.*` and performs **LSN-gated idempotent upserts** into Mongo. This is where the "exactly-once effect under at-least-once delivery" story lives. See [ADR-002](./decisions/002-lsn-gated-upserts.md) when it's written.

The two-stage design isn't accidental. The transformer is CPU-bound (parsing, mapping, validation) and horizontally scales by partition count. The sink is I/O-bound and requires tight idempotency control. Separate services = separate scaling, separate failure domains, separate SLOs, and `transformed.*` serves as a replay buffer when debugging sink issues without touching the WAL.
