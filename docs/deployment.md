# Deployment Guide

How to deploy pg2mongo-cdc to a real environment. The dev `docker-compose.yml`
in the repo root is for local development and explicitly violates several
production invariants for laptop resource reasons (see [`docs/invariants.md`](./invariants.md));
this document is the production path.

> **Audience.** Platform engineer doing the first deployment to a
> staging or production cluster. Assumes Kubernetes 1.28+, an
> external Kafka cluster (managed or self-hosted at RF=3), an
> external MongoDB (replica set or Atlas M30+), and an external
> Postgres with logical replication enabled.

---

## Pre-requisites

| Component | Minimum | Recommended | Why |
|---|---|---|---|
| Postgres | 14 with `wal_level=logical` | 16, managed (RDS / Cloud SQL / Supabase Pro) | Logical replication slot is the source of truth. Managed Postgres handles backup, PITR, and failover. |
| Kafka | 3 brokers, RF=3, `min.insync.replicas=2` | Confluent Cloud / MSK / Aiven, or self-hosted Strimzi | Production invariant #5 in docs/invariants.md. |
| Schema Registry | One instance reachable from Connect | Confluent Schema Registry, Apicurio, or Aiven | Required for Avro envelope evolution (ADR-006). |
| MongoDB | Replica set of 3 (`writeConcern: majority`) | Atlas M30+, or self-managed RS with `writeConcern: majority` and `readConcern: majority` | Idempotency requires linearizable reads after writes. |
| Kubernetes | 1.28+ | 1.30+ with HPA v2 | Sink and transformer are stateless; HPA scales them on partition lag. |
| Container registry | Public or private (e.g. ghcr.io, ECR, GAR) | Same network as cluster to avoid pull-time egress | Build → push images → Helm references the registry. |
| Secrets management | Anything not env vars in plain Helm values | HashiCorp Vault, AWS Secrets Manager, External Secrets Operator | See [`security.md`](./security.md). |
| Observability | Prometheus + Grafana | Grafana Cloud Free + Loki for logs | Dashboards live in `observability/grafana/`. |

## Topology overview

```
                  ┌──────────────────────────┐
                  │  Postgres (managed)      │
                  │  wal_level=logical       │
                  │  publication: zdt_pub    │
                  │  slot: zdt_slot          │
                  └────────────┬─────────────┘
                               │ (TLS, replication user)
                               ▼
┌────────────────────────────────────────────────────────────┐
│  Kubernetes namespace: pg2mongo-cdc                         │
│                                                             │
│  Deployment: kafka-connect (3 replicas, distributed mode)   │
│   └─ Debezium PostgresConnector → produces cdc.*            │
│                                                             │
│  Deployment: transformer (3 replicas, scaled on consumer    │
│              group lag of cdc.*)                            │
│   └─ Reads cdc.*, writes transformed.*                      │
│                                                             │
│  Deployment: sink       (3 replicas, scaled on consumer     │
│              group lag of transformed.*)                    │
│   └─ Reads transformed.*, writes MongoDB with LSN gate      │
│                                                             │
│  Note: loadgen is OMITTED from production. It exists in     │
│        dev compose for k6 chaos testing only.               │
└─────┬─────────────────┬─────────────────────────┬───────────┘
      │                 │                         │
      ▼                 ▼                         ▼
┌──────────┐   ┌────────────────────┐    ┌──────────────────┐
│  Kafka   │   │  Schema Registry   │    │  MongoDB         │
│  RF=3    │   │  (Avro evolution)  │    │  3-node RS,      │
│  ISR≥2   │   │                    │    │  writeConcern:   │
└──────────┘   └────────────────────┘    │   majority       │
                                         └──────────────────┘
```

## Step-by-step deployment

### 1. Source Postgres setup

Run as a Postgres superuser:

```sql
ALTER SYSTEM SET wal_level = 'logical';
ALTER SYSTEM SET max_replication_slots = 10;
ALTER SYSTEM SET max_wal_senders = 10;
-- Restart Postgres to apply
```

Then apply the schema and create the Debezium role with least privilege:

```bash
psql -h <pg-host> -U postgres -d <db> -f schema/postgres/001_init.sql
```

The init file creates the `debezium` role with `LOGIN REPLICATION` and
`SELECT` on the source tables - no other privilege.

### 2. Build and push images

Each service has a multi-stage Dockerfile under `services/<service>/`.

