#!/usr/bin/env bash
# PASS: after SIGKILL on the transformer mid-stream, row counts match and
#       no DLQ entries appear. Recovery completes within 60s of restart.
set -euo pipefail

DURATION="${DURATION:-30}"
# TARGET=sink kills the Go sink. TARGET=connect kills Connect (the
# off-the-shelf MongoSinkConnector path - kept for comparison).
TARGET="${TARGET:-sink}"

echo "Scenario 01: kill $TARGET mid-stream"
echo "============================================"
echo "Starting background load for ${DURATION}s..."
# Inline psql load. k6 covers the heavier mix in load/k6/.
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
