package ui

import (
	"context"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/macrodigital283/lightxray/internal/db"
)

// defaultWorkerConn is the safe baseline install.sh sets (v8). The distro
// default of 768 was far too low: Cloudflare opens a separate origin
// connection per WS from each edge PoP, so a node's connection count runs well
// above its user count and exhausts 768 quickly (this took k64 down). 16384 ×
// cores leaves ample headroom; the page lets you raise it further.
const defaultWorkerConn = 16384

// slotsPerWSConn — a single WS connection transits nginx TWICE (the :443 stream
// SNI router AND the :8443 http backend), and nginx counts every socket
// (client + upstream) against worker_connections: 2 in stream + 2 in http.
// NB: one customer = SEVERAL WS connections (one per Cloudflare edge PoP +
// per device), so the connection ceiling is well above this in "users".
const slotsPerWSConn = 4

// workerConnRE pulls the live worker_connections out of nginx.conf — the
// source of truth, so the page reflects reality (not just a DB record).
var workerConnRE = regexp.MustCompile(`(?m)^\s*worker_connections\s+(\d+);`)

// readNginxWorkerConn returns the worker_connections value from nginx.conf, or
// 0 if it can't be read/parsed.
func readNginxWorkerConn() int {
	b, err := os.ReadFile("/etc/nginx/nginx.conf")
	if err != nil {
		return 0
	}
	m := workerConnRE.FindSubmatch(b)
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(string(m[1]))
	return n
}

// perfView is what the Performance page renders.
type perfView struct {
	WorkerConn int    // current desired worker_connections
	Cores      int    // runtime.NumCPU() — worker_processes auto matches this
	EstUsers   int    // rough concurrent-WS-user ceiling
	IsDefault  bool   // WorkerConn == defaultWorkerConn ("off")
	Output     string // helper stdout/stderr from the last apply
	Error      string
	Applied    bool
}

// currentPerf reads the desired worker_connections from the DB (absent =
// stock default) and computes the rough capacity estimate.
func (u *UI) currentPerf(r *http.Request) perfView {
	// nginx.conf is the source of truth; fall back to the DB record / baseline.
	wc := readNginxWorkerConn()
	if wc <= 0 {
		wc = defaultWorkerConn
		if v, _ := u.store.GetSetting(r.Context(), db.SettingNginxWorkerConn); v != "" {
			if n, err := strconv.Atoi(v); err == nil && n > 0 {
				wc = n
			}
		}
	}
	cores := runtime.NumCPU()
	return perfView{
		WorkerConn: wc,
		Cores:      cores,
		EstUsers:   cores * wc / slotsPerWSConn,
		IsDefault:  wc == defaultWorkerConn,
	}
}

// perfPage — GET /ui/perf/
func (u *UI) perfPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "perf_page", map[string]any{"V": u.currentPerf(r)})
}

// perfApply — POST /ui/perf/
// Validates the requested worker_connections (or a reset to the 1024 baseline),
// shells out to `sudo lightxray-applytuning <n>` which edits nginx.conf and
// does a graceful reload, then persists the choice. No xray restart, no path
// change — keys and the pool are unaffected.
func (u *UI) perfApply(w http.ResponseWriter, r *http.Request) {
	cur := u.currentPerf(r)
	fail := func(msg string) {
		v := cur
		v.Error = msg
		u.renderPerf(w, v)
	}
	if err := r.ParseForm(); err != nil {
		fail("bad form")
		return
	}

	val := defaultWorkerConn // the "reset to default" button submits no value
	if r.PostFormValue("reset") == "" {
		n, err := strconv.Atoi(strings.TrimSpace(r.PostFormValue("worker_connections")))
		if err != nil || n < 1024 || n > 262144 {
			fail("worker_connections must be a whole number between 1024 and 262144.")
			return
		}
		val = n
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	slog.Info("perf: applying worker_connections", "value", val)
	out, err := exec.CommandContext(ctx, "sudo", "-n",
		"/usr/local/bin/lightxray-applytuning", strconv.Itoa(val)).CombinedOutput()
	if err != nil {
		slog.Error("perf: apply failed", "err", err)
		v := cur
		v.Output = string(out)
		v.Error = "Apply failed (exit " + err.Error() + "). nginx was left unchanged. See output below."
		u.renderPerf(w, v)
		return
	}

	if err := u.store.SetSetting(r.Context(), db.SettingNginxWorkerConn, strconv.Itoa(val)); err != nil {
		slog.Error("perf: save setting", "err", err)
	}
	v := u.currentPerf(r)
	v.Output = string(out)
	v.Applied = true
	u.renderPerf(w, v)
}

func (u *UI) renderPerf(w http.ResponseWriter, v perfView) {
	u.render(w, "perf_page", map[string]any{"V": v})
}
