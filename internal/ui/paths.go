package ui

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/macrodigital283/lightxray/internal/db"
)

// Validation mirrors lightxray-applypaths exactly so we reject garbage in the
// handler before ever invoking sudo.
//
//	client path → 4-64 URL-safe chars, no slashes (config.go trims slashes)
//	ws path     → a single leading slash then 2-128 URL-safe chars (Hiddify
//	              uses one short shared token like /VXz0nhwTxdZGAh3J7dkqi7Z)
var (
	clientPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)
	wsPathRE     = regexp.MustCompile(`^/[A-Za-z0-9_/-]{2,128}$`)
)

// pathsView is what the Paths page renders.
type pathsView struct {
	ClientPath string // current LX_CLIENT_PROXY_PATH (no slashes)
	WSPath     string // current live WS path (/<ws_path_token> in shared mode)
	Output     string // captured stdout/stderr from the last apply
	Error      string
	Applied    bool
}

// currentPaths reads the live client + WS path. The WS path source of truth in
// the default "shared" mode is the DB ws_path_token, not the env default, so
// we read that and fall back to the env value.
func (u *UI) currentPaths(r *http.Request) pathsView {
	v := pathsView{ClientPath: u.cfg.ClientProxyPath, WSPath: u.cfg.VLESSWSPath}
	if tok, _ := u.store.GetSetting(r.Context(), db.SettingWSPathToken); tok != "" {
		if tok[0] != '/' {
			tok = "/" + tok
		}
		v.WSPath = tok
	}
	return v
}

// pathsPage — GET /ui/paths/
func (u *UI) pathsPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "paths_page", map[string]any{"V": u.currentPaths(r)})
}

// pathsApply — POST /ui/paths/
// Validates the new paths, then shells out to the privileged helper
// `sudo lightxray-applypaths <client> <ws>` (passing "-" for whichever path is
// unchanged). The helper rewrites config.env, regenerates nginx (+ xray when
// the WS path changed), reloads nginx, and restarts lightxrayd (+ xray)
// out-of-band. Because the admin path is untouched, this dashboard session
// survives the restart — just refresh after a couple of seconds.
func (u *UI) pathsApply(w http.ResponseWriter, r *http.Request) {
	cur := u.currentPaths(r)
	if err := r.ParseForm(); err != nil {
		u.renderPaths(w, pathsView{ClientPath: cur.ClientPath, WSPath: cur.WSPath, Error: "bad form"})
		return
	}

	newClient := strings.Trim(strings.TrimSpace(r.PostFormValue("client_path")), "/")
	newWS := strings.TrimSpace(r.PostFormValue("ws_path"))
	if newWS != "" && !strings.HasPrefix(newWS, "/") {
		newWS = "/" + newWS
	}

	if !clientPathRE.MatchString(newClient) {
		u.renderPaths(w, pathsView{ClientPath: cur.ClientPath, WSPath: cur.WSPath,
			Error: "Invalid client path — 4-64 chars, letters/digits/_/- only (no slashes)."})
		return
	}
	if !wsPathRE.MatchString(newWS) {
		u.renderPaths(w, pathsView{ClientPath: cur.ClientPath, WSPath: cur.WSPath,
			Error: "Invalid WS path — a leading / then 2-128 chars of letters/digits/_/-//."})
		return
	}
	if newClient == cur.ClientPath && newWS == cur.WSPath {
		u.renderPaths(w, pathsView{ClientPath: cur.ClientPath, WSPath: cur.WSPath,
			Error: "No change — both paths are already set to those values."})
		return
	}

	// Pass "-" for whichever path is unchanged so the helper skips its side
	// effects (e.g. don't restart xray for a client-path-only change).
	clientArg, wsArg := "-", "-"
	if newClient != cur.ClientPath {
		clientArg = newClient
	}
	if newWS != cur.WSPath {
		wsArg = newWS
	}

	// Background context that outlives this handler — the helper schedules the
	// lightxrayd restart for ~2s after it exits, well after we've responded.
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	slog.Info("paths: applying", "client", clientArg, "ws", wsArg)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/usr/local/bin/lightxray-applypaths", clientArg, wsArg)
	out, err := cmd.CombinedOutput()
	view := pathsView{ClientPath: newClient, WSPath: newWS, Output: string(out)}
	if err != nil {
		slog.Error("paths: apply failed", "err", err)
		view.Error = "Apply failed (exit " + err.Error() + "). See output below — nothing was changed if it failed validation."
		view.ClientPath, view.WSPath = cur.ClientPath, cur.WSPath
	} else {
		view.Applied = true
	}
	u.renderPaths(w, view)
}

func (u *UI) renderPaths(w http.ResponseWriter, v pathsView) {
	u.render(w, "paths_page", map[string]any{"V": v})
}
