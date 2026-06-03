#!/usr/bin/env bash
# lightxray bare-metal installer for Ubuntu 22.04/24.04.
#
# What it does on a fresh box:
#   1. Installs Go, Postgres, nginx, certbot, xray-core.
#   2. Creates the `lightxray` and `xray` system users.
#   3. Generates random admin UUID + admin/client proxy paths if not provided.
#   4. Builds lightxrayd from this checkout (or clones if curl-piped).
#   5. Writes /etc/lightxray/config.env, /etc/xray/config.json,
#      systemd units, and the nginx vhost.
#   6. Requests an LE cert via certbot --webroot (HTTP-01).
#   7. Starts xray.service + lightxrayd.service + nginx.
#
# Re-run with RESET=1 to wipe DB + regenerate secrets + reissue cert.
#
# Required env:
#   DOMAIN=node1.example.com
# Optional env (auto-generated if absent):
#   ADMIN_UUID, ADMIN_PROXY_PATH, CLIENT_PROXY_PATH, PG_PASSWORD
#
# Usage:
#   DOMAIN=node1.example.com bash deploy/install.sh
#   curl -fsSL https://raw.githubusercontent.com/.../install.sh | DOMAIN=... bash
set -euo pipefail

DOMAIN="${DOMAIN:-}"
[[ -z "$DOMAIN" ]] && { echo "DOMAIN env var required" >&2; exit 1; }

REPO_URL="${REPO_URL:-https://github.com/macrodigital283/lightxray}"
REPO_REF="${REPO_REF:-main}"
SRC_DIR="${SRC_DIR:-/opt/lightxray-src}"
RESET="${RESET:-0}"

ADMIN_UUID="${ADMIN_UUID:-$(cat /proc/sys/kernel/random/uuid)}"
ADMIN_PROXY_PATH="${ADMIN_PROXY_PATH:-$(openssl rand -hex 12)}"
CLIENT_PROXY_PATH="${CLIENT_PROXY_PATH:-$(openssl rand -hex 12)}"
PG_PASSWORD="${PG_PASSWORD:-$(openssl rand -hex 20)}"
VLESS_WS_PATH="${VLESS_WS_PATH:-/v2ray}"

if [[ $EUID -ne 0 ]]; then
    echo "must run as root" >&2; exit 1
fi

log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# ── 1. system deps ────────────────────────────────────────────────────
log "installing system packages"
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    git curl ca-certificates nginx certbot \
    postgresql postgresql-contrib build-essential

# xray-core needs Go 1.24+; Ubuntu's apt repos lag, so install from the
# official binary tarball into /usr/local/go. Idempotent: bails out if
# the version is already present.
GO_VERSION="${GO_VERSION:-1.25.0}"
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    log "installing go ${GO_VERSION} from go.dev"
    rm -rf /usr/local/go
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" \
        | tar -C /usr/local -xzf -
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go

