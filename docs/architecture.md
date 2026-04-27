# Architecture

Companion to the top-level [README](../README.md).

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
              │          transformed.<table>                              │
              │          _migration_checkpoints (sink-owned)              │
              └────────┬───────────────────────────────────────────────────┘
                       │
                       ▼
              ┌────────────────────┐
              │  transformer-svc   │
              │  Go, franz-go      │
              │  stateless         │
              └────────┬───────────┘
                       │
                       ▼
              ┌─────────────────────┐
              │  sink-svc           │
              │  Go, mongo-driver   │
              │  LSN-gated upserts  │
              └─────────┬───────────┘
                        │
                        ▼
              ┌─────────────────────┐
              │     MongoDB 7       │
              │  migration DB:      │
              │    users            │
              │    orders           │
              │    order_items      │
              │    _migration_*     │
              └─────────────────────┘

Observability:
   Prometheus -> [kafka-exporter, connect /metrics, transformer /metrics,
                  sink /metrics, mongo-exporter, postgres-exporter]
              -> Grafana + Alertmanager
   Toxiproxy injects faults between kafka <-> transformer and transformer <-> mongo.
   k6 drives writes via the loadgen HTTP API.
```

## Topic-level invariants

| Topic | Partitions | RF (dev) | RF (prod) | Retention | Notes |
|---|---|---|---|---|---|
| `cdc.users` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `cdc.orders` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `cdc.order_items` | 6 | 1 | 3 | 7d | Partitioned by `id` (PK) |
| `transformed.*` | 6 | 1 | 3 | 3d | Written by transformer |
| `dlq.source` | 3 | 1 | 3 | ∞ | Debezium failures |
| `dlq.sink` | 3 | 1 | 3 | ∞ | Sink failures |

**Why PK partitioning, not random.** Same-row events must stay in order. A later UPDATE cannot overtake an earlier INSERT for the same row, or the sink writes stale data. Co-partitioning by PK guarantees a single consumer processes all events for a given row in order. See [invariant #1 in docs/invariants.md](./invariants.md).

## Replication slot / WAL safety

- Postgres: `wal_level=logical`, `max_replication_slots=10`, `max_wal_senders=10`.
- Debezium uses the `pgoutput` plugin (built into Postgres, no extension required).
- Slot name `zdt_slot` persists across Debezium restarts → restart is crash-safe.
- Heartbeat interval 1s prevents WAL from being recycled when the target tables are idle but the broader DB is busy (the #1 cause of silent data loss in real deployments).
- `REPLICA IDENTITY FULL` on all source tables so DELETE events carry the full before-image - crucial for correct tombstone handling downstream.

## Services

- **`transformer-svc`** (Go, franz-go). Consume + produce loop. Loads
  `schema/transforms/*.yml` at boot, renames `payload.after` keys per rule,
  publishes to `transformed.<table>`. Tombstones forwarded as-is. Offsets
  commit only after produce is ack'd. See `services/transformer/`.

- **`sink-svc`** (Go, franz-go + mongo-driver). Decodes the Debezium envelope,
  builds LSN-gated upserts, dispatches one `BulkWrite` per collection per poll
  batch. See `services/sink/` and `docs/decisions/002-lsn-gated-upserts.md`.

- **`loadgen`** (Go, pgx). Translates k6 HTTP into Postgres writes. Used by
  `load/k6/write-mix.js`.

The transformer / sink split keeps a CPU-bound transform pipeline scaling on
partition count separate from the I/O-bound sink; both share the
commit-after-side-effect invariant at different granularities (per-produce vs
per-batch).
