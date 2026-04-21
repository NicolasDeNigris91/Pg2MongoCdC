# Operations Guide

Day-to-day operating procedures for a deployed pg2mongo-cdc pipeline.
Companion to [`runbook.md`](./runbook.md) (per-alert response procedures)
and [`deployment.md`](./deployment.md) (initial deployment).

> **Audience.** On-call operator who has just been paged or is about
> to deploy a change. Assumes the pipeline is already running in
> production-shaped infrastructure (Kafka cluster RF=3, MongoDB
> replica set, managed Postgres with logical replication enabled).

---

## Health model

The pipeline has three layers of health, in order from cheapest to
most expensive to verify:

| Layer | Signal | What it tells you |
|---|---|---|
| Container `/healthz` | HTTP 200 | The Go process is alive and the HTTP server is responsive. **Says nothing about CDC progress.** |
| Consumer group lag | `kafka_consumer_lag` per group | The transformer or sink is consuming in step with what Debezium produces. |
| Row-count parity | PG count vs Mongo count per table | The pipeline is end-to-end correct. The slowest signal but the only one that matters for correctness. |

A common failure mode is "container healthy + zero progress" — see the
v1.0-polish entry in [chaos-findings.md](./chaos-findings.md) for the
silent transformer cold-start hang where every container reported
healthy while no record was being processed. Always check at least
two layers when investigating an incident.

## Daily checks (5 minutes)

Before standup or before logging off, verify:

1. **Replication lag p99 < 5 s** (Grafana → Migration Overview → "Replication lag p99").
2. **No alerts active** in Alertmanager / Grafana (status: green).
3. **Consumer group lag flat or trending down** for both `zdt-transformer` and `zdt-sink`.
4. **Connect task state RUNNING** for `zdt-postgres-source`. Anything else (PAUSED, FAILED, RESTARTING) needs investigation.

Quick CLI version:

```bash
# Lag per consumer group
kubectl exec -it kafka-0 -- kafka-consumer-groups \
  --bootstrap-server localhost:9092 --describe --all-groups \
  | awk '$5 != "-" && $5 + 0 > 1000'  # only show groups with > 1k lag

# Connector status
curl -fsS https://connect.your.cluster/connectors?expand=status \
  | jq '.[] | {name: .status.name, state: .status.connector.state, tasks: [.status.tasks[].state]}'
```

## Weekly checks (15 minutes)

1. **Postgres replication slot retention.** Should be < 1 GB under
   normal load. If higher, Debezium is falling behind:
   ```sql
   SELECT slot_name, active,
          pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained_wal
   FROM pg_replication_slots WHERE slot_name = 'zdt_slot';
   ```
   Investigate before retained WAL approaches `max_wal_size` × 2.
2. **MongoDB oplog window.** `rs.printReplicationInfo()` should show
   ≥24h of oplog. If shorter, secondaries can fall behind primary
   recovery and split-brain becomes possible during failover.
3. **Disk usage on Postgres / Mongo / Kafka brokers.** Trend, not
   absolute. Anything growing >5% / week is worth a capacity ticket.
4. **CI integration-stack history.** Should be 100% green. A flaky
   integration test left unaddressed becomes a real incident.

## Deployment

Reference: [`deployment.md`](./deployment.md) for the full procedure.
The summary is:

- **Code change to a Go service.** Green CI on `main` → tag a release
  → Helm rolls out new image. Each service rolls independently
  (transformer and sink share no state at runtime). Default rollout
  is `RollingUpdate` with `maxUnavailable: 0` so consumers don't
  unbalance the group during deploy.
- **Schema rule change.** YAML edit under `schema/transforms/`,
  PR review, merge to `main`, redeploy transformer. Sink does not
  need a redeploy — it consumes whatever shape transformer produces.
- **Kafka topic config change.** Avoid in-place mutation of
  partitions or replication factor under load. The right path is
  create a new topic with desired config, dual-write, cut consumers
  over, retire old topic. See ADR-006 for the dual-write pattern.

### Pre-deploy checklist

- [ ] CI green on the commit being deployed.
- [ ] CHANGELOG entry under `[Unreleased]` describes the change.
- [ ] Replication lag p99 < 1 s in the last 5 minutes (deploying
      under existing lag amplifies it).
- [ ] No active P1/P2 alerts.
- [ ] If the change touches the sink, run scenario 01 in staging
      first and confirm `INTEGRITY OK`.

### Post-deploy checklist

- [ ] All replicas of the new image report `Ready` within 60 s.
- [ ] Replication lag p99 returns to baseline within 5 minutes.
- [ ] No new error-counter increments
      (`migration_write_errors_total`, sink `loop error` log lines).
- [ ] Consumer group lag does not grow.

### Rollback

Helm rollback is the standard path:

```bash
helm rollback pg2mongo-cdc <previous-revision>
```

The Go services are stateless — rollback is safe at any time. Kafka
consumer groups will rebalance, which produces a brief lag spike
but no data loss (LSN gate absorbs any redelivery).

The **only** non-trivial rollback case is when the rolled-back image
predates a schema change in `schema/transforms/`. The transformer
will reject events shaped for the new schema. If you need to roll
back across a schema change, also revert the YAML rule and redeploy.

## Scaling

