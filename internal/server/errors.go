package server

import (
	"encoding/json"
	"net/http"
)

// Hiddify-style error envelope. The pool's HiddifyApiError just stores
// the body text, so any reasonable shape works; we match Django REST
// Framework's `detail` convention which Hiddify itself uses.
type errEnvelope struct {
	Detail string `json:"detail"`
	Status int    `json:"status"`
}

func writeJSON(w http.ResponseWriter, code int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(body)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, errEnvelope{Detail: msg, Status: code})
}
