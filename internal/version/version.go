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
const Number = "3"
