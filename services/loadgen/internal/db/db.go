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

type Store struct {
	Pool *pgxpool.Pool
}

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

func (s *Store) Close() {
	s.Pool.Close()
}

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

// Pick a random row via TABLESAMPLE SYSTEM — orders-of-magnitude faster than
// ORDER BY random() on large tables. Falls back to nothing when empty.
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