```bash
# Set your registry and tag
REGISTRY=ghcr.io/your-org
TAG=v1.0.0

for svc in transformer sink; do
  docker build -t "$REGISTRY/pg2mongo-cdc-$svc:$TAG" "./services/$svc"
  docker push "$REGISTRY/pg2mongo-cdc-$svc:$TAG"
done

# Connect image (Debezium + plugins) - separate Dockerfile
docker build -t "$REGISTRY/pg2mongo-cdc-connect:$TAG" "./connectors/connect-image"
docker push "$REGISTRY/pg2mongo-cdc-connect:$TAG"
```

Multi-stage builds yield ~25 MB final images for the Go services
(`alpine:3.19` runtime, statically-linked Go binary, non-root `USER`).

### 3. Configure secrets

The pipeline needs four secrets:

| Secret | Used by | Format |
|---|---|---|
| `postgres-credentials` | Connect | `username`, `password` for the `debezium` role |
| `mongo-uri` | Sink | Full URI: `mongodb+srv://user:pass@cluster/?retryWrites=true&w=majority` |
| `kafka-credentials` | All three services | `username`, `password` (SASL/SCRAM) plus `bootstrap-servers` |
| `schema-registry-credentials` | Connect, Transformer | `username`, `password`, `url` |

Provisioning patterns:

- **External Secrets Operator** + Vault / AWS Secrets Manager / GCP
  Secret Manager is the recommended pattern. Define `SecretStore`
  pointing at your secret backend, then `ExternalSecret` resources
  per consumer.
- **Sealed Secrets** is acceptable for smaller deployments - secrets
  are encrypted in git, decrypted by the controller in-cluster.
- **Plain `kubectl create secret`** is acceptable for initial
  bootstrap only. Document who has the key material out-of-band.

Never commit `.env` files or unencrypted secrets to the repo. The
`.env` pattern in this codebase is dev-only; see
[`security.md`](./security.md).

### 4. Deploy via Helm

A reference Helm chart lives in `deploy/helm/pg2mongo-cdc/` (added
in v1.0). Customize `values.yaml` to point at your registry, secrets,
and external dependencies:

```yaml
image:
  registry: ghcr.io/your-org
  tag: v1.0.0

postgres:
  host: pg.your-internal-domain
  port: 5432
  database: app
  credentialsSecret: postgres-credentials

kafka:
  bootstrapServers: kafka-bootstrap.your-domain:9092
  credentialsSecret: kafka-credentials
  schemaRegistry:
    url: https://schema-registry.your-domain
    credentialsSecret: schema-registry-credentials

mongo:
  uriSecret: mongo-uri
  database: migration

transformer:
  replicaCount: 3
  resources:
    requests: { cpu: 200m, memory: 256Mi }
    limits:   { cpu: 1,    memory: 512Mi }
  hpa:
    enabled: true
    minReplicas: 3
    maxReplicas: 12
    targetConsumerLag: 1000

sink:
  replicaCount: 3
  resources:
    requests: { cpu: 200m, memory: 256Mi }
    limits:   { cpu: 1,    memory: 512Mi }
  hpa:
    enabled: true
    minReplicas: 3
    maxReplicas: 12
    targetConsumerLag: 1000

connect:
  replicaCount: 2
  resources:
    requests: { cpu: 500m, memory: 1Gi }
    limits:   { cpu: 2,    memory: 2Gi }
```

Install:

```bash
helm install pg2mongo-cdc deploy/helm/pg2mongo-cdc \
  --namespace pg2mongo-cdc --create-namespace \
  -f values.production.yaml
```

### 5. Register the Debezium connector

Once `kafka-connect` is `Ready`:

```bash
kubectl port-forward -n pg2mongo-cdc svc/kafka-connect 8083:8083 &
bash scripts/register-connectors.sh
```

Verify the connector + tasks are `RUNNING`:

```bash
curl -fsS localhost:8083/connectors?expand=status \
  | jq '.[] | {name: .status.name, state: .status.connector.state, tasks: [.status.tasks[].state]}'
```

### 6. Smoke test

Insert a row in Postgres and verify it lands in Mongo within 5 s:

```bash
psql -h <pg-host> -U app -d <db> -c \
  "INSERT INTO users (email, full_name) VALUES ('smoketest-$(date +%s)@deploy.test', 'Smoke');"
sleep 5
mongosh "$(get-secret mongo-uri)" --quiet \
  --eval "db.getSiblingDB('migration').users.findOne({email: /smoketest-/}, {email: 1, sourceLsn: 1})"
```

Expect a document with `sourceLsn` populated. If absent, check sink
logs for errors and consumer group lag.

### 7. Wire up alerts

