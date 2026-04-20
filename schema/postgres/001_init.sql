-- Zero-Downtime Migration — source schema.
-- Runs on first container start via /docker-entrypoint-initdb.d.
--
-- Goals:
--   1. Create a realistic, normalized source schema (users, orders, order_items)
--      that exercises different CDC cases: PK updates, FK relationships,
--      JSONB columns, soft deletes.
--   2. Create a dedicated replication role for Debezium with the minimum
--      privileges needed (REPLICATION + SELECT on target tables).
--   3. Pre-create a publication so Debezium does not need CREATE privilege
--      on the database (least-privilege pattern from ADR-001).

BEGIN;

-- ---------------------------------------------------------------
-- Application tables (the "live production" data we are migrating)
-- ---------------------------------------------------------------
CREATE TABLE IF NOT EXISTS users (
    id          BIGSERIAL PRIMARY KEY,
    email       TEXT NOT NULL UNIQUE,
    full_name   TEXT NOT NULL,
    profile     JSONB NOT NULL DEFAULT '{}'::jsonb,
    is_active   BOOLEAN NOT NULL DEFAULT TRUE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS orders (
    id          BIGSERIAL PRIMARY KEY,
    user_id     BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    status      TEXT NOT NULL CHECK (status IN ('pending','paid','shipped','cancelled')),
    total_cents BIGINT NOT NULL CHECK (total_cents >= 0),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS orders_user_id_idx ON orders(user_id);
CREATE INDEX IF NOT EXISTS orders_status_idx ON orders(status);

CREATE TABLE IF NOT EXISTS order_items (
    id          BIGSERIAL PRIMARY KEY,
    order_id    BIGINT NOT NULL REFERENCES orders(id) ON DELETE CASCADE,
    sku         TEXT NOT NULL,
    qty         INT NOT NULL CHECK (qty > 0),
    price_cents BIGINT NOT NULL CHECK (price_cents >= 0)
);
CREATE INDEX IF NOT EXISTS order_items_order_id_idx ON order_items(order_id);

-- Ensure full before-image is emitted for UPDATEs/DELETEs so downstream can
-- do precise idempotency checks. Without REPLICA IDENTITY FULL, Debezium
-- only emits the PK on DELETEs, which makes tombstone handling brittle.
ALTER TABLE users       REPLICA IDENTITY FULL;
ALTER TABLE orders      REPLICA IDENTITY FULL;
ALTER TABLE order_items REPLICA IDENTITY FULL;

-- ---------------------------------------------------------------
-- Debezium replication role (least privilege)
-- ---------------------------------------------------------------
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'debezium') THEN
        CREATE ROLE debezium WITH LOGIN REPLICATION PASSWORD 'debezium';
    END IF;
END
$$;

GRANT CONNECT ON DATABASE app TO debezium;
GRANT USAGE ON SCHEMA public TO debezium;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO debezium;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO debezium;

-- ---------------------------------------------------------------
-- Publication for Debezium. Named so the Debezium connector config
-- can reference it explicitly (publication.autocreate.mode=disabled).
-- ---------------------------------------------------------------
DROP PUBLICATION IF EXISTS zdt_publication;
CREATE PUBLICATION zdt_publication FOR TABLE users, orders, order_items;

COMMIT;

-- ---------------------------------------------------------------
-- Seed data (tiny — just enough to verify the pipeline end-to-end).
-- Load tests use load/k6/write-mix.js for real volume.
-- ---------------------------------------------------------------
INSERT INTO users (email, full_name, profile) VALUES
    ('alice@example.com', 'Alice Anderson', '{"plan":"pro"}'),
    ('bob@example.com',   'Bob Baker',      '{"plan":"free"}')
ON CONFLICT (email) DO NOTHING;
