package server

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/google/uuid"

	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/util"
)

// userResponse — JSON shape MUST match Hiddify's HiddifyUser. The pool
// uses fields name, comment, current_usage_GB, usage_limit_GB,
// package_days, start_date, enable, is_active, last_online,
// last_reset_time, mode, added_by_uuid, id — preserve every one.
type userResponse struct {
	UUID            string  `json:"uuid"`
	Name            string  `json:"name"`
	Comment         string  `json:"comment"`
	CurrentUsageGB  float64 `json:"current_usage_GB"`
	UsageLimitGB    float64 `json:"usage_limit_GB"`
	PackageDays     int     `json:"package_days"`
	StartDate       *string `json:"start_date"` // null until first connect
	Enable          bool    `json:"enable"`
	IsActive        bool    `json:"is_active"`
	LastOnline      string  `json:"last_online"`
	LastResetTime   string  `json:"last_reset_time"`
	Mode            string  `json:"mode"`
	AddedByUUID     string  `json:"added_by_uuid"`
	ID              int64   `json:"id"`
	Lang            string  `json:"lang"`
	TelegramID      *int64  `json:"telegram_id"` // always null for us
}

func toResponse(u db.User) userResponse {
	var startDate *string
	if u.StartDate != nil {
		s := util.HiddifyDate(*u.StartDate)
		startDate = &s
	}
	return userResponse{
		UUID:           u.UUID.String(),
		Name:           u.Name,
		Comment:        u.Comment,
		CurrentUsageGB: util.BytesToGB(u.UsageBytes),
		UsageLimitGB:   util.BytesToGB(u.UsageLimitBytes),
		PackageDays:    u.PackageDays,
		StartDate:      startDate,
		Enable:         u.Enable,
		// is_active mirrors enable for now; if/when we add quota/expiry
		// enforcement, derive this from enable AND not-over-limit AND
		// not-expired (Hiddify's semantics).
		IsActive:      u.Enable,
		LastOnline:    util.HiddifyTimePtr(u.LastOnline),
		LastResetTime: util.HiddifyTime(u.LastResetTime),
		Mode:          u.Mode,
		AddedByUUID:   u.AddedByUUID.String(),
		ID:            u.SerialID,
		Lang:          u.Lang,
		TelegramID:    nil,
	}
}

// adminListUsers — GET /api/v2/admin/user/
// Returns every user. Pool calls this every usage-sync tick.
func (d Deps) adminListUsers(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := reqCtx(r)
	defer cancel()
	users, err := d.store.ListUsers(ctx)
	if err != nil {
		slog.Error("listUsers", "err", err)
		writeError(w, http.StatusInternalServerError, "list users failed")
		return
	}
	out := make([]userResponse, 0, len(users))
	for _, u := range users {
		out = append(out, toResponse(u))
	}
	writeJSON(w, http.StatusOK, out)
}

// cursorDelta mirrors reconciler.delta: the increment prev→cur, or — if the
// counter dropped because xray restarted — cur treated as the delta itself.
func cursorDelta(prev, cur int64) int64 {
	if cur >= prev {
		return cur - prev
	}
	return cur
}

// liveUsageBytes returns dbUsage topped up with the bytes still sitting in
// xray's live counters that the reconciler hasn't persisted yet (everything
// since the last stats_cursor write, up to one reconcile period). This makes
// GET user authoritative-to-the-second instead of up-to-ReconcilePeriod
// stale — critical for the pool, which reads usage via GET user immediately
// before deleting a rotated key, so any unsynced tail would otherwise be
// lost forever on that delete (systematic under-count, worst for keys that
// rotate often).
//
// READ-ONLY by design: it does NOT AddUsage or advance the stats_cursor, so
// the reconciler stays the sole writer and there's no double-count race with
// a concurrent tick. On any xray/db hiccup it falls back to the plain DB
// value, so it's never worse than before.
func (d Deps) liveUsageBytes(ctx context.Context, id uuid.UUID, dbUsage int64) int64 {
	up, down, err := d.xc.PerUserTraffic(ctx, id)
	if err != nil {
		return dbUsage
	}
	cur, err := d.store.GetCursor(ctx, id)
	if err != nil {
		return dbUsage
	}
	pending := cursorDelta(cur.LastUplink, up) + cursorDelta(cur.LastDownlink, down)
	if pending < 0 {
		pending = 0
	}
	return dbUsage + pending
}

