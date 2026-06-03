package sub

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/macrodigital283/lightxray/internal/config"
)

// vlessGRPC builds the single vless:// share-link for a VLESS+gRPC+TLS
// connection through the given edge host. Same shape as the WS form but
// `type=grpc` + `serviceName=...` instead of WS path/host headers.
//
// V2Ray clients (v2rayN, v2rayNG, Streisand) interpret the URL like:
//
//	vless://<uuid>@<host>:<port>?type=grpc&serviceName=<svc>&mode=gun&security=tls&sni=<host>&...
//
// `mode=gun` is the canonical V2Ray gRPC mode (single-stream); supported
// universally. `multi` exists but matters only inside the gRPC layer and
// xray uses gun-mode framing by default.
func vlessGRPC(cfg config.Config, host, userUUID, displayName string) string {
	q := url.Values{}
	q.Set("type", "grpc")
	q.Set("security", "tls")
	q.Set("encryption", "none")
	q.Set("serviceName", cfg.VLESSGRPCService)
	q.Set("mode", "gun")
	q.Set("sni", host)
	q.Set("host", host)
	if cfg.VLESSAlpn != "" {
		// For gRPC, h2 is preferable to http/1.1 (gRPC IS HTTP/2). Most
		// clients tolerate "http/1.1" in the URL but fall back to h2
		// internally. We pass through whatever the operator set.
		q.Set("alpn", cfg.VLESSAlpn)
	}
	q.Set("fp", "chrome")
	return fmt.Sprintf(
		"vless://%s@%s:%d?%s#%s",
		userUUID, host, cfg.VLESSPort, q.Encode(),
		fragment(host+" grpc", displayName),
	)
}

// writeClashGRPCProxy emits one Clash-Meta YAML proxy block for the
// gRPC variant of the given edge host. Called by BuildClashMeta when a
// CDN row's transport is grpc or both.
func writeClashGRPCProxy(b *strings.Builder, cfg config.Config, h, name, userUUID string) {
	pname := fmt.Sprintf("%s grpc | %s", h, name)
	fmt.Fprintf(b, "  - name: %q\n", pname)
	fmt.Fprintf(b, "    type: vless\n")
	fmt.Fprintf(b, "    server: %s\n", h)
	fmt.Fprintf(b, "    port: %d\n", cfg.VLESSPort)
	fmt.Fprintf(b, "    uuid: %s\n", userUUID)
	fmt.Fprintf(b, "    udp: true\n")
	fmt.Fprintf(b, "    tls: true\n")
	fmt.Fprintf(b, "    servername: %s\n", h)
	fmt.Fprintf(b, "    skip-cert-verify: false\n")
	fmt.Fprintf(b, "    client-fingerprint: chrome\n")
	// gRPC favours h2 but we mirror the configured ALPN if set.
	if cfg.VLESSAlpn != "" {
		fmt.Fprintf(b, "    alpn:\n")
		for _, a := range splitTrim(cfg.VLESSAlpn) {
			fmt.Fprintf(b, "      - %s\n", a)
		}
	}
	fmt.Fprintf(b, "    network: grpc\n")
	fmt.Fprintf(b, "    grpc-opts:\n")
	fmt.Fprintf(b, "      grpc-service-name: %s\n", cfg.VLESSGRPCService)
}

// grpcProxyName returns the name we use for the gRPC variant of a host
// — keeps name calculation in one place so the proxy-groups list lines
// up with the proxies list.
func grpcProxyName(host, name string) string {
	return fmt.Sprintf("%s grpc | %s", host, name)
}

func splitTrim(s string) []string {
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
