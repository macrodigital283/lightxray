package ui

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Validation mirrors lightxray-applyadmin. Admin path: a URL-safe token (no
// slashes). Admin UUID: a standard lowercase UUID — it MUST match the nginx
// login-by-URL regex ([a-f0-9-]{36}) and is the HMAC key for sessions + the
// Hiddify-API-Key, so anything else would break login.
var (
	adminPathRE = regexp.MustCompile(`^[A-Za-z0-9_-]{4,64}$`)
	adminUUIDRE = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

// adminView is what the Admin-access page renders.
type adminView struct {
	AdminPath   string // current admin proxy path
	AdminUUID   string // current admin UUID (the credential)
	NewLoginURL string // after a successful change: the one-click login URL
	Output      string // helper stdout/stderr
	Error       string
	Applied     bool
}

// adminPage — GET /ui/admin/
func (u *UI) adminPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "admin_page", map[string]any{
		"V": adminView{AdminPath: u.cfg.AdminProxyPath, AdminUUID: u.cfg.AdminUUID},
	})
}

// adminApply — POST /ui/admin/
// Validates the new admin path + UUID, then shells out to
// `sudo lightxray-applyadmin <path> <uuid>` ("-" for whichever is unchanged),
// which rewrites config.env, regenerates nginx (only if the path changed) and
// restarts lightxrayd. Because the admin UUID is the session HMAC key AND the
// cookie path is scoped to the admin prefix, applying EITHER logs the operator
// out — so on success we show the new values + the new login URL to re-enter.
func (u *UI) adminApply(w http.ResponseWriter, r *http.Request) {
	cur := adminView{AdminPath: u.cfg.AdminProxyPath, AdminUUID: u.cfg.AdminUUID}
	fail := func(msg string) {
		v := cur
		v.Error = msg
		u.renderAdmin(w, v)
	}
	if err := r.ParseForm(); err != nil {
		fail("bad form")
		return
	}

	newPath := strings.Trim(strings.TrimSpace(r.PostFormValue("admin_path")), "/")
	newUUID := strings.ToLower(strings.TrimSpace(r.PostFormValue("admin_uuid")))

	if !adminPathRE.MatchString(newPath) {
		fail("Invalid admin path — 4-64 chars, letters/digits/_/- only (no slashes).")
		return
	}
	if !adminUUIDRE.MatchString(newUUID) {
		fail("Invalid admin UUID — must be a standard UUID like 11111111-2222-4333-8444-555555555555.")
		return
	}
	if newPath == cur.AdminPath && newUUID == cur.AdminUUID {
		fail("No change — both values are already set to those.")
		return
	}

	pathArg, uuidArg := "-", "-"
	if newPath != cur.AdminPath {
		pathArg = newPath
	}
	if newUUID != cur.AdminUUID {
		uuidArg = newUUID
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	// NB: never log the UUID itself.
	slog.Info("admin: applying access change", "path_changed", pathArg != "-", "uuid_changed", uuidArg != "-")
	out, err := exec.CommandContext(ctx, "sudo", "-n",
		"/usr/local/bin/lightxray-applyadmin", pathArg, uuidArg).CombinedOutput()
	if err != nil {
		slog.Error("admin: apply failed", "err", err)
		v := cur
		v.Output = string(out)
		v.Error = "Apply failed (exit " + err.Error() + "). Nothing changed if it failed validation. See output below."
		u.renderAdmin(w, v)
		return
	}

	host := u.cfg.PublicHost
	if host == "" {
		host = r.Host
	}
	u.renderAdmin(w, adminView{
		AdminPath:   newPath,
		AdminUUID:   newUUID,
		NewLoginURL: "https://" + host + "/" + newPath + "/" + newUUID,
		Output:      string(out),
		Applied:     true,
	})
}

func (u *UI) renderAdmin(w http.ResponseWriter, v adminView) {
	u.render(w, "admin_page", map[string]any{"V": v})
}
