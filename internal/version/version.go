// Package version holds the human-facing release number shown on the
// dashboard. The exact git commit is stamped separately into the binary
// (main.version) and shown as a tooltip for precision.
package version

// Number is the simple release version (e.g. "1", "2", …) displayed in the
// dashboard header so an operator can tell at a glance whether a node is
// up to date. BUMP THIS when you cut a release worth distinguishing.
//
// v2 — TLS session resumption on the nginx http backend (shared session
//      cache + 1-day window) to flatten the CPU spikes from reconnect storms.
// v3 — dashboard Performance page: optionally raise nginx worker_connections
//      for large servers (default off / stock 1024).
// v4 — reconciler now ENFORCES quotas + expiry: over-cap / expired users are
//      auto-disabled and evicted from xray each tick (previously never were).
// v5 — connection stability: TCP keepalive on the Reality inbound (keeps mobile
//      NAT mappings warm) + CPU priority (data plane outranks the panel).
// v6 — dashboard Admin Access page: rotate the admin path + admin UUID (the
//      credential) from the UI, via the lightxray-applyadmin helper.
// v7 — expiry off-by-one fix: users are now disabled ON their expiry date
//      (start_date + package_days <= today), not the day after.
// v8 — raise nginx worker_connections baseline to 16384 (the distro 768 was
//      far too low — Cloudflare fan-out runs a node's connection count well
//      above its user count, exhausting it). Perf page reads the real value.
// v9 — usage accuracy: GET user now tops up the DB usage_bytes with the
//      un-synced live xray counters (read-only, no cursor write) so the pool
//      bills the true usage when it reads-then-deletes a rotated key. Fixes a
//      systematic under-count that grew with per-device key rotation.
// v10 — deploy: update_lightxray.py now REGENERATES the nginx http backend
//       (/etc/nginx/conf.d/lightxray-http.conf) from the template on every roll,
//       so the generated config can no longer drift behind the binary. Retrofits
//       the one-click admin login route (/<admin_path>/<admin_uuid>) and the v2
//       TLS-session-resumption block onto nodes that were updated binary-only and
//       silently missed them. Regen uses the node's existing config.env values,
//       so data-plane routes stay byte-identical — graceful reload, no customer
//       impact. (Root cause of w46's one-click ERR_HTTP2_PROTOCOL_ERROR.)
// v11 — dashboard Transports page: per-transport on/off toggles (WS / gRPC /
//       XHTTP). Turning one off bulk-removes its users from the xray inbound
//       over the gRPC API AND drops its URL from subscriptions — immediate, no
//       restart, scoped (other transports untouched), and persisted in DB so it
//       survives restarts (applied before hydrate). Adds VLESS+XHTTP (SplitHTTP,
//       mode=packet-up) as a CDN-frontable transport that blends as plain HTTP —
//       the answer to ISP IP-blocking that kills direct Reality and to Cloudflare
//       flagging long-lived WS tunnels as HTTP DDoS.
const Number = "11"
