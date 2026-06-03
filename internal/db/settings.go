package db

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// Settings keys used across the codebase. Centralised here so renames
// are a single edit rather than a grep.
const (
	SettingRealityEnabled = "reality_enabled"
	SettingRealityPort    = "reality_port"
	SettingRealityTarget  = "reality_target"
	SettingRealityPubKey  = "reality_pubkey"
	SettingRealityShortID = "reality_short_id"
)

// GetSetting returns the stored value or "" if absent (NOT an error —
// makes callers' default-value paths cleaner: `v, _ := store.GetSetting(...)`
// then check for empty).
func (s *Store) GetSetting(ctx context.Context, key string) (string, error) {
	var v string
	err := s.Pool.QueryRow(ctx,
		`SELECT value FROM settings WHERE key = $1`, key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetSetting upserts a single key/value pair.
func (s *Store) SetSetting(ctx context.Context, key, value string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO settings (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE
		  SET value = EXCLUDED.value, updated_at = NOW()`,
		key, value,
	)
	return err
}

// GetSettings batch fetch — one query, returns a map for easy lookup.
// Missing keys are simply absent from the map (no zero entries).
func (s *Store) GetSettings(ctx context.Context, keys ...string) (map[string]string, error) {
	out := make(map[string]string, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx,
		`SELECT key, value FROM settings WHERE key = ANY($1)`, keys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}
