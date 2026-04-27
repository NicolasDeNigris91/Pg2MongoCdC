# Runbook - what to do when each alert fires

Every alert from `observability/prometheus/alerts.yml` has a section here. Keep the sections short, declarative, and in the same order as the alerts file.

---

## ReplicationLagHigh

**Symptom.** `histogram_quantile(0.99, ... replication_lag_seconds ...) > 10s` for 5 minutes.

**Likely causes (in order of probability):**

1. Sink-svc CPU saturated → scale out (`docker compose up --scale sink=3`).
2. Mongo write latency up → check `db.serverStatus().wiredTiger.concurrentTransactions.write.available`.
3. Kafka broker under-replicated → check `kafka_server_ReplicaManager_UnderReplicatedPartitions`.
4. Transformer blocked on a slow YAML rule (rare) → check `migration_events_processed_total` rate by stage.

**First move.** Check which stage is lagging. `rate(migration_events_processed_total{stage="transformer"}[1m])` vs `{stage="sink"}`. Whichever is *lower* is the bottleneck.

---

## ConsumerLagHigh

**Symptom.** A consumer group's lag exceeds 10k records for 2 minutes.

**Likely causes.**

1. Crash-recovery in progress - lag should shrink within one `max.poll.interval.ms` (5 min default).
2. A stuck consumer thread - check logs for `Rebalance in progress`.
3. Downstream write failure loop - correlate with `migration_write_errors_total`.

**Do NOT** restart blindly. A rebalance storm amplifies lag. Wait 5 min first.

---

## DLQNonEmpty

**Symptom.** At least one message in a `dlq.*` topic.

**First move.** Inspect it:

```bash
docker compose exec kafka kafka-console-consumer \
  --bootstrap-server kafka:29092 --topic dlq.source \
  --from-beginning --max-messages 10 --timeout-ms 5000 \
  --property print.headers=true
```

The headers include: `__dlq_error_reason`, `__dlq_source_offset`, `__dlq_source_topic`. These tell you what, where, and why.

**Reprocessing.** Once the root cause is fixed in code / config, replay:

```bash
./scripts/reprocess-dlq.sh dlq.source      # reads, re-validates, re-produces
```

Never `kafka-console-producer` the old event back verbatim - it will fail the same way.

---

## CheckpointStaleness

**Symptom.** `migration_checkpoint_staleness_seconds > 60` for 2 minutes.

Fires when `sink-svc` has stopped writing `_migration_checkpoints` docs. It usually means either:

- The sink process is alive but its consume loop has stalled (deadlock, blocked Mongo write).
- The clock on the sink host is skewed forward.

**First move.** `docker compose logs sink | tail -200` and look for repeated errors around `BulkWrite`. If logs are silent but the process is up, check the goroutine dump at `/debug/pprof`.

---

## WriteErrorRateHigh

**Symptom.** Sink write error rate > 0.1% for 5 minutes.

**Likely causes.**

1. Mongo is in a failover / primary election (usually recovers in < 30s).
2. Schema-registry version mismatch after a producer upgrade.
3. Disk full on Mongo data volume.

**Check.** `docker compose exec mongo mongosh --eval "rs.status()"`.

---

## ReplicationSlotInactive

**Symptom.** `pg_replication_slots_active == 0` for 2 minutes.

**Severity: critical.** WAL will accumulate on the source until disk fills. In a real prod migration this is a paging alert.

**First move.**

1. Is `connect` running and healthy? `docker compose ps connect`.
2. Is Debezium's task in FAILED state? `curl localhost:8083/connectors/zdt-postgres-source/status | jq '.tasks[].state'`.
3. If FAILED, read the trace and restart:
   ```bash
   curl -X POST localhost:8083/connectors/zdt-postgres-source/restart
   ```
4. If the slot was dropped externally (someone ran `pg_drop_replication_slot`), you lost data from the WAL window since Debezium's last confirmed LSN. Full resnapshot required:
   ```bash
   curl -X DELETE localhost:8083/connectors/zdt-postgres-source
   # re-register with snapshot.mode=initial - incurs a full table scan
   ```

---

## Discovery / debugging generics

- **Connector status in one line:** `curl -sS localhost:8083/connectors?expand=status | jq '.[].status | {name, state: .connector.state, tasks: [.tasks[].state]}'`
- **Postgres slot state:** `SELECT slot_name, active, restart_lsn, confirmed_flush_lsn, pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained_wal FROM pg_replication_slots;`
- **Kafka topic offsets:** `docker compose exec kafka kafka-get-offsets --bootstrap-server kafka:29092 --topic-pattern 'cdc\..*'`
- **Mongo per-collection counts:** `docker compose exec mongo mongosh --quiet mongodb://localhost:27017/migration?replicaSet=rs0 --eval 'db.getCollectionNames().forEach(n => print(n, db[n].countDocuments()))'`
