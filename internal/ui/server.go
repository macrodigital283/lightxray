// Package ui serves the lightxray admin dashboard — server-rendered HTML
// for managing users, mounted under the SAME admin-path-prefix as the
// JSON API. Routes live at `/<admin_proxy_path>/ui/...` (the prefix is
// stripped by nginx before requests reach lightxrayd, so the Go server
// only sees `/ui/...`).
//
// Templates + CSS are embedded into the binary via embed.FS so deploys
// are still a single static file — no separate template directory to
// install.
package ui

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"time"

	"github.com/macrodigital283/lightxray/internal/config"
	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/xray"
)

//go:embed templates/*.html
var templateFS embed.FS

//go:embed static/*
var staticFS embed.FS

// UI groups everything dashboard handlers need: config (for prefix +
// admin uuid), the user store, the xray client (for enable/disable),
// and the parsed template set.
type UI struct {
	cfg       config.Config
	store     *db.Store
	xc        *xray.Client
	templates *template.Template
}

// New parses all embedded templates and returns a UI ready to Register
// onto an http.ServeMux. Panics if a template fails to parse — that's
// always a build-time bug.
func New(cfg config.Config, store *db.Store, xc *xray.Client) *UI {
	tmpl := template.Must(
		template.New("").Funcs(funcMap).ParseFS(templateFS, "templates/*.html"),
	)
	return &UI{cfg: cfg, store: store, xc: xc, templates: tmpl}
}

// Register mounts dashboard routes onto an existing ServeMux. The caller
// is responsible for wrapping in logging / TLS / etc.
func (u *UI) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /ui/", u.requireAuth(u.usersList))
	mux.HandleFunc("GET /ui/login", u.loginForm)
	mux.HandleFunc("POST /ui/login", u.loginSubmit)
	mux.HandleFunc("POST /ui/logout", u.logout)
	mux.HandleFunc("GET /ui/users/", u.requireAuth(u.usersList))
	mux.HandleFunc("GET /ui/users/new", u.requireAuth(u.usersNewForm))
	mux.HandleFunc("POST /ui/users/new", u.requireAuth(u.usersNewSubmit))
	mux.HandleFunc("GET /ui/users/{uuid}/", u.requireAuth(u.usersDetail))
	mux.HandleFunc("POST /ui/users/{uuid}/delete", u.requireAuth(u.usersDelete))
	mux.HandleFunc("POST /ui/users/{uuid}/toggle", u.requireAuth(u.usersToggle))

	// CDN host pool — operator-managed via the dashboard.
	mux.HandleFunc("GET /ui/cdn/", u.requireAuth(u.cdnList))
	mux.HandleFunc("POST /ui/cdn/", u.requireAuth(u.cdnAdd))
	mux.HandleFunc("POST /ui/cdn/{id}/toggle", u.requireAuth(u.cdnToggle))
	mux.HandleFunc("POST /ui/cdn/{id}/transport", u.requireAuth(u.cdnSetTransport))
	mux.HandleFunc("POST /ui/cdn/{id}/delete", u.requireAuth(u.cdnDelete))
	mux.HandleFunc("GET /ui/backup/", u.requireAuth(u.backupPage))
	mux.HandleFunc("GET /ui/backup/export.json", u.requireAuth(u.backupExport))
	mux.HandleFunc("POST /ui/backup/restore", u.requireAuth(u.backupRestore))

	// Static — single CSS file. http.FileServer serves the whole subdir
	// so adding e.g. favicon.ico later just drops in.
	staticSub, err := fs.Sub(staticFS, "static")
	if err != nil {
		// embed.FS is rooted at "static/"; this only fails if the dir
		// doesn't exist, which is a build-time bug.
		panic(err)
	}
	mux.Handle("GET /ui/static/", http.StripPrefix("/ui/static/", http.FileServer(http.FS(staticSub))))
}

// absURL builds a browser-visible URL under the dashboard's admin
// prefix. Used for redirects + form actions + nav links inside templates
// so we don't have to repeat the prefix everywhere.
func (u *UI) absURL(path string) string {
	return "/" + u.cfg.AdminProxyPath + path
}

// render executes a template into the response, wrapping errors as 500.
// Every page uses the same base layout via `define`-blocks; we just
// invoke the per-page template name and template inheritance handles the
// rest.
func (u *UI) render(w http.ResponseWriter, name string, data map[string]any) {
	if data == nil {
		data = map[string]any{}
	}
	// Make these always available — every template references them.
	data["Prefix"] = "/" + u.cfg.AdminProxyPath
	data["Host"] = u.cfg.PublicHost

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := u.templates.ExecuteTemplate(w, name, data); err != nil {
		slog.Error("ui render", "template", name, "err", err)
		http.Error(w, "render failed", http.StatusInternalServerError)
	}
}

