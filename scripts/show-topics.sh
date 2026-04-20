#!/usr/bin/env bash
# Show Kafka topics + peek at recent CDC events.
set -euo pipefail

echo "Topics:"
docker compose exec -T kafka kafka-topics \
  --bootstrap-server kafka:29092 --list | sort
echo ""

for topic in cdc.users cdc.orders cdc.order_items; do
  echo "=== Last 2 events on $topic ==="
  docker compose exec -T kafka kafka-console-consumer \
    --bootstrap-server kafka:29092 \
    --topic "$topic" \
    --max-messages 2 \
    --from-beginning \
    --timeout-ms 3000 2>/dev/null || echo "(no events or topic not yet created)"
  echo ""
done
