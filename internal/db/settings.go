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
	// WS path mode — Hiddify uses a single short shared random token in
	// the WS URL ("/VXz0nhwTxdZGAh3J7dkqi7Z"), not a multi-segment
	// per-user path. Mobile-carrier DPI seems sensitive to the request
	// line length / structure; the short shared form survives.
	//   per_user → /<client>/<uuid>/v2ray  (lightxray's original; verbose)
	//   shared   → /<random_token>         (Hiddify-compatible; default)
	SettingWSPathMode  = "ws_path_mode"  // "per_user" | "shared"
	SettingWSPathToken = "ws_path_token" // the random token used in `shared` mode
	// nginx worker_connections override set from the dashboard Performance
	// page. Absent/empty = the stock 1024 baseline ("off"); a larger number
	// raises the concurrent-connection ceiling on a large server.
	SettingNginxWorkerConn = "nginx_worker_connections"
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
