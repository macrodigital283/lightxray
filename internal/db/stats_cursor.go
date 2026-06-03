package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// StatsCursor remembers the last raw (monotonic) xray counter values we
// saw for a user. The reconciler uses this to compute deltas across
// xray restarts: if a new read is lower than last, the counter reset
// and the new read is itself the delta.
type StatsCursor struct {
	UUID         uuid.UUID
	LastUplink   int64
	LastDownlink int64
	LastSeenAt   time.Time
}

// GetCursor returns the cursor row, or zeroes if absent.
func (s *Store) GetCursor(ctx context.Context, id uuid.UUID) (StatsCursor, error) {
	var c StatsCursor
	err := s.Pool.QueryRow(ctx,
		`SELECT uuid, last_uplink, last_downlink, last_seen_at FROM stats_cursor WHERE uuid = $1`,
		id,
	).Scan(&c.UUID, &c.LastUplink, &c.LastDownlink, &c.LastSeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		c.UUID = id
		return c, nil
	}
	return c, err
}

// UpsertCursor saves the latest counter snapshot.
func (s *Store) UpsertCursor(ctx context.Context, c StatsCursor) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO stats_cursor (uuid, last_uplink, last_downlink, last_seen_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (uuid) DO UPDATE
		  SET last_uplink   = EXCLUDED.last_uplink,
		      last_downlink = EXCLUDED.last_downlink,
		      last_seen_at  = EXCLUDED.last_seen_at`,
		c.UUID, c.LastUplink, c.LastDownlink, c.LastSeenAt,
	)
	return err
}
