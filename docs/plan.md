# Zero-Downtime Data Migration Tool — Architecture & Implementation Plan

## Context

**Why this project exists.** This is a portfolio artifact designed to signal Staff-level Data Engineering competency. It demonstrates the hardest problem in data platforms: moving a live production table from a SQL system to a NoSQL system **without stopping writes, losing events, or corrupting the destination**. Any junior engineer can write a batch ETL script. A Staff engineer has to reason about WAL semantics, partition ordering, exactly-once effects under at-least-once delivery, backpressure, idempotency keys, and failure modes most teams only discover in production incidents.

**Locked tech decisions (from clarification round):**
- Source: **PostgreSQL 16** (logical replication / WAL)
- CDC: **Debezium 2.x** (standalone connector, not embedded)
- Broker: **Apache Kafka** (KRaft mode, 3 brokers)
- Destination: **MongoDB 7**
- Services: **Go 1.22+**
- Orchestration: **Docker Compose** with **Toxiproxy** for chaos and **k6** for load
- Observability: **Prometheus + Grafana + OpenTelemetry**

**Intended outcome.** A `docker compose up` demo that a recruiter or hiring manager can run on a laptop, watch a Grafana dashboard show sub-second replication lag under 5k writes/sec, then kill the transformer mid-stream with Toxiproxy and observe that the system recovers with zero duplicates and zero data loss.

---

## Target Architecture

```
                                ┌─────────────────────┐
                                │   PostgreSQL 16     │
                                │  (logical_decoding) │
                                └──────────┬──────────┘
                                           │ WAL stream (pgoutput)
                                           ▼
                              ┌─────────────────────────┐
                              │ Debezium PG Connector   │
                              │ (Kafka Connect worker)  │
                              └──────────┬──────────────┘
                                         │ produces CDC events
                                         ▼
          ┌──────────────────────────────────────────────────────────┐
          │  Kafka (KRaft, 3 brokers, RF=3, min.insync.replicas=2)   │
          │  topics: cdc.public.<table>  | dlq.<table>               │
          └────────┬────────────────────────────────────┬────────────┘
                   │                                    │
                   ▼                                    │
          ┌────────────────────┐                        │
          │  transformer-svc   │  (Go, consumer group)  │
          │  - schema mapping  │                        │
          │  - validation      │                        │
          └────────┬───────────┘                        │
                   │ produces to transformed.<table>    │
                   ▼                                    │
          ┌─────────────────────┐                       │
          │  sink-svc (Go)      │──── poison events ────┘
          │  - idempotent upsert│
          │  - bulk writes      │
          └────────┬────────────┘
                   │
                   ▼
          ┌─────────────────────┐
          │     MongoDB 7       │  _id = source PK, schemaVersion, sourceLsn
          │  (replica set)      │
          └─────────────────────┘

 Cross-cutting:  Prometheus ── scrapes ──▶ [debezium, transformer, sink, kafka-exporter, mongo-exporter]
                                         ──▶ Grafana dashboards + Alertmanager
                 Toxiproxy   ── proxies ─▶ kafka ⇆ transformer, transformer ⇆ mongo  (chaos injection)
                 k6          ── load ────▶ PostgreSQL  (INSERT/UPDATE/DELETE generator)
```

**Why a two-stage pipeline (transformer → sink) instead of a single consumer?** Separation of concerns. The transformer is CPU-bound (schema mapping, validation) and stateless — horizontally scalable by Kafka partitions. The sink is I/O-bound (Mongo bulk writes) and requires tight idempotency control. Separate services mean separate scaling, separate failure domains, and separate SLOs. It also lets the *transformed* topic serve as a replay buffer for debugging without re-reading the WAL.

---

## Repository Layout

