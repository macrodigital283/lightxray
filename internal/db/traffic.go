package db

import (
	"context"
	"time"
)

// AddDailyTraffic folds one reconciler tick's server-wide byte deltas into
// the traffic_daily row for the UTC day of `at`, creating it on first write.
func (s *Store) AddDailyTraffic(ctx context.Context, at time.Time, up, down int64) error {
	if up <= 0 && down <= 0 {
		return nil
	}
	if up < 0 {
		up = 0
	}
	if down < 0 {
		down = 0
	}
	day := utcDay(at)
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO traffic_daily (day, up_bytes, down_bytes, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (day) DO UPDATE
		  SET up_bytes   = traffic_daily.up_bytes + EXCLUDED.up_bytes,
		      down_bytes = traffic_daily.down_bytes + EXCLUDED.down_bytes,
		      updated_at = NOW()`,
		day, up, down,
	)
	return err
}

// TrafficWindow is the server-wide traffic total over a trailing window of
// UTC days (today included).
type TrafficWindow struct {
	Days      int // window length requested, e.g. 30
	UpBytes   int64
	DownBytes int64
	FirstDay  *time.Time // earliest day with data inside the window; nil = none yet
	Covered   int        // days from FirstDay through today, capped at Days
}

// TotalBytes is up + down.
func (w TrafficWindow) TotalBytes() int64 { return w.UpBytes + w.DownBytes }

// Partial reports whether the node has less history than the window asks
// for (the table only starts when v24 first ran here), so a caller can say
// "since <date>" instead of implying a full 30 days.
func (w TrafficWindow) Partial() bool { return w.Covered < w.Days }

// TrafficLastDays sums traffic_daily over the last `days` UTC days, today
// included (days=30 → today and the 29 days before it).
func (s *Store) TrafficLastDays(ctx context.Context, days int) (TrafficWindow, error) {
	if days < 1 {
		days = 1
	}
	w := TrafficWindow{Days: days}
	today := utcDay(time.Now())
	since := today.AddDate(0, 0, -(days - 1))
	var first *time.Time
	err := s.Pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(up_bytes), 0)::BIGINT,
		       COALESCE(SUM(down_bytes), 0)::BIGINT,
		       MIN(day)
		FROM traffic_daily
		WHERE day >= $1`,
		since,
	).Scan(&w.UpBytes, &w.DownBytes, &first)
	if err != nil {
		return w, err
	}
	if first != nil {
		f := utcDay(*first)
		w.FirstDay = &f
		w.Covered = int(today.Sub(f).Hours()/24) + 1
		if w.Covered > days {
			w.Covered = days
		}
		if w.Covered < 1 {
			w.Covered = 1
		}
	}
	return w, nil
}

// utcDay truncates t to midnight UTC (a DATE-shaped time.Time).
func utcDay(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}
