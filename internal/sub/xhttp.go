package sub

import (
	"fmt"
	"net/url"

	"github.com/macrodigital283/lightxray/internal/config"
)

// vlessXHTTP builds the vless:// share-link for a VLESS + XHTTP + TLS
// connection through the given edge host. XHTTP (a.k.a. SplitHTTP) splits
// the tunnel into ordinary HTTP request/response transactions instead of
// one long-lived WebSocket, so it blends into normal web traffic and
// survives CDN edges that flag a persistent WS tunnel as an HTTP DDoS.
// Fronted through a CDN it rides the CDN's shared anycast IPs, so an ISP
// that IP-blocks/throttles datacenter ranges can't target the origin.
//
//	vless://<uuid>@<host>:<port>?type=xhttp&path=<path>&host=<host>&mode=packet-up&security=tls&sni=<host>&alpn=h2&fp=chrome#<frag>
//
// mode=packet-up is the most CDN-compatible (many short POSTs up + a GET
// stream down; needs no special CDN feature toggle). Operators on a CDN
// with gRPC support can switch the client to stream-up for throughput.
//
// alpn=h2 pins the client↔CDN leg to HTTP/2 over TCP: XHTTP/SplitHTTP rides
// HTTP/2 best, and excluding h3 keeps it off QUIC/UDP — which mobile-carrier
// DPI (e.g. Myanmar's Ooredoo/MPT/ATOM) routinely blocks or throttles. The
// CDN→origin leg is independent (nginx proxies it as HTTP/1.1).
func vlessXHTTP(cfg config.Config, host, userUUID, displayName string) string {
	q := url.Values{}
	q.Set("type", "xhttp")
	q.Set("security", "tls")
	q.Set("encryption", "none")
	q.Set("path", cfg.VLESSXHTTPPath)
	q.Set("host", host)
	q.Set("sni", host)
	q.Set("mode", "packet-up")
	q.Set("alpn", "h2")
	q.Set("fp", "chrome")
	return fmt.Sprintf(
		"vless://%s@%s:%d?%s#%s",
		userUUID, host, cfg.VLESSPort, q.Encode(),
		fragment(host+" xhttp", displayName),
	)
}
