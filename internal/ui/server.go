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
	u.render(w, "error.html", map[string]any{
		"Code":    code,
		"Message": msg,
	})
}

// funcMap holds tiny formatting helpers available to all templates.
var funcMap = template.FuncMap{
	"gb": func(b int64) string {
		return fmt.Sprintf("%.2f GB", float64(b)/(1024*1024*1024))
	},
	// "ago" formats a *time.Time as a relative duration like "3h 12m ago"
	// or "—" for nil (never-connected).
	"ago": func(t *time.Time) string {
		if t == nil {
			return "—"
		}
		d := time.Since(*t)
		if d < time.Minute {
			return "just now"
		}
		if d < time.Hour {
			return fmt.Sprintf("%dm ago", int(d.Minutes()))
		}
		if d < 24*time.Hour {
			return fmt.Sprintf("%dh %dm ago", int(d.Hours()), int(d.Minutes())%60)
		}
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
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
}

// ErrNotImplemented is just a sentinel for stub pages — kept so any
// helper that returns "not yet" can be matched in tests.
var ErrNotImplemented = errors.New("not implemented")
