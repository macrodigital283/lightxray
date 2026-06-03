// Package server wires every HTTP route. All admin routes are gated by
// the Hiddify-API-Key middleware in auth.go; customer sub routes are
// public-by-UUID (the UUID itself is the credential).
//
// Path layout — nginx strips the configured admin/client proxy-path
// prefixes before proxying, so lightxrayd only ever sees the clean
// paths below.
package server

import (
	"context"
	"net/http"
	"time"

	"github.com/eikhinephyo/lightxray/internal/config"
	"github.com/eikhinephyo/lightxray/internal/db"
	"github.com/eikhinephyo/lightxray/internal/xray"
)

// Deps bundles everything handlers need. Avoids globals; passed by value
// so handlers can capture in closures without race conditions.
type Deps struct {
	cfg     config.Config
	store   *db.Store
	xc      *xray.Client
	version string
	booted  time.Time
}

// New returns an http.Handler with every route registered.
func New(cfg config.Config, store *db.Store, xc *xray.Client, version string) http.Handler {
	d := Deps{cfg: cfg, store: store, xc: xc, version: version, booted: time.Now()}

	mux := http.NewServeMux()

	// ── admin (auth required) ─────────────────────────────────────────
	mux.Handle("GET /api/v2/admin/me/",                 d.requireAdmin(d.adminMe))
	mux.Handle("GET /api/v2/admin/server_status/",      d.requireAdmin(d.adminServerStatus))
	mux.Handle("GET /api/v2/admin/all-configs/",        d.requireAdmin(d.adminAllConfigs))
	mux.Handle("GET /api/v2/admin/user/",               d.requireAdmin(d.adminListUsers))
	mux.Handle("GET /api/v2/admin/user/{uuid}/",        d.requireAdmin(d.adminGetUser))
	mux.Handle("POST /api/v2/admin/user/",              d.requireAdmin(d.adminCreateUser))
	mux.Handle("PATCH /api/v2/admin/user/{uuid}/",      d.requireAdmin(d.adminPatchUser))
	mux.Handle("DELETE /api/v2/admin/user/{uuid}/",     d.requireAdmin(d.adminDeleteUser))

	// ── customer sub (UUID is the credential) ─────────────────────────
	mux.HandleFunc("GET /{uuid}/sub/",     d.customerSub)
	mux.HandleFunc("GET /{uuid}/sub64/",   d.customerSub) // alias — same body
	mux.HandleFunc("GET /{uuid}/auto/",    d.customerSub) // Hiddify's auto endpoint

	// ── operational ───────────────────────────────────────────────────
	mux.HandleFunc("GET /healthz", d.healthz)

	return logRequest(mux)
}

// healthz is a 200 with no body — for load balancers and `curl` smoke checks.
// Intentionally outside auth so the operator can poll it without a token.
func (d Deps) healthz(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}

// reqCtx returns a request-scoped context with a sane handler timeout.
// Used by every DB / xray call inside handlers so a hung dep can't hang
// a connection forever.
func reqCtx(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 15*time.Second)
}
