package ui

import (
	"encoding/base64"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/sub"
	"github.com/macrodigital283/lightxray/internal/sysstat"
	"github.com/macrodigital283/lightxray/internal/util"
)

// loginForm renders the login page. Already-authenticated visitors get
// punted straight to the user list.
func (u *UI) loginForm(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil && u.validateSession(c.Value) == nil {
		http.Redirect(w, r, u.absURL("/ui/users/"), http.StatusFound)
		return
	}
	u.render(w, "login", nil)
}

// loginSubmit validates the supplied admin UUID and on match sets the
// session cookie. Wrong UUID re-renders the form with a flash.
func (u *UI) loginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.render(w, "login", map[string]any{"Error": "bad form"})
		return
	}
	if !u.checkAdminUUID(r.PostFormValue("admin_uuid")) {
		u.render(w, "login", map[string]any{"Error": "Invalid admin UUID."})
		return
	}
	u.setSessionCookie(w, u.mintSession())
	http.Redirect(w, r, u.absURL("/ui/users/"), http.StatusFound)
}

// logout clears the cookie and bounces back to the login page.
func (u *UI) logout(w http.ResponseWriter, r *http.Request) {
	u.clearSessionCookie(w)
	http.Redirect(w, r, u.absURL("/ui/login"), http.StatusFound)
}

// usersList — the main dashboard. Shows the full user table plus a
// total row at the bottom so the operator can eyeball the user count
// without counting rows.
// onlineWindow is how recently a user must have transferred bytes to count
// as "online" — matches the user table's "online" pill (see funcMap "ago").
const onlineWindow = 2 * time.Minute

func (u *UI) usersList(w http.ResponseWriter, r *http.Request) {
	users, err := u.store.ListUsers(r.Context())
	if err != nil {
		slog.Error("ui users list", "err", err)
		u.renderError(w, http.StatusInternalServerError, "list users failed")
		return
	}
	online, err := u.store.CountOnlineUsers(r.Context(), time.Now().Add(-onlineWindow))
	if err != nil {
		slog.Warn("ui users online count", "err", err)
	}
	u.render(w, "users_list", map[string]any{
		"Users":  users,
		"Count":  len(users),
		"Online": online,
		"Stats":  sysstat.Sample("/"),
	})
}

// usersNewForm — the create-user form.
func (u *UI) usersNewForm(w http.ResponseWriter, r *http.Request) {
	u.render(w, "users_new", nil)
}

// usersNewSubmit inserts the user + registers it with xray. On any
// failure we re-render the form with the typed-in values intact so the
// operator doesn't have to redo everything.
func (u *UI) usersNewSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.render(w, "users_new", map[string]any{"Error": "bad form"})
		return
	}
	name := r.PostFormValue("name")
	if name == "" {
		u.render(w, "users_new", map[string]any{
			"Error":  "Name is required.",
			"Values": r.PostForm,
		})
		return
	}
	limitGB, _ := strconv.ParseFloat(r.PostFormValue("usage_limit_GB"), 64)
	pkgDays, _ := strconv.Atoi(r.PostFormValue("package_days"))
	comment := r.PostFormValue("comment")
	adminID, _ := uuid.Parse(u.cfg.AdminUUID)

	row, _, err := u.store.CreateUser(r.Context(), db.CreateUserInput{
		Name:            name,
		Comment:         comment,
		UsageLimitBytes: util.GBToBytes(limitGB),
		PackageDays:     pkgDays,
		Mode:            "no_reset",
		Lang:            "en",
		Enable:          true,
		AddedByUUID:     adminID,
	})
	if err != nil {
		slog.Error("ui create user", "err", err)
		u.render(w, "users_new", map[string]any{
			"Error":  "Create failed: " + err.Error(),
			"Values": r.PostForm,
		})
		return
	}
	if err := u.xc.AddUser(r.Context(), row.UUID); err != nil {
		// xray didn't accept the user — roll the DB row back so we don't
		// end up with a phantom that can't connect.
		slog.Error("ui create xray AddUser failed — rolling back", "uuid", row.UUID, "err", err)
		_ = u.store.DeleteUser(r.Context(), row.UUID)
		u.render(w, "users_new", map[string]any{
			"Error":  "xray AddUser failed (rolled back): " + err.Error(),
			"Values": r.PostForm,
		})
		return
	}
	http.Redirect(w, r, u.absURL("/ui/users/"+row.UUID.String()+"/"), http.StatusFound)
}

