#!/usr/bin/env bash
# lightxray bare-metal installer for Ubuntu 22.04/24.04.
#
# Installs the full single-domain topology:
#
#   :443 ── nginx stream (ssl_preread SNI router)
#             ├─ SNI = REALITY_TARGET (default www.google.com) → xray Reality :7443
#             └─ default (mgmt domain + any future CDN host)   → nginx http :8443
#                                                                  ├ admin API / UI
#                                                                  ├ sub fetch
#                                                                  └ WS / gRPC data plane
#   :80  ── ACME challenge + redirect
#
# Everything xray serves binds to loopback; the public socket is nginx's.
# Reality runs on the standard :443 (best DPI evasion). Adding CDN hosts
# later is a dashboard-only action — they hit the stream default route, so
# NO nginx change is ever needed for a new CDN domain.
#
# Re-run safe: reuses admin UUID / proxy paths / pg password / WS token /
# Reality keypair from the existing /etc/lightxray/config.env. RESET=1
# wipes the DB + regenerates everything.
#
# Required env:
#   DOMAIN=node1.example.com          # management / grey-cloud domain
# Optional env (auto-generated / defaulted if absent):
#   REALITY_TARGET=www.google.com     # SNI the Reality handshake mimics
#   ADMIN_UUID, ADMIN_PROXY_PATH, CLIENT_PROXY_PATH, PG_PASSWORD,
#   VLESS_WS_PATH, VLESS_GRPC_SERVICE, GO_VERSION
#
# Usage:
#   DOMAIN=node1.example.com bash deploy/install.sh
#   DOMAIN=node1.example.com bash <(curl -fsSL https://raw.githubusercontent.com/macrodigital283/lightxray/main/deploy/install.sh)
set -euo pipefail

DOMAIN="${DOMAIN:-}"
[[ -z "$DOMAIN" ]] && { echo "DOMAIN env var required" >&2; exit 1; }

REPO_URL="${REPO_URL:-https://github.com/macrodigital283/lightxray}"
REPO_REF="${REPO_REF:-main}"
SRC_DIR="${SRC_DIR:-/opt/lightxray-src}"
RESET="${RESET:-0}"
REALITY_TARGET="${REALITY_TARGET:-www.google.com}"

if [[ $EUID -ne 0 ]]; then echo "must run as root" >&2; exit 1; fi
log() { printf '\033[1;36m==>\033[0m %s\n' "$*"; }

# ── reuse prior secrets on re-run (idempotency) ───────────────────────
if [[ "$RESET" != "1" && -f /etc/lightxray/config.env ]]; then
    # shellcheck disable=SC1091
    source /etc/lightxray/config.env
    ADMIN_UUID="${ADMIN_UUID:-${LX_ADMIN_UUID:-}}"
    ADMIN_PROXY_PATH="${ADMIN_PROXY_PATH:-${LX_ADMIN_PROXY_PATH:-}}"
    CLIENT_PROXY_PATH="${CLIENT_PROXY_PATH:-${LX_CLIENT_PROXY_PATH:-}}"
    VLESS_WS_PATH="${VLESS_WS_PATH:-${LX_VLESS_WS_PATH:-}}"
    VLESS_GRPC_SERVICE="${VLESS_GRPC_SERVICE:-${LX_VLESS_GRPC_SERVICE:-}}"
    REALITY_PUBKEY="${REALITY_PUBKEY:-${LX_REALITY_PUBKEY:-}}"
    REALITY_PRIVKEY="${REALITY_PRIVKEY:-${LX_REALITY_PRIVKEY:-}}"
    REALITY_SHORT_ID="${REALITY_SHORT_ID:-${LX_REALITY_SHORT_ID:-}}"
    REALITY_TARGET="${LX_REALITY_TARGET:-$REALITY_TARGET}"
    if [[ -n "${LX_DATABASE_URL:-}" ]]; then
        PG_PASSWORD="${PG_PASSWORD:-$(printf '%s' "$LX_DATABASE_URL" | sed -E 's|.*lightxray:([^@]+)@.*|\1|')}"
    fi