```
pg2mongo-cdc/
├── README.md                          ← recruiter-facing, "Why" over "What"
├── CLAUDE.md                          ← AI agent context (conventions, invariants)
├── docker-compose.yml                 ← full stack: pg, kafka, connect, transformer, sink, mongo, prom, grafana, toxiproxy, k6
├── docker-compose.chaos.yml           ← overlay that injects Toxiproxy between services
├── Makefile                           ← `make demo`, `make chaos`, `make load`, `make verify`
│
├── services/
│   ├── transformer/                   ← Go service, Kafka→Kafka
│   │   ├── cmd/transformer/main.go
│   │   ├── internal/consumer/         ← franz-go consumer, manual commit
│   │   ├── internal/mapper/           ← schema registry client + transform rules
│   │   ├── internal/producer/         ← idempotent producer (enable.idempotence=true)
│   │   └── internal/metrics/          ← Prometheus collectors
│   │
│   └── sink/                          ← Go service, Kafka→Mongo
│       ├── cmd/sink/main.go
│       ├── internal/consumer/
│       ├── internal/writer/           ← Mongo BulkWrite with upserts
│       ├── internal/dedupe/           ← LSN-based skip for replay safety
│       └── internal/metrics/
│
├── connectors/
│   └── debezium-postgres.json         ← Debezium config (snapshot.mode=initial, publication.autocreate.mode=filtered)
│
├── schema/
│   ├── postgres/001_init.sql          ← source schema (users, orders, order_items)
│   ├── schema-registry/               ← Avro schemas for each table
│   └── transforms/                    ← declarative SQL→Mongo mapping rules (YAML)
│
├── observability/
│   ├── prometheus/prometheus.yml
│   ├── grafana/dashboards/
│   │   ├── migration-overview.json    ← replication lag, throughput, DLQ depth
│   │   └── per-table-slo.json
│   └── alerts/alerts.yml
│
├── chaos/
│   ├── scenarios/
│   │   ├── kill-transformer.sh
│   │   ├── kafka-partition.sh         ← Toxiproxy: add 500ms latency + 10% loss
│   │   ├── mongo-primary-stepdown.sh
│   │   └── pg-wal-pressure.sh
│   └── verify-integrity.sh            ← compares row counts + checksums pg↔mongo
│
├── load/
│   └── k6/write-mix.js                ← 70% INSERT, 20% UPDATE, 10% DELETE at target RPS
│
└── docs/
    ├── architecture.md                ← deep dive diagrams
    ├── decisions/                     ← ADRs (one per pillar below)
    └── runbook.md                     ← what to do when each alert fires
```

**Critical files to implement in order:** `docker-compose.yml` → `connectors/debezium-postgres.json` → `services/sink/internal/writer/` → `services/transformer/internal/mapper/` → `chaos/verify-integrity.sh`.

---

## Pillar 1 — Messaging Architecture (No Message Loss)

**Goal.** Zero message loss across any single-component failure: source crash, broker crash, consumer crash, sink crash, or network partition.

**Design.**

| Layer | Setting | Why |
|---|---|---|
| Postgres | `wal_level=logical`, `max_replication_slots=10` | Debezium reads WAL via replication slot — slot persists across Debezium restarts, so no WAL gaps. |
| Debezium | `snapshot.mode=initial`, `heartbeat.interval.ms=1000` | Initial snapshot then streaming. Heartbeat prevents WAL from being recycled during low-traffic periods — this is the #1 cause of "mysterious data loss" in Debezium deployments. |
| Kafka brokers | `min.insync.replicas=2`, RF=3, `unclean.leader.election.enable=false` | Writes only ack after 2 replicas persist. No leader election to an out-of-sync replica ever. |
| Producer (transformer + Connect) | `acks=all`, `enable.idempotence=true`, `max.in.flight.requests.per.connection=5` | Durable writes + idempotent producer prevents duplicates from retries. |
| Consumer (transformer + sink) | `enable.auto.commit=false`, `isolation.level=read_committed` | Manual commits only *after* downstream write succeeds. `read_committed` ensures we don't see aborted transactional batches. |
| DLQ | Separate `dlq.<table>` topic per source table, infinite retention | Poison events get routed here with full headers (original offset, LSN, error reason) for human triage — never dropped. |

**Partition key strategy.** Kafka topics are partitioned by the source table's **primary key**. This guarantees that all events for the same row land on the same partition and are processed in order by a single consumer. Without this, an UPDATE can overtake an INSERT on a different partition and the sink writes stale data. This is the invariant that makes all downstream idempotency logic sound.

