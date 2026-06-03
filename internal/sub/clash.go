package sub

import (
	"fmt"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// BuildClashMeta returns a Clash Meta (mihomo) YAML config that points
// at our VLESS+WS+TLS inbound for the given user. Self-contained — many
// Clash Meta clients (Clash Verge Rev, FlClash on Android, Stash on iOS,
// some older mihomo party builds) treat a subscription as a *complete*
// profile and won't fill in sensible defaults for the top-level options.
// Without `mixed-port`, `mode`, `dns:` etc. some apps silently leave the
// system proxy off and traffic never even reaches us — exactly the
// "added subscription but speedtest still on direct connection" symptom.
//
// Layout follows the de-facto standard Clash Meta config:
//   1. global runtime knobs (port, mode, log-level, dns)
//   2. proxies: our single VLESS server
//   3. proxy-groups: PROXY group with DIRECT fallback
//   4. rules: minimal LAN bypass + MATCH PROXY
//
// Why YAML hand-written instead of yaml.Marshal: the Clash Meta config
// shape is opinionated (field ordering, quoting style); yaml.Marshal
// output looks scruffy and some clients trip on its quoting. Hand-built
// also stays zero-dep + zero-alloc.
func BuildClashMeta(cfg config.Config, userUUID, displayName string) string {
	name := safeName(displayName, "lightxray")
	wsPath := "/" + cfg.ClientProxyPath + "/" + userUUID + cfg.VLESSWSPath

	var b strings.Builder
	fmt.Fprintf(&b, "# lightxray Clash Meta profile for %s\n", name)
	fmt.Fprintf(&b, "# Drop this into Clash Meta / mihomo / Stash as a profile.\n\n")

	// ── runtime knobs ────────────────────────────────────────────────
	// mixed-port: SOCKS5 + HTTP on the same port; standard 7890.
	// Anything not matched by a rule falls through MATCH → PROXY.
	fmt.Fprintf(&b, "mixed-port: 7890\n")
	fmt.Fprintf(&b, "allow-lan: false\n")
	fmt.Fprintf(&b, "mode: rule\n")
	fmt.Fprintf(&b, "log-level: warning\n")
	fmt.Fprintf(&b, "ipv6: false\n\n")

	// ── dns ─────────────────────────────────────────────────────────
	// fake-ip is the standard Clash Meta pattern — resolves to a fake
	// IP locally, the real DNS happens through the proxy. Without this
	// some apps leak DNS to the system resolver.
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

	// ── proxies ─────────────────────────────────────────────────────
	fmt.Fprintf(&b, "proxies:\n")
	fmt.Fprintf(&b, "  - name: %q\n", name)
	fmt.Fprintf(&b, "    type: vless\n")
	fmt.Fprintf(&b, "    server: %s\n", cfg.PublicHost)
	fmt.Fprintf(&b, "    port: %d\n", cfg.VLESSPort)
	fmt.Fprintf(&b, "    uuid: %s\n", userUUID)
	fmt.Fprintf(&b, "    udp: true\n")
	fmt.Fprintf(&b, "    tls: true\n")
	fmt.Fprintf(&b, "    servername: %s\n", cfg.VLESSSNI)
	fmt.Fprintf(&b, "    skip-cert-verify: false\n")
	fmt.Fprintf(&b, "    client-fingerprint: chrome\n")
	fmt.Fprintf(&b, "    network: ws\n")
	fmt.Fprintf(&b, "    ws-opts:\n")
	fmt.Fprintf(&b, "      path: %s\n", wsPath)
	fmt.Fprintf(&b, "      headers:\n")
	fmt.Fprintf(&b, "        Host: %s\n", cfg.PublicHost)
	fmt.Fprintln(&b)

	// ── proxy-groups ────────────────────────────────────────────────
	// PROXY is the selectable group. DIRECT in the list lets the
	// operator pick "no proxy" without uninstalling the profile.
	fmt.Fprintf(&b, "proxy-groups:\n")
	fmt.Fprintf(&b, "  - name: PROXY\n")
	fmt.Fprintf(&b, "    type: select\n")
	fmt.Fprintf(&b, "    proxies:\n")
	fmt.Fprintf(&b, "      - %q\n", name)
	fmt.Fprintf(&b, "      - DIRECT\n")
	fmt.Fprintln(&b)

	// ── rules ───────────────────────────────────────────────────────
	// Bypass private + LAN + own DNS server; everything else proxied.
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