# xray-core via official installer (stable channel).
if ! command -v xray >/dev/null 2>&1; then
    log "installing xray-core"
    bash <(curl -fsSL https://github.com/XTLS/Xray-install/raw/main/install-release.sh) install >/dev/null
fi

# ── 2. service users ──────────────────────────────────────────────────
id -u lightxray >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin lightxray
id -u xray      >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin xray
mkdir -p /var/log/lightxray /var/log/xray /etc/lightxray /etc/xray /var/www/letsencrypt
chown lightxray:lightxray /var/log/lightxray
chown xray:xray /var/log/xray

# ── 3. source + build ─────────────────────────────────────────────────
if [[ -d "$SRC_DIR/.git" ]]; then
    log "updating existing checkout in $SRC_DIR"
    git -C "$SRC_DIR" fetch --quiet
    git -C "$SRC_DIR" reset --hard "origin/$REPO_REF"
else
    log "cloning $REPO_URL into $SRC_DIR"
    rm -rf "$SRC_DIR"
    git clone --depth 1 --branch "$REPO_REF" "$REPO_URL" "$SRC_DIR"
fi

log "building lightxrayd"
cd "$SRC_DIR"
CGO_ENABLED=0 /usr/local/bin/go build \
    -trimpath -ldflags="-s -w -X main.version=$(git rev-parse --short HEAD)" \
    -o /usr/local/bin/lightxrayd ./cmd/lightxrayd
chmod 755 /usr/local/bin/lightxrayd

# ── 4. postgres ───────────────────────────────────────────────────────
log "provisioning postgres"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='lightxray'" \
    | grep -q 1 || sudo -u postgres psql -c \
    "CREATE ROLE lightxray LOGIN PASSWORD '$PG_PASSWORD'"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='lightxray'" \
    | grep -q 1 || sudo -u postgres createdb -O lightxray lightxray

if [[ "$RESET" == "1" ]]; then
    log "RESET=1 — wiping lightxray DB"
    sudo -u postgres psql -c "DROP DATABASE lightxray"
    sudo -u postgres createdb -O lightxray lightxray
fi

# ── 5. config files ───────────────────────────────────────────────────
log "writing /etc/lightxray/config.env"
cat > /etc/lightxray/config.env <<EOF
LX_HTTP_ADDR=127.0.0.1:8080
LX_DATABASE_URL=postgres://lightxray:${PG_PASSWORD}@127.0.0.1:5432/lightxray?sslmode=disable
LX_ADMIN_UUID=${ADMIN_UUID}
LX_ADMIN_PROXY_PATH=${ADMIN_PROXY_PATH}
LX_CLIENT_PROXY_PATH=${CLIENT_PROXY_PATH}
LX_PUBLIC_HOST=${DOMAIN}
LX_VLESS_SNI=${DOMAIN}
LX_VLESS_WS_PATH=${VLESS_WS_PATH}
LX_XRAY_GRPC_ADDR=127.0.0.1:10085
LX_XRAY_INBOUND_TAG=vless-ws-in
LX_RECONCILE_PERIOD=60s
EOF
chmod 640 /etc/lightxray/config.env
chown root:lightxray /etc/lightxray/config.env

log "writing /etc/xray/config.json"
sed "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    "$SRC_DIR/deploy/xray-config.json.tmpl" > /etc/xray/config.json

log "installing systemd units"
install -m 0644 "$SRC_DIR/deploy/systemd/lightxrayd.service" /etc/systemd/system/
install -m 0644 "$SRC_DIR/deploy/systemd/xray.service"        /etc/systemd/system/xray.service
systemctl daemon-reload

# ── 6. nginx + TLS ────────────────────────────────────────────────────
log "writing nginx vhost"
sed -e "s|__LX_DOMAIN__|${DOMAIN}|g" \
    -e "s|__LX_ADMIN_PROXY_PATH__|${ADMIN_PROXY_PATH}|g" \
    -e "s|__LX_CLIENT_PROXY_PATH__|${CLIENT_PROXY_PATH}|g" \
    -e "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    "$SRC_DIR/deploy/nginx/lightxray.conf.tmpl" \
    > /etc/nginx/sites-available/lightxray-${DOMAIN}.conf
ln -sf /etc/nginx/sites-available/lightxray-${DOMAIN}.conf /etc/nginx/sites-enabled/

# Stub HTTPS server-name to a self-signed cert so nginx -t passes BEFORE
# certbot has issued the real one. Replaced after issuance.
if [[ ! -f /etc/letsencrypt/live/${DOMAIN}/fullchain.pem ]]; then
    mkdir -p /etc/letsencrypt/live/${DOMAIN}
    openssl req -x509 -newkey rsa:2048 -keyout /etc/letsencrypt/live/${DOMAIN}/privkey.pem \
        -out /etc/letsencrypt/live/${DOMAIN}/fullchain.pem -days 1 -nodes \
        -subj "/CN=${DOMAIN}" >/dev/null 2>&1
fi
nginx -t
systemctl reload nginx

log "issuing Let's Encrypt cert"
certbot certonly --webroot -w /var/www/letsencrypt -d "${DOMAIN}" \
    --non-interactive --agree-tos --register-unsafely-without-email --keep-until-expiring
systemctl reload nginx

# ── 7. start services ────────────────────────────────────────────────
log "enabling + starting services"
systemctl enable --now xray.service
systemctl enable --now lightxrayd.service
systemctl status --no-pager xray.service lightxrayd.service | sed -n '1,15p'

cat <<EOF

──────────────────────────────────────────────────────────────────────
lightxray installed.

  Domain:             https://${DOMAIN}
  Admin proxy path:   ${ADMIN_PROXY_PATH}
  Client proxy path:  ${CLIENT_PROXY_PATH}
  Admin UUID:         ${ADMIN_UUID}
  base_url for pool:  https://${DOMAIN}/${ADMIN_PROXY_PATH}

  Add as a pool API:
    INSERT INTO apis (label, base_url, admin_uuid, client_proxy_path, ...)
    VALUES ('lightxray-${DOMAIN}',
            'https://${DOMAIN}/${ADMIN_PROXY_PATH}',
            '${ADMIN_UUID}',
            '${CLIENT_PROXY_PATH}', ...);

  Smoke test:
    curl -H "Hiddify-API-Key: ${ADMIN_UUID}" \\
         https://${DOMAIN}/${ADMIN_PROXY_PATH}/api/v2/admin/me/
──────────────────────────────────────────────────────────────────────
EOF
