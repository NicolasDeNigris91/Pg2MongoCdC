# Zero-Downtime PostgreSQL → MongoDB Migration

> Migration from Postgres to MongoDB without stopping writes. The off-the-shelf MongoDB Kafka Connector lost **1 row in ~30** under a single SIGKILL; the Go rewrite in this repo survives **4 consecutive kill-cycles with 0 loss and 0 duplicates**. Every number below is from a real run, reproducible with `docker compose up`.

[![CI](https://github.com/NicolasDeNigris91/Pg2MongoCdC/actions/workflows/ci.yml/badge.svg?branch=main)](https://github.com/NicolasDeNigris91/Pg2MongoCdC/actions/workflows/ci.yml)
[![Go](https://img.shields.io/badge/Go-1.26-00ADD8?logo=go)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](./LICENSE)
[![chaos](https://img.shields.io/badge/chaos--suite-4%2F4_pass-brightgreen)](./docs/chaos-findings.md)
[![throughput](https://img.shields.io/badge/sink_drain_rate-%E2%89%A57.3k%20w%2Fs-brightgreen)](./docs/chaos-findings.md)
[![k6](https://img.shields.io/badge/k6_sustained-3.7k%20RPS%20%7C%200%20failures-brightgreen)](./docs/chaos-findings.md)
[![tests](https://img.shields.io/badge/tests-unit%20%2B%20integration%20%2B%20chaos-blue)](./services/sink/)

---

## The 90-second pitch

**Problem.** Moving a live Postgres table to MongoDB without stopping writes is the hardest problem in data platform engineering. Dual-writes drift. Batch ETL misses writes during cutover. The only correct answer is CDC — and getting CDC *correct* under failure requires solving five sub-problems that most tutorials skip:

1. No message loss across broker, consumer, or sink crashes.
2. Checkpointing so a crashed service resumes exactly where it stopped.
3. Idempotent sinks so at-least-once delivery doesn't corrupt the destination.
4. Schema evolution without pipeline downtime.
5. **Measured** resilience — prove it under failure, don't just claim it.

**What's in here.** A reproducible stack that solves all five, with every claim backed by a chaos-suite number.

```
PG ── WAL ──▶ Debezium ──▶ Kafka ──▶ transformer (Go)  ──▶ Kafka ──▶ sink (Go) ──▶ MongoDB
                                      │                              │
                                      └── YAML rules (ADR-004)       └── LSN-gated upsert (ADR-002)
                                                                         commit-after-side-effect (ADR-003)
                                                                         batched BulkWrite
```

## Run it (60 seconds)

```bash
cp .env.example .env
docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d --build --wait
bash scripts/register-connectors.sh

# Write path proof — renamed fields land in Mongo as the YAML rule said
bash scripts/seed.sh
# -> 'users:1' doc has fullName, createdAt, isActive, sourceLsn, schemaVersion

# Crash-survival proof
bash chaos/scenarios/01-kill-transformer.sh   # SIGKILL the sink mid-stream
# -> INTEGRITY OK (PG count = Mongo count, content hashes match)

# Load proof
MSYS_NO_PATHCONV=1 docker compose \
  -f docker-compose.yml -f docker-compose.chaos.yml \
  run --rm --no-deps k6 run --vus 20 --duration 30s /scripts/write-mix.js
# -> 3.7k RPS sustained, 0 failures
```

Grafana at http://localhost:3000 · Prometheus at http://localhost:9090 · Toxiproxy at http://localhost:8474

## Measured results

Full logs and methodology: [docs/chaos-findings.md](./docs/chaos-findings.md).

| Scenario | Week 1 baseline (off-the-shelf sink) | Week 2-4 (Go stack) |
|---|---|---|
| 03 — Mongo primary stepdown | **PASS** | **PASS** |
| 04 — Postgres WAL pressure (2-min Connect pause) | **PASS** | **PASS** |
| 05 — Poison event (1MB JSONB blob) | **PASS** | **PASS** |
| 02 — Kafka network partition (Toxiproxy: 500ms latency + 10% loss × 30s) | *toothless* | **PASS** |
| 01 — Kill sink mid-stream (× 4 consecutive) | **FAIL** (1 row lost) | **PASS** (0 loss, 0 dup) |

| Perf | Before batching fix | After batching fix |
|---|---|---|
| Sink drain rate (post-burst) | ~240 w/s | **≥ 7,300 w/s** (~30×) |
| PG↔Mongo converge after 10s post-load | 8k / 74k | **73k / 73k** |

k6: 111,822 iterations in 30s, 3,755 req/s sustained, 0 failed requests, p95 ≈ 7.8ms.

A sample Mongo doc written through the full `transformer → sink` pipeline against the YAML rule at [schema/transforms/users.yml](./schema/transforms/users.yml):

```js
{
  _id:           "users:1",            // namespaced PK (ADR-002)
  _pkId:         1,                    // renamed from `id`
  fullName:      "Alice Anderson",     // renamed from `full_name`
  email:         "alice@example.com",
  isActive:      true,                 // renamed from `is_active`
  createdAt:     "2026-04-20T23:33:08.065680Z",
  updatedAt:     "2026-04-20T23:33:08.065680Z",
  profile:       "{\"plan\": \"pro\"}",
  schemaVersion: 1,
  sourceLsn:     26478736              // LSN gate (ADR-002)
}
```

## Why these decisions

Every architectural choice is an ADR under [docs/decisions/](./docs/decisions/). Each one explains the failure mode it prevents.

| # | Decision | Why |
|---|---|---|
| [001](./docs/decisions/001-kafka-over-rabbitmq.md) | Kafka over RabbitMQ | Partition-ordered delivery keyed on PK + native offset storage + replay from LSN |
| [002](./docs/decisions/002-lsn-gated-upserts.md) | LSN-gated upserts, not 2PC | 2PC across Kafka+Mongo doesn't compose; idempotent sink does. **Proven with integration test.** |
| [003](./docs/decisions/003-commit-after-sideeffect.md) | Commit after side-effect | Crash-between-commit-and-write = data loss; this pattern makes it impossible. **Proven with chaos 01.** |
| [004](./docs/decisions/004-yaml-transforms-over-code.md) | YAML transform rules | Adding a table = adding a file, not a deploy. **Proven by live pipeline.** |
| [005](./docs/decisions/005-toxiproxy-for-chaos.md) | Toxiproxy for chaos | Reproducible, scriptable, laptop-friendly. **Proven with chaos 02.** |
| [006](./docs/decisions/006-schema-registry-plus-yaml.md) | Schema Registry + YAML | Wire-level compat (registry) + semantic transform (YAML) |

## What this demonstrates (for hiring managers)

- **Distributed systems reasoning.** Partition ordering, consumer-group semantics, WAL internals, LSN monotonicity.
- **Production SRE instincts.** Golden signals, SLOs, runbooks, chaos-as-CI, "I found my own bug and fixed it" (see the MarkCommitOffsets story in chaos-findings).
- **Pragmatic architecture.** Two-service split (transformer + sink) — neither monolith nor microservice sprawl. Three Go services total; nothing more.
- **Data modeling.** Relational → document with schema evolution via declarative YAML rules, not per-table code.
- **Test discipline.** 17 unit tests (TDD, red-green-refactor visible in commit history) + 1 integration test against live Mongo (6 ordering cases for LSN gate) + 4 chaos scenarios + GitHub Actions CI wiring it all together.

## Trade-offs I deliberately made (the honest section)

- **No multi-region.** Single-DC demo. Real prod needs MirrorMaker 2 and tombstone-aware cross-region replication. Doubles infra scope, adds nothing to the core story.
- **No Kafka transactions across heterogeneous sinks.** 2PC between Kafka and Mongo doesn't compose. Idempotent sink is the correct pattern. ADR-002 explains.
- **Laptop-simplified Mongo.** Single-node replica set in dev compose, not a 3-node set. Noted in [CLAUDE.md](./CLAUDE.md).
- **JsonConverter instead of Avro+Schema Registry for wire format.** Week-4 simplification — Schema Registry still runs but is unused. ADR-006 documents the flip-on path.
- **No DLQ web UI.** `make reprocess-dlq` is sufficient for a portfolio demo.
- **Secrets via `.env`.** Real prod needs Vault/SSM. Out of scope.

A senior engineer knows what NOT to build. These omissions are signals, not gaps.

## Prerequisites

| Tool | Install |
|---|---|
| Docker Desktop 24+ with Compose v2 | <https://www.docker.com/> |
| `make` (optional) | macOS/Linux preinstalled. Windows: `choco install make` or use the raw `docker compose …` commands |
| `jq`, `curl` | Usually preinstalled; on Windows `choco install jq` |
| Go 1.22+ (only to rebuild services or run `go test` locally) | <https://go.dev/dl/> |
| k6 (optional — dockerised via chaos overlay) | <https://k6.io/docs/get-started/installation/> |

## Project status

Weeks 1-4 + polish pass complete. See [docs/plan.md](./docs/plan.md) for the per-phase exit criteria and the commits that delivered each. Upcoming changes are listed in [CHANGELOG.md](./CHANGELOG.md).

**Known limitations** (see the "Trade-offs" section above):
- Single-DC demo. Multi-region would require MirrorMaker 2 (out of scope).
- Dev compose relaxes `min.insync.replicas=2` / `RF=3` for laptop resource reasons. Production compose restores them; see [CLAUDE.md](./CLAUDE.md).
- Secrets via `.env` for local dev only. Production requires Vault / AWS Secrets Manager / External Secrets. See [SECURITY.md](./SECURITY.md).

## Documentation

- [docs/architecture.md](./docs/architecture.md) — Component diagram and data flow.
- [docs/deployment.md](./docs/deployment.md) — Production deployment to Kubernetes (Helm), pre-requisites, secrets.
- [docs/operations.md](./docs/operations.md) — Day-to-day ops: deploys, scaling, capacity, incident response.
- [docs/runbook.md](./docs/runbook.md) — Per-alert response procedures.
- [docs/security.md](./docs/security.md) — Threat model, defense-in-depth, secrets management.
- [docs/slo.md](./docs/slo.md) — SLI / SLO definitions and error budgets.
- [docs/chaos-findings.md](./docs/chaos-findings.md) — Chaos suite findings in `Finding → Fix → Re-measure` format.
- [docs/plan.md](./docs/plan.md) — Per-phase delivery plan and exit criteria.
- [docs/decisions/](./docs/decisions/) — Architecture Decision Records.
- [CHANGELOG.md](./CHANGELOG.md) — Version history (Keep a Changelog format).
- [SECURITY.md](./SECURITY.md) — Vulnerability disclosure process.

## Security

See [SECURITY.md](./SECURITY.md) for vulnerability disclosure process and response timelines.

## Contributing

Issues and pull requests welcome. Conventional Commits format expected on commit messages (`feat(sink):`, `fix(chaos):`, `docs(plan):`, `ci:`, `polish:`). Every transform rule change needs a test in `services/transformer/internal/mapper/mapper_test.go`. Every chaos scenario needs a `# PASS:` criterion comment.

## License

[MIT](./LICENSE) — see the file for the full text.
