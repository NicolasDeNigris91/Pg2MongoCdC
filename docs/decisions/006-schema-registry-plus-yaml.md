# ADR-006: Schema Registry + YAML transform rules, together

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Schema Evolution

## Context

The source schema will evolve during the pipeline's lifetime: new columns, renamed columns, changed types, dropped columns. If the pipeline cannot handle these gracefully, every schema change becomes a downtime event — defeating the project's whole premise of "zero-downtime migration".

Two kinds of schema guarantees matter:

1. **Wire compatibility.** Can producer version N+1 and consumer version N still talk to each other?
2. **Semantic compatibility.** Does the new column land in the right place in the Mongo document, with the right type, indexed correctly?

## Decision

**Use both, and be explicit about which solves what.**

- **Confluent Schema Registry with `BACKWARD` compatibility mode** — enforces wire compatibility between Debezium (producer) and transformer (consumer).
- **YAML transform rules (from [ADR-004](./004-yaml-transforms-over-code.md)) versioned via `schemaVersion:` field** — describes the semantic mapping and is the unit of change for dual-write windows.
- **`schemaVersion` written into every Mongo document** — gives us an audit trail and enables dual-read by downstream consumers of Mongo.

### Week-1 exception: JsonConverter

The Week 1 walking skeleton uses `org.apache.kafka.connect.json.JsonConverter` (embedded schemas) rather than Avro + Schema Registry. Reason: the Debezium Connect image does not ship Confluent's Avro libraries, and installing them via `confluent-hub` on the image builder added boot friction that did not pay for itself at the "prove CDC flows through" stage.

The Avro switch happens at Week 2 when we replace the off-the-shelf Mongo sink connector with our own Go sink. At that point we control both ends of the wire and can pin exact client versions, eliminating the packaging ambiguity.

The Schema Registry container **still runs** in the dev stack today, unused, so the compose graph does not change when Avro flips on. The wire compatibility story in this ADR applies the moment we switch — no architectural redesign needed.

## Evolution playbook

### Additive change (new nullable column)

1. Add column to Postgres.
2. Debezium publishes new Avro schema (registry accepts BACKWARD evolution).
3. Transformer sees the new field, falls through to generic passthrough if no YAML rule entry exists.
4. Mongo absorbs new field (flexible schema).
5. Optionally: add field entry to YAML rule in a follow-up PR to make it explicit.

**Downtime: zero. Config change: optional.**

### Breaking change (column rename or type change)

This is the hard case. Solved with a **dual-write window**:

| Step | Action | Why |
|---|---|---|
| 1 | Deploy transformer v2 that writes BOTH `old_field` and `new_field` | Old and new readers can both find their data |
| 2 | Let dual-write run until all downstream Mongo consumers migrate to `new_field` | Caller migration, not pipeline migration |
| 3 | Deploy transformer v3 that writes only `new_field` | Complete the cutover |
| 4 | Bump `schemaVersion` in the YAML rule and backfill | Future replays from Kafka use v3 mapping |

The `schemaVersion` field on every Mongo document tells us at a glance which mapping was active when the document was written. Crucial for debugging "why is this doc shaped differently from that one".

### Destructive change (column drop)

1. Transformer writes the field as `null` for one release cycle.
2. Alert if any downstream Mongo consumer queries this field (via Mongo profiler).
3. Drop from the YAML rule, bump `schemaVersion`.

Never silently stop writing a field — that surfaces as a quiet null-filling bug in downstream code months later.

## Why both mechanisms

- **Registry without YAML:** wire-level compatibility is enforced but the transformer is hand-coded Go that must be redeployed for any mapping change. Re-introduces the problem ADR-004 solved.
- **YAML without registry:** a malformed Avro event at the wire takes down the transformer because deserialization fails before any YAML rule runs. Losing compatibility enforcement at the wire means every consumer becomes a validator.
- **Both:** registry catches wire-level drift at the boundary; YAML + `schemaVersion` handles semantic evolution with an explicit audit trail.

## Trade-offs we accept

- **`BACKWARD` compatibility mode forbids removing required fields.** Deliberate. Dropping a field requires the destructive-change playbook above, not a quiet schema update.
- **Confluent licensing.** The open-source community edition of Schema Registry is sufficient for a portfolio. A real production deployment may prefer [Apicurio](https://www.apicur.io/registry/) (Apache 2.0). The wire protocol is the same.

## Alternatives considered

- **JSON Schema over Avro.** Larger payloads, slower serde, weaker tooling. Rejected.
- **Protobuf.** Strong compatibility story but Debezium + Postgres CDC has first-class Avro support and weaker Protobuf support. Not worth fighting the tools.
- **Schema-on-read (no wire schema at all).** Rejected — every consumer would need to re-implement validation.

## Consequences

- Every topic has a registered Avro schema at `schema-registry:8081`.
- Every YAML rule file has a `schemaVersion: N` field; the transformer fails fast if a document on disk has a higher version than the binary supports.
- Every Mongo document has a `schemaVersion: N` field injected by the sink.
