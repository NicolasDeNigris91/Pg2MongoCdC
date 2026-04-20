# ADR-004: Declarative YAML transform rules, not hand-written Go per table

- **Status:** Accepted
- **Date:** 2026-04-20
- **Context pillar:** Schema Transformation

## Context

The transformer service has to map a Postgres row into a Mongo document. Every table needs its own mapping: which fields transfer as-is, which rename, which embed child rows, which pivot JSONB into nested documents. There are two ways to express this:

1. **Go code per table.** A `MapUsers(row) doc`, `MapOrders(row) doc`, etc.
2. **Declarative rules in YAML**, interpreted by a single generic mapper.

## Decision

**Declarative YAML rules under `schema/transforms/<table>.yml`, loaded at transformer startup.**

Example:

```yaml
source: public.users
target: users
schemaVersion: 3
idStrategy: pk
fields:
  id:         { type: int, target: _pkId }
  email:      { type: string, target: email, index: true }
  created_at: { type: timestamp, target: createdAt, format: iso8601 }
  profile:    { type: jsonb, target: profile, embed: true }
joins:
  - kind: embed
    source: public.addresses
    on: addresses.user_id = users.id
    target: addresses
```

## Why

1. **Adding a new table = adding a file.** No code changes, no code review for business mappings, no deploy. The interesting engineering (idempotency, observability, chaos) is shared code that every table benefits from automatically.

2. **Separation of concerns.** Code owners vs. schema owners. A DBA can author a transform rule without knowing Go. A Go engineer can fix a bug in the generic mapper without reviewing 30 per-table mapping functions.

3. **Testability.** The mapper is a pure function: `(rule, row) → doc`. Table-driven tests cover every field type exhaustively. Per-table Go code multiplies the test surface linearly.

4. **Auditability.** A YAML file is a diff you can read in a PR. A GitOps workflow for schema changes falls out for free.

## Trade-offs we accept

- **Expressive power ceiling.** Declarative rules cannot express arbitrary transforms (e.g., "if order.total_cents > 100k, enrich with a risk score from an external API"). When we hit that ceiling we add a **post-transform hook** (Go function called after YAML mapping) rather than rewriting everything in Go. No such hook is needed yet.
- **Rule-DSL versioning.** The YAML schema itself evolves over time. We pin `ruleSchemaVersion` at the top of every rule file and fail fast on unknown versions. See [ADR-006](./006-schema-registry-plus-yaml.md) for the full evolution story.

## Alternatives considered

- **Hand-written Go mappers.** Rejected — boilerplate, linearly growing test surface, new code for every new table.
- **General-purpose transform language (Jsonnet, CUE, Jq).** Rejected — adds a runtime dependency and a learning curve for contributors, with no benefit over a narrow YAML DSL scoped to our exact problem.
- **Schema registry transforms (SMTs) on Debezium.** Rejected — SMTs are wire-level and brittle for anything beyond field renames.

## Consequences

- Every new table requires a YAML file in `schema/transforms/`.
- Every new YAML file requires a test case in `services/transformer/internal/mapper/mapper_test.go`.
- Changes to the YAML DSL require bumping `ruleSchemaVersion` and a migration plan for existing rule files.