fi
ADMIN_UUID="${ADMIN_UUID:-$(cat /proc/sys/kernel/random/uuid)}"
ADMIN_PROXY_PATH="${ADMIN_PROXY_PATH:-$(openssl rand -hex 12)}"
CLIENT_PROXY_PATH="${CLIENT_PROXY_PATH:-$(openssl rand -hex 12)}"
PG_PASSWORD="${PG_PASSWORD:-$(openssl rand -hex 20)}"
# Hiddify-shape short shared WS token (22 base64url chars).
VLESS_WS_PATH="${VLESS_WS_PATH:-/$(openssl rand -base64 22 | tr -d '=' | tr '+/' '-_' | head -c 22)}"
VLESS_GRPC_SERVICE="${VLESS_GRPC_SERVICE:-GunService}"
REALITY_SHORT_ID="${REALITY_SHORT_ID:-$(openssl rand -hex 8)}"

# ── 1. system deps ────────────────────────────────────────────────────
log "installing system packages"
DEBIAN_FRONTEND=noninteractive apt-get update -qq
DEBIAN_FRONTEND=noninteractive apt-get install -y -qq \
    git curl ca-certificates nginx certbot \
    postgresql postgresql-contrib build-essential

GO_VERSION="${GO_VERSION:-1.25.0}"
if ! /usr/local/go/bin/go version 2>/dev/null | grep -q "go${GO_VERSION}"; then
    log "installing go ${GO_VERSION} from go.dev"
    rm -rf /usr/local/go
    curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-amd64.tar.gz" | tar -C /usr/local -xzf -
fi
ln -sf /usr/local/go/bin/go /usr/local/bin/go

