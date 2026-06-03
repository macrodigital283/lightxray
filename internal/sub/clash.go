package sub

import (
	"fmt"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
	"github.com/macrodigital283/lightxray/internal/db"
)

// BuildClashMeta returns a Clash Meta (mihomo) YAML config that points
// at our VLESS inbound for the given user. Emits one proxy per
// (host × enabled-transport) pair and wraps them in a `url-test` AUTO
// proxy-group so the client picks the fastest edge.
//
// `hosts` is the structured cdn_hosts table; each row carries its own
// transport choice (ws / grpc / both). Empty hosts → no WS proxies (the
// grey-cloud PublicHost is never emitted as a key); the profile then
// carries only Reality if enabled, falling back to DIRECT otherwise.
func BuildClashMeta(cfg config.Config, hosts []db.CDNHost, reality RealityConfig, userUUID, displayName string) string {
	name := safeName(displayName, "lightxray")
	// cfg.VLESSWSPath is the FULL path the caller resolved (per_user or
	// shared mode are both materialised before Build runs).
	wsPath := cfg.VLESSWSPath

	var b strings.Builder
	fmt.Fprintf(&b, "# lightxray Clash Meta profile for %s\n", name)
	fmt.Fprintf(&b, "# Drop this into Clash Meta / mihomo / Stash as a profile.\n\n")

	// ── runtime knobs ────────────────────────────────────────────────
	fmt.Fprintf(&b, "mixed-port: 7890\n")
	fmt.Fprintf(&b, "allow-lan: false\n")
	fmt.Fprintf(&b, "mode: rule\n")
	fmt.Fprintf(&b, "log-level: warning\n")
	fmt.Fprintf(&b, "ipv6: false\n\n")

	// ── dns ─────────────────────────────────────────────────────────
	fmt.Fprintf(&b, "dns:\n")
	fmt.Fprintf(&b, "  enable: true\n")
	fmt.Fprintf(&b, "  listen: 0.0.0.0:53\n")
	fmt.Fprintf(&b, "  ipv6: false\n")
	fmt.Fprintf(&b, "  enhanced-mode: fake-ip\n")
	fmt.Fprintf(&b, "  fake-ip-range: 198.18.0.1/16\n")
	fmt.Fprintf(&b, "  default-nameserver:\n")
	fmt.Fprintf(&b, "    - 1.1.1.1\n")
	fmt.Fprintf(&b, "    - 8.8.8.8\n")
	fmt.Fprintf(&b, "  nameserver:\n")
	fmt.Fprintf(&b, "    - https://1.1.1.1/dns-query\n")
	fmt.Fprintf(&b, "    - https://8.8.8.8/dns-query\n")
	fmt.Fprintln(&b)

	// ── proxies: one per (host × transport) combo ───────────────────
	proxyNames := make([]string, 0, len(hosts)*2)
	fmt.Fprintf(&b, "proxies:\n")
	for _, h := range hosts {
		if h.Transport == db.TransportWS || h.Transport == db.TransportBoth {
			pname := fmt.Sprintf("%s | %s", h.Hostname, name)
			proxyNames = append(proxyNames, pname)
			writeClashWSProxy(&b, cfg, h.Hostname, pname, userUUID, wsPath)
		}
		if h.Transport == db.TransportGRPC || h.Transport == db.TransportBoth {
			pname := grpcProxyName(h.Hostname, name)
			proxyNames = append(proxyNames, pname)
			writeClashGRPCProxy(&b, cfg, h.Hostname, name, userUUID)
		}
	}
	if reality.HasValue() {
		pname := realityProxyName(reality.Host, name)
		proxyNames = append(proxyNames, pname)
		writeClashRealityProxy(&b, reality, name, userUUID)
	}
	fmt.Fprintln(&b)

	// ── proxy-groups ────────────────────────────────────────────────
	fmt.Fprintf(&b, "proxy-groups:\n")
	fmt.Fprintf(&b, "  - name: AUTO\n")
	fmt.Fprintf(&b, "    type: url-test\n")
	fmt.Fprintf(&b, "    url: http://www.gstatic.com/generate_204\n")
	fmt.Fprintf(&b, "    interval: 300\n")
	fmt.Fprintf(&b, "    tolerance: 50\n")
	fmt.Fprintf(&b, "    proxies:\n")
	for _, p := range proxyNames {
		fmt.Fprintf(&b, "      - %q\n", p)
	}
	fmt.Fprintf(&b, "  - name: PROXY\n")
	fmt.Fprintf(&b, "    type: select\n")
	fmt.Fprintf(&b, "    proxies:\n")
	// AUTO is only useful when there's at least one real proxy to test;
	// list it first when we have proxies, else PROXY just maps to DIRECT
	// so the profile stays valid with no servers configured.
	if len(proxyNames) > 0 {
		fmt.Fprintf(&b, "      - AUTO\n")
	}
	fmt.Fprintf(&b, "      - DIRECT\n")
	for _, p := range proxyNames {
		fmt.Fprintf(&b, "      - %q\n", p)
	}
	fmt.Fprintln(&b)

	// ── rules ───────────────────────────────────────────────────────
	fmt.Fprintf(&b, "rules:\n")
	fmt.Fprintf(&b, "  - DOMAIN-SUFFIX,local,DIRECT\n")
	fmt.Fprintf(&b, "  - IP-CIDR,127.0.0.0/8,DIRECT\n")
	fmt.Fprintf(&b, "  - IP-CIDR,10.0.0.0/8,DIRECT\n")
	fmt.Fprintf(&b, "  - IP-CIDR,172.16.0.0/12,DIRECT\n")
	fmt.Fprintf(&b, "  - IP-CIDR,192.168.0.0/16,DIRECT\n")
	fmt.Fprintf(&b, "  - GEOIP,LAN,DIRECT\n")
	fmt.Fprintf(&b, "  - MATCH,PROXY\n")
	return b.String()
}

