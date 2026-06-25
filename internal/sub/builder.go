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
// from `/<client_proxy_path>/<uuid>/sub/`. Emits one or more vless:// URLs
// per CDN host depending on the host's transport ("ws", "grpc", "both",
// or "xhttp"); empty `hosts` produces no CDN keys. If `reality.Enabled`
// is on it appends an extra direct-IP Reality URL to the bundle.
func Build(cfg config.Config, hosts []db.CDNHost, reality RealityConfig, userUUID, displayName string) string {
	lines := buildLines(cfg, hosts, reality, userUUID, displayName)
	bundle := strings.Join(lines, "\n") + "\n"
	return base64.StdEncoding.EncodeToString([]byte(bundle))
}

// BuildPlain returns the same content as Build but without the base64
// wrap. Used by the dashboard "Show decoded VLESS link" disclosure.
func BuildPlain(cfg config.Config, hosts []db.CDNHost, reality RealityConfig, userUUID, displayName string) string {
	return strings.Join(buildLines(cfg, hosts, reality, userUUID, displayName), "\n")
}

// buildLines walks hosts × transport-modes and produces a vless URL per
// combination, then appends one Reality URL if Reality is enabled.
// Operator-set transport modes (per CDN host):
//
//	ws    → vlessWSTLS only
//	grpc  → vlessGRPC only
//	both  → WS + gRPC, WS first (V2Ray clients prefer the first valid server)
//	xhttp → vlessXHTTP only (SplitHTTP; needs the XHTTP master switch on)
//
// IMPORTANT: PublicHost (the grey-cloud management/sub-fetch domain) is
// NEVER used for a WS key — that would expose the unfronted origin IP
// inside customers' VLESS URLs. When no CDN hosts are enabled the WS
// bundle is simply empty; only Reality (a deliberately-direct, camouflaged
// transport) and the configured CDN hosts ever produce keys. An empty
// return is intentional and valid — the customer's client just shows no
// WS servers until the operator adds a CDN host or enables Reality.
func buildLines(cfg config.Config, hosts []db.CDNHost, reality RealityConfig, userUUID, displayName string) []string {
	// Per-transport on/off is driven by the operator's Transports dashboard,
	// which rewrites LX_XRAY_INBOUND_TAG (cfg.XrayInboundTag) — the SAME set
	// that gates xray user-registration. So a transport toggled off here is
	// both un-served in the bundle AND has no users at the xray layer.
	wsOn := transportEnabled(cfg, "vless-ws-in")
	grpcOn := transportEnabled(cfg, "vless-grpc-in")
	xhttpOn := transportEnabled(cfg, "vless-xhttp-in") && cfg.VLESSXHTTPPath != ""

	var lines []string
	for _, h := range hosts {
		// "wsxhttp" emits BOTH a WS and an XHTTP key for the host, so iOS
		// clients (which can't sustain XHTTP under load) ride WS while
		// Android rides XHTTP — off the same domain. Each leg is still gated
		// by its own master switch, so on a box with XHTTP off, "wsxhttp"
		// degrades to WS-only.
		if wsOn && (h.Transport == db.TransportWS || h.Transport == db.TransportBoth || h.Transport == db.TransportWSXHTTP) {
			lines = append(lines, vlessWSTLS(cfg, h.Hostname, userUUID, displayName))
		}
		if grpcOn && (h.Transport == db.TransportGRPC || h.Transport == db.TransportBoth) {
			lines = append(lines, vlessGRPC(cfg, h.Hostname, userUUID, displayName))
		}
		// XHTTP is a first-class per-host pick: "xhttp" emits one VLESS+XHTTP
		// (SplitHTTP) key, "wsxhttp" emits it alongside the WS key. xhttpOn is
		// the Transports-page master switch — it also gates xray user-
		// registration on vless-xhttp-in and needs LX_VLESS_XHTTP_PATH set —
		// so a host is only served over XHTTP when that inbound is live.
		if xhttpOn && (h.Transport == db.TransportXHTTP || h.Transport == db.TransportWSXHTTP) {
			lines = append(lines, vlessXHTTP(cfg, h.Hostname, userUUID, displayName))
		}
	}
	if reality.HasValue() {
		lines = append(lines, vlessReality(cfg, reality, userUUID, displayName))
	}
	return lines
}

// transportEnabled reports whether the given xray inbound tag is in the
// operator's enabled set (cfg.XrayInboundTag, comma-separated). This is the
// single source of truth the Transports dashboard toggles rewrite.
func transportEnabled(cfg config.Config, tag string) bool {
	for _, t := range splitTrim(cfg.XrayInboundTag) {
		if t == tag {
			return true
		}
	}
	return false
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
//
// The WS path is taken from cfg.VLESSWSPath verbatim — operators in
// "shared" mode set it to a single short random token (Hiddify-style:
// "/VXz0nhwTxdZGAh3J7dkqi7Z"); operators in "per_user" mode build the
// per-user form ("/<client>/<uuid>/v2ray") and store the FULL path
// there before calling Build.
func vlessWSTLS(cfg config.Config, host, userUUID, displayName string) string {
	wsPath := cfg.VLESSWSPath

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
