package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// CDNHost is one row in the cdn_hosts table — a CDN-fronted hostname
// that the subscription bundle emits one vless URL per.
type CDNHost struct {
	ID        int64
	Hostname  string
	Enabled   bool
	CreatedAt time.Time
}

// ListCDNHosts returns every row, ordered by id for stable display.
// Used by the dashboard.
func (s *Store) ListCDNHosts(ctx context.Context) ([]CDNHost, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, hostname, enabled, created_at FROM cdn_hosts ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDNHost
	for rows.Next() {
		var h CDNHost
		if err := rows.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListEnabledCDNHostnames returns just the hostnames where enabled=true.
// Called by the sub builders on every subscription fetch — keep this
// query cheap. Indexed on (enabled).
func (s *Store) ListEnabledCDNHostnames(ctx context.Context) ([]string, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT hostname FROM cdn_hosts WHERE enabled = TRUE ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// AddCDNHost inserts a hostname. Idempotent on conflict — returns the
// pre-existing row if hostname already present, which keeps the
// startup-seed path safe to re-run.
func (s *Store) AddCDNHost(ctx context.Context, hostname string) (CDNHost, error) {
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO cdn_hosts (hostname) VALUES ($1)
		ON CONFLICT (hostname) DO UPDATE SET hostname = EXCLUDED.hostname
		RETURNING id, hostname, enabled, created_at`,
		hostname,
	)
	var h CDNHost
	err := row.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.CreatedAt)
	return h, err
}

// DeleteCDNHost removes a row by id. NotFound is returned if id is unknown.
func (s *Store) DeleteCDNHost(ctx context.Context, id int64) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM cdn_hosts WHERE id = $1`, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ToggleCDNHost flips `enabled` and returns the post-toggle row.
func (s *Store) ToggleCDNHost(ctx context.Context, id int64) (CDNHost, error) {
	row := s.Pool.QueryRow(ctx, `
		UPDATE cdn_hosts SET enabled = NOT enabled WHERE id = $1
		RETURNING id, hostname, enabled, created_at`,
		id,
	)
	var h CDNHost
	err := row.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CDNHost{}, ErrNotFound
	}
	return h, err
}