// usersDetail — single-user page. Shows three Hiddify-style subscription
// URLs so operators can hand the right one to each client family:
//
//	/auto/       → V2Ray (v2rayN, v2rayNG, Streisand, …)
//	/sub/        → base64 fallback for older V2Ray apps
//	/clashmeta/  → Clash Meta / Stash / mihomo (YAML profile)
//
// URL shape matches Hiddify exactly — ?asn=unknown query param + #<name>
// fragment — so any client UI that already expects that shape labels the
// connection identically.
func (u *UI) usersDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		u.renderError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	user, err := u.store.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		u.renderError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("ui get user", "err", err)
		u.renderError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	base := "https://" + u.cfg.PublicHost + "/" + u.cfg.ClientProxyPath +
		"/" + user.UUID.String()
	frag := "#" + url.PathEscape(user.Name)

	// Pull the current CDN hosts + Reality config so the decoded VLESS
	// preview reflects what subscribers will actually receive. Resolve
	// the effective WS path (per_user / shared mode) the same way the
	// customer-facing handlers do.
	hosts, _ := u.store.ListEnabledCDNHosts(r.Context())
	reality := u.fetchRealityConfig(r)
	cfgForUser := u.cfg
	mode, _ := u.store.GetSetting(r.Context(), db.SettingWSPathMode)
	if mode == "shared" {
		if tok, _ := u.store.GetSetting(r.Context(), db.SettingWSPathToken); tok != "" {
			if tok[0] != '/' {
				tok = "/" + tok
			}
			cfgForUser.VLESSWSPath = tok
		}
	} else {
		cfgForUser.VLESSWSPath = "/" + cfgForUser.ClientProxyPath + "/" + user.UUID.String() + u.cfg.VLESSWSPath
	}
	vlessLink := sub.BuildPlain(cfgForUser, hosts, reality, user.UUID.String(), user.Name)
	_ = base64.StdEncoding // kept for future use; encoding helper retained for legacy callers

	u.render(w, "users_detail", map[string]any{
		"User":         user,
		"URLAuto":      base + "/auto/?asn=unknown" + frag,
		"URLSub":       base + "/sub/?asn=unknown" + frag,
		"URLClashMeta": base + "/clashmeta/?asn=unknown" + frag,
		"VLESSLink":    vlessLink,
	})
}

