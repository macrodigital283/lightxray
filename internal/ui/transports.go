package ui

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/macrodigital283/lightxray/internal/db"
)

// transportsView is what the Transports page renders — the on/off state of
// each switchable VLESS transport. Source of truth is the DB settings
// (transport_*_enabled); an unset setting falls back to whether the inbound
// tag is in LX_XRAY_INBOUND_TAG, so existing nodes keep their transports
// until the operator first toggles. Reality has its own page.
type transportsView struct {
	WS              bool
	GRPC            bool
	XHTTP           bool
	HasXHTTPInbound bool // LX_VLESS_XHTTP_PATH set → the xhttp inbound exists to receive users
	Note            string
	Error           string
	Applied         bool
}

// tagInEnv reports whether tag is listed in the static LX_XRAY_INBOUND_TAG.
func (u *UI) tagInEnv(tag string) bool {
	for _, t := range strings.Split(u.cfg.XrayInboundTag, ",") {
		if strings.TrimSpace(t) == tag {
			return true
		}
	}
	return false
}

// currentTransports derives toggle state from DB settings, falling back to
// the env inbound-tag set for any setting not yet persisted.
func (u *UI) currentTransports(ctx context.Context) transportsView {
	m, _ := u.store.GetSettings(ctx,
		db.SettingTransportWSEnabled, db.SettingTransportGRPCEnabled, db.SettingTransportXHTTPEnabled)
	state := func(key, tag string) bool {
		if v, ok := m[key]; ok {
			return v == "true"
		}
		return u.tagInEnv(tag)
	}
	return transportsView{
		WS:              state(db.SettingTransportWSEnabled, "vless-ws-in"),
		GRPC:            state(db.SettingTransportGRPCEnabled, "vless-grpc-in"),
		XHTTP:           state(db.SettingTransportXHTTPEnabled, "vless-xhttp-in"),
		HasXHTTPInbound: u.cfg.VLESSXHTTPPath != "",
	}
}

// transportsPage — GET /ui/transports/
func (u *UI) transportsPage(w http.ResponseWriter, r *http.Request) {
	u.render(w, "transports_page", map[string]any{"V": u.currentTransports(r.Context())})
}

// transportsApply — POST /ui/transports/
// Applies the three checkboxes WITHOUT sudo or a restart: for each changed
// transport it persists the DB flag, flips the xray client's active-tag set
// (so new users + post-restart hydration follow it), and bulk add/removes
// every enabled user on that one inbound via the xray gRPC API. Effect is
// immediate and scoped — other transports' live connections are untouched.
func (u *UI) transportsApply(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	cur := u.currentTransports(ctx)
	fail := func(msg string) {
		v := cur
		v.Error = msg
		u.render(w, "transports_page", map[string]any{"V": v})
	}
	if err := r.ParseForm(); err != nil {
		fail("bad form")
		return
	}
	want := map[string]bool{
		db.SettingTransportWSEnabled:    r.PostFormValue("ws") == "on",
		db.SettingTransportGRPCEnabled:  r.PostFormValue("grpc") == "on",
		db.SettingTransportXHTTPEnabled: r.PostFormValue("xhttp") == "on",
	}
	if !want[db.SettingTransportWSEnabled] && !want[db.SettingTransportGRPCEnabled] && !want[db.SettingTransportXHTTPEnabled] {
		fail("At least one transport must stay enabled — refusing to disable all.")
		return
	}
	if want[db.SettingTransportXHTTPEnabled] && u.cfg.VLESSXHTTPPath == "" {
		fail("XHTTP can't be enabled — this node has no vless-xhttp-in inbound (LX_VLESS_XHTTP_PATH is unset).")
		return
	}

	old := map[string]bool{
		db.SettingTransportWSEnabled:    cur.WS,
		db.SettingTransportGRPCEnabled:  cur.GRPC,
		db.SettingTransportXHTTPEnabled: cur.XHTTP,
	}

	// Bulk add/remove can be 100s of gRPC calls — give it room beyond the
	// request's own deadline so a refresh-happy client can't cancel midway.
	bctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ids, err := u.store.AllUUIDsEnabled(bctx)
	if err != nil {
		fail("Couldn't list users to apply the change: " + err.Error())
		return
	}

	var changes []string
	for key, on := range want {
		if on == old[key] {
			continue // unchanged
		}
		tag := db.TransportTag[key]
		if err := u.store.SetSetting(bctx, key, boolStr(on)); err != nil {
			slog.Error("transports: persist setting", "key", key, "err", err)
			fail("Save failed: " + err.Error())
			return
		}
		if on {
			u.xc.EnableTag(tag)
			for _, id := range ids {
				if e := u.xc.AddUserToTag(bctx, id, tag); e != nil {
					slog.Warn("transports: AddUserToTag", "tag", tag, "uuid", id, "err", e)
				}
			}
		} else {
			u.xc.DisableTag(tag)
			for _, id := range ids {
				if e := u.xc.RemoveUserFromTag(bctx, id, tag); e != nil {
					slog.Warn("transports: RemoveUserFromTag", "tag", tag, "uuid", id, "err", e)
				}
			}
		}
		state := "disabled"
		if on {
			state = "enabled"
		}
		changes = append(changes, tag+" "+state)
		slog.Info("transports: applied", "tag", tag, "enabled", on, "users", len(ids))
	}

	view := u.currentTransports(bctx)
	view.Applied = true
	if len(changes) == 0 {
		view.Note = "No change — transports already in that state."
	} else {
		view.Note = "Applied: " + strings.Join(changes, ", ") + ". Took effect immediately for all " +
			itoa(len(ids)) + " users; subscriptions now reflect it on next fetch."
	}
	u.render(w, "transports_page", map[string]any{"V": view})
}

// itoa is a tiny non-fmt int formatter for the flash message.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
