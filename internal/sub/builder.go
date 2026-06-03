// Package sub builds the customer subscription bundle — a base64-encoded
// blob of vless:// URIs, one per CDN-fronted domain × enabled transport.
// Matches what Hiddify's /sub/ endpoint returns so V2Ray clients accept
// it unchanged.
package sub

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
	"github.com/macrodigital283/lightxray/internal/db"
)

// Build returns the base64 bundle a customer's V2Ray client will pull
// from `/<client_proxy_path>/<uuid>/sub/`. Emits one or two vless:// URLs
// per CDN host depending on the host's transport ("ws", "grpc", or
// "both"); empty `hosts` falls back to a single WS URL on PublicHost.
func Build(cfg config.Config, hosts []db.CDNHost, userUUID, displayName string) string {
	lines := buildLines(cfg, hosts, userUUID, displayName)
	bundle := strings.Join(lines, "\n") + "\n"
	return base64.StdEncoding.EncodeToString([]byte(bundle))
}

// BuildPlain returns the same content as Build but without the base64
// wrap. Used by the dashboard "Show decoded VLESS link" disclosure.
func BuildPlain(cfg config.Config, hosts []db.CDNHost, userUUID, displayName string) string {
	return strings.Join(buildLines(cfg, hosts, userUUID, displayName), "\n")
}

// buildLines walks hosts × transport-modes and produces a vless URL per
// combination. Operator-set transport modes:
//
//	ws    → vlessWSTLS only
//	grpc  → vlessGRPC only
//	both  → both, WS first (V2Ray clients prefer the first valid server)
func buildLines(cfg config.Config, hosts []db.CDNHost, userUUID, displayName string) []string {
	if len(hosts) == 0 {
		// no CDN configured — single WS URL pointing at the management host
		return []string{vlessWSTLS(cfg, cfg.PublicHost, userUUID, displayName)}
	}
	var lines []string
	for _, h := range hosts {
		if h.Transport == db.TransportWS || h.Transport == db.TransportBoth {
			lines = append(lines, vlessWSTLS(cfg, h.Hostname, userUUID, displayName))
		}
		if h.Transport == db.TransportGRPC || h.Transport == db.TransportBoth {
			lines = append(lines, vlessGRPC(cfg, h.Hostname, userUUID, displayName))
		}
	}
	if len(lines) == 0 {
		// every CDN row toggled off both transports somehow — fall back.
		lines = []string{vlessWSTLS(cfg, cfg.PublicHost, userUUID, displayName)}
	}
	return lines
}

// vlessWSTLS assembles a single vless:// URI per the de-facto V2Ray
// "share link" spec used by v2rayN, v2rayNG, Streisand, etc.
//
//	vless://<uuid>@<host>:<port>?<params>#<fragment>
//
// `host` here is the EDGE hostname (CDN or PublicHost). It is used as
// both the TCP connect target AND the TLS SNI AND the WS Host header,
// because for a CDN-fronted setup the edge name is the only thing the
// CDN's TLS terminator sees.
func vlessWSTLS(cfg config.Config, host, userUUID, displayName string) string {
	wsPath := "/" + cfg.ClientProxyPath + "/" + userUUID + cfg.VLESSWSPath

	q := url.Values{}
	q.Set("type", "ws")
	q.Set("security", "tls")
	q.Set("encryption", "none")
	q.Set("host", host)
	q.Set("sni", host)
	q.Set("path", wsPath)
	if cfg.VLESSAlpn != "" {
		q.Set("alpn", cfg.VLESSAlpn)
	}
	if cfg.VLESSFlow != "" {
		q.Set("flow", cfg.VLESSFlow)
	}
	q.Set("fp", "chrome")

	return fmt.Sprintf(
		"vless://%s@%s:%d?%s#%s",
		userUUID,
		host,
		cfg.VLESSPort,
		q.Encode(),
		fragment(host, displayName),
	)
}

// fragment is the human label V2Ray clients show in their server list.
// Format: "<host> | <display-name>" so operators can tell which CDN
// each entry uses without inspecting the URL.
func fragment(host, displayName string) string {
	parts := make([]string, 0, 2)
	if host != "" {
		parts = append(parts, host)
	}
	if displayName != "" {
		parts = append(parts, displayName)
	}
	return url.PathEscape(strings.Join(parts, " | "))
}
