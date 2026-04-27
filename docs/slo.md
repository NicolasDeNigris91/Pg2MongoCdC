# Service Level Objectives

Quantitative reliability targets for pg2mongo-cdc. SLIs (what we
measure), SLOs (the targets), and error budgets (how much the
target allows us to spend before it constrains feature work).

> **Audience.** Anyone deciding whether the pipeline is currently
> "healthy enough", or anyone proposing a change that might
> consume budget.

---

## SLI / SLO summary

| SLI | SLO target | Measurement window | Error budget |
|---|---|---|---|
| Replication lag | 99% of events propagate PG → Mongo in < 5 s | 30 days rolling | 1% × 30d × 86400s ≈ 25,920 s of "lag > 5s" allowed |
| Pipeline availability | 99.9% of minutes have at least one successful sink batch | 30 days rolling | 0.1% × 30d × 1440 min = 43.2 min/month of zero-progress allowed |
| Data integrity | 100% of `verify-integrity.sh` runs return `INTEGRITY OK` | continuous | 0 - any drift is a P1 incident, no budget |
| Sink write success rate | 99.95% of attempted Mongo writes succeed | 30 days rolling | 0.05% × `total_writes` failures budgeted |

The integrity SLO has zero budget because data drift is the
hardest-to-recover-from class of failure. Lag and availability
budgets are spent freely on intentional risks (deploys, scale events,
chaos drills); integrity is a do-not-spend invariant.

## SLI definitions (precise)

### Replication lag

```promql
histogram_quantile(
  0.99,
  sum by (le) (rate(migration_replication_lag_seconds_bucket[5m]))
)
```

Where `migration_replication_lag_seconds` is observed by the sink
on each successful write as `now() - event.source_ts_ms / 1000`.

**Why p99, not p95.** A p95 SLO masks the worst 5% of events
indefinitely. For CDC, the worst 5% can include exactly the events
that matter - an UPDATE to the row a user is actively reading.
p99 keeps the tail under the SLO too. p99.9 is overkill at
demo scale; revisit at >10k events/sec.

### Pipeline availability

```promql
(
  count_over_time(
    (rate(migration_events_processed_total{stage="sink"}[1m]) > 0)
    [30d:1m]
  )
) / (30 * 24 * 60)
```

Reads as: "fraction of 1-minute buckets in the last 30 days where
the sink processed at least one event". A bucket counts as down
when zero events flow through, regardless of cause (sink crash,
upstream Postgres idle, Kafka outage).

This SLI is honest about end-to-end behavior. A sink that's "alive"
(container healthy) but processing zero events is not available by
this definition. That alignment is the whole point - see the v1.0-polish
finding in [chaos-findings.md](./chaos-findings.md) for the
silent-cold-start failure mode this SLI would have caught
immediately.

### Data integrity

`bash chaos/verify-integrity.sh` returning exit 0 (`INTEGRITY OK`)
on a quiesced pipeline - meaning per-table row counts match between
Postgres and Mongo, and the per-row content checksum matches.

Continuous measurement is impractical (the verify script is
expensive on large tables), so we run it:

- After every chaos scenario in CI.
- Once daily in production via cron, on a quiet hour, against a
  sample of the largest tables.
- On demand after every deploy that touches the sink.

A single `INTEGRITY FAILED` is a P1 incident - page immediately,
all-hands. Never silent-fail a drift detection.

### Sink write success rate

```promql
1 - (
  sum(rate(migration_write_errors_total{stage="sink"}[5m]))
  /
  sum(rate(migration_events_processed_total{stage="sink"}[5m]))
)
```

Idempotent-skip events (duplicate-key errors that the LSN gate
absorbs as no-ops) are **not** counted as errors. Only true write
failures (network, write conflict that didn't resolve, Mongo
unavailability) count.

## Error budgets in practice

The budget is consumed minute-by-minute by SLO violations. Once
50% of the monthly budget is consumed, feature work is throttled:

| % budget consumed | Effect |
|---|---|
| 0 - 50% | Normal operation. Feature work proceeds. |
| 50 - 80% | Risky changes (sink internals, Kafka topic config, schema migrations) require explicit approval. |
| 80 - 100% | Feature work halts. All effort goes to reliability. Postmortems delivered for every consumed budget incident. |
| > 100% | SLO violation. Postmortem delivered to stakeholders. Reliability investment plan delivered next cycle. |

Note: the "data integrity" SLO has no budget. Even one minute of
detected drift is a P1, regardless of how much budget the other
SLOs have left.

## Alerting on SLO burn

Burn-rate alerts page when the **rate** of budget consumption is
high enough that the entire budget will be consumed before the
window ends.

For the replication lag SLO (1% over 30 days = 0.0036% per hour):

| Burn rate | Window | Alert |
|---|---|---|
| 14.4× | 1h | P1 - paging. The hourly rate alone burns 24h of budget per hour. |
| 6× | 6h | P2 - page on-call. Sustained, will burn ~30% of monthly budget if uncorrected. |
| 1× | 30d | (no alert - this is the SLO breaking exactly on schedule) |

Rules in `observability/prometheus/alerts.yml` implement these.

## What an SLO miss looks like

Concrete example. Say replication lag p99 spikes to 12 s for 1 hour
during a Kafka broker rolling restart:

- Lag > 5 s budget consumed: 3,600 s × 1.0 (every second of the
  hour was over budget) = **3,600 s consumed of 25,920 s monthly**.
- ~14% of monthly budget consumed in 1 hour.
- Triggers the 14.4× burn-rate alert (since "1 hour at 100% over
  budget" = ~14.4× the 30-day burn rate).
- Pager fires; on-call investigates; broker restart proceeds slower
  than planned; lessons learned filed in `docs/incidents/`.

## SLOs vs invariants

A useful distinction:

- **SLOs are budgeted.** It is acceptable to violate them
  occasionally; the budget exists to absorb that.
- **Invariants are not budgeted.** They cannot be violated even
  once.

Invariants for this pipeline live in [`docs/invariants.md`](./invariants.md):

1. Partition key = source PK (no overtake).
2. Commit-after-side-effect.
3. LSN-gated writes.
4. Producer `acks=all` + `enable.idempotence=true`.
5. `min.insync.replicas=2`, RF=3 (production).
6. DLQ is write-only from services.

A violation of any of these is by definition a P1 incident with
zero budget - same severity as the integrity SLO.

## Choice of measurement window

30 days is conventional. The argument for shorter windows
(7 days) is faster signal, more reactive ops. The argument for
longer (90 days) is less false alarm during one bad week.

For this pipeline at this scale, 30 days is the right balance -
long enough that one bad day doesn't trigger panic, short enough
that systemic regressions become visible within a single quarter.

## What this doc does NOT cover

- The actual Grafana dashboard implementing these SLIs - that lives
  in `observability/grafana/migration-overview.json`.
- The Prometheus alert rules - `observability/prometheus/alerts.yml`.
- Per-alert response procedures - [`runbook.md`](./runbook.md).
- Day-to-day operations - [`operations.md`](./operations.md).
