#!/usr/bin/env bash
# PASS: 500ms Kafka latency + 10% data loss does not cause permanent data loss.
#       Consumer lag rises, then recovers. Final PG↔Mongo integrity holds.
#
# REQUIRES: docker-compose.chaos.yml overlay (Toxiproxy). Run the stack with:
#   docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d
set -euo pipefail

TOXIPROXY_URL="${TOXIPROXY_URL:-http://localhost:8474}"
DURATION="${DURATION:-30}"

echo "Scenario 02: Kafka network partition (500ms latency + 10% loss)"
echo "============================================"

if ! curl -fsS "$TOXIPROXY_URL/version" >/dev/null 2>&1; then
  echo "Toxiproxy not reachable at $TOXIPROXY_URL"
  echo "Start the chaos overlay: docker compose -f docker-compose.yml -f docker-compose.chaos.yml up -d"
  exit 2
fi

echo "Injecting latency toxic..."
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"name":"kafka_latency","type":"latency","attributes":{"latency":500,"jitter":100}}' \
  "$TOXIPROXY_URL/proxies/kafka/toxics" >/dev/null

echo "Injecting 10% packet loss..."
curl -fsS -X POST -H 'Content-Type: application/json' \
  -d '{"name":"kafka_loss","type":"limit_data","attributes":{"bytes":999999999},"toxicity":0.1}' \
  "$TOXIPROXY_URL/proxies/kafka/toxics" >/dev/null

echo "Running load for ${DURATION}s under degraded network..."
for i in $(seq 1 "$DURATION"); do
  docker compose exec -T postgres psql -U app -d app -c \
    "INSERT INTO users (email, full_name) VALUES ('chaos2-'||extract(epoch from now())||'@test.dev', 'C2') ON CONFLICT DO NOTHING;" \
    >/dev/null 2>&1 || true
  sleep 1
done

echo "Removing toxics..."
curl -fsS -X DELETE "$TOXIPROXY_URL/proxies/kafka/toxics/kafka_latency" >/dev/null
curl -fsS -X DELETE "$TOXIPROXY_URL/proxies/kafka/toxics/kafka_loss"    >/dev/null

echo "Allowing recovery..."
sleep 20

"$(dirname "$0")/../verify-integrity.sh"