**Why Kafka and not RabbitMQ.** Kafka's log-based model gives us three things RabbitMQ would force us to build: (1) replayable history (rewind a consumer group to an LSN), (2) native offset storage in `__consumer_offsets` (our checkpoint, free), (3) partition-ordered delivery keyed on PK. RabbitMQ's work-queue model forces message-by-message acks and has no replay — we'd lose the entire "exactly-once-effect" story.

---

## Pillar 2 — Checkpointing & Offset Management

**Goal.** After a crash, each service resumes from the exact record it last processed. No full table rescan. No gap.

**Three independent checkpoint layers:**

1. **Debezium → Kafka Connect.** Connect stores source offsets (the Postgres LSN of the last emitted event) in the `connect-offsets` topic (RF=3, compacted). On restart, Debezium reads this offset and resumes the replication slot from exactly that LSN. **We do nothing custom here** — this is the whole point of using Debezium.

2. **Transformer & Sink → Kafka consumer group offsets.** Both Go services use `franz-go` with manual commits. The commit happens **only after** the downstream operation succeeds:
   ```
   for msg := range consumer.Poll() {
       transformed, err := mapper.Transform(msg)
       if err != nil { dlq.Send(msg, err); continue }
       if err := producer.Produce(transformed); err != nil { return err } // no commit, will redeliver
       consumer.MarkCommit(msg)  // only reached on success
   }
   consumer.CommitMarked()  // batched commit every 5s or every 1000 msgs
   ```
   Crash between produce-success and commit → the message is redelivered on restart → idempotency layer (Pillar 3) absorbs the duplicate. This is the classic "commit after side-effect" pattern and it's the correct choice for at-least-once with idempotent sinks.

3. **Sink → Mongo write-ahead marker.** The sink writes a `_migration_checkpoint` document in Mongo on every batch flush: `{ _id: "sink:<topic>:<partition>", lastLsn, lastKafkaOffset, updatedAt }`. Purpose: **cross-system reconciliation**. If Kafka's `__consumer_offsets` is ever lost (disaster scenario), we can reset the consumer group from the LSN stored in Mongo and replay — idempotency again absorbs re-processed events.

**What NOT to do.** Do not store offsets in Postgres source (creates a write amplification loop against the system being migrated). Do not commit before the sink write (guarantees loss on crash). Do not commit every message (kills throughput — batch commits at 1000 msgs or 5s, whichever first).

---

## Pillar 3 — Idempotency Guarantees

**Goal.** Under at-least-once delivery, re-processing an event must not produce duplicates, stale overwrites, or inconsistent state in Mongo.

**Three-layer defense.**

**Layer A — Deterministic document ID.** Mongo `_id` is set to the source table's primary key, namespaced by table: `"users:42"`. A replayed INSERT becomes a no-op upsert. This alone handles ~95% of duplicate scenarios.

**Layer B — LSN-gated conditional upsert.** Every event carries its Postgres LSN (a monotonically-increasing 64-bit log position). The sink writes:
```javascript
db.users.updateOne(
  { _id: "users:42", $or: [ { sourceLsn: { $lt: event.lsn } }, { sourceLsn: { $exists: false } } ] },
  { $set: { ...mappedFields, sourceLsn: event.lsn, schemaVersion: 3 } },
  { upsert: true }
)
```
If the document already reflects an equal or newer LSN, the filter fails and the write is a no-op. This prevents **out-of-order reprocessing** from overwriting newer data with older data — a scenario that happens when you rewind a consumer group or replay the DLQ.

**Layer C — Tombstone handling for DELETEs.** Debezium emits a tombstone event (`value=null`) for deletes. The sink translates this to `deleteOne({_id, sourceLsn: {$lt: event.lsn}})` — deletes are also LSN-gated, so a replayed delete can't remove a row that was subsequently re-inserted.

**Why not Mongo transactions?** A multi-document transaction per event would 5x the write latency. With PK-scoped upserts and LSN gating, single-document atomicity is sufficient — Mongo guarantees atomicity on a single document.

**Verification.** `chaos/verify-integrity.sh` computes `SELECT md5(string_agg(row::text, '')) FROM users ORDER BY id` on Postgres and the equivalent hash over Mongo documents, and asserts equality. This runs after every chaos scenario.

---

## Pillar 4 — Resilience & Chaos Testing

**Goal.** Prove — not claim — that the system survives real failure modes. Every scenario has a pass criterion written *before* the scenario runs.

