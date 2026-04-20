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

## Delivered state (Week 4 complete)

All three services exist, all are Go, all are tested and chaos-verified:

- **`transformer-svc`** — Go, franz-go consume+produce loop. Loads `schema/transforms/*.yml` at boot, renames `payload.after` keys per rule on each event, publishes to `transformed.<table>`. Tombstones forwarded as-is. Commit-after-produce (ADR-003). See `services/transformer/`.

- **`sink-svc`** — Go, franz-go consumer + Mongo driver. Decodes the Debezium envelope, builds LSN-gated upserts (ADR-002), dispatches one `BulkWrite` per collection per poll batch (ADR-003). ~7,300 w/s sustained drain rate on commodity hardware (measured; the Week-3 "240 w/s" finding is closed).

- **`loadgen`** — Go, pgx. Translates k6 HTTP into Postgres writes. Proves 3.7k RPS sustained with 0 failures on the loadgen side.

The two-service split (transformer + sink) isn't accidental. Transformer is CPU-bound (parsing, mapping, validation) and scales by partition count. Sink is I/O-bound and owns LSN-gating correctness. Separate services = separate scaling, separate failure domains, separate SLOs. Both inherit the same commit-after-side-effect invariant at slightly different granularities (per-produce for transformer, per-batch for sink).
