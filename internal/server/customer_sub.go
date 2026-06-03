package server

import (
	"errors"
	"net/http"

	"github.com/macrodigital283/lightxray/internal/db"
	"github.com/macrodigital283/lightxray/internal/sub"
)

// customerSub — GET /sub/{uuid}, /sub64/{uuid}, /auto/{uuid}
//
// Public-by-UUID endpoint a customer's V2Ray client polls. Returns a
// base64-encoded vless:// bundle for that user. /auto/ technically
// negotiates by User-Agent in Hiddify; we always emit base64 because
// every modern V2Ray client accepts it.
//
// Disabled or unknown users get 404 (matches Hiddify — neither leaks
// whether a uuid exists vs. is just turned off).
func (d Deps) customerSub(w http.ResponseWriter, r *http.Request) {
	u, ok := d.subLookup(w, r)
	if !ok {
		return
	}
	body := sub.Build(d.cfg, u.UUID.String(), u.Name)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Subscription-Userinfo", subscriptionUserinfo(u))
	_, _ = w.Write([]byte(body))
}

// customerClashMeta — GET /clashmeta/{uuid}
//
// Returns a Clash Meta (mihomo) YAML profile. Clash-family clients
// (Clash Meta, Stash, ClashX Meta, mihomo party) consume this URL
// directly as a "subscription".
func (d Deps) customerClashMeta(w http.ResponseWriter, r *http.Request) {
	u, ok := d.subLookup(w, r)
	if !ok {
		return
	}
	body := sub.BuildClashMeta(d.cfg, u.UUID.String(), u.Name)
	w.Header().Set("Content-Type", "text/yaml; charset=utf-8")
	w.Header().Set("Profile-Update-Interval", "12")
	w.Header().Set("Subscription-Userinfo", subscriptionUserinfo(u))
	_, _ = w.Write([]byte(body))
}

// subLookup centralises the auth/404 logic shared by every customer
// subscription endpoint. Returns the user row + ok=true on success;
// writes the response itself on failure and returns ok=false.
func (d Deps) subLookup(w http.ResponseWriter, r *http.Request) (db.User, bool) {
	id, ok := pathUUID(r, "uuid", w)
	if !ok {
		return db.User{}, false
	}
	ctx, cancel := reqCtx(r)
	defer cancel()
	u, err := d.store.GetUser(ctx, id)
	if errors.Is(err, db.ErrNotFound) || (err == nil && !u.Enable) {
		http.NotFound(w, r)
		return db.User{}, false
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "subscription lookup failed")
		return db.User{}, false
	}
	return u, true
}

// subscriptionUserinfo formats the standard Subscription-Userinfo header
// V2Ray clients show as "X GB / Y GB used". Format:
//
//	upload=N; download=N; total=N; expire=UNIX
//
// We don't separate upload/download in our DB (we store the sum), so we
// report it all as download — clients combine them anyway.
func subscriptionUserinfo(u db.User) string {
	exp := int64(0)
	if u.PackageDays > 0 && u.StartDate != nil {
		exp = u.StartDate.AddDate(0, 0, u.PackageDays).Unix()
	}
	return formatUserinfo(0, u.UsageBytes, u.UsageLimitBytes, exp)
}

func formatUserinfo(up, down, total, expire int64) string {
	// hand-built to skip fmt.Sprintf overhead — called per sub request
	buf := make([]byte, 0, 96)
	buf = append(buf, "upload="...)
	buf = appendInt(buf, up)
	buf = append(buf, "; download="...)
	buf = appendInt(buf, down)
	buf = append(buf, "; total="...)
	buf = appendInt(buf, total)
	if expire > 0 {
		buf = append(buf, "; expire="...)
		buf = appendInt(buf, expire)
	}
	return string(buf)
}

func appendInt(buf []byte, n int64) []byte {
	if n == 0 {
		return append(buf, '0')
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var tmp [20]byte
	i := len(tmp)
	for n > 0 {
		i--
		tmp[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		buf = append(buf, '-')
	}
	return append(buf, tmp[i:]...)
}
