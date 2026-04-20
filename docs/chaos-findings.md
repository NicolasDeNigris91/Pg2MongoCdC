# Chaos Findings — 2026-04-20

Real results from running the chaos suite against the Week 1 walking skeleton (Debezium + Kafka + off-the-shelf MongoDB Kafka Connector as the sink).

> **Why this document exists.** A portfolio that claims "measured resilience" has to publish measurements. This document tracks what actually happened when we ran `chaos/scenarios/*.sh` against the pipeline, including a row-loss regression that validates the motivation for Week 2.

## Setup

- Stack: `docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d --build --wait`
- Sink: off-the-shelf [MongoDB Kafka Connector](https://www.mongodb.com/docs/kafka-connector/current/) 1.13.0 with Debezium's `PostgresHandler`
- No Week 2 services yet — no Go transformer, no Go sink, no LSN-gating
- Baseline: 27 rows pre-chaos (`verify-integrity.sh` green)

## Results

| # | Scenario | Outcome | Rows PG | Rows Mongo | Notes |
|---|---|---|---|---|---|
| 3 | Mongo primary stepdown | **PASS** | 30 | 30 | Driver retry path absorbs the ~10s re-election window cleanly |
| 4 | Postgres WAL pressure (connect paused 120s) | **PASS** | 167 | 167 | Replication slot retained WAL; Debezium caught up on unpause |
| 5 | Poison event (1MB JSONB blob) | **PASS** | (event recorded) | post-poison event present | DLQ topic `dlq.sink` was auto-created; pipeline did not block |
| 1 | Kill Connect mid-stream (SIGKILL + restart) | **FAIL** | 200 | 199 | **1 row lost out of ~30 written during the chaos window** |

## The finding — scenario 01 lost data

After `docker kill -s SIGKILL zdt-connect` during an active insert stream and a restart 3 seconds later, we observed **200 rows in Postgres and 199 documents in MongoDB**. One insert was acknowledged by Postgres but never made it to MongoDB.

### Likely mechanism

The off-the-shelf MongoDB Kafka Connector inherits Kafka Connect's default sink-offset semantics. Kafka Connect commits offsets periodically **independent of whether the downstream `BulkWrite` has fully succeeded**. If Connect dies during a `BulkWrite` where *some* records in the batch have been flushed to Mongo and *some* haven't, but the offset was committed for the full batch, the un-flushed records are dropped on restart.

This is the classic **commit-before-side-effect** failure mode [ADR-003](./decisions/003-commit-after-sideeffect.md) exists to prevent. The off-the-shelf connector can be configured to mitigate this (smaller batch sizes, more frequent offset commits), but the correctness property is not structural — it's a race window we are one config change away from re-opening.

### Why this validates the Week 2 motivation

The whole point of building our own Go sink in Week 2 is to guarantee commit-after-side-effect at the code level, not the config level:

```go
for _, msg := range consumer.Poll(...) {
    if err := mongo.BulkWrite(ctx, models); err != nil {
        return err              // no commit; will be redelivered
    }
    consumer.MarkCommit(msg)    // only reached on success
}
consumer.CommitMarked()
```

Combined with LSN-gated upserts from [ADR-002](./decisions/002-lsn-gated-upserts.md), the Week 2 sink makes this failure mode structurally impossible — any redelivery hits a no-op upsert. The chaos suite will re-run against the Go sink in Week 2 with pass criterion "**200 = 200** under 10 consecutive kill cycles".

### What this tells a recruiter

Off-the-shelf tools have config-tunable correctness; hand-rolled code has structural correctness. This project demonstrates both, and the chaos suite enforces the difference with data, not prose.

## Scenario 02 — Kafka network partition via Toxiproxy (Week 3)

**PASS.** Baseline PG=2 / Mongo=2, 30s of inserts under injected 500ms latency + 10% packet loss, pipeline drained after toxics removed, final PG=32 / Mongo=32, `INTEGRITY OK`.

### How we gave Toxiproxy real teeth

Kafka now advertises a dedicated `PROXIED` listener whose advertised host is `toxiproxy:19092`. Toxiproxy forwards that to Kafka's internal listener on `kafka:39092`. Clients bootstrapping via `toxiproxy:19092` therefore receive metadata that also points back through the proxy — every subsequent produce/fetch flows through Toxiproxy, so injected toxics affect actual traffic rather than just the initial TCP handshake.

The routing is opt-in via `docker-compose.toxiproxy.yml` overlay (sets `BOOTSTRAP_SERVERS` for Connect and the Go sink to `toxiproxy:19092`). Without the overlay, clients use the normal `INTERNAL://kafka:29092` listener and Toxiproxy has no effect — important so the default `make demo` doesn't pay proxy overhead.

```bash
docker compose \
  -f docker-compose.yml \
  -f docker-compose.chaos.yml \
  -f docker-compose.toxiproxy.yml \
  up -d --wait
bash scripts/register-connectors.sh
bash chaos/scenarios/02-kafka-partition.sh
# -> INTEGRITY OK
```

## Week 2 results — Go sink in place

Second run of the chaos suite after replacing the off-the-shelf MongoDB Kafka Connector with our Go sink (`services/sink/`). Same harness, same SIGKILL-mid-stream scenario, on a clean-slate stack (`docker compose down -v` + full rebuild).

Four consecutive iterations of chaos 01, cumulative load:

| Iter | PG rows | Mongo docs | Status |
|---|---|---|---|
| 1 | 32  | 32  | **PASS** |
| 2 | 62  | 62  | **PASS** |
| 3 | 92  | 92  | **PASS** |
| 4 | 122 | 122 | **PASS** |

`verify-integrity.sh` returned exit 0 after every iteration and the final cumulative check. **Zero loss, zero duplicates, across four consecutive SIGKILL + restart cycles.**

### What changed

The Go sink implements commit-after-side-effect structurally (not via config):

1. `kgo.DisableAutoCommit()` and `kgo.MetadataMaxAge(10s)` so offset commits are never implicit and pattern-subscription picks up fresh topics fast.
2. `Loop.RunOnce` marks a record's offset with `MarkCommitRecords` only after `MongoWriter.Apply` returns nil. On write failure it `break`s the batch, calls `CommitMarked` (which commits only the records that succeeded), and returns the error so the caller backs off.
3. `MongoWriter.Apply` builds a LSN-gated upsert (`{_id, $or: [$lt, $exists:false]}` filter, `$set` with `sourceLsn` and `schemaVersion`) and swallows E11000 duplicate-key errors from the upsert as idempotent no-ops — exactly the stale-replay case ADR-002 calls out.

The integration test in `services/sink/internal/writer/mongo_writer_integration_test.go` already proved the LSN-gate at the database level across six ordering cases. The chaos run above proves the whole loop under a realistic crash.

### A bug we found and fixed along the way

First attempt used `kgo.MarkCommitOffsets(map[...]EpochOffset{..., Epoch: -1})` as the commit path. That compiled and tests with the fake consumer passed, but the real Kafka broker reported `CURRENT-OFFSET = -` indefinitely — meaning offsets were never actually being committed, and the sink was silently re-processing from the beginning of each topic on every restart. LSN-gating masked the correctness impact (idempotency is robust), but the work being done was wildly wasteful. Switching to the documented `MarkCommitRecords(*kgo.Record)` path (with the raw record carried through the Record abstraction as an opaque `Raw any` field) fixed it, and the consumer-group display stabilized.

## Week 3 — load numbers from the k6 sidecar

Ran `load/k6/write-mix.js` (70% INSERT / 20% UPDATE / 10% DELETE) against the new `services/loadgen/` sidecar which translates k6 HTTP into Postgres SQL via pgx. k6 at 20 VUs for 30s:

```
checks.........................: 100.00% ✓ 112676   ✗ 0
http_req_failed................: 0.00%    ✓ 0       ✗ 112676
http_req_duration..............: avg=5.22ms  p95=7.79ms  p99≈15ms  max=242.73ms
http_reqs......................: 112676    3755.17/s
zdt_insert_latency_ms..........: avg=5.27ms  p95=7.7ms
zdt_update_latency_ms..........: avg=5.24ms  p95=7.88ms
zdt_delete_latency_ms..........: avg=4.88ms  p95=8.53ms
```

**Sidecar sustained 3,755 write-ops/sec with zero failed requests.** All k6 thresholds (p99 < 100ms insert, < 150ms update) passed.

### Finding → Fix → Re-measure (closed)

**Before batching** — under the 3.7k RPS burst, the Go sink drained at **~240 writes/sec**, roughly 15× slower than ingestion. Mongo stayed at ~57k docs vs PG's ~74k rows after 2.5 minutes. Data eventually reconciled (every event is LSN-gated and idempotent), but replication lag during the burst exceeded the 5s SLO.

Root cause: `MongoWriter.Apply` called `BulkWrite` with exactly one `WriteModel` per event — the "Bulk" name was vestigial.

**Fix.** Refactored the `Writer` interface to `ApplyBatch(ctx, []CDCEvent) error` and updated `Loop.RunOnce` to:

1. Decode every record from the poll batch into `[]CDCEvent` in one pass (tombstones skipped, not dispatched).
2. Dispatch the full slice through one `ApplyBatch` call.
3. `MongoWriter.ApplyBatch` groups events by collection and issues exactly one `BulkWrite` per collection with `ordered=false`, so a single E11000 on a stale-replay row does not abort the rest of the batch.
4. On batch success, `MarkCommit` is called on every record and `CommitMarked` fires once — commit-after-side-effect at batch granularity.
5. On batch failure, nothing is committed; the whole poll batch is redelivered and LSN-gating absorbs whatever records flushed to Mongo before the failure.

The ADR-002 (LSN gate) and ADR-003 (commit-after-side-effect) invariants are preserved — the semantic is the same, just at batch granularity.

**After batching — measured on the same stack, same k6 run (20 VUs × 30s, 111k iterations):**

| Metric | Before | After | Δ |
|---|---|---|---|
| Sink drain rate (burst) | ~240 w/s | **≥7,300 w/s** | ~30× |
| PG↔Mongo reconciliation after 10s post-load | 8k / 74k | **73k / 73k** | converged |
| Chaos 01 × 4 iterations | PASS | **PASS** (invariants unchanged) | — |
| Integration test (LSN gate × 6 cases) | PASS | **PASS** | — |

Final state after a k6 burst ending: `PG=73,064 / Mongo=73,064`, `verify-integrity.sh` exit 0. The 5s replication-lag SLO is now comfortably met on commodity hardware.

### Commands to reproduce

```bash
# Boot the full stack
docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d --build --wait
bash scripts/register-connectors.sh

# Smoke-test loadgen
curl -X POST -H 'Content-Type: application/json' \
  -d '{"email":"x@y.z","full_name":"X"}' http://localhost:8086/users

# 30-second load burst
MSYS_NO_PATHCONV=1 docker compose \
  -f docker-compose.yml -f docker-compose.chaos.yml \
  run --rm --no-deps k6 run --vus 20 --duration 30s /scripts/write-mix.js
```

(`MSYS_NO_PATHCONV=1` is only needed on Git Bash / MSYS on Windows, which otherwise rewrites `/scripts/write-mix.js` into `C:/Program Files/Git/scripts/write-mix.js`.)

## What's still next

1. **Sink batching** — the perf gap above. Should be a single TDD cycle refactor of the Loop and one new method on Writer.
2. **Toxiproxy rewire** — Kafka currently advertises its real listener, so injecting toxics has no effect on clients that cache the metadata. Add a PROXIED listener whose advertised host routes through Toxiproxy. Scenario 02 then has teeth.
3. **CI** — GitHub Actions workflow that runs the whole chaos suite on every PR.
4. **Transformer service** — reads `cdc.*`, applies YAML rules from `schema/transforms/`, publishes to `transformed.*`. Sink consumes `transformed.*` afterward. Destrava a ADR-004 demo em código rodando.

## Reproduction

```bash
# From a clean state:
docker compose -f docker-compose.yml -f docker-compose.chaos.yml down -v
docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d --build --wait
bash scripts/register-connectors.sh
bash scripts/seed.sh     # baseline 27 rows
bash chaos/scenarios/01-kill-transformer.sh
# Expect: INTEGRITY FAILED with ~1-2 rows missing from Mongo
```

The loss rate is stochastic — depends on how many Mongo `BulkWrite` calls are in flight at the moment of SIGKILL. Under our default k6-less load pattern (1 insert/sec via psql), we observed 1 loss per chaos cycle in 2 of 3 trial runs. The point is not the exact rate; the point is that it's **not zero**, and Week 2 makes it structurally zero.