// adminGetUser — GET /api/v2/admin/user/{uuid}/
func (d Deps) adminGetUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "uuid", w)
	if !ok {
		return
	}
	ctx, cancel := reqCtx(r)
	defer cancel()
	u, err := d.store.GetUser(ctx, id)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("getUser", "err", err)
		writeError(w, http.StatusInternalServerError, "get user failed")
		return
	}
	// Top up with un-synced live xray bytes so the pool's settle (which reads
	// here right before deleting a rotated key) bills the true final usage,
	// not a value up to one reconcile period stale.
	u.UsageBytes = d.liveUsageBytes(ctx, id, u.UsageBytes)
	writeJSON(w, http.StatusOK, toResponse(u))
}

// createUserBody mirrors HiddifyUserCreate. `uuid` is OPTIONAL: omit it for
// a normal new user (the server generates one) or supply it to create the
// user with that exact UUID — this is what lets the pool re-create a user
// on a rebuilt server with the SAME key (disaster recovery). Matches real
// Hiddify, which also honors a supplied uuid.
type createUserBody struct {
	UUID         *string  `json:"uuid"`
	Name         string   `json:"name"`
	UsageLimitGB *float64 `json:"usage_limit_GB"`
	PackageDays  *int     `json:"package_days"`
	Enable       *bool    `json:"enable"`
	Comment      *string  `json:"comment"`
	Mode         *string  `json:"mode"`
	Lang         *string  `json:"lang"`
}

// adminCreateUser — POST /api/v2/admin/user/
// Inserts DB row, then registers the UUID with xray. If xray fails we
// roll back the DB row to avoid an "exists in DB, unknown to xray"
// inconsistency that would silently drop connections.
func (d Deps) adminCreateUser(w http.ResponseWriter, r *http.Request) {
	var body createUserBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.Name == "" {
		writeError(w, http.StatusBadRequest, "`name` is required")
		return
	}

	in := db.CreateUserInput{
		Name:        body.Name,
		Comment:     deref(body.Comment, ""),
		PackageDays: deref(body.PackageDays, 0),
		Mode:        deref(body.Mode, "no_reset"),
		Lang:        deref(body.Lang, "en"),
		Enable:      deref(body.Enable, true),
		AddedByUUID: mustUUID(d.cfg.AdminUUID),
	}
	if body.UsageLimitGB != nil {
		in.UsageLimitBytes = util.GBToBytes(*body.UsageLimitGB)
	}
	// Optional caller-supplied UUID — the pool sends this to re-create a
	// user with their original key (disaster recovery). Reject a malformed
	// value rather than silently minting a different UUID, which would hand
	// the customer a broken key.
	if body.UUID != nil && *body.UUID != "" {
		id, perr := uuid.Parse(*body.UUID)
		if perr != nil {
			writeError(w, http.StatusBadRequest, "invalid `uuid`: "+perr.Error())
			return
		}
		in.UUID = &id
	}

	ctx, cancel := reqCtx(r)
	defer cancel()

	u, created, err := d.store.CreateUser(ctx, in)
	if err != nil {
		slog.Error("createUser db", "err", err)
		writeError(w, http.StatusInternalServerError, "create user failed")
		return
	}

	// Register with xray. AddUser is idempotent, so re-pushing a user xray
	// already knows is a no-op. Only roll the DB row back if WE just created
	// it — never delete a pre-existing user because xray hiccuped. Disabled
	// users (enable=false) skip AddUser; they shouldn't be able to connect.
	if in.Enable {
		if err := d.xc.AddUser(ctx, u.UUID); err != nil {
			slog.Error("createUser xray AddUser failed", "uuid", u.UUID, "created", created, "err", err)
			if created {
				if delErr := d.store.DeleteUser(ctx, u.UUID); delErr != nil {
					slog.Error("createUser rollback delete failed", "uuid", u.UUID, "err", delErr)
				}
				writeError(w, http.StatusBadGateway, "xray AddUser failed: "+err.Error())
				return
			}
		}
	}

	writeJSON(w, http.StatusOK, toResponse(u))
}

