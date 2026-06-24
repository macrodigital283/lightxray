package ui

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"time"
)

// transportsView is what the Transports page renders — the on/off state of
// each switchable VLESS transport. The source of truth is the live
// LX_XRAY_INBOUND_TAG (cfg.XrayInboundTag): a tag's presence means that
// inbound gets users AND its URL is emitted in subscriptions. Reality has
// its own page so it isn't listed here.
type transportsView struct {
	WS      bool
	GRPC    bool
	XHTTP   bool
	HasXHTTPInbound bool // LX_VLESS_XHTTP_PATH set → the xhttp inbound exists to receive users
	Output  string
	Error   string
	Applied bool
}

// currentTransports derives the toggle state from cfg.XrayInboundTag.
func (u *UI) currentTransports() transportsView {
	has := func(tag string) bool {
		for _, t := range strings.Split(u.cfg.XrayInboundTag, ",") {
			if strings.TrimSpace(t) == tag {
				return true
			}
		}
		return false
	}
	return transportsView{
		WS:              has("vless-ws-in"),
		GRPC:            has("vless-grpc-in"),
		XHTTP:           has("vless-xhttp-in"),
		HasXHTTPInbound: u.cfg.VLESSXHTTPPath != "",
	}
}

// transportsPage — GET /ui/transports/
func (u *UI) transportsPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "transports_page", map[string]any{"V": u.currentTransports()})
}

// transportsApply — POST /ui/transports/
// Reads the three checkboxes and shells out to the privileged helper
// `sudo lightxray-applytransports <ws> <grpc> <xhttp>` (1/0 each). The
// helper rewrites LX_XRAY_INBOUND_TAG and restarts xray + lightxrayd
// out-of-band, so a disabled transport's inbound loses all its users (goes
// dark on the wire) and drops out of new subscription bundles. The admin
// path is untouched, so this dashboard session survives the restart —
// refresh after a couple of seconds.
func (u *UI) transportsApply(w http.ResponseWriter, r *http.Request) {
	cur := u.currentTransports()
	fail := func(msg string) {
		v := cur
		v.Error = msg
		u.render(w, "transports_page", map[string]any{"V": v})
	}
	if err := r.ParseForm(); err != nil {
		fail("bad form")
		return
	}
	flag := func(name string) string {
		if r.PostFormValue(name) == "on" {
			return "1"
		}
		return "0"
	}
	ws, grpc, xhttp := flag("ws"), flag("grpc"), flag("xhttp")

	// Refuse to disable every transport — that would brick the data plane.
	if ws == "0" && grpc == "0" && xhttp == "0" {
		fail("At least one transport must stay enabled — refusing to disable all.")
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	slog.Info("transports: applying", "ws", ws, "grpc", grpc, "xhttp", xhttp)
	cmd := exec.CommandContext(ctx, "sudo", "-n", "/usr/local/bin/lightxray-applytransports", ws, grpc, xhttp)
	out, err := cmd.CombinedOutput()
	view := transportsView{
		WS: ws == "1", GRPC: grpc == "1", XHTTP: xhttp == "1",
		HasXHTTPInbound: cur.HasXHTTPInbound, Output: string(out),
	}
	if err != nil {
		slog.Error("transports: apply failed", "err", err)
		view.Error = "Apply failed (" + err.Error() + "). See output below — nothing changed if validation failed."
		view.WS, view.GRPC, view.XHTTP = cur.WS, cur.GRPC, cur.XHTTP
	} else {
		view.Applied = true
	}
	u.render(w, "transports_page", map[string]any{"V": view})
}
