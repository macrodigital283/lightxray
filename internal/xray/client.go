// Package xray wraps xray-core's gRPC admin API.
//
// We use exactly two services:
//   - app/proxyman/command.HandlerService — add/remove users on a live inbound
//   - app/stats/command.StatsService      — read per-user traffic counters
//
// The "user identity" inside xray is the protocol.User.Email field. We set
// it to the user's UUID string so xray's stat keys come out as
// "user>>>UUID>>>traffic>>>uplink" — easy to map back to our DB.
//
// gRPC plaintext on loopback is fine; xray's admin port (10085 by default)
// must NEVER be exposed beyond 127.0.0.1.
package xray

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"

	"github.com/macrodigital283/lightxray/internal/db"

	// xray-core's gRPC + proto bindings. serial.ToTypedMessage takes the
	// v2 proto.Message interface (google.golang.org/protobuf/proto), which
	// every xray-generated *Operation type implements via ProtoReflect.
	"google.golang.org/protobuf/proto"

	proxymancmd "github.com/xtls/xray-core/app/proxyman/command"
	statscmd "github.com/xtls/xray-core/app/stats/command"
	"github.com/xtls/xray-core/common/protocol"
	"github.com/xtls/xray-core/common/serial"
	"github.com/xtls/xray-core/proxy/vless"
)

// Client is the gRPC handle to a running xray-core. All public methods
// are safe to call concurrently — grpc.ClientConn is goroutine-safe.
//
// inboundTags is the list of xray VLESS inbound tags that mirror the
// same user list. Adding a user calls AlterInbound on EACH tag so the
// user is reachable via every transport (WS, gRPC, Reality) without
// the operator manually picking which to register.
type Client struct {
	conn        *grpc.ClientConn
	handler     proxymancmd.HandlerServiceClient
	stats       statscmd.StatsServiceClient
	inboundTags []string
}

// Dial opens a plaintext gRPC connection to xray's admin port and verifies
// it answers by issuing a no-op stats query.
//
// inboundTagsCSV is a comma-separated list of inbound tags (e.g.
// "vless-ws-in,vless-grpc-in,vless-reality-in"). A bare hostname
// without commas still works — treated as a single-element list.
func Dial(ctx context.Context, addr, inboundTagsCSV string) (*Client, error) {
	dctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	conn, err := grpc.DialContext(
		dctx, addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	tags := splitTags(inboundTagsCSV)
	if len(tags) == 0 {
		conn.Close()
		return nil, fmt.Errorf("no xray inbound tags configured")
	}
	c := &Client{
		conn:        conn,
		handler:     proxymancmd.NewHandlerServiceClient(conn),
		stats:       statscmd.NewStatsServiceClient(conn),
		inboundTags: tags,
	}
	// Sanity: list zero stats. Returns OK on a healthy xray with the
	// StatsService enabled.
	probectx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	defer pcancel()
	if _, err := c.stats.QueryStats(probectx, &statscmd.QueryStatsRequest{Pattern: "lightxray-probe-no-match"}); err != nil {
		conn.Close()
		return nil, fmt.Errorf("xray stats probe: %w", err)
	}
	return c, nil
}

// DialWithRetry retries Dial until it succeeds or the deadline expires.
// xray may still be coming up under systemd when lightxrayd starts.
func DialWithRetry(ctx context.Context, addr, inboundTag string, total time.Duration) (*Client, error) {
	deadline := time.Now().Add(total)
	backoff := 500 * time.Millisecond
	var lastErr error
	for {
		c, err := Dial(ctx, addr, inboundTag)
		if err == nil {
			return c, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("xray unreachable after %s: %w", total, lastErr)
		}
		slog.Info("xray not ready yet, retrying", "err", err, "in", backoff)
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		if backoff < 5*time.Second {
			backoff *= 2
		}
	}
}

// Close releases the underlying gRPC connection.
func (c *Client) Close() error {
	if c == nil || c.conn == nil {
		return nil
	}
	return c.conn.Close()
}

// AddUser registers a VLESS user with EVERY configured xray inbound
// (WS, gRPC, Reality, …). Idempotent per inbound: if the user already
// exists xray returns AlreadyExists, which we treat as success.
//
// The `email` field doubles as the xray stat key — we always set it to
// the user's UUID string so stat lookups round-trip cleanly. The same
// `email` is shared across inbounds; xray namespaces stats by inbound
// tag internally so there's no collision.
func (c *Client) AddUser(ctx context.Context, userUUID uuid.UUID) error {
	var lastErr error
	for _, tag := range c.inboundTags {
		op := &proxymancmd.AddUserOperation{
			User: &protocol.User{
				Level: 0,
				Email: userUUID.String(),
				Account: serial.ToTypedMessage(&vless.Account{
					Id: userUUID.String(),
				}),
			},
		}
		if err := c.alterWithRetry(ctx, tag, op); err != nil {
			slog.Warn("xray AddUser", "tag", tag, "uuid", userUUID, "err", err)
			lastErr = err
		}
	}
	return lastErr
}

// RemoveUser deregisters a VLESS user from EVERY configured inbound.
// NotFound is treated as success (matches the pool's deleteWithRetry
// expectations).
func (c *Client) RemoveUser(ctx context.Context, userUUID uuid.UUID) error {
	var lastErr error
	for _, tag := range c.inboundTags {
		op := &proxymancmd.RemoveUserOperation{Email: userUUID.String()}
		if err := c.alterWithRetry(ctx, tag, op); err != nil {
			slog.Warn("xray RemoveUser", "tag", tag, "uuid", userUUID, "err", err)
			lastErr = err
		}
	}
	return lastErr
}

// alterWithRetry wraps AlterInbound with the same 3-attempt / 0.8s|1.6s
// backoff our pool uses for upstream deletes. Stops early on
// AlreadyExists / NotFound which are both terminal-success conditions.
//
// op is either *AddUserOperation or *RemoveUserOperation; both satisfy
// the v2 proto.Message interface ToTypedMessage takes. `tag` selects
// which xray inbound to mutate — caller iterates over inboundTags.
func (c *Client) alterWithRetry(ctx context.Context, tag string, op proto.Message) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := c.handler.AlterInbound(reqctx, &proxymancmd.AlterInboundRequest{
			Tag:       tag,
			Operation: serial.ToTypedMessage(op),
		})
		cancel()
		if err == nil {
			return nil
		}
		// terminal-success cases — already in the desired state
		st, ok := status.FromError(err)
		if ok {
			msg := st.Message()
			if containsAny(msg, "already exists", "not found") {
				return nil
			}
		}
		lastErr = err
		if attempt < 2 {
			time.Sleep(time.Duration(800*(1<<attempt)) * time.Millisecond)
		}
	}
	return fmt.Errorf("xray AlterInbound: %w", lastErr)
}