// patchUserBody — fields PATCH may update. All optional.
type patchUserBody struct {
	Name         *string  `json:"name"`
	Comment      *string  `json:"comment"`
	UsageLimitGB *float64 `json:"usage_limit_GB"`
	PackageDays  *int     `json:"package_days"`
	Enable       *bool    `json:"enable"`
	Mode         *string  `json:"mode"`
	Lang         *string  `json:"lang"`
}

// adminPatchUser — PATCH /api/v2/admin/user/{uuid}/
func (d Deps) adminPatchUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "uuid", w)
	if !ok {
		return
	}
	var body patchUserBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	patch := db.PatchUserInput{
		Name:        body.Name,
		Comment:     body.Comment,
		PackageDays: body.PackageDays,
		Enable:      body.Enable,
		Mode:        body.Mode,
		Lang:        body.Lang,
	}
	if body.UsageLimitGB != nil {
		v := util.GBToBytes(*body.UsageLimitGB)
		patch.UsageLimitBytes = &v
	}

	ctx, cancel := reqCtx(r)
	defer cancel()

	u, err := d.store.PatchUser(ctx, id, patch)
	if errors.Is(err, db.ErrNotFound) {
		writeError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("patchUser db", "err", err)
		writeError(w, http.StatusInternalServerError, "patch user failed")
		return
	}

	// Reflect enable changes in xray. Idempotent so safe to call always.
	if body.Enable != nil {
		var xerr error
		if *body.Enable {
			xerr = d.xc.AddUser(ctx, id)
		} else {
			xerr = d.xc.RemoveUser(ctx, id)
		}
		if xerr != nil {
			// DB already updated; best-effort log and return — the
			// reconciler / hydrate will eventually converge.
			slog.Warn("patchUser xray sync failed", "uuid", id, "enable", *body.Enable, "err", xerr)
		}
	}

	writeJSON(w, http.StatusOK, toResponse(u))
}

// adminDeleteUser — DELETE /api/v2/admin/user/{uuid}/
// Removes from xray first (so no new bytes flow) then from DB. If xray
// RemoveUser fails we still proceed with DB delete — the pool's
// cleanup tooling handles orphans on the xray side via re-hydrate /
// out-of-band sweeps.
func (d Deps) adminDeleteUser(w http.ResponseWriter, r *http.Request) {
	id, ok := pathUUID(r, "uuid", w)
	if !ok {
		return
	}
	ctx, cancel := reqCtx(r)
	defer cancel()

	if err := d.xc.RemoveUser(ctx, id); err != nil {
		slog.Warn("deleteUser xray RemoveUser failed (proceeding)", "uuid", id, "err", err)
	}
	if err := d.store.DeleteUser(ctx, id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		slog.Error("deleteUser db", "err", err)
		writeError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// pathUUID extracts and parses {uuid} from the route. On failure writes
// the response itself and returns ok=false — caller just returns.
func pathUUID(r *http.Request, name string, w http.ResponseWriter) (uuid.UUID, bool) {
	raw := r.PathValue(name)
	id, err := uuid.Parse(raw)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid uuid: "+raw)
		return uuid.Nil, false
	}
	return id, true
}

func mustUUID(s string) uuid.UUID {
	id, err := uuid.Parse(s)
	if err != nil {
		// validated at config-load time; should never happen at runtime.
		return uuid.Nil
	}
	return id
}

func deref[T any](p *T, def T) T {
	if p == nil {
		return def
	}
	return *p
}
