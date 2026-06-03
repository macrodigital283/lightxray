# lightxray

A Go HTTP server that drop-in replaces **Hiddify Panel** as the control
plane for an xray-core VLESS+WS+TLS node. Mimics the Hiddify Panel 12.x
admin API exactly so existing pool managers (v2sub-hiddify and friends)
can talk to it with zero code changes.

**~20 MB RAM** per panel instance vs. ~200–400 MB for Hiddify.
xray-core itself is unchanged.

## Quick start (Docker, single node)

```bash
git clone https://github.com/eikhinephyo/lightxray
cd lightxray
cp .env.example .env
# Edit .env — at minimum: LX_PUBLIC_HOST, LX_ADMIN_UUID, LX_DATABASE_URL
docker compose -f deploy/docker-compose.yml up -d --build
```

## Production install (Ubuntu, no Docker)

```bash
curl -fsSL https://raw.githubusercontent.com/eikhinephyo/lightxray/main/deploy/install.sh \
  | DOMAIN=node1.example.com bash
```

Generates random admin/client proxy paths and an admin UUID, prints the
values, installs xray-core, lightxrayd, nginx, and a Let's Encrypt cert.

## API

Implements the Hiddify Panel 12.x admin API. See `CLAUDE.md` for the
endpoint table and field semantics.

```bash
ADMIN=https://node1.example.com/$ADMIN_PROXY_PATH
curl -H "Hiddify-API-Key: $ADMIN_UUID" $ADMIN/api/v2/admin/me/
curl -H "Hiddify-API-Key: $ADMIN_UUID" $ADMIN/api/v2/admin/user/

curl -X POST -H "Hiddify-API-Key: $ADMIN_UUID" -H "Content-Type: application/json" \
  -d '{"name":"alice","usage_limit_GB":50,"package_days":30,"comment":"hello"}' \
  $ADMIN/api/v2/admin/user/
```

## License

MIT
