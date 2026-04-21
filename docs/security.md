# Security

Threat model, defense-in-depth strategy, and secrets management
guidance for production deployments of pg2mongo-cdc. For vulnerability
disclosure, see [`SECURITY.md`](../SECURITY.md) at the repo root.

> **Audience.** Security reviewer or platform engineer about to put
> the pipeline in front of real data. The dev compose in this repo
> is **not production-secure**; this document describes what changes.

---

## Threat model

The pipeline handles three categories of data with three different
trust levels:

| Data | Confidentiality | Integrity | Availability |
|---|---|---|---|
| User application data (rows in PG) | High — typically PII | High — corruption is the worst outcome | Medium — short outages tolerable |
| CDC events on Kafka | Same as source rows | Critical — drift = silent data loss | Same as source |
| Operational secrets (DB credentials, API keys) | Critical | High | Low — short rotation pain tolerable |

### Threat actors considered

- **External attacker, internet-reachable surfaces.** None — the
  pipeline has no public ingress in the production topology.
- **External attacker, network-adjacent.** Anyone who reaches the
  cluster's pod network. Mitigated by network policies + mTLS on
  inter-service traffic.
- **Compromised dependency.** A malicious upstream package making
  it into the build. Mitigated by `gosec` + `trivy` scans in CI,
  pinned go.mod versions, image scanning at deploy time.
- **Compromised operator account.** An engineer's kubectl access
  used to exfiltrate data. Mitigated by RBAC, audit logging, and
  not putting secrets in plain ConfigMaps.
- **Insider with broker access.** Anyone with Kafka cluster admin
  could tamper with topics. Mitigated by ACLs, audit logs, and
  separation of dev/prod credentials.

### Threats explicitly not considered (out of scope)

- Side-channel attacks against the host kernel.
- Compromised hardware / supply-chain attacks below the container
  registry level.
- Physical access to data center hardware.

## Network architecture

```
                                         (no public ingress)
                                         ─────────────────────

┌───────────────────────────────────────────────────────────────┐
│  Cluster network (private)                                     │
│                                                                │
│   ┌──────────────┐       (mTLS or SASL/SCRAM)                  │
│   │  Connect     │─────────────────────────► Kafka cluster     │
│   │              │                                              │
│   │  Transformer │─────────────────────────► Kafka cluster     │
│   │              │                                              │
│   │  Sink        │─────────────────────────► Kafka cluster     │
│   │              │─────────────────────────► MongoDB           │
│   └──────────────┘                                              │
│         │                                                       │
│         │ (TLS, replication user credentials)                   │
│         ▼                                                       │
│   Postgres source (managed, in same VPC / VNet)                │
└───────────────────────────────────────────────────────────────┘
```

Required network controls in production:

1. **No public load balancer or ingress in front of any service.**
   The Connect REST API, sink and transformer health endpoints, and
   Prometheus metrics endpoints are reachable only from within the
   cluster network (Kubernetes `ClusterIP` services).
2. **Network policies** restricting egress per workload:
   - Connect can reach Postgres (5432) and Kafka (9092).
   - Transformer can reach Kafka only.
   - Sink can reach Kafka and Mongo only.
3. **TLS on all external connections**:
   - Postgres: `sslmode=verify-full` with the cluster CA.
   - Kafka: SASL/SCRAM-SHA-512 over TLS at minimum; mTLS preferred
     where supported by the broker.
   - Mongo: `tls=true&authMechanism=SCRAM-SHA-256` at minimum.
4. **Schema Registry** behind basic auth or OAuth, never anonymous
   write.

## Secrets management

The dev `.env` pattern (`KAFKA_PASSWORD=devpass`, etc.) is **dev only**.
Production must:

| What | How |
|---|---|
| Provision secrets | External Secrets Operator pulling from Vault / AWS Secrets Manager / GCP Secret Manager / Azure Key Vault. Sealed Secrets for smaller deployments. |
| Mount secrets | Kubernetes Secret → environment variable on pod. **Never** into a ConfigMap. **Never** into a `volumeMount` of a container that runs untrusted code. |
| Rotate secrets | Quarterly minimum for non-database creds. Database creds: every 90 days, with overlapping validity windows so the rotation does not require downtime. |
| Audit access | All `kubectl get secret` and `vault read` calls logged centrally; alert on bulk secret access by a single principal. |
| Limit blast radius | One Kubernetes Secret per service per credential. Never a "shared credentials" secret containing every password. |

### Secrets the pipeline needs

Reference: [`deployment.md`](./deployment.md) "Configure secrets".

- `postgres-credentials` — `username` + `password` for the Debezium
  replication user. Scope: `LOGIN REPLICATION` + `SELECT` on source
  tables only.
- `mongo-uri` — full SRV URI with embedded credentials for a Mongo
  user with `readWrite` on the `migration` database, no other
  privileges.
- `kafka-credentials` — SASL/SCRAM username + password. Each service
  gets its own user with ACLs scoped to the topics it produces or
  consumes:
  - Connect: produce on `cdc.*` and `dlq.source`.
  - Transformer: consume `cdc.*`, produce `transformed.*`.
  - Sink: consume `transformed.*`, produce `dlq.sink`.
