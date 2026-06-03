# CLAUDE.md

Guidance for Claude working in this repo.

## What this is

A lightweight HTTP server that drop-in replaces **Hiddify Panel** for the
v2sub-hiddify pool. Mimics the Hiddify Panel 12.x admin API exactly so
`v2sub-hiddify/src/lib/hiddify.ts` can talk to it with zero code changes —
point an `apis` row at lightxray's URL/admin-uuid and it just works.

**Why:** Hiddify is Python (Flask + workers + Telegram + multi-protocol
runtime) and burns ~200–400 MB RAM per panel. lightxray is a Go single
binary at ~20 MB. **xray-core itself** (the actual data plane carrying
VLESS+WS+TLS traffic) is unchanged — lightxray only replaces the *control
plane*.

## Architecture

```
client → Cloudflare → :443 nginx ─┐
                                   │ /<admin>/api/v2/admin/…  → :8080
                                   │ /<client>/<uuid>/sub/   → :8080
                                   ▼
                              lightxrayd (Go)
                                   │ gRPC 127.0.0.1:10085
                                   ▼
                              xray-core  (VLESS+WS+TLS inbound)
                                   ▲
                              (reconciler polls stats every 60s)
                              writes counters + last_online → Postgres
```

- **nginx** — TLS termination (Let's Encrypt) + path routing.
- **lightxrayd** — Go HTTP server. Talks to Postgres + xray gRPC.
- **xray-core** — unchanged binary; lightxrayd mutates its user list at
  runtime via `HandlerService.AlterInbound`.
- **Postgres** — one DB per panel instance. Stores users, traffic
  counters, metadata. (Shared with the pool's Postgres on the same box,
  separate database.)

## Hiddify compat surface

We implement EXACTLY what `v2sub-hiddify/src/lib/hiddify.ts` calls:

| method | path | notes |
|---|---|---|
| GET    | `/api/v2/admin/me/`               | auth probe |
| GET    | `/api/v2/admin/server_status/`    | CPU / RAM / disk / load |
| GET    | `/api/v2/admin/all-configs/`      | exposes `chconfigs.0.proxy_path_client` |
| GET    | `/api/v2/admin/user/`             | full list |
| GET    | `/api/v2/admin/user/<uuid>/`      | single |
| POST   | `/api/v2/admin/user/`             | **panel assigns uuid server-side** |
| PATCH  | `/api/v2/admin/user/<uuid>/`      | partial update |
| DELETE | `/api/v2/admin/user/<uuid>/`      | 204; 404 if gone |
| GET    | `/<client_proxy_path>/<uuid>/sub/?asn=…`     | base64 VLESS bundle |
| GET    | `/<client_proxy_path>/<uuid>/sub64/?asn=…`   | alias of /sub/ |

Auth on `/api/v2/admin/…`: header `Hiddify-API-Key: <admin_uuid>`,
constant-time compared. Subscription URLs are public-by-UUID (Hiddify's
own model — the UUID itself is the credential).

## Field semantics (must match Hiddify)

`HiddifyUser` JSON fields, in the exact format the pool expects:
- `uuid` — UUID v4. Server-assigned on POST regardless of body.
- `name` — display name. Free-text.
- `comment` — free-text. Pool stamps `v2sub-hiddify sub_id=N` here.
- `current_usage_GB` — float, derived from `usage_bytes / 1024^3`.
- `usage_limit_GB` — float, 0 = unlimited.
- `package_days` — int, 0 = unlimited.
- `start_date` — `"YYYY-MM-DD"` UTC, or `null` until first connect.
- `last_online` — `"YYYY-MM-DD HH:MM:SS"` UTC, or `"0001-01-01 00:00:00"`
  sentinel for never-connected. The pool's cleanup tooling distinguishes
  these two cases — preserve the distinction.
- `last_reset_time` — same format. Stamped at creation for `no_reset`
  users; used by manual-cleanup as a fallback age signal when
  `start_date` is null.
- `enable` — bool. `is_active` mirrors this.
- `mode` — `"no_reset" | "monthly" | "weekly" | "daily"`. We default to
  `no_reset` (pool only creates that mode).
- `added_by_uuid` — always the admin's uuid.
- `id` — monotonic integer (we use a sequence).

## Directory layout

```
cmd/lightxrayd/main.go         entry point
internal/
  config/                      env-var config
  db/                          pgx-backed user store (+ schema.sql)
  server/                      HTTP handlers, one file per route group
  xray/                        xray-core gRPC client wrapper
  reconciler/                  periodic stats poll + last_online update
  sub/                         vless:// URL builder + base64 encoder
  util/                        Hiddify-format time helpers
deploy/                        Dockerfile, systemd unit, nginx + xray templates, install.sh
scripts/                       one-shot tools (migration from Hiddify, etc.)
```

## Conventions

- All DB writes go through `internal/db`. Handlers never reach into pgx.
- **Hiddify time** = `"YYYY-MM-DD HH:MM:SS"` UTC. Never = `"0001-01-01
  00:00:00"`. Use `util.HiddifyTime` / `util.HiddifyDate`.
- **GB at the wire, bytes in the DB.** Convert at handler boundary.
- **UUIDs are server-assigned** on `POST /user/` — ignore body uuid.
- **Never log `admin_uuid`** — it IS the credential. Redact in any error
  surface the pool might persist.
- xray gRPC calls have a 5 s timeout and 3× retry on transient errors
  (matches the pool's `deleteWithRetry` pattern).
- On startup, lightxrayd **rehydrates** xray's in-memory user list from
  the DB. xray restarts wipe its user list; the DB is source of truth.
- The `enable=false` path calls `RemoveUser` on xray (so disabled users
  can't connect); `enable=true` calls `AddUser`.

## Local dev

You need:
- Go 1.23+
- Postgres 16 (`psql` reachable as default user)
- `xray` binary on PATH (or use `deploy/docker-compose.yml`)

```bash
cp .env.example .env
# edit .env (LX_ADMIN_UUID, LX_DATABASE_URL, LX_PUBLIC_HOST, ...)
psql -c "CREATE DATABASE lightxray;"
make run
```

API smoke test:
```bash
curl -H "Hiddify-API-Key: $LX_ADMIN_UUID" http://127.0.0.1:8080/$LX_ADMIN_PROXY_PATH/api/v2/admin/me/
```

## Production deploy

`deploy/install.sh` on a fresh Ubuntu 24 box:
- installs xray-core (official install script)
- installs the lightxrayd binary to `/usr/local/bin`
- writes `/etc/lightxray/config.env`, the systemd unit, the nginx vhost
- requests an LE cert via `certbot --webroot`
- starts `xray.service` + `lightxrayd.service` + nginx

Re-run with `RESET=1` to wipe and rebuild. Honours `DOMAIN`,
`ADMIN_UUID`, `ADMIN_PROXY_PATH`, `CLIENT_PROXY_PATH` env vars
(generates random UUIDs/paths if omitted).

## Migration from a live Hiddify panel

`scripts/migrate-from-hiddify.go` — point at a Hiddify panel URL + admin
UUID, and a lightxray panel URL + admin UUID. Walks the source user list
and POSTs each user to the target. **UUIDs will change** (the target
panel assigns its own), so after migration the pool's `subscription_keys`
rows need to be re-pointed to the new UUIDs — there's a companion SQL
patch in `scripts/migrate-rebind.sql` that does this by `comment` match.

## When extending

- Adding a new Hiddify endpoint the pool needs: add the handler in
  `internal/server/`, register it in `server.go`, mirror Hiddify's
  response shape exactly.
- Adding a new xray protocol/inbound (Reality, etc.): add a new
  `xray.Inbound` enum value, extend the sub builder, add a column to
  `users` if per-user config differs. The control plane stays the same.
- Don't add deps without a clear need. Current set: pgx, grpc, uuid,
  xray-core proto packages. That's it.

## Known gaps (v0.1)

- `PATCH /user/` only honours `enable`, `comment`, `name`, `usage_limit_GB`,
  `package_days`. `mode` change is accepted but doesn't reset counters.
- No GB-limit / expiry **enforcement loop** yet — reconciler updates
  counters but doesn't auto-disable. Add when needed.
- `serverStatus()` returns real CPU/RAM but `top5` is empty.
- `/sub/` only emits VLESS+WS+TLS. Add other formats (sing-box JSON,
  clash YAML) when a client needs them.
- No SSRF guard — lightxray doesn't make outbound HTTP calls except to
  xray gRPC on loopback, so the attack surface is different from
  Hiddify. Revisit if we add outbound features.
