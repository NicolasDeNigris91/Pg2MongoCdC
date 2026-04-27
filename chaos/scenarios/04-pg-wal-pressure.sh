#!/usr/bin/env bash
# PASS: pausing the connect worker for 2 minutes while writes continue does
#       not lose events - the replication slot keeps WAL alive; on resume,
#       Debezium catches up and PG↔Mongo converge.
set -euo pipefail

PAUSE_SECS="${PAUSE_SECS:-120}"

echo "Scenario 04: Postgres WAL pressure (pause connect for ${PAUSE_SECS}s)"
echo "============================================"

echo "Measuring replication slot state BEFORE ..."
docker compose exec -T postgres psql -U app -d app -c \
  "SELECT slot_name, active, restart_lsn, confirmed_flush_lsn FROM pg_replication_slots;"

echo "Pausing connect (Debezium can't advance the slot) ..."
docker compose pause connect

echo "Generating writes for ${PAUSE_SECS}s ..."
for i in $(seq 1 "$PAUSE_SECS"); do
  docker compose exec -T postgres psql -U app -d app -c \
    "INSERT INTO users (email, full_name) VALUES ('chaos4-'||extract(epoch from now())||'@test.dev', 'C4') ON CONFLICT DO NOTHING;" \
    >/dev/null 2>&1 || true
  sleep 1
done

echo "Measuring slot state DURING (WAL should have accumulated):"
docker compose exec -T postgres psql -U app -d app -c \
  "SELECT slot_name, active, restart_lsn, confirmed_flush_lsn,
          pg_size_pretty(pg_wal_lsn_diff(pg_current_wal_lsn(), restart_lsn)) AS retained_wal
   FROM pg_replication_slots;"

echo "Unpausing connect ..."
docker compose unpause connect

echo "Waiting for catchup..."
sleep 30

"$(dirname "$0")/../verify-integrity.sh"
