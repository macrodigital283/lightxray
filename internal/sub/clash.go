package sub

import (
	"fmt"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// BuildClashMeta returns a Clash Meta (mihomo) YAML config that points
// at our VLESS+WS+TLS inbound for the given user. Emits ONE proxy entry
// per CDN host in cfg.CDNHosts (fallback: PublicHost only) and wraps
// them in a `url-test` proxy-group so the client auto-picks the fastest
// edge. Self-contained — many Clash Meta clients (Clash Verge Rev,
// FlClash, Stash) treat a subscription as the *complete* profile and
// won't fill in sensible defaults for the top-level options.
func BuildClashMeta(cfg config.Config, hosts []string, userUUID, displayName string) string {
	name := safeName(displayName, "lightxray")
	if len(hosts) == 0 {
		hosts = []string{cfg.PublicHost}
	}
	wsPath := "/" + cfg.ClientProxyPath + "/" + userUUID + cfg.VLESSWSPath

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

	// ── proxies: one per CDN host ───────────────────────────────────
	proxyNames := make([]string, 0, len(hosts))
	fmt.Fprintf(&b, "proxies:\n")
	for _, h := range hosts {
		pname := fmt.Sprintf("%s | %s", h, name)
		proxyNames = append(proxyNames, pname)
		fmt.Fprintf(&b, "  - name: %q\n", pname)
		fmt.Fprintf(&b, "    type: vless\n")
		fmt.Fprintf(&b, "    server: %s\n", h)
		fmt.Fprintf(&b, "    port: %d\n", cfg.VLESSPort)
		fmt.Fprintf(&b, "    uuid: %s\n", userUUID)
		fmt.Fprintf(&b, "    udp: true\n")
		fmt.Fprintf(&b, "    tls: true\n")
		fmt.Fprintf(&b, "    servername: %s\n", h)
		fmt.Fprintf(&b, "    skip-cert-verify: false\n")
		fmt.Fprintf(&b, "    client-fingerprint: chrome\n")
		// alpn mirrors what the vless:// URL advertises so the Clash
		// client's TLS handshake doesn't accidentally pick h2 and trip
		// over the WS upgrade.
		if cfg.VLESSAlpn != "" {
			fmt.Fprintf(&b, "    alpn:\n")
			for _, a := range strings.Split(cfg.VLESSAlpn, ",") {
				a = strings.TrimSpace(a)
				if a != "" {
					fmt.Fprintf(&b, "      - %s\n", a)
				}
			}
		}
		fmt.Fprintf(&b, "    network: ws\n")
		fmt.Fprintf(&b, "    ws-opts:\n")
		fmt.Fprintf(&b, "      path: %s\n", wsPath)
		fmt.Fprintf(&b, "      headers:\n")
		fmt.Fprintf(&b, "        Host: %s\n", h)
	}
	fmt.Fprintln(&b)

	// ── proxy-groups ────────────────────────────────────────────────
	// AUTO: url-test picks the fastest edge automatically every 5 min.
	// PROXY: manual select with AUTO + DIRECT + individual edges.
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
	fmt.Fprintf(&b, "      - AUTO\n")
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
