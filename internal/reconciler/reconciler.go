// Package reconciler periodically pulls per-user traffic from xray and
// reconciles it with our DB:
//
//   - bumps usage_bytes by the delta since the last poll
//   - bumps last_online if the delta is positive (user is actually online)
//   - persists the latest raw counter to stats_cursor so the next tick
//     can compute another delta — including correctly handling xray
//     restarts (counter goes monotonic→0; we treat the new read as
//     itself the delta in that case).
package reconciler

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/macrodigital283/lightxray/internal/config"
	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/xray"
)

type Reconciler struct {
	store *db.Store
	xc    *xray.Client
	cfg   config.Config
}

func New(store *db.Store, xc *xray.Client, cfg config.Config) *Reconciler {
	return &Reconciler{store: store, xc: xc, cfg: cfg}
}

// Run loops until ctx is cancelled. Each tick is bounded; one slow xray
// call won't queue up overlapping work.
func (r *Reconciler) Run(ctx context.Context) {
	slog.Info("reconciler started", "period", r.cfg.ReconcilePeriod)
	t := time.NewTicker(r.cfg.ReconcilePeriod)
	defer t.Stop()

	// fire immediately on startup so freshly-installed boxes don't wait
	// a full period to show traffic.
	r.tick(ctx)

	for {
		select {
		case <-ctx.Done():
			slog.Info("reconciler stopping")
			return
		case <-t.C:
			r.tick(ctx)
		}
	}
}

func (r *Reconciler) tick(ctx context.Context) {
	start := time.Now()
	tctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	traffic, err := r.xc.QueryAllUserTraffic(tctx)
	if err != nil {
		slog.Warn("reconciler: stats query failed", "err", err)
		return
	}

	var (
		updated   int
		online    int
		enforced  int
		totalDeltaUp   int64
		totalDeltaDown int64
	)
	now := time.Now().UTC()

	var ghosts int
	for uidStr, vals := range traffic {
		uid, err := uuid.Parse(uidStr)
		if err != nil {
			continue
		}
		// xray retains StatsService entries for deleted users (its stats
		// namespace doesn't auto-GC). Skip any UUID we no longer have a
		// row for — otherwise we count ghost traffic as `online`, churn
		// pointless UPDATEs, and trip the stats_cursor FK on insert.
		ok, err := r.store.UserExists(tctx, uid)
		if err != nil {
			slog.Warn("reconciler: user exists", "uuid", uid, "err", err)
			continue
		}
		if !ok {
			ghosts++
			continue
		}
		curUp, curDown := vals[0], vals[1]

		prev, err := r.store.GetCursor(tctx, uid)
		if err != nil {
			slog.Warn("reconciler: get cursor", "uuid", uid, "err", err)
			continue
		}

		// Compute deltas. If the counter dropped, xray restarted — the
		// new value IS the delta since restart (we lost whatever bytes
		// flowed between last persist and restart, but it's bounded by
		// the reconcile period).
		dUp := delta(prev.LastUplink, curUp)
		dDown := delta(prev.LastDownlink, curDown)
		totalDelta := dUp + dDown
		totalDeltaUp += dUp
		totalDeltaDown += dDown

		if totalDelta > 0 {
			if err := r.store.AddUsage(tctx, uid, totalDelta); err != nil {
				slog.Warn("reconciler: AddUsage", "uuid", uid, "err", err)
			}
			if err := r.store.TouchLastOnline(tctx, uid, now); err != nil {
				slog.Warn("reconciler: TouchLastOnline", "uuid", uid, "err", err)
			}
			online++
		}
		if err := r.store.UpsertCursor(tctx, db.StatsCursor{
			UUID:         uid,
			LastUplink:   curUp,
			LastDownlink: curDown,
			LastSeenAt:   now,
		}); err != nil {
			slog.Warn("reconciler: UpsertCursor", "uuid", uid, "err", err)
		}
		updated++
	}

	// Enforce quota + expiry. One atomic UPDATE flips enable=FALSE for every
	// enabled user who has hit their data cap or passed expiry; we then evict
	// each from xray so a disabled user can't open new connections. This runs
	// every tick regardless of traffic, so idle-but-expired users are caught
	// too — not only those who moved bytes this tick. (An already-established
	// connection drains until it closes; the eviction blocks RE-connection,
	// which is the Hiddify-equivalent behaviour.)
	if disabled, derr := r.store.DisableOverLimitOrExpired(tctx); derr != nil {
		slog.Warn("reconciler: enforce limits failed", "err", derr)
	} else {
		for _, uid := range disabled {
			if err := r.xc.RemoveUser(tctx, uid); err != nil {
				slog.Warn("reconciler: evict from xray failed", "uuid", uid, "err", err)
			}
			slog.Info("reconciler: disabled user (over data cap / expired)", "uuid", uid)
		}
		enforced = len(disabled)
	}

	slog.Info("reconciler tick",
		"users", updated,
		"online", online,
		"ghosts", ghosts,
		"enforced", enforced,
		"delta_up_bytes", totalDeltaUp,
		"delta_down_bytes", totalDeltaDown,
		"took", time.Since(start),
	)
}

// delta returns the increment from prev → cur. If cur < prev the counter
// reset; we treat cur as the delta itself (xray restarted, started from 0,
// has cur bytes since then).
func delta(prev, cur int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}