| Bottleneck | Symptom | Action |
|---|---|---|
| Transformer | `rate(migration_events_processed_total{stage="transformer"}[5m])` < `{stage="sink"}` | Scale transformer replicas up. Bounded by partition count of `cdc.*` topics. |
| Sink | Replication lag growing while transformer is keeping up | Scale sink replicas up. Same partition-count bound on `transformed.*`. |
| Mongo write capacity | Sink p99 batch latency > 100 ms | Vertical scale primary, or shard. See [`deployment.md`](./deployment.md) for sharding pattern. |
| Kafka throughput | Producer-side metrics show backpressure | Add brokers + reassign partitions. Plan a maintenance window. |

**Horizontal scaling rule of thumb.** Adding a transformer or sink
replica beyond the partition count of its source topic adds
*nothing* — the extra consumer sits idle. If you need more
parallelism, increase partitions first (and accept the rebalance
that follows), then add replicas.

## Capacity planning

For a steady-state workload of `R` rows-per-second with average
event payload `B` bytes:

- **Kafka throughput**: `R × B × 2` (CDC + transformed topics).
  At RF=3, multiply by 3 again for replication traffic.
- **Mongo write IOPS**: `R / batch_size`. Default sink batch size
  is whatever a single Kafka poll returns (~500–1000 records under
  load), so IOPS ≈ `R / 750`.
- **Postgres WAL retention**: at least
  `lag_tolerance_seconds × wal_generation_rate`. Track
  `pg_wal_size` weekly to verify.
- **Disk**: 7-day retention on `cdc.*` (default) means
  `R × B × 86400 × 7` per topic. Add 50% headroom.

## Incident response

When an alert fires:

1. Acknowledge in pager (PagerDuty / Opsgenie).
2. Open the alert's section in [`runbook.md`](./runbook.md). Each
   alert there has likely causes ranked by probability and a
   "first move" command.
3. If the runbook doesn't resolve the issue in 15 minutes, escalate.
4. After the incident: write a postmortem in `docs/incidents/`
   following the format used by [chaos-findings.md](./chaos-findings.md)
   ("Symptom / Investigation / Root cause / Fix / Re-measure").
   Action items go into the issue tracker, not into the postmortem
   itself.

### Severity levels

| Severity | Definition | Response |
|---|---|---|
| P1 | Replication stopped or data drift detected | Page immediately, all-hands |
| P2 | Replication lag > SLO sustained > 15 min | Page, single on-caller |
| P3 | Single component degraded but pipeline healthy | Ticket, fix in next business hour |
| P4 | Cosmetic, optimization, doc update | Ticket, fix in next sprint |

## Maintenance windows

The pipeline tolerates short maintenance windows in the dependencies
without data loss:

- **Postgres**: any pause < `max_replication_slot_age` (default
  unlimited if `max_slot_wal_keep_size = -1`). Verify the slot
  resumes from `restart_lsn` after the maintenance.
- **Kafka**: rolling broker restart with `min.insync.replicas=2`
  on RF=3 keeps producers writing throughout. Plan restarts so no
  more than one broker is offline at any time.
- **Mongo**: stepdown of the primary triggers a ~10s window where
  writes are buffered by the driver. Sink retries automatically.
- **Sink / Transformer**: rolling restart triggers a consumer group
  rebalance (~5–10 s lag spike). Acceptable inside business hours.

## Common operator tasks

### Replay events from a specific LSN forward

If a downstream sink is reset (e.g. Mongo dropped and recreated),
restart the Debezium connector from scratch. The sink's idempotency
absorbs the resulting full replay:

```bash
curl -X DELETE https://connect.your.cluster/connectors/zdt-postgres-source
# Re-register with snapshot.mode=initial
bash scripts/register-connectors.sh
```

Expect a full table snapshot to flow through. Replication lag will
spike during the snapshot and recover when it transitions to
streaming mode (`snapshot:"last"` event in the log).

### Check whether a specific row reached Mongo

```bash
PG_ID=12345

# Postgres truth
docker compose exec postgres psql -U app -d app -tAc \
  "SELECT id, email, updated_at FROM users WHERE id = $PG_ID"

# Mongo state
docker compose exec mongo mongosh --quiet \
  mongodb://localhost:27017/migration?replicaSet=rs0 \
  --eval "JSON.stringify(db.users.findOne({_pkId: $PG_ID}))"
```

If Mongo lacks the row, look at the consumer group lag for `zdt-sink`
and the sink's logs around the row's `updated_at` timestamp.

### Pause the pipeline gracefully

```bash
# Pause Debezium — stops new events at the source
curl -X PUT https://connect.your.cluster/connectors/zdt-postgres-source/pause
# Wait for downstream to drain
# (verify via consumer group lag = 0)
# Resume
curl -X PUT https://connect.your.cluster/connectors/zdt-postgres-source/resume
```

Pausing Debezium is **safer** than stopping the sink for short windows
— the replication slot retains the WAL position; the sink's offset
state is unaffected.

## What this guide does NOT cover

- Initial cluster provisioning → see [`deployment.md`](./deployment.md).
- Per-alert response → see [`runbook.md`](./runbook.md).
- Architecture and data flow → see [`architecture.md`](./architecture.md).
- Threat model and secrets → see [`security.md`](./security.md).
- SLO definitions and error budgets → see [`slo.md`](./slo.md).