// renderError serves a fully-formed error page so 5xx and 4xx pages
// still look like the rest of the dashboard.
func (u *UI) renderError(w http.ResponseWriter, code int, msg string) {
	w.WriteHeader(code)
	u.render(w, "error_page", map[string]any{
		"Code":    code,
		"Message": msg,
	})
}

// funcMap holds tiny formatting helpers available to all templates.
var funcMap = template.FuncMap{
	"gb": func(b int64) string {
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	},
	// "ago" formats a *time.Time as a Last-Connection cell:
	//   nil               →  "—"        state=never
	//   < 2 min ago       →  "online"   state=online   (still on a live tick)
	//   < 60 min ago      →  "Nm ago"   state=recent
	//   < 24h ago         →  "Nh Nm ago" state=stale
	//   else              →  "Nd ago"   state=stale
	//
	// 2 minutes is the threshold because the reconciler refreshes
	// last_online every LX_RECONCILE_PERIOD (60s by default) whenever a
	// user has any byte delta — a 2-minute window covers one full tick
	// plus a bit of slack and won't flicker the "online" label on a
	// healthy connection.
	"ago": func(t *time.Time) onlineDisplay {
		if t == nil {
			return onlineDisplay{Text: "—", State: "never"}
		}
		d := time.Since(*t)
		if d < 0 {
			d = 0 // clock skew between DB and process — treat as just-now
		}
		if d < 2*time.Minute {
			return onlineDisplay{Text: "online", State: "online"}
		}
		if d < time.Hour {
			return onlineDisplay{Text: fmt.Sprintf("%dm ago", int(d.Minutes())), State: "recent"}
		}
		if d < 24*time.Hour {
			return onlineDisplay{
				Text:  fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60),
				State: "stale",
			}
		}
		return onlineDisplay{Text: fmt.Sprintf("%dd ago", int(d.Hours()/24)), State: "stale"}
	},
	// pct: percent of usage_bytes / usage_limit_bytes, clamped 0-100.
	"pct": func(used, limit int64) int {
		if limit <= 0 {
			return 0
		}
		p := int((used * 100) / limit)
		if p < 0 {
			p = 0
		}
		if p > 100 {
			p = 100
		}
		return p
	},
	// expiry: computes the expiry date of a user from start_date +
	// package_days and renders it Hiddify-style:
	//   unlimited          - package_days == 0
	//   "—"                - start_date == null (not yet started)
	//   "2026-07-15"       - normal future expiry
	//   "2026-06-15 (-3d)" - past expiry (already expired N days ago)
	//   "2026-07-15 (+12d)" - future expiry (N days remaining)
	// Returns a struct with parsed text + a "state" hint the template
	// uses to colour-code the cell (ok/warn/bad/dim).
	"expiry": func(u db.User) expiryDisplay {
		if u.PackageDays == 0 {
			return expiryDisplay{Text: "unlimited", State: "dim"}
		}
		if u.StartDate == nil {
			return expiryDisplay{Text: "—", State: "dim"}
		}
		exp := u.StartDate.AddDate(0, 0, u.PackageDays)
		days := int(time.Until(exp).Hours() / 24)
		date := exp.Format("2006-01-02")
		switch {
		case days < 0:
			return expiryDisplay{Text: fmt.Sprintf("%s (%dd ago)", date, -days), State: "bad"}
		case days <= 7:
			return expiryDisplay{Text: fmt.Sprintf("%s (in %dd)", date, days), State: "warn"}
		default:
			return expiryDisplay{Text: fmt.Sprintf("%s (in %dd)", date, days), State: "ok"}
		}
	},
}

// expiryDisplay wraps the rendered expiry string + a colour-class hint
// so templates can pick "ok" (green) / "warn" (yellow) / "bad" (red) /
// "dim" (grey for unlimited / not-yet-started).
type expiryDisplay struct {
	Text  string
	State string
}

// onlineDisplay is the structured "Last Connection" cell. State maps to
// a `conn-{online,recent,stale,never}` CSS class so the template can
// colour-code at a glance.
type onlineDisplay struct {
	Text  string
	State string
}

// ErrNotImplemented is just a sentinel for stub pages — kept so any
// helper that returns "not yet" can be matched in tests.
var ErrNotImplemented = errors.New("not implemented")