// HydrateFromDB re-adds every enabled user from the DB to xray. Called on
// startup so an xray restart doesn't drop everyone. Returns the count
// that were (re)added; per-user failures are logged and skipped.
func (c *Client) HydrateFromDB(ctx context.Context, store *db.Store) (int, error) {
	ids, err := store.AllUUIDsEnabled(ctx)
	if err != nil {
		return 0, err
	}
	added := 0
	for _, id := range ids {
		if err := c.AddUser(ctx, id); err != nil {
			slog.Warn("hydrate AddUser failed", "uuid", id, "err", err)
			continue
		}
		added++
	}
	return added, nil
}

// PerUserTraffic returns (uplinkBytes, downlinkBytes) for the given user
// since xray's last process start. NOT a delta — the caller (reconciler)
// is responsible for computing deltas vs. the persisted stats_cursor.
func (c *Client) PerUserTraffic(ctx context.Context, userUUID uuid.UUID) (up, down int64, err error) {
	reqctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// QueryStats with a per-user prefix is one round-trip for both
	// directions and tolerates either stat being absent (returns []).
	resp, err := c.stats.QueryStats(reqctx, &statscmd.QueryStatsRequest{
		Pattern: fmt.Sprintf("user>>>%s>>>", userUUID.String()),
		Reset_:  false,
	})
	if err != nil {
		return 0, 0, err
	}
	for _, s := range resp.GetStat() {
		switch {
		case endsWith(s.Name, ">>>uplink"):
			up = s.Value
		case endsWith(s.Name, ">>>downlink"):
			down = s.Value
		}
	}
	return up, down, nil
}

// QueryAllUserTraffic returns a map keyed by UUID string of (up, down) for
// every user xray has stats for. Cheaper than calling PerUserTraffic N
// times when the reconciler is walking the whole list.
func (c *Client) QueryAllUserTraffic(ctx context.Context) (map[string][2]int64, error) {
	reqctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	resp, err := c.stats.QueryStats(reqctx, &statscmd.QueryStatsRequest{
		Pattern: "user>>>",
		Reset_:  false,
	})
	if err != nil {
		return nil, err
	}
	out := make(map[string][2]int64)
	for _, s := range resp.GetStat() {
		// name shape: user>>>UUID>>>traffic>>>{uplink|downlink}
		uid, dir, ok := parseStatName(s.Name)
		if !ok {
			continue
		}
		cur := out[uid]
		if dir == "uplink" {
			cur[0] = s.Value
		} else if dir == "downlink" {
			cur[1] = s.Value
		}
		out[uid] = cur
	}
	return out, nil
}

// IsTransient reports whether err looks like a temporary gRPC blip the
// caller might retry past (e.g. xray restart in progress).
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	st, ok := status.FromError(err)
	if !ok {
		return errors.Is(err, context.DeadlineExceeded)
	}
	// Unavailable, Aborted, DeadlineExceeded — all retryable.
	switch st.Code().String() {
	case "Unavailable", "Aborted", "DeadlineExceeded":
		return true
	}
	return false
}

// parseStatName splits "user>>>UUID>>>traffic>>>uplink" into (UUID, "uplink").
func parseStatName(name string) (uid, dir string, ok bool) {
	const prefix = "user>>>"
	if !startsWith(name, prefix) {
		return "", "", false
	}
	rest := name[len(prefix):]
	// rest = UUID>>>traffic>>>uplink|downlink
	sep := indexOf(rest, ">>>traffic>>>")
	if sep < 0 {
		return "", "", false
	}
	return rest[:sep], rest[sep+len(">>>traffic>>>"):], true
}

// tiny string helpers — saves an import of strings just for two functions.
func startsWith(s, prefix string) bool { return len(s) >= len(prefix) && s[:len(prefix)] == prefix }
func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}
func containsAny(s string, needles ...string) bool {
	for _, n := range needles {
		if indexOf(s, n) >= 0 {
			return true
		}
	}
	return false
}

// splitTags parses a comma-separated tag list, trimming whitespace and
// dropping empties. Returns nil for an empty/whitespace-only input so
// the Dial caller can detect misconfiguration.
func splitTags(s string) []string {
	if s == "" {
		return nil
	}
	out := make([]string, 0, 4)
	cur := ""
	flush := func() {
		t := cur
		// trim
		for len(t) > 0 && (t[0] == ' ' || t[0] == '\t') {
			t = t[1:]
		}
		for len(t) > 0 && (t[len(t)-1] == ' ' || t[len(t)-1] == '\t') {
			t = t[:len(t)-1]
		}
		if t != "" {
			out = append(out, t)
		}
		cur = ""
	}
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			flush()
			continue
		}
		cur += string(s[i])
	}
	flush()
	return out
}