// usersDelete — POST /ui/users/<uuid>/delete. Removes from xray, then
// from the DB. Best-effort on xray failure (the reconciler / hydrate
// converges eventually).
func (u *UI) usersDelete(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		u.renderError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	if err := u.xc.RemoveUser(r.Context(), id); err != nil {
		slog.Warn("ui delete: xray RemoveUser failed (proceeding)", "uuid", id, "err", err)
	}
	if err := u.store.DeleteUser(r.Context(), id); err != nil && !errors.Is(err, db.ErrNotFound) {
		slog.Error("ui delete db", "err", err)
		u.renderError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	http.Redirect(w, r, u.absURL("/ui/users/"), http.StatusFound)
}

// usersToggle — POST /ui/users/<uuid>/toggle. Flips `enable` and syncs
// xray (AddUser if newly enabled, RemoveUser if disabled).
func (u *UI) usersToggle(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		u.renderError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	current, err := u.store.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		u.renderError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		u.renderError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	want := !current.Enable
	if _, err := u.store.PatchUser(r.Context(), id, db.PatchUserInput{Enable: &want}); err != nil {
		slog.Error("ui toggle db", "err", err)
		u.renderError(w, http.StatusInternalServerError, "toggle failed")
		return
	}
	var xerr error
	if want {
		xerr = u.xc.AddUser(r.Context(), id)
	} else {
		xerr = u.xc.RemoveUser(r.Context(), id)
	}
	if xerr != nil {
		slog.Warn("ui toggle xray sync", "uuid", id, "want", want, "err", xerr)
	}
	http.Redirect(w, r, u.absURL("/ui/users/"+id.String()+"/"), http.StatusFound)
}

// currentExpiry renders a user's computed expiry as YYYY-MM-DD, or "" when
// the user is unlimited (package_days == 0) or hasn't started (start_date
// null) — neither of which has an absolute date.
func currentExpiry(user db.User) string {
	if user.PackageDays == 0 || user.StartDate == nil {
		return ""
	}
	return user.StartDate.AddDate(0, 0, user.PackageDays).Format("2006-01-02")
}

// editData is the template payload for the edit form. ExpiryValue pre-fills
// the date input, but only when the expiry is today-or-later — a past date
// would violate the input's min and block submission. The current expiry is
// still shown in help text regardless, via CurrentExpiry.
func editData(user db.User, errMsg string) map[string]any {
	today := time.Now().UTC().Format("2006-01-02")
	cur := currentExpiry(user)
	val := cur
	if cur != "" && cur < today {
		val = ""
	}
	return map[string]any{
		"User":          user,
		"CurrentExpiry": cur,
		"ExpiryValue":   val,
		"Today":         today,
		"Error":         errMsg,
	}
}

// usersEditForm — GET /ui/users/<uuid>/edit. Pre-fills the edit form.
func (u *UI) usersEditForm(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		u.renderError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	user, err := u.store.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		u.renderError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		slog.Error("ui edit form get", "err", err)
		u.renderError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	u.render(w, "users_edit", editData(user, ""))
}

// usersEditSubmit — POST /ui/users/<uuid>/edit. Applies a partial update
// (name, comment, usage limit, package days, enabled) and syncs xray when
// the enabled state flips.
func (u *UI) usersEditSubmit(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("uuid"))
	if err != nil {
		u.renderError(w, http.StatusBadRequest, "invalid uuid")
		return
	}
	current, err := u.store.GetUser(r.Context(), id)
	if errors.Is(err, db.ErrNotFound) {
		u.renderError(w, http.StatusNotFound, "user not found")
		return
	}
	if err != nil {
		u.renderError(w, http.StatusInternalServerError, "lookup failed")
		return
	}
	if err := r.ParseForm(); err != nil {
		u.render(w, "users_edit", editData(current, "bad form"))
		return
	}
	name := strings.TrimSpace(r.PostFormValue("name"))
	if name == "" {
		u.render(w, "users_edit", editData(current, "Name is required."))
		return
	}
	comment := r.PostFormValue("comment")
	limitGB, _ := strconv.ParseFloat(r.PostFormValue("usage_limit_GB"), 64)
	pkgDays, _ := strconv.Atoi(r.PostFormValue("package_days"))
	enable := r.PostFormValue("enable") == "on"
	limitBytes := util.GBToBytes(limitGB)

	patch := db.PatchUserInput{
		Name:            &name,
		Comment:         &comment,
		UsageLimitBytes: &limitBytes,
		Enable:          &enable,
	}

	// An explicit "Expires on" date overrides package days. Translate it
	// into the start_date + package_days model so the computed expiry lands
	// exactly on the chosen date: keep the existing start_date when set (no
	// drift) and derive package_days from it; only anchor start_date to
	// today when the user never started. A blank date falls back to the
	// package_days field.
	if expStr := strings.TrimSpace(r.PostFormValue("expires_at")); expStr != "" {
		exp, perr := time.Parse("2006-01-02", expStr)
		if perr != nil {
			u.render(w, "users_edit", editData(current, "Invalid expiry date."))
			return
		}
		anchor := current.StartDate
		if anchor == nil {
			today := time.Now().UTC().Truncate(24 * time.Hour)
			patch.StartDate = &today
			anchor = &today
		}
		days := int(exp.Sub(anchor.UTC().Truncate(24*time.Hour)).Hours() / 24)
		if days < 0 {
			days = 0
		}
		pkgDays = days
	}
	patch.PackageDays = &pkgDays

	if _, err := u.store.PatchUser(r.Context(), id, patch); err != nil {
		slog.Error("ui edit user", "err", err)
		u.render(w, "users_edit", editData(current, "Save failed: "+err.Error()))
		return
	}

	// Keep xray in lockstep when the enabled flag changed.
	if enable != current.Enable {
		var xerr error
		if enable {
			xerr = u.xc.AddUser(r.Context(), id)
		} else {
			xerr = u.xc.RemoveUser(r.Context(), id)
		}
		if xerr != nil {
			slog.Warn("ui edit xray sync", "uuid", id, "enable", enable, "err", xerr)
		}
	}
	http.Redirect(w, r, u.absURL("/ui/users/"+id.String()+"/"), http.StatusFound)
}

// usersDeleteBulk — POST /ui/users/delete-bulk. Deletes every checked user
// (form field `uuids`, repeated) from xray + the DB. Per-row failures are
// logged and skipped so one bad uuid never aborts the batch.
func (u *UI) usersDeleteBulk(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderError(w, http.StatusBadRequest, "bad form")
		return
	}
	ids := r.PostForm["uuids"]
	deleted := 0
	for _, s := range ids {
		id, err := uuid.Parse(s)
		if err != nil {
			continue
		}
		if err := u.xc.RemoveUser(r.Context(), id); err != nil {
			slog.Warn("ui bulk delete xray RemoveUser", "uuid", id, "err", err)
		}
		if err := u.store.DeleteUser(r.Context(), id); err != nil && !errors.Is(err, db.ErrNotFound) {
			slog.Error("ui bulk delete db", "uuid", id, "err", err)
			continue
		}
		deleted++
	}
	slog.Info("ui bulk delete", "requested", len(ids), "deleted", deleted)
	http.Redirect(w, r, u.absURL("/ui/users/"), http.StatusFound)
}
