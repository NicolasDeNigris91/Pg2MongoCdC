# ADR-002: LSN-gated idempotent upserts, not distributed transactions

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Idempotency Guarantees

## Context

Our consumer-commit protocol (ADR-003) intentionally allows at-least-once delivery: a crash between "downstream write succeeded" and "Kafka offset committed" causes the same event to be re-processed on restart. The sink must absorb these duplicates without producing inconsistent state.

There are two broad ways to do this:

1. **Distributed transactions.** Use Kafka's exactly-once semantics (transactional producer + read-committed consumer) and a transactional sink. Across heterogeneous systems (Kafka + Mongo), this requires a two-phase commit or a transactional outbox pattern.
2. **Idempotent sink writes.** Design every sink write so that applying it twice produces the same state as applying it once.

## Decision

**Idempotent sink writes, gated by the Postgres LSN carried on every event.**

Every Mongo upsert is structured as:

```javascript
db.<collection>.updateOne(
  {
    _id: "<table>:<pk>",
    $or: [
      { sourceLsn: { $lt: event.lsn } },
      { sourceLsn: { $exists: false } }
    ]
  },
  { $set: { ...mappedFields, sourceLsn: event.lsn, schemaVersion: N } },
  { upsert: true }
)
```

Deletes are LSN-gated too:

```javascript
db.<collection>.deleteOne({ _id: "<table>:<pk>", sourceLsn: { $lt: event.lsn } })
```

## Why

1. **2PC across heterogeneous sinks does not compose.** Kafka transactions are transactional only across Kafka topics. A Mongo write is not part of the Kafka transaction. Papering over this gap with a transactional outbox simply relocates the idempotency problem to a new table - you have not eliminated the need for idempotent writes, you have just added a layer.

2. **Single-document atomicity is sufficient.** Our per-event write touches exactly one document (one row → one document, including embedded sub-documents per the transform rules). MongoDB guarantees atomicity on a single document. We do not need multi-document transactions, which would 5× the write latency.

3. **The LSN is a monotonically-increasing 64-bit Postgres log position.** It gives us a total order over all events on the source. Two re-delivered copies of the same event carry the same LSN. Two different events for the same row carry different LSNs (later one is larger). The filter `sourceLsn < event.lsn` rejects both the exact-duplicate case (equal LSN) and the out-of-order replay case (smaller LSN).

4. **The `$exists: false` branch handles first-write.** On the initial INSERT for a row there is no `sourceLsn` yet. Without the `$or`, the filter would reject the first write.

## Implementation note - E11000 on upsert is a feature

The filter `{_id, $or: [$lt, $exists:false]}` only *matches* docs that are older than the incoming event. When the stored doc has `sourceLsn >= event.lsn` (same-LSN replay or stale replay), the filter matches nothing. With `upsert=true`, Mongo then tries to INSERT a new document with `_id = "<table>:<pk>"` - and the unique `_id` index rejects it with error code **11000** (duplicate key).

This is the correct behavior. Semantically: "the doc already reflects state at or newer than this event; the event is redundant." The sink treats E11000 from a BulkWrite as a successful idempotent no-op and moves on.

The integration test in `services/sink/internal/writer/mongo_writer_integration_test.go` exercises all six ordering cases against a live Mongo (insert, same-LSN replay, newer update, stale replay, delete, stale delete after re-insert) and asserts the observable state after each. When it passes, ADR-002 is proven at the database level, not just at the filter-construction level.

## What this prevents

- **Exact duplicates from consumer-group rebalance.** On rebalance, the same event may be redelivered. Filter rejects it; Mongo returns `matchedCount=0, modifiedCount=0`. No-op.
- **Stale overwrite from DLQ replay.** If we replay an old DLQ event after newer events have been applied, the old event's LSN is smaller than the stored one → filter rejects it.
- **DELETE racing a later INSERT.** If a row is deleted and then the same PK is re-inserted with a higher LSN, a replayed DELETE carries the old (smaller) LSN and is rejected.

## What this does NOT prevent

- **Logical bugs in the transform rules.** If `profile.embed=true` is flipped to `false` mid-migration, new documents will have a different shape than old ones. LSN-gating is orthogonal to transform-rule changes. Solution: explicit `schemaVersion` field + dual-write window (ADR-006).
- **Lost writes from Kafka itself.** If the producer side loses an event before Kafka persists it, LSN-gating cannot recover it. Mitigated by `acks=all` + `min.insync.replicas=2` + idempotent producer (ADR-001).

## Trade-offs we accept

- **Every write costs one extra field.** `sourceLsn` and `schemaVersion` are metadata overhead on every doc. Negligible compared to domain fields.
- **No cross-document atomicity.** If we needed "update user AND insert audit log atomically" we would be stuck. We do not need it - audit logs are handled downstream of Mongo by a separate pipeline.
- **An observable `migration_idempotent_skip_total` counter.** We monitor this as a metric rather than an error. A sustained non-zero rate during normal operation indicates a misconfigured consumer group or an unexpected replay.

## Verification

- Unit tests for `BuildUpsertModel` / `BuildDeleteModel` in `services/sink/internal/writer/writer_test.go` cover: first-write, re-delivery, stale replay, tombstone on re-inserted PK.
- Chaos scenario #1 (kill transformer mid-stream) explicitly asserts zero duplicates in Mongo after restart, via `chaos/verify-integrity.sh`.

## Alternatives considered

- **Kafka transactional outbox.** Rejected - moves the idempotency problem into a new table rather than eliminating it.
- **Content hashing.** `md5(row)` as a dedupe key. Rejected - does not handle legitimate UPDATEs (row content changes, same PK).
- **Per-row version number sourced from Postgres.** Requires adding a column to every source table. Too intrusive on the system being migrated.
