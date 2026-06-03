package server

import (
	"crypto/subtle"
	"log/slog"
	"net/http"
	"time"
)

// requireAdmin checks the `Hiddify-API-Key` header in constant time
// against the configured admin uuid and rejects with 401 on mismatch.
// Never logs the supplied key (it IS a credential).
func (d Deps) requireAdmin(next func(w http.ResponseWriter, r *http.Request)) http.Handler {
	expected := []byte(d.cfg.AdminUUID)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := []byte(r.Header.Get("Hiddify-API-Key"))
		if len(got) == 0 || subtle.ConstantTimeCompare(got, expected) != 1 {
			writeError(w, http.StatusUnauthorized, "invalid or missing Hiddify-API-Key")
			return
		}
		next(w, r)
	})
}

// logRequest emits a single JSON line per request — method, path, status,
// duration. Never logs Authorization-class headers.
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sr := &statusRecorder{ResponseWriter: w, status: 200}
		next.ServeHTTP(sr, r)
		slog.Info("req",
			"method", r.Method,
			"path", r.URL.Path,
			"status", sr.status,
			"bytes", sr.bytes,
			"took_ms", time.Since(start).Milliseconds(),
		)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}
func (s *statusRecorder) Write(b []byte) (int, error) {
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}
