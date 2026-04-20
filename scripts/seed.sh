#!/usr/bin/env bash
# Insert a small burst of rows into Postgres and verify they land in Mongo.
# Equivalent of `make seed` — works without make.
set -euo pipefail

N="${N:-25}"

echo "Inserting $N users into Postgres ..."
docker compose exec -T postgres psql -U app -d app -c \
  "INSERT INTO users (email, full_name, profile)
   SELECT 'seed'||g||'@test.dev', 'Seed User '||g, jsonb_build_object('batch', g)
   FROM generate_series(1, $N) g
   ON CONFLICT (email) DO NOTHING;"

echo ""
echo "Waiting 3s for CDC propagation ..."
sleep 3

echo ""
echo "Postgres row count:"
docker compose exec -T postgres psql -U app -d app -tAc "SELECT count(*) FROM users;"

echo ""
echo "Mongo document count:"
docker compose exec -T mongo mongosh --quiet \
  "mongodb://localhost:27017/migration?replicaSet=rs0" \
  --eval "print(db.users.countDocuments())"

echo ""
echo "Sample Mongo document:"
docker compose exec -T mongo mongosh --quiet \
  "mongodb://localhost:27017/migration?replicaSet=rs0" \
  --eval "printjson(db.users.findOne())"
