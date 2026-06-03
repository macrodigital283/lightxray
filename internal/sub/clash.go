package sub

import (
	"fmt"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// BuildClashMeta returns a Clash Meta (mihomo) YAML config that points at
// our VLESS+WS+TLS inbound for the given user. Single proxy + one
// proxy-group + a MATCH rule — minimal but complete; Clash Meta clients
// drop it in as a full config and start routing through us.
//
// Why YAML hand-written instead of yaml.Marshal: the Clash Meta config
// shape is opinionated (field ordering, quoting style) and downstream
// linters complain about Go's default marshaller output. Hand-crafted
// is also zero-dep + zero-alloc.
func BuildClashMeta(cfg config.Config, userUUID, displayName string) string {
	name := safeName(displayName, "lightxray")
	wsPath := "/" + cfg.ClientProxyPath + "/" + userUUID + cfg.VLESSWSPath

	var b strings.Builder
	fmt.Fprintf(&b, "# lightxray Clash Meta config for %s\n", name)
	fmt.Fprintf(&b, "# Drop this into Clash Meta / mihomo / Stash as a profile.\n\n")

	fmt.Fprintf(&b, "proxies:\n")
	fmt.Fprintf(&b, "  - name: %q\n", name)
	fmt.Fprintf(&b, "    type: vless\n")
	fmt.Fprintf(&b, "    server: %s\n", cfg.PublicHost)
	fmt.Fprintf(&b, "    port: %d\n", cfg.VLESSPort)
	fmt.Fprintf(&b, "    uuid: %s\n", userUUID)
	fmt.Fprintf(&b, "    tls: true\n")
	fmt.Fprintf(&b, "    udp: true\n")
	fmt.Fprintf(&b, "    servername: %s\n", cfg.VLESSSNI)
	fmt.Fprintf(&b, "    network: ws\n")
	fmt.Fprintf(&b, "    client-fingerprint: chrome\n")
	fmt.Fprintf(&b, "    ws-opts:\n")
	fmt.Fprintf(&b, "      path: %s\n", wsPath)
	fmt.Fprintf(&b, "      headers:\n")
	fmt.Fprintf(&b, "        Host: %s\n", cfg.PublicHost)
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "proxy-groups:\n")
	fmt.Fprintf(&b, "  - name: PROXY\n")
	fmt.Fprintf(&b, "    type: select\n")
	fmt.Fprintf(&b, "    proxies:\n")
	fmt.Fprintf(&b, "      - %q\n", name)
	fmt.Fprintln(&b)

	fmt.Fprintf(&b, "rules:\n")
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
