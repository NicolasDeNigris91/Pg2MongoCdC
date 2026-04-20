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

## Scenario 02 (Kafka network partition via Toxiproxy) — deferred

Not run in this pass. Rationale: the current stack routes Debezium and the Mongo sink to `kafka:29092` directly, not through Toxiproxy's `:19092` proxy. Injecting a toxic has no effect until `BOOTSTRAP_SERVERS` is rewired through Toxiproxy.

Fix is a one-line overlay (`docker-compose.chaos.yml`: set `BOOTSTRAP_SERVERS=toxiproxy:19092` for connect and the Week 2 services). Deferred to Week 3 cleanup so Week 2 can focus on the sink rewrite.

## What's next

1. **Week 2.** Replace the MongoDB Kafka Connector with `services/sink/` (Go + franz-go + mongo-go-driver). Implement commit-after-side-effect + LSN-gated upserts. Re-run chaos 01 and assert 0 loss.
2. **Week 3.** Rewire `BOOTSTRAP_SERVERS` through Toxiproxy so chaos 02 has teeth. Add the k6 sidecar so `load/k6/write-mix.js` has a target.
3. **Week 4.** CI workflow that runs the chaos suite on every commit.

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