- `schema-registry-credentials` — basic auth for write access (if
  Schema Registry is enabled).

## Authentication and authorization

| Control | Dev compose | Production |
|---|---|---|
| Postgres | trust auth on local socket | password + TLS, IP allowlist, replication-only role |
| Kafka | PLAINTEXT, no SASL | SASL/SCRAM-SHA-512 over TLS, per-service user with topic ACLs |
| Schema Registry | open | basic auth or OAuth |
| MongoDB | no auth | SCRAM-SHA-256, per-service user, IP allowlist, TLS |
| Connect REST | open on port 8083 | behind cluster ingress with TLS + network policy |
| Sink/transformer `/healthz` | open | open within cluster only (Kubernetes liveness/readiness consumer) |
| Sink/transformer `/metrics` | open | open within cluster only (Prometheus consumer) |

### Kafka ACL examples

For a 3-broker production cluster with SASL users `connect`, `transformer`, `sink`:

```bash
# Connect: produce to cdc.* and dlq.source
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:connect --producer --topic 'cdc.' --resource-pattern-type prefixed
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:connect --producer --topic dlq.source

# Transformer: consume cdc.*, produce transformed.*
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:transformer --consumer --topic 'cdc.' --resource-pattern-type prefixed --group zdt-transformer
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:transformer --producer --topic 'transformed.' --resource-pattern-type prefixed

# Sink: consume transformed.*, produce dlq.sink
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:sink --consumer --topic 'transformed.' --resource-pattern-type prefixed --group zdt-sink
kafka-acls --bootstrap-server <broker> --command-config admin.properties \
  --add --allow-principal User:sink --producer --topic dlq.sink
```

## Encryption

- **In transit.** TLS 1.2+ on every connection between services. No
  unencrypted protocol on production network.
- **At rest.** Cloud-provider-managed encryption at rest for
  Postgres, MongoDB, and Kafka volumes (KMS-backed). Verify the
  KMS key is per-environment and access-logged.
- **Backups.** Encrypted with the same KMS key chain. Test restore
  quarterly to verify the key chain is intact.

## Compliance considerations

| Regime | What it requires from the pipeline |
|---|---|
| GDPR (EU) | Right-to-delete must propagate through the pipeline. The DELETE event on Postgres flows through Debezium → Kafka → sink → Mongo. **Verify backups also honor the deletion** within the regulatory window (typically 30 days). |
| CCPA (California) | Same as GDPR for the read+delete path. |
| HIPAA | Out of scope for v1.0. Production HIPAA deployment would require BAA-covered managed services (Atlas HIPAA, MSK HIPAA, etc.) and an audit trail beyond what this doc specifies. |
| SOC 2 | Most controls (access logging, encryption at rest/in transit, MFA on operator accounts) are infrastructure-level, not pipeline-specific. The pipeline contributes via: structured logging, SLOs (`docs/slo.md`), incident postmortems (`docs/chaos-findings.md` format), CI security scans (lint + gosec + trivy in `.github/workflows/ci.yml`). |

## Defense-in-depth: what runs where

| Layer | Mitigations active |
|---|---|
| Container | Non-root user, read-only root filesystem, distroless / alpine base, no shell in production image |
| Pod | Restricted Pod Security Standard, no privileged escalation, no host network |
| Namespace | Network policies restricting east-west traffic, RBAC limiting `kubectl exec` |
| Cluster | Audit log, admission control rejecting `:latest` images |
| External services | TLS, SASL/SCRAM, IP allowlists, KMS-encrypted at rest |

## What this codebase contributes

These controls are present in the code today (not just doc):

- **Non-root user in every Dockerfile.** `services/sink/Dockerfile`,
  `services/transformer/Dockerfile`, `services/loadgen/Dockerfile`
  all create and switch to a dedicated UID 1000 user before
  `ENTRYPOINT`.
- **Multi-stage builds.** Final images are
  `alpine:3.19` + statically-linked Go binary. No build tools in
  the runtime image.
- **CI security scans.** `.github/workflows/ci.yml` runs `gosec`
  on Go source, `trivy` on Dockerfiles and dependency manifests,
  on every push and PR.
- **Least-privilege Postgres role.** `schema/postgres/001_init.sql`
  creates the `debezium` role with `LOGIN REPLICATION` + `SELECT`
  on source tables only. No DDL, no DELETE, no CONNECT to other
  databases.
- **Lint discipline.** `.golangci.yml` includes `errcheck`,
  `errorlint`, `bodyclose`, `noctx` — the linters that catch
  unhandled errors, unwrapped errors, leaked HTTP bodies, and
  context-less HTTP requests on the hot path.

## Vulnerability disclosure

See [`SECURITY.md`](../SECURITY.md) at the repo root.

## What this guide does NOT cover

- General Kubernetes hardening (CIS benchmark, etc.) — out of scope
  but assumed.
- Application-level authentication — there is no public application
  surface to authenticate.
- Cryptographic key generation procedures — assume your platform
  team has a documented KMS workflow.
