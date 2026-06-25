// Package config loads runtime configuration from environment variables.
// All values are prefixed LX_ to avoid colliding with anything else on
// the box. See .env.example for the canonical list with descriptions.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	XrayInboundTag string // comma-separated inbound tags AddUser/RemoveUser hit; default = ws+xhttp (gRPC + Reality off by default; xhttp stays inert until LX_VLESS_XHTTP_PATH + the inbound are provisioned)

	ReconcilePeriod time.Duration // how often to poll xray stats

	VLESSPort        int    // 443 in front of Cloudflare
	VLESSSNI         string // TLS SNI; defaults to PublicHost
	VLESSWSPath      string // WS path xray listens on
	VLESSXHTTPPath   string // XHTTP path xray listens on (CDN-frontable); empty = not configured
	VLESSFlow        string // optional
	VLESSAlpn        string // optional
	VLESSGRPCService string // gRPC serviceName (must match xray config)
	SubName          string // friendly name in vless:// fragment

	// CDNHosts is the list of CDN-fronted domains the subscription
	// bundle emits one VLESS URL per. Operators front lightxray behind
	// (e.g.) Cloudflare on multiple hostnames; the management API + sub
	// fetch stays on PublicHost (grey-cloud, no proxy) but the actual
	// WS data plane is reachable via these proxied hostnames. Each URL
	// in the bundle uses the same UUID + path; only server/sni/Host
	// changes. If empty, the bundle just contains PublicHost — the
	// no-CDN single-server fallback.
	CDNHosts []string
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
		// Comma-separated list — AddUser/RemoveUser fan out across all
		// listed inbounds so the user is reachable via every transport
		// xray serves. Add/remove tags here to match xray-config.json's
		// inbound `tag` fields.
		XrayInboundTag: getenv("LX_XRAY_INBOUND_TAG", "vless-ws-in,vless-xhttp-in"),
		ReconcilePeriod: getdur("LX_RECONCILE_PERIOD", 60*time.Second),
		VLESSPort:       getint("LX_VLESS_PORT", 443),
		VLESSSNI:        os.Getenv("LX_VLESS_SNI"),
		VLESSWSPath:     getenv("LX_VLESS_WS_PATH", "/v2ray"),
		VLESSXHTTPPath:  os.Getenv("LX_VLESS_XHTTP_PATH"),
		VLESSFlow: os.Getenv("LX_VLESS_FLOW"),
		// http/1.1 default — WS upgrade is HTTP/1.1-only in the
		// classic spec (RFC 6455). Advertising it in ALPN locks the
		// TLS handshake to h1 from the start, so the CF edge / nginx
		// doesn't pick h2 and force an awkward downgrade (or worse:
		// reject the upgrade entirely with backends that don't speak
		// RFC 8441 WS-over-h2). Override with LX_VLESS_ALPN= to clear,
		// or set to "h2,http/1.1" for non-WS transports.
		VLESSAlpn:        getenv("LX_VLESS_ALPN", "http/1.1"),
		VLESSGRPCService: getenv("LX_VLESS_GRPC_SERVICE", "GunService"),
		SubName:         getenv("LX_SUB_NAME", "lightxray"),
		CDNHosts:        splitCSV(os.Getenv("LX_CDN_HOSTS")),
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
	if len(missing) > 0 {
		return c, fmt.Errorf("missing required env: %v", missing)
	}
	// PublicHost is NOT required at startup — a bare install (no
	// management domain yet) leaves it empty until the operator sets it
	// from the dashboard's Domains page. While empty, sub/Reality URLs
	// emit an empty host (harmless — no customer keys are usable yet),
	// the dashboard is reached over the box's self-signed cert by IP.
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

// splitCSV trims + drops empties. Used for LX_CDN_HOSTS so the operator
// can paste "a.example.com, b.example.com," without worrying about
// trailing commas or spaces.
func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
