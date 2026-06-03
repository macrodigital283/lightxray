package ui

import (
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/macrodigital283/lightxray/internal/db"
)

// hostnameRE — basic sanity check for a CDN hostname. We deliberately
// don't try to be exhaustive (IDNs, punycode, etc.); just block
// obviously-bogus inputs like spaces or slashes that would corrupt the
// emitted vless URLs. A FQDN per RFC 1035 fits easily.
var hostnameRE = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9.\-]{1,253}[a-zA-Z0-9]$`)

// cdnList — GET /ui/cdn/
// Shows every cdn_hosts row with add form + per-row toggle/delete.
func (u *UI) cdnList(w http.ResponseWriter, r *http.Request) {
	hosts, err := u.store.ListCDNHosts(r.Context())
	if err != nil {
		slog.Error("ui cdn list", "err", err)
		u.renderError(w, http.StatusInternalServerError, "list cdn hosts failed")
		return
	}
	u.render(w, "cdn_list", map[string]any{
		"Hosts": hosts,
		"Count": len(hosts),
	})
}

// cdnAdd — POST /ui/cdn/  (form submit)
// Validates hostname and inserts. ON CONFLICT in the DB layer means
// re-adding an existing hostname is a no-op rather than an error —
// which is what we want for the "I forgot if I added it" case.
func (u *UI) cdnAdd(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		u.cdnListWithError(w, r, "bad form")
		return
	}
	host := strings.TrimSpace(strings.ToLower(r.PostFormValue("hostname")))
	if host == "" {
		u.cdnListWithError(w, r, "Hostname is required.")
		return
	}
	if !hostnameRE.MatchString(host) {
		u.cdnListWithError(w, r, "Hostname looks invalid: "+host)
		return
	}
	if _, err := u.store.AddCDNHost(r.Context(), host); err != nil {
		slog.Error("ui cdn add", "host", host, "err", err)
		u.cdnListWithError(w, r, "Add failed: "+err.Error())
		return
	}
	http.Redirect(w, r, u.absURL("/ui/cdn/"), http.StatusFound)
}

// cdnToggle — POST /ui/cdn/{id}/toggle
func (u *UI) cdnToggle(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id", w)
	if !ok {
		return
	}
	if _, err := u.store.ToggleCDNHost(r.Context(), id); err != nil {
		if errors.Is(err, db.ErrNotFound) {
			u.renderError(w, http.StatusNotFound, "cdn host not found")
			return
		}
		slog.Error("ui cdn toggle", "id", id, "err", err)
		u.renderError(w, http.StatusInternalServerError, "toggle failed")
		return
	}
	http.Redirect(w, r, u.absURL("/ui/cdn/"), http.StatusFound)
}

// cdnDelete — POST /ui/cdn/{id}/delete
func (u *UI) cdnDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathInt64(r, "id", w)
	if !ok {
		return
	}
	if err := u.store.DeleteCDNHost(r.Context(), id); err != nil && !errors.Is(err, db.ErrNotFound) {
		slog.Error("ui cdn delete", "id", id, "err", err)
		u.renderError(w, http.StatusInternalServerError, "delete failed")
		return
	}
	http.Redirect(w, r, u.absURL("/ui/cdn/"), http.StatusFound)
}

// cdnListWithError re-renders the list page with a flash error message.
// Centralised so add-form failures display nicely without ditching the
// page state.
func (u *UI) cdnListWithError(w http.ResponseWriter, r *http.Request, msg string) {
	hosts, _ := u.store.ListCDNHosts(r.Context())
	u.render(w, "cdn_list", map[string]any{
		"Hosts": hosts,
		"Count": len(hosts),
		"Error": msg,
	})
}

// pathInt64 extracts a numeric path segment ({id}) — used by both
// cdnToggle and cdnDelete. Mirrors pathUUID's pattern (writes the
// error response itself, returns ok=false on failure).
func pathInt64(r *http.Request, name string, w http.ResponseWriter) (int64, bool) {
	raw := r.PathValue(name)
	n, err := strconv.ParseInt(raw, 10, 64)
	if err != nil {
		http.Error(w, "invalid id: "+raw, http.StatusBadRequest)
		return 0, false
	}
	return n, true
}
