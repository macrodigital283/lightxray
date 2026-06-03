// Package sub builds the customer subscription bundle — a base64-encoded
// blob of vless:// URIs, one per CDN-fronted domain. Matches what
// Hiddify's /sub/ endpoint returns so V2Ray clients accept it unchanged.
package sub

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// Build returns the base64 bundle a customer's V2Ray client will pull
// from `/<client_proxy_path>/<uuid>/sub/`. Emits one vless:// URL per
// host in `hosts`; if `hosts` is empty, falls back to PublicHost only
// (single-server / no-CDN mode).
//
// All URLs share UUID + WS path; only server/sni/Host differ. Clients
// (v2rayN, v2rayNG, Streisand) treat each as a separate server and
// the user picks the fastest. `hosts` comes from the cdn_hosts table
// at request time — the caller is responsible for the DB lookup so
// this stays a pure function.
func Build(cfg config.Config, hosts []string, userUUID, displayName string) string {
	if len(hosts) == 0 {
		hosts = []string{cfg.PublicHost}
	}
	lines := make([]string, 0, len(hosts))
	for _, h := range hosts {
		lines = append(lines, vlessWSTLS(cfg, h, userUUID, displayName))
	}
	bundle := strings.Join(lines, "\n") + "\n"
	return base64.StdEncoding.EncodeToString([]byte(bundle))
}

// BuildPlain is Build without the base64 wrapping — for the UI
// "Show decoded VLESS link" disclosure where each URL can be copied
// individually.
func BuildPlain(cfg config.Config, hosts []string, userUUID, displayName string) string {
	if len(hosts) == 0 {
		hosts = []string{cfg.PublicHost}
	}
	lines := make([]string, 0, len(hosts))
	for _, h := range hosts {
		lines = append(lines, vlessWSTLS(cfg, h, userUUID, displayName))
	}
	return strings.Join(lines, "\n")
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