**Five scenarios, all scripted in `chaos/scenarios/`:**

| # | Scenario | Script | Pass criterion |
|---|---|---|---|
| 1 | Kill transformer mid-stream | `kill -9` on transformer container during k6 load | Row count + checksum match between PG and Mongo within 60s of restart. Zero DLQ entries. |
| 2 | Kafka broker partition | `toxiproxy-cli toxic add -t latency -a latency=500 kafka` then `-t limit_data` | Consumer lag rises, then recovers. No data loss. `replication_lag_seconds` metric spikes and recovers on Grafana. |
| 3 | Mongo primary stepdown | `rs.stepDown()` during load | Sink service retries with exponential backoff (driver handles this), resumes within ~10s. Checkpoint doc shows monotonic progress. |
| 4 | Postgres WAL pressure | Pause Debezium container for 5min while load runs | WAL grows on source. On resume, Debezium catches up. Heartbeat prevents slot from being marked inactive. |
| 5 | Poison event | Inject a malformed row via `INSERT INTO users VALUES (..., <invalid-utf8>, ...)` | Transformer sends to DLQ with full context headers. Main pipeline continues. DLQ depth metric increments. |

**Chaos runner harness.** Each scenario is a bash script that: (1) snapshots PG row count + checksum, (2) starts k6 load in background, (3) executes the chaos action, (4) waits for steady state, (5) stops load, (6) waits for consumer lag = 0, (7) runs `verify-integrity.sh`, (8) prints PASS/FAIL. `make chaos` runs all five sequentially and fails CI on any failure.

**Why this matters for a portfolio.** Most candidates *talk* about resilience. This project *measures* it and publishes the results. The README links to a CI run showing all five scenarios passing.

---

## Pillar 5 — Observability & Metrics

**Goal.** Four Golden Signals plus domain-specific KPIs, all exposed on `/metrics` endpoints and dashboarded in Grafana.

**KPIs exposed via Prometheus:**

| Metric | Type | Meaning | SLO target |
|---|---|---|---|
| `migration_replication_lag_seconds` | Gauge | `now() - event.source_ts` at sink commit | p99 < 5s under 5k writes/sec |
| `migration_events_processed_total{stage, table, op}` | Counter | Throughput by stage (transformer/sink), table, and op (insert/update/delete) | — (rate graphed) |
| `migration_dlq_depth{table}` | Gauge | Unconsumed messages in DLQ topic | alert > 0 |
| `migration_consumer_lag{service, topic, partition}` | Gauge | Kafka consumer lag (records) | alert > 10k sustained |
| `migration_write_errors_total{sink, reason}` | Counter | Mongo write failures by category | alert on sustained non-zero rate |
| `migration_idempotent_skip_total{table}` | Counter | LSN-gate rejections (expected during replay, unexpected otherwise) | — (useful for debugging) |
| `migration_schema_version{table}` | Gauge | Current schema version applied | — (aids evolution audits) |
| `migration_checkpoint_staleness_seconds{service}` | Gauge | `now() - last_checkpoint_ts` | alert > 30s |

**Dashboards (Grafana JSON provisioned at container start):**
- **Migration Overview** — replication lag heatmap, throughput by table, DLQ depth, consumer lag per partition
- **Per-Table SLO** — error rate, write latency p50/p99, idempotent-skip rate
- **Chaos View** — annotations for injected failures overlaid on lag + error graphs

**Tracing.** OpenTelemetry spans from Debezium → transformer → sink, correlated by Kafka header `traceparent`. Exported to Jaeger (optional container). A single CDC event can be traced end-to-end across the entire pipeline — this is the observability story that impresses in interviews.

**Alerts (Alertmanager rules in `observability/alerts/`):** DLQ depth > 0, replication lag p99 > 10s for 5min, consumer lag > 10k for 2min, checkpoint staleness > 60s, write error rate > 0.1% for 5min.

---

## Pillar 6 — Schema Transformation & Evolution

**Goal.** Handle the relational↔document impedance mismatch, and survive schema changes on the source *without pipeline downtime*.

