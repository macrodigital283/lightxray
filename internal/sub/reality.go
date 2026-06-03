package sub

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// RealityConfig is the runtime view of the Reality settings the
// customer_sub handlers fetch from the DB and pass through to the
// builders. Held by value so the builders stay pure.
type RealityConfig struct {
	Enabled bool
	Host    string // origin server hostname or IP (typically cfg.PublicHost)
	Port    int    // default 8443
	Target  string // SNI to mimic, e.g. "www.microsoft.com"
	PubKey  string // x25519 public key (base64-url)
	ShortID string // hex, 0-16 chars; can be empty
}

// HasValue reports whether the Reality block is complete enough to emit
// a working URL. Missing pubkey or empty target means xray's Reality
// inbound isn't configured — skip emission entirely.
func (r RealityConfig) HasValue() bool {
	return r.Enabled && r.PubKey != "" && r.Target != "" && r.Host != "" && r.Port > 0
}

// vlessReality assembles the customer's Reality share-link:
//
//	vless://<uuid>@<host>:<port>?type=tcp&security=reality&pbk=<pubkey>
//	  &sid=<shortid>&sni=<target>&fp=chrome&flow=xtls-rprx-vision
//
// flow=xtls-rprx-vision is the canonical XTLS flow that Reality pairs
// with — gives the best perf vs vanilla VLESS-TCP and is the form every
// modern client (v2rayN, v2rayNG, Streisand, Hiddify, NekoBox) expects.
func vlessReality(cfg config.Config, r RealityConfig, userUUID, displayName string) string {
	q := url.Values{}
	q.Set("type", "tcp")
	q.Set("security", "reality")
	q.Set("encryption", "none")
	q.Set("pbk", r.PubKey)
	if r.ShortID != "" {
		q.Set("sid", r.ShortID)
	}
	q.Set("sni", r.Target)
	q.Set("fp", "chrome")
	q.Set("flow", "xtls-rprx-vision")
	q.Set("spx", "/")

	return fmt.Sprintf(
		"vless://%s@%s:%d?%s#%s",
		userUUID, r.Host, r.Port, q.Encode(),
		fragment(r.Host+" reality", displayName),
	)
}

// writeClashRealityProxy emits the Clash Meta YAML proxy block for the
// Reality endpoint. Clash Meta accepts Reality natively under
// `reality-opts` since v1.16.
func writeClashRealityProxy(b *strings.Builder, r RealityConfig, name, userUUID string) {
	pname := fmt.Sprintf("%s reality | %s", r.Host, name)
	fmt.Fprintf(b, "  - name: %q\n", pname)
	fmt.Fprintf(b, "    type: vless\n")
	fmt.Fprintf(b, "    server: %s\n", r.Host)
	fmt.Fprintf(b, "    port: %d\n", r.Port)
	fmt.Fprintf(b, "    uuid: %s\n", userUUID)
	fmt.Fprintf(b, "    udp: true\n")
	fmt.Fprintf(b, "    tls: true\n")
	fmt.Fprintf(b, "    flow: xtls-rprx-vision\n")
	fmt.Fprintf(b, "    servername: %s\n", r.Target)
	fmt.Fprintf(b, "    client-fingerprint: chrome\n")
	fmt.Fprintf(b, "    network: tcp\n")
	fmt.Fprintf(b, "    reality-opts:\n")
	fmt.Fprintf(b, "      public-key: %s\n", r.PubKey)
	if r.ShortID != "" {
		fmt.Fprintf(b, "      short-id: %s\n", r.ShortID)
	}
}

// realityProxyName mirrors the name used in writeClashRealityProxy so
// the proxy-groups list can reference it without recomputing the format.
func realityProxyName(host, name string) string {
	return fmt.Sprintf("%s reality | %s", host, name)
}
