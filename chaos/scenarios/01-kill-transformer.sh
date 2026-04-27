#!/usr/bin/env bash
# PASS: after SIGKILL on the transformer mid-stream, row counts match and
#       no DLQ entries appear. Recovery completes within 60s of restart.
#
# REQUIRES: Week 2+ (transformer-svc container). Until then, this script
# runs a no-op on the current off-the-shelf Mongo sink connector path
# (kills the connect container instead - which exercises the same crash-
# recovery story at a coarser granularity).
set -euo pipefail

DURATION="${DURATION:-30}"
# Default to our Week 2 Go sink now that it exists. Override with
# TARGET=connect to replay the Week 1 baseline (off-the-shelf MongoSinkConnector).
TARGET="${TARGET:-sink}"

echo "Scenario 01: kill $TARGET mid-stream"
echo "============================================"
echo "Starting background load for ${DURATION}s..."
# Background load - replace with k6 once Week 3 lands
(
  for i in $(seq 1 "$DURATION"); do
    docker compose exec -T postgres psql -U app -d app -c \
      "INSERT INTO users (email, full_name, profile)
       VALUES ('chaos1-'||extract(epoch from now())||'@test.dev', 'Chaos Load', '{\"src\":\"chaos-01\"}')
       ON CONFLICT DO NOTHING;" >/dev/null 2>&1 || true
    sleep 1
  done
) &
LOAD_PID=$!

sleep 5

echo "Killing $TARGET ..."
docker compose kill -s SIGKILL "$TARGET"

echo "Waiting 3s, then restarting $TARGET ..."
sleep 3
docker compose up -d "$TARGET"

echo "Waiting for load to finish..."
wait $LOAD_PID

echo "Waiting for pipeline to drain..."
sleep 15

# Re-register connectors if connect was the one killed
if [ "$TARGET" = "connect" ]; then
  "$(dirname "$0")/../../scripts/register-connectors.sh" >/dev/null 2>&1 || true
fi

"$(dirname "$0")/../verify-integrity.sh"