if ! command -v xray >/dev/null 2>&1; then
    log "installing xray-core"
    bash <(curl -fsSL https://github.com/XTLS/Xray-install/raw/main/install-release.sh) install >/dev/null
fi

# ── 2. service users + dirs ───────────────────────────────────────────
id -u lightxray >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin lightxray
id -u xray      >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin xray
mkdir -p /var/log/lightxray /var/log/xray /etc/lightxray /usr/local/etc/xray \
         /var/www/letsencrypt /etc/nginx/stream-conf.d
chown lightxray:lightxray /var/log/lightxray
chown -R xray:xray /var/log/xray

# ── 3. source + build ─────────────────────────────────────────────────
if [[ -d "$SRC_DIR/.git" ]]; then
    log "updating checkout in $SRC_DIR"
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

# ── 4. Reality keypair (generate once, reuse on re-run) ───────────────
if [[ -z "${REALITY_PRIVKEY:-}" || -z "${REALITY_PUBKEY:-}" ]]; then
    log "generating Reality x25519 keypair"
    KP=$(/usr/local/bin/xray x25519)
    REALITY_PRIVKEY=$(echo "$KP" | awk -F': ' '/Private/ {print $2}')
    REALITY_PUBKEY=$(echo "$KP"  | awk -F': ' '/Public/  {print $2}')
fi

# ── 5. postgres ───────────────────────────────────────────────────────
log "provisioning postgres"
sudo -u postgres psql -tAc "SELECT 1 FROM pg_roles WHERE rolname='lightxray'" | grep -q 1 \
    || sudo -u postgres psql -c "CREATE ROLE lightxray LOGIN PASSWORD '$PG_PASSWORD'"
# keep role password in sync with whatever we're about to write to config.env
sudo -u postgres psql -c "ALTER ROLE lightxray PASSWORD '$PG_PASSWORD'" >/dev/null
sudo -u postgres psql -tAc "SELECT 1 FROM pg_database WHERE datname='lightxray'" | grep -q 1 \
    || sudo -u postgres createdb -O lightxray lightxray
if [[ "$RESET" == "1" ]]; then
    log "RESET=1 — wiping lightxray DB"
    sudo -u postgres psql -c "DROP DATABASE lightxray"
    sudo -u postgres createdb -O lightxray lightxray
fi

# ── 6. config.env ─────────────────────────────────────────────────────
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
LX_VLESS_GRPC_SERVICE=${VLESS_GRPC_SERVICE}
LX_XRAY_GRPC_ADDR=127.0.0.1:10085
LX_XRAY_INBOUND_TAG=vless-ws-in,vless-grpc-in,vless-reality-in
LX_RECONCILE_PERIOD=60s
LX_REALITY_TARGET=${REALITY_TARGET}
LX_REALITY_PUBKEY=${REALITY_PUBKEY}
LX_REALITY_PRIVKEY=${REALITY_PRIVKEY}
LX_REALITY_SHORT_ID=${REALITY_SHORT_ID}
EOF
chmod 640 /etc/lightxray/config.env
chown root:lightxray /etc/lightxray/config.env

# ── 7. xray config (loopback inbounds; Reality on 127.0.0.1:7443) ─────
log "writing /usr/local/etc/xray/config.json"
sed -e "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    -e "s|__LX_VLESS_GRPC_SERVICE__|${VLESS_GRPC_SERVICE}|g" \
    -e "s|__LX_REALITY_TARGET__|${REALITY_TARGET}|g" \
    -e "s|__LX_REALITY_PRIVKEY__|${REALITY_PRIVKEY}|g" \
    -e "s|__LX_REALITY_SHORT_ID__|${REALITY_SHORT_ID}|g" \
    "$SRC_DIR/deploy/xray-config.json.tmpl" > /usr/local/etc/xray/config.json
chown xray:xray /usr/local/etc/xray/config.json
chmod 640 /usr/local/etc/xray/config.json

log "installing systemd units"
install -m 0644 "$SRC_DIR/deploy/systemd/lightxrayd.service" /etc/systemd/system/
install -m 0644 "$SRC_DIR/deploy/systemd/xray.service"        /etc/systemd/system/xray.service
systemctl daemon-reload

# ── 8. nginx http backend + stream SNI router ────────────────────────
log "writing nginx http backend (127.0.0.1:8443) + stream router (:443)"
mkdir -p /etc/nginx/conf.d
[[ -f /etc/nginx/conf.d/default.conf ]] && \
    mv /etc/nginx/conf.d/default.conf /etc/nginx/conf.d/default.conf.disabled-by-lightxray
sed -e "s|__LX_DOMAIN__|${DOMAIN}|g" \
    -e "s|__LX_ADMIN_PROXY_PATH__|${ADMIN_PROXY_PATH}|g" \
    -e "s|__LX_CLIENT_PROXY_PATH__|${CLIENT_PROXY_PATH}|g" \
    -e "s|__LX_VLESS_WS_PATH__|${VLESS_WS_PATH}|g" \
    -e "s|__LX_VLESS_GRPC_SERVICE__|${VLESS_GRPC_SERVICE}|g" \
    "$SRC_DIR/deploy/nginx/lightxray.conf.tmpl" \
    > /etc/nginx/conf.d/lightxray-http.conf
sed -e "s|__LX_REALITY_TARGET__|${REALITY_TARGET}|g" \
    "$SRC_DIR/deploy/nginx/stream-sni-router.conf.tmpl" \
    > /etc/nginx/stream-conf.d/sni-router.conf
# Wire the top-level stream {} include into nginx.conf once.
if ! grep -q "stream-conf.d" /etc/nginx/nginx.conf; then
    cat >> /etc/nginx/nginx.conf <<'NGX'

stream {
    include /etc/nginx/stream-conf.d/*.conf;
}
NGX
fi

# ── 9. TLS cert ───────────────────────────────────────────────────────
CERT_DIR="/etc/letsencrypt/live/${DOMAIN}"
mkdir -p "$CERT_DIR"
if [[ ! -f $CERT_DIR/fullchain.pem ]]; then
    openssl req -x509 -newkey rsa:2048 -keyout "$CERT_DIR/privkey.pem" \
        -out "$CERT_DIR/fullchain.pem" -days 1 -nodes -subj "/CN=${DOMAIN}" >/dev/null 2>&1
fi
nginx -t
systemctl restart nginx
# remove the self-signed stub so certbot can lay a real lineage
if [[ -f $CERT_DIR/fullchain.pem ]]; then
    SI=$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -issuer 2>/dev/null | sed 's/^issuer=//')
    SS=$(openssl x509 -in "$CERT_DIR/fullchain.pem" -noout -subject 2>/dev/null | sed 's/^subject=//')
    if [[ "$SI" == "$SS" ]]; then
        rm -rf "$CERT_DIR" "/etc/letsencrypt/archive/${DOMAIN}" "/etc/letsencrypt/renewal/${DOMAIN}.conf"
    fi
fi
log "issuing Let's Encrypt cert (needs DNS-only / grey-cloud for HTTP-01)"
certbot certonly --webroot -w /var/www/letsencrypt -d "${DOMAIN}" \
    --non-interactive --agree-tos --register-unsafely-without-email --keep-until-expiring
systemctl restart nginx

# ── 10. start services ────────────────────────────────────────────────
log "enabling + starting services"
systemctl enable --now xray.service
systemctl enable --now lightxrayd.service
sleep 3   # let lightxrayd migrate the DB before we seed settings

# ── 11. seed Reality + WS-path settings into the DB ──────────────────
log "seeding Reality + WS-path settings"
sudo -u postgres psql -d lightxray >/dev/null <<SQL
INSERT INTO settings (key, value) VALUES
  ('reality_enabled', 'true'),
  ('reality_port',    '443'),
  ('reality_target',  '${REALITY_TARGET}'),
  ('reality_pubkey',  '${REALITY_PUBKEY}'),
  ('reality_short_id','${REALITY_SHORT_ID}'),
  ('ws_path_mode',    'shared'),
  ('ws_path_token',   '${VLESS_WS_PATH}')
ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value;
SQL

systemctl is-active --quiet xray && systemctl is-active --quiet lightxrayd && systemctl is-active --quiet nginx \
  && log "all services active" || log "WARNING: a service is not active — check 'systemctl status'"

cat <<EOF

──────────────────────────────────────────────────────────────────────
lightxray installed (single-domain, Reality-on-443 topology).

  Management domain:  https://${DOMAIN}      (sub URL + admin + Reality)
  Reality target:     ${REALITY_TARGET}      (mimicked TLS handshake)
  Admin proxy path:   ${ADMIN_PROXY_PATH}
  Client proxy path:  ${CLIENT_PROXY_PATH}
  Admin UUID:         ${ADMIN_UUID}
  WS token:           ${VLESS_WS_PATH}
  Reality public key: ${REALITY_PUBKEY}
  Reality short id:   ${REALITY_SHORT_ID}

  Dashboard:  https://${DOMAIN}/${ADMIN_PROXY_PATH}/ui/login
  base_url for pool:  https://${DOMAIN}/${ADMIN_PROXY_PATH}

  Right now the bundle carries ONLY Reality (no CDN hosts yet).
  Add CDN domains anytime in the dashboard → /ui/cdn/ — they route
  automatically (no nginx change). DNS-only direct domains: add the
  hostname to the box's nginx server-name is NOT needed; the http
  backend is a catch-all, so any hostname pointed here just works.

  Smoke test:
    curl -H "Hiddify-API-Key: ${ADMIN_UUID}" \\
         https://${DOMAIN}/${ADMIN_PROXY_PATH}/api/v2/admin/me/
──────────────────────────────────────────────────────────────────────
EOF
