# Security Policy

## Supported Versions

This project is in active development; only the `main` branch receives
security fixes. Tagged releases on the `v1.x` line are maintained on a
best-effort basis.

| Version | Supported          |
| ------- | ------------------ |
| `main`  | :white_check_mark: |
| `v1.x`  | :white_check_mark: |
| < v1.0  | :x:                |

## Reporting a Vulnerability

**Please do not report vulnerabilities via public GitHub issues.**

Email: **nicolas.denigris91@icloud.com** with the subject line
`[SECURITY] pg2mongo-cdc: <short description>`.

Include:

- A description of the vulnerability and its impact.
- Reproduction steps (commands, configs, or a minimal case).
- Affected version (commit SHA or tag).
- Your assessment of severity, if you have one.
- Whether you would like public credit after disclosure.

## Response Timeline

- **Initial acknowledgement:** within 72 hours.
- **Triage and severity assessment:** within 7 days.
- **Fix or mitigation plan:** within 30 days for High/Critical,
  90 days for Medium, best-effort for Low.
- **Public disclosure:** coordinated with the reporter, typically after
  a fix has been released and users have had a reasonable window to
  upgrade (≥30 days unless actively exploited).

## Scope

In scope:

- The Go services under `services/` (`sink`, `transformer`, `loadgen`).
- The Kafka/Connect/Mongo wiring documented in this repository.
- The chaos suite and the integrity verifier (`chaos/`).
- Build- and deploy-time supply chain (Dockerfiles, CI workflows,
  dependency manifests).

Out of scope (report upstream instead):

- Vulnerabilities in Debezium, Kafka, MongoDB, Postgres, or
  third-party Go modules — report to their respective projects.
- Denial-of-service by overwhelming an un-rate-limited demo instance
  that a user chose to deploy publicly. Production deployments are
  expected to add an upstream rate limit; see
  [`docs/security.md`](./docs/security.md) if present.

## Hardening Guidance for Operators

The default `docker-compose.yml` in this repository is a **development**
configuration. It deliberately violates some production invariants
(e.g. single-broker Kafka, plaintext SASL) for laptop reproducibility.
Before deploying to a network others can reach, review:

- Secrets management — the `.env` pattern is for local development only.
  Production must use Vault / AWS Secrets Manager / External Secrets.
- mTLS + SASL on Kafka.
- TLS + authenticated replica set on MongoDB.
- `wal_level=logical` with least-privileged replication role on Postgres.
- Network isolation: neither Postgres, Kafka, Connect, nor MongoDB should
  be reachable from the public internet.

See [`CLAUDE.md`](./CLAUDE.md) for the full list of production invariants
the demo compose relaxes.
