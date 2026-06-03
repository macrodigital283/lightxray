// Package config loads runtime configuration from environment variables.
// All values are prefixed LX_ to avoid colliding with anything else on
// the box. See .env.example for the canonical list with descriptions.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	HTTPAddr    string // 127.0.0.1:8080 behind nginx
	DatabaseURL string // postgres DSN

	AdminUUID       string // Hiddify-API-Key value (the credential)
	AdminName       string // cosmetic, shown by /me/
	AdminProxyPath  string // nginx routes /<this>/api/v2/admin/... here
	ClientProxyPath string // nginx routes /<this>/<uuid>/sub/... here
	PublicHost      string // FQDN for subscription URLs

	XrayGRPCAddr   string // 127.0.0.1:10085
	XrayInboundTag string // matches xray inbound's `tag`

	ReconcilePeriod time.Duration // how often to poll xray stats

	VLESSPort   int    // 443 in front of Cloudflare
	VLESSSNI    string // TLS SNI; defaults to PublicHost
	VLESSWSPath string // WS path xray listens on
	VLESSFlow   string // optional
	VLESSAlpn   string // optional
	SubName     string // friendly name in vless:// fragment
}

// Load reads all LX_* env vars, applies defaults, and validates required
// fields. Returns a self-contained Config the rest of the binary can rely on.
func Load() (Config, error) {
	c := Config{
		HTTPAddr:        getenv("LX_HTTP_ADDR", "127.0.0.1:8080"),
		DatabaseURL:     os.Getenv("LX_DATABASE_URL"),
		AdminUUID:       os.Getenv("LX_ADMIN_UUID"),
		AdminName:       getenv("LX_ADMIN_NAME", "lightxray-admin"),
		AdminProxyPath:  os.Getenv("LX_ADMIN_PROXY_PATH"),
		ClientProxyPath: os.Getenv("LX_CLIENT_PROXY_PATH"),
		PublicHost:      os.Getenv("LX_PUBLIC_HOST"),
		XrayGRPCAddr:    getenv("LX_XRAY_GRPC_ADDR", "127.0.0.1:10085"),
		XrayInboundTag:  getenv("LX_XRAY_INBOUND_TAG", "vless-ws-in"),
		ReconcilePeriod: getdur("LX_RECONCILE_PERIOD", 60*time.Second),
		VLESSPort:       getint("LX_VLESS_PORT", 443),
		VLESSSNI:        os.Getenv("LX_VLESS_SNI"),
		VLESSWSPath:     getenv("LX_VLESS_WS_PATH", "/v2ray"),
		VLESSFlow:       os.Getenv("LX_VLESS_FLOW"),
		VLESSAlpn:       os.Getenv("LX_VLESS_ALPN"),
		SubName:         getenv("LX_SUB_NAME", "lightxray"),
	}
	if c.VLESSSNI == "" {
		c.VLESSSNI = c.PublicHost
	}

	// proxy paths are URL-positional secrets — trim any leading/trailing
	// slashes so the operator can paste either form.
	c.AdminProxyPath = trimSlashes(c.AdminProxyPath)
	c.ClientProxyPath = trimSlashes(c.ClientProxyPath)

	var missing []string
	if c.DatabaseURL == "" {
		missing = append(missing, "LX_DATABASE_URL")
	}
	if c.AdminUUID == "" {
		missing = append(missing, "LX_ADMIN_UUID")
	}
	if c.AdminProxyPath == "" {
		missing = append(missing, "LX_ADMIN_PROXY_PATH")
	}
	if c.ClientProxyPath == "" {
		missing = append(missing, "LX_CLIENT_PROXY_PATH")
	}
	if c.PublicHost == "" {
		missing = append(missing, "LX_PUBLIC_HOST")
	}
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %v", missing)
	}
	if c.ReconcilePeriod < 5*time.Second {
		return c, errors.New("LX_RECONCILE_PERIOD must be >= 5s")
	}
	return c, nil
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
func getint(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
func getdur(k string, def time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}
func trimSlashes(s string) string {
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