// writeClashWSProxy emits one Clash-Meta YAML proxy block for a
// WS-transport CDN host. Mirrors what we used to inline directly in
// BuildClashMeta — extracted so the per-host transport-switch in
// BuildClashMeta stays readable.
func writeClashWSProxy(b *strings.Builder, cfg config.Config, host, pname, userUUID, wsPath string) {
	fmt.Fprintf(b, "  - name: %q\n", pname)
	fmt.Fprintf(b, "    type: vless\n")
	fmt.Fprintf(b, "    server: %s\n", host)
	fmt.Fprintf(b, "    port: %d\n", cfg.VLESSPort)
	fmt.Fprintf(b, "    uuid: %s\n", userUUID)
	fmt.Fprintf(b, "    udp: true\n")
	fmt.Fprintf(b, "    tls: true\n")
	fmt.Fprintf(b, "    servername: %s\n", host)
	fmt.Fprintf(b, "    skip-cert-verify: false\n")
	fmt.Fprintf(b, "    client-fingerprint: chrome\n")
	if cfg.VLESSAlpn != "" {
		fmt.Fprintf(b, "    alpn:\n")
		for _, a := range splitTrim(cfg.VLESSAlpn) {
			fmt.Fprintf(b, "      - %s\n", a)
		}
	}
	fmt.Fprintf(b, "    network: ws\n")
	fmt.Fprintf(b, "    ws-opts:\n")
	fmt.Fprintf(b, "      path: %s\n", wsPath)
	fmt.Fprintf(b, "      headers:\n")
	fmt.Fprintf(b, "        Host: %s\n", host)
}

// safeName collapses anything weird in the display name down to a plain
// string Clash Meta YAML can quote without escaping headaches. Empty
// names fall back to `def`.
func safeName(name, def string) string {
	name = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '"' {
			return -1
		}
		return r
	}, strings.TrimSpace(name))
	if name == "" {
		return def
	}
	return name
}
