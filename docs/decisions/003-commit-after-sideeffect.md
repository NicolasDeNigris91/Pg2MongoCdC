# ADR-003: Commit Kafka offsets AFTER the downstream write succeeds

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Checkpointing & Offset Management

## Context

Both Go services (`transformer-svc`, `sink-svc`) consume from Kafka and write to a downstream system (another Kafka topic, or Mongo). When and how they commit the consumer-group offset determines whether the pipeline is at-most-once, at-least-once, or lossy.

## Decision

**Disable auto-commit. Manually commit the offset *after* the downstream write succeeds. Batch commits every 1000 messages or 5 seconds, whichever comes first.**

```go
// transformer-svc / sink-svc consume loop (pseudo-Go)
for _, msg := range consumer.Poll(...) {
    out, err := process(msg)
    if err != nil {
        dlq.Send(msg, err)
        consumer.MarkCommit(msg)   // DLQ IS a successful downstream write
        continue
    }
    if err := downstream.Write(out); err != nil {
        // do NOT mark - will be redelivered on restart
        return err
    }
    consumer.MarkCommit(msg)
}
consumer.CommitMarked()  // batched commit
```

Kafka consumer settings:
- `enable.auto.commit = false`
- `isolation.level = read_committed`
- `session.timeout.ms = 45000`
- `max.poll.interval.ms = 300000`

## Why

1. **Commit-before-write is data loss.** If we commit first and then crash, the consumer resumes from the *next* offset on restart - the in-flight message is silently dropped.

2. **Commit-after-write is at-least-once.** If we crash between the write and the commit, the message is redelivered on restart. The write is re-applied. The idempotency layer from [ADR-002](./002-lsn-gated-upserts.md) makes this a no-op. We trade "exactly once" for "exactly one effect" - the latter is what actually matters.

3. **Batched commits keep throughput up.** Committing every message caps throughput at the Kafka commit round-trip time (~5ms) per message. Batching at 1000 or 5s lifts it by two orders of magnitude, at a worst-case redelivery cost of up to 1000 duplicates on crash - all absorbed by ADR-002.

4. **DLQ-send counts as a successful downstream write.** Once a poison event is in `dlq.source` or `dlq.sink`, its Kafka-level durability guarantees are the same as any other message. Marking the original offset committed prevents a poison-loop where the same bad event blocks the pipeline indefinitely.

## Three independent checkpoint layers

| Layer | Owner | Stored in | Purpose |
|---|---|---|---|
| Debezium source offset | Debezium | `_connect_offsets` (Kafka topic, compacted, RF=3) | Resume WAL read from the exact LSN |
| Transformer / sink consumer offsets | Kafka broker | `__consumer_offsets` (built-in) | Resume at-least-once processing |
| Sink checkpoint doc | sink-svc | `migration._migration_checkpoints` (Mongo) | Cross-system recovery if `__consumer_offsets` is lost |

The third layer is belt-and-braces. In the happy path it is only written for observability (`migration_checkpoint_staleness_seconds`). It earns its keep in the disaster-recovery scenario where Kafka's internal offsets topic is lost - we can reset the consumer group from the LSN recorded in Mongo and replay, with ADR-002 absorbing the duplicates.

## What this prevents

- **Silent data loss on consumer crash.** In the commit-before-write model, any crash between commit and write loses data. In this model, no configuration of crashes can cause data loss.
- **Poison-loop starvation.** Without "DLQ-send counts as success", a bad message would block the pipeline forever, rising consumer lag without end.

## What this does NOT prevent

- **Duplicates in the destination.** By design. [ADR-002](./002-lsn-gated-upserts.md) absorbs them.
- **Reordering on rebalance across partitions.** Handled by PK-partitioning (see [ADR-001](./001-kafka-over-rabbitmq.md)) - same PK = same partition = ordered delivery.

## Trade-offs we accept

- **Observable duplicate rate.** `migration_idempotent_skip_total` will tick non-zero during consumer-group rebalances and after crashes. This is expected behaviour, not an alert.
- **Up to 5 seconds / 1000 messages of re-work on crash.** Acceptable - duplicates are free under ADR-002.

## Alternatives considered

- **`enable.auto.commit=true`.** Rejected - non-deterministic commit timing relative to downstream writes.
- **Commit every message synchronously.** Rejected - throughput collapse.
- **Kafka transactions + exactly-once.** Rejected across heterogeneous sinks (see ADR-002 rationale).