**Transformation model — declarative YAML, not code:**
```yaml
# schema/transforms/users.yml
source: public.users
target: users
schemaVersion: 3
idStrategy: pk           # _id = "users:{id}"
fields:
  id:         { type: int, target: _pkId }
  email:      { type: string, target: email, index: true }
  created_at: { type: timestamp, target: createdAt, format: iso8601 }
  profile:    { type: jsonb, target: profile, embed: true }   # jsonb → nested doc
joins:
  - kind: embed
    source: public.addresses
    on: addresses.user_id = users.id
    target: addresses       # array of embedded addresses
```
The transformer loads these YAML rules at startup. Adding a new table = adding a YAML file. This is the Staff-level move: **configuration over code**.

**Schema evolution strategy — Confluent Schema Registry for Avro, plus explicit versioning:**

1. **Additive changes** (new column, new nullable field): Debezium emits the new field, schema registry accepts a BACKWARD-compatible evolution, transformer passes it through. Mongo's flexible schema absorbs it. **Zero downtime, zero config change.**

2. **Breaking changes** (column rename, type change): Handled by the **dual-write window** pattern.
   - Deploy transformer v2 that writes BOTH old and new field names.
   - Let dual-write run until all consumers of the Mongo collection migrate.
   - Deploy transformer v3 that writes only the new name.
   - `schemaVersion` field on every doc makes the audit trail explicit.

3. **Destructive changes** (column drop): Transformer continues writing the field as `null` for one release cycle, then drops it. Never lose data silently.

**Why YAML transform rules and schema registry together?** Schema registry guarantees producer/consumer compatibility at the *wire* level. YAML rules describe the *semantic* transform (SQL→Mongo shape). Both are needed. Skipping schema registry means a malformed event can take down the transformer. Skipping YAML rules means schema changes require code deploys.

---

## CLAUDE.md (root of repo)

**Purpose:** Give future Claude (and any AI coding assistant) the project invariants in one file so it doesn't accidentally violate them.

```markdown
# Zero-Downtime Migration Tool — Agent Context

## Hard invariants (NEVER violate)
- Kafka topics are partitioned by source PK. Never change partition key without a full rebuild.
- Consumer commits happen AFTER downstream write success. Never `auto.commit=true`.
- Mongo writes are LSN-gated. Never remove the `sourceLsn` filter.
- DLQ is write-only from services. Only humans reprocess from DLQ (via `make reprocess-dlq`).
- `min.insync.replicas=2`, RF=3. Never relax in compose overrides meant for demo.

## Conventions
- Go: `go fmt`, `golangci-lint`, errors wrapped with `%w`, no `panic()` in service code.
- Metrics prefix: `migration_`. Follow Prometheus naming: `_seconds`, `_total`, `_bytes`.
- Every new transform rule gets a test in `services/transformer/internal/mapper/mapper_test.go`.
- Every new chaos scenario gets a pass criterion in the script itself (grep `# PASS:`).

## Verification before claiming "done"
1. `make demo` boots the stack.
2. `make load` runs k6 for 60s.
3. `make chaos` passes all 5 scenarios.
4. `chaos/verify-integrity.sh` returns 0.
5. Grafana "Migration Overview" shows lag < 5s at p99.

## What this project is NOT
- Not a general-purpose ETL tool. Postgres→Mongo only.
- Not production-hardened. Secrets are in compose env vars. Don't ship this.
- Not a replacement for Debezium. It's the thin layer around Debezium that does the hard parts.
```

---

## README.md Outline (recruiter-facing — "Why" over "What")

```
# Zero-Downtime PostgreSQL → MongoDB Migration

> A live demonstration of CDC-based migration with zero downtime, exactly-once
> effects under at-least-once delivery, and measured resilience under failure.

## The Problem (30 seconds)
Migrating a live table between databases without stopping writes is the single
hardest problem in data platform engineering. Dual-writes drift. Batch ETL misses
writes during cutover. The only correct answer is CDC — and getting CDC right
requires solving five sub-problems that most tutorials skip.

## See It Run (60 seconds)
    git clone ... && cd pg2mongo-cdc
    make demo      # boots the full stack
    make load      # 5k writes/sec against Postgres
    # open localhost:3000 for Grafana
    make chaos     # kills services mid-stream, verifies integrity