Apply the alert rules from `observability/prometheus/alerts.yml`
to your Prometheus instance, and the Grafana dashboards from
`observability/grafana/`. Each alert maps to a section in
[`runbook.md`](./runbook.md). Configure your pager (PagerDuty /
Opsgenie / etc.) to route P1/P2 alerts immediately.

## Production Postgres options

| Option | Pros | Cons |
|---|---|---|
| AWS RDS Postgres | Multi-AZ, PITR, automated backups, parameter groups for `wal_level` | Cost; `wal_level=logical` requires a parameter group reboot |
| Cloud SQL Postgres (GCP) | Same as RDS, plus better integration with GKE | Same |
| Supabase Pro | `wal_level=logical` enabled by default, easy to provision | Less control over Postgres config; pricing scales with rows |
| Self-hosted on GKE/EKS via CloudNativePG | Full control, lower cost at scale | You operate it |
| Heroku Postgres | Managed, but logical replication requires Standard or higher tier | Pricier per GB |

The pipeline is agnostic to the choice as long as `wal_level=logical`
is enabled and a `REPLICATION` user can be created.

## Production Kafka options

| Option | Pros | Cons |
|---|---|---|
| Confluent Cloud | Best-in-class, includes Schema Registry, Connect | Most expensive |
| AWS MSK | Tight AWS integration, RF=3 by default | More ops than Confluent Cloud |
| Aiven Kafka | Multi-cloud, includes Schema Registry, Connect | Mid-range pricing |
| Strimzi on Kubernetes | Open-source, runs in your cluster | You operate it |
| Redpanda Cloud | Kafka-protocol-compatible, lower latency | Vendor-specific operational tools |
| Upstash Kafka | (Deprecated 2024) | Don't use |

This pipeline only requires Kafka protocol compatibility. The choice
is operational, not architectural.

## Production MongoDB options

- **MongoDB Atlas** (M30 or higher) - managed RS, automated backups,
  encryption at rest, `writeConcern: majority` available.
- **DocumentDB on AWS** - Mongo-compatible but with caveats; verify
  the LSN gate's `$or: [{$lt}, {$exists:false}]` filter behaves
  correctly under load.
- **Self-managed Mongo on Kubernetes** via the MongoDB Community
  Operator - full control, more ops.

## Sharding (when single replica set isn't enough)

A single Mongo replica set scales until either write IOPS or document
count saturates. When it does, shard on `_id`:

```js
sh.enableSharding("migration")
sh.shardCollection("migration.users", { _id: "hashed" })
```

The pipeline already namespaces `_id` as `users:<pk>`, which gives a
good uniform hash distribution. The LSN gate continues to work
correctly under sharding because each `_id` lives on exactly one shard
and Mongo guarantees per-document linearizability there.

## Multi-region (out of scope for v1)

This pipeline is single-data-center by design. Multi-region replication
of CDC pipelines is a substantially different problem (MirrorMaker 2,
conflict resolution, geo-aware sink) and is out of scope for v1.0.
See the "What this is NOT" section of the [README](../README.md).

## Disaster recovery

| Scenario | RPO | RTO | Recovery procedure |
|---|---|---|---|
| Sink container crash | 0 | < 30s | Kubernetes restarts, sink rejoins consumer group, LSN gate absorbs replay |
| Transformer container crash | 0 | < 30s | Same |
| Single Mongo secondary failure | 0 | 0 (transparent) | RS election if it was primary; ~10s window |
| Kafka broker failure | 0 | 0 (transparent if RF=3, ISR≥2) | ISR shrinks; broker rejoins on recovery |
| Postgres failover | 0 | < 60s | RDS Multi-AZ flips automatically; Debezium reconnects |
| Replication slot dropped externally | seconds-to-minutes of WAL | manual snapshot replay | `DELETE` connector, re-register with `snapshot.mode=initial` |
| Mongo cluster destroyed | full data | hours | Restore from backup, then full snapshot replay from Postgres via the connector |
| Postgres source destroyed | catastrophic | days | Restore from PITR backup, then drift-check against Mongo before resuming |

The single most important DR fact: **Postgres is the source of truth**.
Mongo can be rebuilt from Postgres + Debezium snapshot.mode=initial.
Postgres cannot be rebuilt from Mongo. Back up Postgres first, second,
and third.

## What this guide does NOT cover

- Day-to-day operations → [`operations.md`](./operations.md).
- Per-alert response → [`runbook.md`](./runbook.md).
- Threat model + secrets → [`security.md`](./security.md).
- SLO definitions → [`slo.md`](./slo.md).
- Architecture deep-dive → [`architecture.md`](./architecture.md).
