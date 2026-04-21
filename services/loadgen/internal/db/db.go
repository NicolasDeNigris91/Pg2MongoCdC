// Package db provides a Postgres-backed Store for the loadgen service.
// Uses pgx native API for performance — the sidecar is on the hot path
// when k6 is hammering it at a few thousand RPS.
package db

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Store is the Postgres-backed implementation of the api.Store interface.
// A *pgxpool.Pool is safe for concurrent use by multiple goroutines, which
// matches the usage from the HTTP handlers at k6's target RPS.
type Store struct {
	Pool *pgxpool.Pool
}

// New opens a pgxpool against uri and pings to fail fast on bad configs.
// The returned Store must be Close()'d at process shutdown.
func New(ctx context.Context, uri string) (*Store, error) {
	pool, err := pgxpool.New(ctx, uri)
	if err != nil {
		return nil, fmt.Errorf("db.New: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("db.New: ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close releases the pool's connections. Safe to call multiple times.
func (s *Store) Close() {
	s.Pool.Close()
}

// InsertUser performs an ON CONFLICT upsert keyed on email. A repeat
// insert with the same email turns into an UPDATE (full_name + updated_at),
// which is the natural behaviour under k6's write-mix where a VU may
// replay the same email between restarts.
func (s *Store) InsertUser(ctx context.Context, email, fullName string, profile map[string]any) (int64, error) {
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return 0, fmt.Errorf("marshal profile: %w", err)
	}
	var id int64
	err = s.Pool.QueryRow(ctx,
		`INSERT INTO users (email, full_name, profile)
		 VALUES ($1, $2, $3::jsonb)
		 ON CONFLICT (email) DO UPDATE SET full_name = EXCLUDED.full_name, updated_at = now()
		 RETURNING id`,
		email, fullName, string(profileJSON),
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("insert: %w", err)
	}
	return id, nil
}

// UpdateRandomUser picks a row via TABLESAMPLE SYSTEM — orders-of-magnitude
// faster than `ORDER BY random()` on large tables. Falls back to `ORDER BY id
// LIMIT 1` when the sample returns nothing, which is common on small tables.
// Returns 0 when the table is empty (handler translates to 204).
func (s *Store) UpdateRandomUser(ctx context.Context, newFullName string) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`UPDATE users SET full_name = $1, updated_at = now()
		 WHERE id = (SELECT id FROM users TABLESAMPLE SYSTEM (5) LIMIT 1)
		 RETURNING id`,
		newFullName,
	).Scan(&id)
	if err != nil {
		// pgx returns ErrNoRows when nothing was picked; translate to 0-affected.
		if err.Error() == "no rows in result set" {
			return 0, nil
		}
		// Fallback: small tables may not produce a sample — pick any.
		err2 := s.Pool.QueryRow(ctx,
			`UPDATE users SET full_name = $1, updated_at = now()
			 WHERE id = (SELECT id FROM users ORDER BY id LIMIT 1)
			 RETURNING id`,
			newFullName,
		).Scan(&id)
		if err2 != nil {
			if err2.Error() == "no rows in result set" {
				return 0, nil
			}
			return 0, fmt.Errorf("update: %w", err2)
		}
	}
	return id, nil
}

// DeleteRandomUser removes the lowest-id row — deterministic ordering lets
// the chaos scripts reason about which rows are deleted first. Returns 0
// when the table is empty (handler translates to 204).
func (s *Store) DeleteRandomUser(ctx context.Context) (int64, error) {
	var id int64
	err := s.Pool.QueryRow(ctx,
		`DELETE FROM users
		 WHERE id = (SELECT id FROM users ORDER BY id LIMIT 1)
		 RETURNING id`,
	).Scan(&id)
	if err != nil {
		if err.Error() == "no rows in result set" {
			return 0, nil
		}
		return 0, fmt.Errorf("delete: %w", err)
	}
	return id, nil
}