## Why These Decisions
Link to 6 ADRs, one per pillar. Each explains the failure mode it prevents.
  - adr/001-kafka-over-rabbitmq.md
  - adr/002-lsn-gated-upserts.md
  - adr/003-commit-after-sideeffect.md
  - adr/004-yaml-transforms-over-code.md
  - adr/005-toxiproxy-for-chaos.md
  - adr/006-schema-registry-plus-yaml.md

## What This Demonstrates (for hiring managers)
- Distributed systems reasoning: partition ordering, consumer-group semantics, WAL internals
- Production SRE instincts: golden signals, SLOs, runbooks, chaos as CI
- Pragmatic architecture: two-service split (not monolith, not microservice sprawl)
- Data modeling: relational→document with schema evolution under load

## Measured Results
[Link to CI badge showing chaos scenarios 1-5 passing on every commit]
[Screenshot of Grafana dashboard during 5k writes/sec with replication lag p99 = 2.1s]

## Architecture (one diagram, link to docs/architecture.md)
[ASCII diagram from this plan]

## Trade-offs I Made
Short honest section: what I chose NOT to build and why.
  - No multi-region: single-DC demo, would need mirror-maker for real prod
  - No exactly-once Kafka transactions across services: chose idempotent sink instead
    because transactions across heterogeneous sinks (Kafka+Mongo) don't compose
  - No schema registry UI: CLI is enough for a demo
```

**Why this README structure works for recruiters.** First 90 seconds sells the project. ADRs prove you can *justify* decisions, not just make them. "Trade-offs I Made" is the section senior engineers read first — it's how they judge whether you understand what you built.

---

## Implementation Phases — delivered

| Week | Status | Shipping commit | Exit criterion met |
|---|---|---|---|
| 1 | ✅ Delivered | `0fa8dbc` | INSERT → `db.users.findOne()` within 2s; walking skeleton verified end-to-end |
| 2 | ✅ Delivered | `ce78ffe` … `3fe76ff` | 10 TDD cycles for Go sink; chaos 01 × 4 consecutive PASS (0 loss, 0 dup); integration test proves LSN gate across 6 ordering cases against live Mongo |
| 3 | ✅ Delivered | `3b20af4`, `0bee563` | loadgen sidecar sustained 3,755 req/s / 0 failures; Toxiproxy with Kafka PROXIED listener → chaos 02 PASS under 500ms latency + 10% packet loss |
| 4 | ✅ Delivered | `e60a612`, `c4f30f3` | Transformer service live, YAML rules renaming `full_name → fullName` etc. in Mongo docs; GitHub Actions CI workflow validated with actionlint |
| Polish | ✅ Delivered | `e356cdd` | Sink batching: drain rate ~240 w/s → ≥ 7,300 w/s (~30×); README hero rewritten; architecture diagram + findings doc updated |

---

## Verification (end-to-end)

A reviewer (or CI) proves the system works by running, in order:

```bash
make demo                             # boots 12 containers
make load                             # k6 writes for 90s
curl -s localhost:9090/api/v1/query?query=migration_replication_lag_seconds
# → expect p99 < 5s
make chaos                            # runs all 5 scenarios, each self-verifies
bash chaos/verify-integrity.sh        # PG↔Mongo checksum match
# → expect exit 0 and "INTEGRITY OK: 147832 rows, checksums match"
open http://localhost:3000            # Grafana — Migration Overview dashboard
```

Success = all five chaos scenarios pass AND the final integrity check returns 0 AND Grafana shows replication lag p99 below 5s during the load run.

---

## What I Am NOT Planning

To keep scope honest, the following are **explicitly out of scope** for v1 and will be called out in the README's "Trade-offs" section:
- Multi-region / cross-DC replication (needs MirrorMaker 2, doubles infra)
- Kafka→Mongo exactly-once via 2PC (doesn't compose across heterogeneous sinks — idempotent sink is the correct pattern)
- Web UI for DLQ reprocessing (CLI `make reprocess-dlq` is sufficient for the portfolio story)
- Authentication / secrets management (demo uses compose env vars; real prod would use Vault)
- Supporting non-Postgres sources (Debezium has other connectors but this project is scoped to PG→Mongo)

These omissions are intentional signals — a Staff engineer knows what NOT to build.
