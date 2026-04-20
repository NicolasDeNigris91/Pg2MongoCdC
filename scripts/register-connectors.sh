#!/usr/bin/env bash
# Register Debezium (source) + MongoDB (sink) connectors.
# Cross-platform alternative to `make register-connectors` — runs under
# Git Bash, WSL, macOS, Linux, or any POSIX shell with curl available.
set -euo pipefail

CONNECT_URL="${CONNECT_URL:-http://localhost:8083}"
ROOT="$(cd "$(dirname "$0")/.." && pwd)"

echo "Waiting for Kafka Connect REST at $CONNECT_URL ..."
for i in $(seq 1 60); do
  if curl -fsS "$CONNECT_URL/connectors" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done

if ! curl -fsS "$CONNECT_URL/connectors" >/dev/null 2>&1; then
  echo "ERROR: Connect REST never came up. Check: docker compose logs connect" >&2
  exit 1
fi

register_one() {
  local name="$1"
  local file="$2"
  echo "Registering $name from $file ..."
  # PUT /connectors/<name>/config expects only the inner "config" object.
  # Our JSON files wrap it in {name, config}; extract with a portable here-doc trick.
  # If jq is available, use it; otherwise POST the full JSON to /connectors.
  if command -v jq >/dev/null 2>&1; then
    local payload
    payload=$(jq -c '.config' "$file")
    curl -fsS -X PUT -H "Content-Type: application/json" \
      --data "$payload" \
      "$CONNECT_URL/connectors/$name/config" | sed 's/^/  /'
  else
    # POST full body — creates if absent, 409 if already exists (then PUT the config fallback below)
    local http_code
    http_code=$(curl -sS -o /tmp/connect-resp.$$.json -w '%{http_code}' \
      -X POST -H "Content-Type: application/json" \
      --data @"$file" "$CONNECT_URL/connectors") || true
    if [ "$http_code" = "201" ]; then
      echo "  created"
    elif [ "$http_code" = "409" ]; then
      echo "  already exists — skipping (use DELETE + re-run to replace)"
    else
      echo "  ERROR (HTTP $http_code):"
      cat /tmp/connect-resp.$$.json
      rm -f /tmp/connect-resp.$$.json
      return 1
    fi
    rm -f /tmp/connect-resp.$$.json
  fi
  echo "  done"
}

register_one "zdt-postgres-source" "$ROOT/connectors/debezium-postgres.json"

# As of Week 2, the Mongo sink is our own Go service (services/sink/). The
# off-the-shelf MongoDB Kafka Connector is kept on disk at
# connectors/mongo-sink.json for historical comparison (chaos 01 lost 1
# row against it — see docs/chaos-findings.md). To register the baseline
# explicitly, set REGISTER_OFFSHELF_SINK=1.
if [ "${REGISTER_OFFSHELF_SINK:-0}" = "1" ]; then
  register_one "zdt-mongo-sink" "$ROOT/connectors/mongo-sink.json"
else
  # If a previous run registered it, drop it so it does not race with our Go sink.
  curl -fsS -X DELETE "$CONNECT_URL/connectors/zdt-mongo-sink" 2>/dev/null || true
fi

echo ""
echo "Connector status:"
curl -fsS "$CONNECT_URL/connectors?expand=status" \
  | { command -v jq >/dev/null 2>&1 \
      && jq '.[].status | {name, state: .connector.state, tasks: [.tasks[].state]}' \
      || cat; }
