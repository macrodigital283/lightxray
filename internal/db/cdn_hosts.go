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
	Transport string // "ws" | "grpc" | "both"
	CreatedAt time.Time
}

// Transport mode constants. Values must match the CHECK constraint in
// schema.sql; if a third option is added, update both.
const (
	TransportWS   = "ws"
	TransportGRPC = "grpc"
	TransportBoth = "both"
)

// ListCDNHosts returns every row, ordered by id for stable display.
// Used by the dashboard.
func (s *Store) ListCDNHosts(ctx context.Context) ([]CDNHost, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, hostname, enabled, transport, created_at FROM cdn_hosts ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDNHost
	for rows.Next() {
		var h CDNHost
		if err := rows.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.Transport, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListEnabledCDNHosts is the structured variant the sub builders use —
// returns hostname + transport per row so the builder can emit the
// correct URL shape (WS, gRPC, or both) per CDN.
func (s *Store) ListEnabledCDNHosts(ctx context.Context) ([]CDNHost, error) {
	rows, err := s.Pool.Query(ctx,
		`SELECT id, hostname, enabled, transport, created_at FROM cdn_hosts WHERE enabled = TRUE ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CDNHost
	for rows.Next() {
		var h CDNHost
		if err := rows.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.Transport, &h.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, rows.Err()
}

// ListEnabledCDNHostnames returns just the hostnames where enabled=true.
// Retained for the install-seed path that doesn't care about transport.
func (s *Store) ListEnabledCDNHostnames(ctx context.Context) ([]string, error) {
	hosts, err := s.ListEnabledCDNHosts(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(hosts))
	for _, h := range hosts {
		out = append(out, h.Hostname)
	}
	return out, nil
}

// SetCDNHostTransport changes a row's transport mode. Validates the
// value against the same allowed set the DB CHECK enforces — surface
// a friendly error in the UI before the DB layer would reject it.
func (s *Store) SetCDNHostTransport(ctx context.Context, id int64, transport string) error {
	switch transport {
	case TransportWS, TransportGRPC, TransportBoth:
	default:
		return errors.New("invalid transport: " + transport)
	}
	tag, err := s.Pool.Exec(ctx,
		`UPDATE cdn_hosts SET transport = $2 WHERE id = $1`, id, transport)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// AddCDNHost inserts a hostname. Idempotent on conflict — returns the
// pre-existing row if hostname already present, which keeps the
// startup-seed path safe to re-run. New rows default to transport=ws.
func (s *Store) AddCDNHost(ctx context.Context, hostname string) (CDNHost, error) {
	row := s.Pool.QueryRow(ctx, `
		INSERT INTO cdn_hosts (hostname) VALUES ($1)
		ON CONFLICT (hostname) DO UPDATE SET hostname = EXCLUDED.hostname
		RETURNING id, hostname, enabled, transport, created_at`,
		hostname,
	)
	var h CDNHost
	err := row.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.Transport, &h.CreatedAt)
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
		RETURNING id, hostname, enabled, transport, created_at`,
		id,
	)
	var h CDNHost
	err := row.Scan(&h.ID, &h.Hostname, &h.Enabled, &h.Transport, &h.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return CDNHost{}, ErrNotFound
	}
	return h, err
}
