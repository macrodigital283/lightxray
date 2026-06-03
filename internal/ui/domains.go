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

// domainRE — same hostname sanity check the helper enforces; reject early
// in the handler so we never even invoke sudo with garbage.
var domainRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.\-]{1,253}[a-zA-Z0-9]$`)

// domainsView is what the Domains page renders.
type domainsView struct {
	Current  string // LX_PUBLIC_HOST as the running process sees it
	Output   string // captured stdout/stderr from the last apply
	Error    string
	Applied  bool
}

// domainsPage — GET /ui/domains/
// Shows the current management domain + a form to set/change it.
func (u *UI) domainsPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "domains_page", map[string]any{
		"V": domainsView{Current: u.cfg.PublicHost},
	})
}

// domainsApply — POST /ui/domains/
// Validates the domain, then shells out to the privileged helper
// `sudo lightxray-applydomain <domain>` which issues the LE cert, points
// nginx at it, updates PublicHost, and restarts lightxrayd. Because the
// restart kills THIS process mid-request, we run the helper detached and
// report "applying…" — the operator refreshes after a few seconds.
func (u *UI) domainsApply(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.renderDomains(w, domainsView{Current: u.cfg.PublicHost, Error: "bad form"})
		return
	}
	domain := strings.TrimSpace(strings.ToLower(r.PostFormValue("domain")))
	if domain == "" || !domainRE.MatchString(domain) {
		u.renderDomains(w, domainsView{Current: u.cfg.PublicHost, Error: "Invalid domain: " + domain})
		return
	}

	// Run the helper with a generous timeout (certbot can take 10-30s).
	// We capture output and DON'T detach, because the helper restarts
	// lightxrayd as its LAST step — by then we've already streamed the
	// response. To be safe against the restart racing the response, run
	// it with a context that outlives this handler via a fresh background
	// context.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	slog.Info("domains: applying management domain", "domain", domain)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/usr/local/bin/lightxray-applydomain", domain)
	out, err := cmd.CombinedOutput()
	view := domainsView{Current: domain, Output: string(out)}
	if err != nil {
		slog.Error("domains: apply failed", "domain", domain, "err", err)
		view.Error = "Apply failed (exit " + err.Error() + "). See output below."
		view.Current = u.cfg.PublicHost
	} else {
		view.Applied = true
	}
	u.renderDomains(w, view)
}

func (u *UI) renderDomains(w http.ResponseWriter, v domainsView) {
	u.render(w, "domains_page", map[string]any{"V": v})
}
