// Package version holds the human-facing release number shown on the
// dashboard. The exact git commit is stamped separately into the binary
// (main.version) and shown as a tooltip for precision.
package version

// Number is the simple release version (e.g. "1", "2", …) displayed in the
// dashboard header so an operator can tell at a glance whether a node is
// up to date. BUMP THIS when you cut a release worth distinguishing.
const Number = "1"
