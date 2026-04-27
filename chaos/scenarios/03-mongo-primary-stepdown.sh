#!/usr/bin/env bash
# PASS: primary stepdown during load causes sink to retry; no data lost;
#       checkpoint doc shows monotonic progress after recovery.
set -euo pipefail

DURATION="${DURATION:-20}"

echo "Scenario 03: Mongo primary stepdown"
echo "============================================"

(
  for i in $(seq 1 "$DURATION"); do
    docker compose exec -T postgres psql -U app -d app -c \
      "INSERT INTO users (email, full_name) VALUES ('chaos3-'||extract(epoch from now())||'@test.dev', 'C3') ON CONFLICT DO NOTHING;" \
      >/dev/null 2>&1 || true
    sleep 1
  done
) &
LOAD_PID=$!

sleep 5
echo "Stepping down Mongo primary..."
# In a single-node replica set there is no other member to step up to; stepDown
# forces a re-election which takes ~10s and exercises the sink retry path.
docker compose exec -T mongo mongosh --quiet \
  "mongodb://localhost:27017/?replicaSet=rs0" \
  --eval "try { rs.stepDown(30) } catch(e) { print('stepDown threw (expected):', e.message) }" \
  >/dev/null 2>&1 || true

wait $LOAD_PID

sleep 20

"$(dirname "$0")/../verify-integrity.sh"
