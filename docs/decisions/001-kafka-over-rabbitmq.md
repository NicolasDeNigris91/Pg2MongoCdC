# ADR-001: Kafka (with Debezium) over RabbitMQ for the CDC backbone

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Messaging Architecture (no message loss)

## Context

We need a message backbone between Postgres (source) and MongoDB (sink) that survives single-component failure without losing events, and that supports the idempotency + checkpointing stories in ADRs 002 and 003. The two realistic candidates are Apache Kafka (log-based) and RabbitMQ (queue-based).

## Decision

**Kafka (KRaft mode) with Debezium as the Postgres CDC source.**

## Why

1. **Partition-ordered delivery keyed on PK.** All events for the same source row land on the same partition and are processed in order by a single consumer. An UPDATE cannot overtake an INSERT for the same row. Without this invariant, the LSN-gating logic in ADR-002 would still need to reject out-of-order events but at the cost of constant `migration_idempotent_skip_total` noise and wasted Mongo round-trips. RabbitMQ's work-queue model offers no equivalent co-partitioning without per-row queues - which does not scale.

2. **Native offset storage in `__consumer_offsets`.** Kafka's consumer-group offsets are our checkpoint for the transformer and sink services. This is the free version of what we would otherwise have to build ourselves on top of RabbitMQ (a custom offset store, with its own crash-safety requirements). See ADR-003.

3. **Replayable history.** Log-based storage lets us rewind a consumer group to a specific Kafka offset - or, with a bit of tooling, to a Postgres LSN. This matters for DLQ reprocessing and disaster recovery. RabbitMQ's "consumed means gone" model forecloses this entirely.

4. **Debezium is the de-facto Postgres CDC tool.** It reads the WAL via a replication slot, handles snapshot+streaming transitions, and persists its own source offsets in `connect-offsets`. Writing an equivalent WAL reader ourselves is a multi-month project that adds no portfolio value - the interesting engineering is downstream of Debezium, not in reimplementing it.

## Trade-offs we accept

- **Operational weight.** Kafka is heavier to run than RabbitMQ: 3 brokers, ZooKeeper (or KRaft controllers), and Connect workers. We mitigate this in the dev environment by running single-broker KRaft mode (see [invariant #5 in docs/invariants.md](../invariants.md)). Prod topology is documented but not shipped in the demo compose.
- **More moving parts at the wire level.** Avro + Schema Registry adds a component. The payoff is wire-level schema compatibility enforcement (see ADR-006) which eliminates a whole class of malformed-event bugs.

## Production topology (what the dev compose relaxes)

| Setting | Dev compose (demo) | Production |
|---|---|---|
| Kafka brokers | 1 | 3 |
| Replication factor | 1 | 3 |
| `min.insync.replicas` | 1 | 2 |
| `unclean.leader.election.enable` | false | false (unchanged) |
| Topic partitions | 6 | 6+ per table |

## Alternatives considered

- **RabbitMQ.** Simpler broker, smaller footprint. Rejected because we would have to build: (a) a custom WAL reader, (b) a custom offset store, (c) partition-ordering guarantees via per-row queues. That is months of plumbing that solves already-solved problems and distracts from the actual portfolio narrative.
- **AWS Kinesis / GCP Pub-Sub.** Cloud-managed alternatives. Rejected because the project must run on a laptop with `docker compose up`.
- **Postgres LISTEN/NOTIFY.** Not durable across reconnects. No fit for a correctness-focused pipeline.

## Consequences

- We commit to running Kafka in all environments.
- We commit to Avro + Schema Registry as the wire format (see ADR-006).
- Downstream correctness stories (ADRs 002, 003) assume PK-partitioned topics. Changing partition keys later requires a full topic rebuild.
