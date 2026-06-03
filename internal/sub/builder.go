// Package sub builds a customer subscription bundle — a base64-encoded
// blob of vless:// URIs, one per line. Matches what Hiddify's /sub/
// endpoint returns so V2Ray clients accept it unchanged.
package sub

import (
	"encoding/base64"
	"fmt"
	"net/url"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// Build returns the base64 bundle a customer's client will pull from
// `/<client_proxy_path>/<uuid>/sub/`. For v0.1 this is one VLESS+WS+TLS
// link; extend here when we add more inbounds.
func Build(cfg config.Config, userUUID, displayName string) string {
	link := vlessWSTLS(cfg, userUUID, displayName)
	bundle := link + "\n"
	return base64.StdEncoding.EncodeToString([]byte(bundle))
}

// vlessWSTLS assembles the vless:// URI per the de-facto V2Ray "share
// link" spec used by v2rayN, v2rayNG, Streisand, etc.:
//
//	vless://<uuid>@<host>:<port>?<params>#<fragment>
//
// Required query params for WS+TLS over Cloudflare:
//
//	type=ws, security=tls, host=<sni>, sni=<sni>, path=<ws-path>, encryption=none
func vlessWSTLS(cfg config.Config, userUUID, displayName string) string {
	q := url.Values{}
	q.Set("type", "ws")
	q.Set("security", "tls")
	q.Set("encryption", "none")
	q.Set("host", cfg.VLESSSNI)
	q.Set("sni", cfg.VLESSSNI)
	// The WS path must include the client proxy path + user uuid so that
	// nginx's regex location (which matches /<client>/<uuid><ws_path>)
	// routes it to xray. nginx rewrites it back to bare <ws_path> before
	// proxying so xray's WS inbound (configured with that exact path) accepts.
	q.Set("path", "/"+cfg.ClientProxyPath+"/"+userUUID+cfg.VLESSWSPath)
	if cfg.VLESSAlpn != "" {
		q.Set("alpn", cfg.VLESSAlpn)
	}
	if cfg.VLESSFlow != "" {
		q.Set("flow", cfg.VLESSFlow)
	}
	// fp=chrome blends our TLS fingerprint with mainstream browser traffic.
	// Most modern clients honour it; old ones ignore it harmlessly.
	q.Set("fp", "chrome")

	frag := fragment(cfg.SubName, displayName)
	return fmt.Sprintf(
		"vless://%s@%s:%d?%s#%s",
		userUUID,
		cfg.PublicHost,
		cfg.VLESSPort,
		q.Encode(),
		frag,
	)
}

// fragment is the human label clients show in their server list.
// Format: "<sub-name> | <display-name>" (trimmed if empty).
func fragment(subName, displayName string) string {
	parts := make([]string, 0, 2)
	if subName != "" {
		parts = append(parts, subName)
	}
	if displayName != "" {
		parts = append(parts, displayName)
	}
	return url.PathEscape(strings.Join(parts, " | "))
}
