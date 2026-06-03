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

# Reuse credentials from a prior install if present — re-running install.sh
# must NOT regenerate the admin UUID / proxy paths / postgres password
# (the role+DB already exist with the old values; mismatched config →
# lightxrayd auth-loops on startup). Operator can still force-rotate by
# passing the var explicitly or running with RESET=1.
if [[ "$RESET" != "1" && -f /etc/lightxray/config.env ]]; then
    # shellcheck disable=SC1091
    source /etc/lightxray/config.env
    ADMIN_UUID="${ADMIN_UUID:-${LX_ADMIN_UUID:-}}"
    ADMIN_PROXY_PATH="${ADMIN_PROXY_PATH:-${LX_ADMIN_PROXY_PATH:-}}"
    CLIENT_PROXY_PATH="${CLIENT_PROXY_PATH:-${LX_CLIENT_PROXY_PATH:-}}"
    # Extract pg password from existing LX_DATABASE_URL.
    if [[ -n "${LX_DATABASE_URL:-}" ]]; then
        PG_PASSWORD="${PG_PASSWORD:-$(printf '%s' "$LX_DATABASE_URL" | sed -E 's|.*lightxray:([^@]+)@.*|\1|')}"
    fi
    VLESS_WS_PATH="${VLESS_WS_PATH:-${LX_VLESS_WS_PATH:-/v2ray}}"
fi
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
mkdir -p /var/log/lightxray /var/log/xray /etc/lightxray /usr/local/etc/xray /var/www/letsencrypt
chown lightxray:lightxray /var/log/lightxray
# XTLS installer creates /var/log/xray/{access,error}.log owned by
# nobody:nogroup (its default unit runs as nobody). Our unit runs as
# `xray`, so reclaim ALL existing files too — not just the dir.
chown -R xray:xray /var/log/xray

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

log "writing /usr/local/etc/xray/config.json"
# The XTLS installer drops a systemd "drop-in" at
# /etc/systemd/system/xray.service.d/10-donot_touch_single_conf.conf
# that hard-codes ExecStart to `xray run -config /usr/local/etc/xray/config.json`.
# It overrides the ExecStart in any unit we install — including ours.
# Easier to write the config where xray actually reads it than to
# fight the drop-in.
sed "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    "$SRC_DIR/deploy/xray-config.json.tmpl" > /usr/local/etc/xray/config.json
chown xray:xray /usr/local/etc/xray/config.json

log "installing systemd units"
install -m 0644 "$SRC_DIR/deploy/systemd/lightxrayd.service" /etc/systemd/system/
install -m 0644 "$SRC_DIR/deploy/systemd/xray.service"        /etc/systemd/system/xray.service
systemctl daemon-reload

# ── 6. nginx + TLS ────────────────────────────────────────────────────
log "writing nginx vhost"
# Detect the layout: Debian/Ubuntu nginx uses sites-available/sites-enabled,
# upstream nginx.org packages drop everything in conf.d/. Support both.
if [[ -d /etc/nginx/sites-available && -d /etc/nginx/sites-enabled ]]; then
    NGINX_CONF="/etc/nginx/sites-available/lightxray-${DOMAIN}.conf"
    NGINX_LINK="/etc/nginx/sites-enabled/lightxray-${DOMAIN}.conf"
else
    mkdir -p /etc/nginx/conf.d
    NGINX_CONF="/etc/nginx/conf.d/lightxray-${DOMAIN}.conf"
    NGINX_LINK=""
    # nginx.org's default.conf binds :80 on _ and would shadow our LE
    # challenge route. Push it aside.
    if [[ -f /etc/nginx/conf.d/default.conf ]]; then
        mv /etc/nginx/conf.d/default.conf /etc/nginx/conf.d/default.conf.disabled-by-lightxray
    fi
fi
sed -e "s|__LX_DOMAIN__|${DOMAIN}|g" \
    -e "s|__LX_ADMIN_PROXY_PATH__|${ADMIN_PROXY_PATH}|g" \
    -e "s|__LX_CLIENT_PROXY_PATH__|${CLIENT_PROXY_PATH}|g" \
    -e "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    "$SRC_DIR/deploy/nginx/lightxray.conf.tmpl" \
    > "$NGINX_CONF"
[[ -n "$NGINX_LINK" ]] && ln -sf "$NGINX_CONF" "$NGINX_LINK"

# Cert provisioning.
# ── Mode A (default): Let's Encrypt via certbot --webroot (HTTP-01).
#    Requires port 80 reachable on this box from the public internet
#    (DNS A record pointing here, CF in DNS-only / grey-cloud).
# ── Mode B: Cloudflare Origin Certificate (or any pre-issued PEM pair).
#    Set ORIGIN_CERT_FILE + ORIGIN_KEY_FILE to file paths on this box
#    containing the cert chain and private key. Skips certbot entirely;
#    works when CF is in front in "Full (strict)" mode.
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"
mkdir -p "$CERT_DIR"

if [[ -n "${ORIGIN_CERT_FILE:-}" && -n "${ORIGIN_KEY_FILE:-}" ]]; then
    log "installing pre-issued cert from ${ORIGIN_CERT_FILE} + ${ORIGIN_KEY_FILE}"
    [[ -f "$ORIGIN_CERT_FILE" ]] || { echo "ORIGIN_CERT_FILE not found: $ORIGIN_CERT_FILE" >&2; exit 1; }
    [[ -f "$ORIGIN_KEY_FILE"  ]] || { echo "ORIGIN_KEY_FILE not found: $ORIGIN_KEY_FILE" >&2; exit 1; }
    install -m 0644 "$ORIGIN_CERT_FILE" "$CERT_DIR/fullchain.pem"
    install -m 0600 "$ORIGIN_KEY_FILE"  "$CERT_DIR/privkey.pem"
    nginx -t
    systemctl restart nginx
else
    # Stub self-signed first so `nginx -t` passes before certbot runs.
    if [[ ! -f $CERT_DIR/fullchain.pem ]]; then
        openssl req -x509 -newkey rsa:2048 -keyout $CERT_DIR/privkey.pem \
            -out $CERT_DIR/fullchain.pem -days 1 -nodes \
            -subj "/CN=${DOMAIN}" >/dev/null 2>&1
    fi
    nginx -t
    systemctl restart nginx

    # Before invoking certbot, nuke the self-signed stub. certbot refuses
    # to write into a pre-existing /etc/letsencrypt/live/<domain>/ dir
    # (errors with "live directory exists for …"), so we detect a stub by
    # comparing issuer == subject (self-signed) and remove it. Real LE
    # certs have issuer="Let's Encrypt" which never matches a CN-only
    # self-signed.
    if [[ -f $CERT_DIR/fullchain.pem ]]; then
        STUB_ISSUER=$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -issuer 2>/dev/null | sed 's/^issuer=//')
        STUB_SUBJECT=$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -subject 2>/dev/null | sed 's/^subject=//')
        if [[ "$STUB_ISSUER" == "$STUB_SUBJECT" ]]; then
            log "removing self-signed stub so certbot can issue a real cert"
            rm -rf "$CERT_DIR" \
                   "/etc/letsencrypt/archive/${DOMAIN}" \
                   "/etc/letsencrypt/renewal/${DOMAIN}.conf"
        fi
    fi

    log "issuing Let's Encrypt cert"
    certbot certonly --webroot -w /var/www/letsencrypt -d "${DOMAIN}" \
        --non-interactive --agree-tos --register-unsafely-without-email --keep-until-expiring
    systemctl restart nginx
fi

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
