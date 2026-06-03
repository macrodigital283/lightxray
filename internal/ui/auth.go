// Session handling for the lightxray admin dashboard.
//
// Auth model: single shared admin UUID (same secret as the JSON API).
// Login form accepts the UUID; on match we mint a signed cookie whose
// signature is HMAC-SHA256 with the admin UUID as the key. Rotating the
// admin UUID instantly invalidates every existing session — same
// property as v2sub-hiddify's dashboard.
package ui

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"net/http"
	"time"
)

const (
	sessionCookieName = "lxs"
	sessionMaxAge     = 12 * time.Hour
	signatureLen      = sha256.Size
)

// mintSession returns the cookie value to set on a successful login:
//
//	base64url( exp_unix(8 bytes BE) || HMAC-SHA256(adminUUID, exp_unix) )
//
// Compact, opaque to the client, tamper-evident on the server.
func (u *UI) mintSession() string {
	payload := make([]byte, 8)
	binary.BigEndian.PutUint64(payload, uint64(time.Now().Add(sessionMaxAge).Unix()))
	mac := hmac.New(sha256.New, []byte(u.cfg.AdminUUID))
	mac.Write(payload)
	full := append(payload, mac.Sum(nil)...)
	return base64.RawURLEncoding.EncodeToString(full)
}

// validateSession returns nil iff the cookie value is well-formed,
// signed by our admin UUID, and not expired.
func (u *UI) validateSession(value string) error {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return errors.New("malformed cookie")
	}
	if len(raw) != 8+signatureLen {
		return errors.New("wrong size")
	}
	payload, sig := raw[:8], raw[8:]
	mac := hmac.New(sha256.New, []byte(u.cfg.AdminUUID))
	mac.Write(payload)
	if subtle.ConstantTimeCompare(sig, mac.Sum(nil)) != 1 {
		return errors.New("bad signature")
	}
	if time.Now().After(time.Unix(int64(binary.BigEndian.Uint64(payload)), 0)) {
		return errors.New("expired")
	}
	return nil
}

// setSessionCookie writes the auth cookie on `w`. SameSite=Strict + Secure
// + HttpOnly defangs CSRF and XSS-cookie-theft both. Path is scoped to /ui
// so the cookie isn't sent on JSON-API calls under the same admin prefix.
func (u *UI) setSessionCookie(w http.ResponseWriter, value string) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    value,
		Path:     "/" + u.cfg.AdminProxyPath + "/ui",
		MaxAge:   int(sessionMaxAge.Seconds()),
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// clearSessionCookie wipes the cookie (logout).
func (u *UI) clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/" + u.cfg.AdminProxyPath + "/ui",
		MaxAge:   -1,
		HttpOnly: true,
		Secure:   true,
		SameSite: http.SameSiteStrictMode,
	})
}

// requireAuth wraps a handler so it 302s to the login page when there's
// no valid session cookie. Honour the dashboard's URL prefix when
// building the redirect target.
func (u *UI) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || u.validateSession(c.Value) != nil {
			http.Redirect(w, r, u.absURL("/ui/login"), http.StatusFound)
			return
		}
		next(w, r)
	}
}

// checkAdminUUID is the login-form's password check.
// constant-time to avoid leaking the secret via timing.
func (u *UI) checkAdminUUID(supplied string) bool {
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(u.cfg.AdminUUID)) == 1
}
