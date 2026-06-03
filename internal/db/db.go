// Package db is the only place that touches Postgres. Handlers go through
// the typed methods on *Store; nobody else holds a *pgxpool.Pool.
package db

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed schema.sql
var schemaSQL string

// Store is a thin pgxpool wrapper. Created via Open; closed via Close.
type Store struct {
	Pool *pgxpool.Pool
}

// Open dials the given DSN and pings to confirm credentials work.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	// Cap connections so a runaway query can't exhaust Postgres; lightxray
	// is single-process and never bursts above a few in-flight handlers.
	cfg.MaxConns = 10
	cfg.MinConns = 1

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{Pool: pool}, nil
}

// Close releases the underlying pool.
func (s *Store) Close() {
	if s != nil && s.Pool != nil {
		s.Pool.Close()
	}
}

// Migrate runs the embedded schema. Statements are CREATE … IF NOT EXISTS,
// so this is safe to re-run on every startup.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, schemaSQL); err != nil {
		return fmt.Errorf("schema: %w", err)
	}
	return nil
}
