package ui

import (
	"log/slog"
	"net/http"
	"strconv"

	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/sub"
)

// realityView is the structured shape the Reality settings page renders.
// Pulled from the singleton key/value `settings` table.
type realityView struct {
	Enabled    bool
	Port       int
	Target     string
	PubKey     string
	ShortID    string
	PublicHost string
	NoKeypair  bool   // true when install.sh hasn't seeded a pubkey yet
	Error      string // flash message from a failed save
	Saved      bool
}

// fetchRealityConfig returns the Reality view used by both the dashboard
// page and the user-detail "decoded VLESS link" preview. Best-effort —
// a failed read renders the page in "not configured yet" mode.
func (u *UI) fetchRealityConfig(r *http.Request) sub.RealityConfig {
	m, err := u.store.GetSettings(r.Context(),
		db.SettingRealityEnabled, db.SettingRealityPort,
		db.SettingRealityTarget, db.SettingRealityPubKey,
		db.SettingRealityShortID,
	)
	if err != nil || m[db.SettingRealityEnabled] != "true" {
		return sub.RealityConfig{}
	}
	port := 8443
	if v := m[db.SettingRealityPort]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			port = n
		}
	}
	return sub.RealityConfig{
		Enabled: true,
		Host:    u.cfg.PublicHost,
		Port:    port,
		Target:  m[db.SettingRealityTarget],
		PubKey:  m[db.SettingRealityPubKey],
		ShortID: m[db.SettingRealityShortID],
	}
}

// realityPage — GET /ui/reality/
// Renders current Reality settings + a form to edit them.
func (u *UI) realityPage(w http.ResponseWriter, r *http.Request) {
	u.renderRealityPage(w, r, realityView{})
}

// realitySave — POST /ui/reality/
// Form submit. Toggles enable, persists port / target / short_id. The
// public key is install-time-generated and shown read-only; we never
// touch the private key (lives in xray's config file).
func (u *UI) realitySave(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderRealityPage(w, r, realityView{Error: "bad form"})
		return
	}
	enabled := r.PostFormValue("enabled") == "on"
	port := r.PostFormValue("port")
	target := r.PostFormValue("target")
	shortID := r.PostFormValue("short_id")

	if enabled {
		if target == "" {
			u.renderRealityPage(w, r, realityView{Error: "Target SNI is required when enabling Reality."})
			return
		}
		if pn, err := strconv.Atoi(port); err != nil || pn <= 0 || pn > 65535 {
			u.renderRealityPage(w, r, realityView{Error: "Port must be a number 1-65535."})
			return
		}
	}

	for k, v := range map[string]string{
		db.SettingRealityEnabled: boolStr(enabled),
		db.SettingRealityPort:    port,
		db.SettingRealityTarget:  target,
		db.SettingRealityShortID: shortID,
	} {
		if v == "" && k != db.SettingRealityShortID {
			continue
		}
		if err := u.store.SetSetting(r.Context(), k, v); err != nil {
			slog.Error("ui reality save", "key", k, "err", err)
			u.renderRealityPage(w, r, realityView{Error: "Save failed: " + err.Error()})
			return
		}
	}

	u.renderRealityPage(w, r, realityView{Saved: true})
}

func (u *UI) renderRealityPage(w http.ResponseWriter, r *http.Request, view realityView) {
	m, _ := u.store.GetSettings(r.Context(),
		db.SettingRealityEnabled, db.SettingRealityPort,
		db.SettingRealityTarget, db.SettingRealityPubKey,
		db.SettingRealityShortID,
	)
	view.Enabled = m[db.SettingRealityEnabled] == "true"
	view.Port = 8443
	if v := m[db.SettingRealityPort]; v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			view.Port = n
		}
	}
	view.Target = m[db.SettingRealityTarget]
	view.PubKey = m[db.SettingRealityPubKey]
	view.ShortID = m[db.SettingRealityShortID]
	view.PublicHost = u.cfg.PublicHost
	view.NoKeypair = view.PubKey == ""
	u.render(w, "reality_page", map[string]any{"V": view})
}

func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
