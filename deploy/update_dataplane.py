#!/usr/bin/env python3
"""
Retrofit the XHTTP (VLESS SplitHTTP) data plane onto existing lightxray nodes —
idempotently, restarting xray ONLY when the rendered config actually changes.

Fresh nodes already get XHTTP from install.sh. This tool brings already-installed
nodes to the same state (older nodes have no vless-xhttp-in inbound; some have it
but with the literal __LX_VLESS_XHTTP_PATH__ placeholder because LX_VLESS_XHTTP_PATH
was never generated). Run it before toggling XHTTP on in the pool/panel.

Per node it:
  - SKIPS anything that isn't a lightxray box (no /opt/lightxray-src + config.env
    + xray template) — so non-lightxray servers are never touched
  - git-resets /opt/lightxray-src to origin/main so the xray template is current
    (does NOT rebuild lightxrayd — that's update_lightxray.py's job)
  - ensures LX_VLESS_XHTTP_PATH in config.env (generates one, install.sh shape,
    if missing) and that LX_XRAY_INBOUND_TAG lists vless-xhttp-in (so lightxrayd
    hydrates users into the xhttp inbound)
  - re-renders /usr/local/etc/xray/config.json from the template (full placeholder
    set incl. XHTTP); guarded against leftover __LX_ placeholders + invalid JSON;
    installs + restarts xray ONLY if the file actually changed
  - (re)writes the nginx XHTTP snippet for the current path (idempotent: only when
    missing/stale), nginx -t + reload with auto-revert
  - restarts lightxrayd only if config.env changed (re-hydrates users)

Already-correct nodes change nothing and restart nothing (zero customer impact).
Nodes that gain XHTTP see one brief xray restart (a short data-plane blip).

Usage:
    pip install paramiko
    python update_dataplane.py --only k03,k64          # a subset (test first!)
    python update_dataplane.py --parallel 12
    python update_dataplane.py --servers servers-install.json --expect <sha>
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import socket
import sys
from pathlib import Path

try:
    import paramiko
except ImportError:
    sys.exit("paramiko is required:  pip install paramiko")

# The remote retrofit. Prints exactly one machine-readable RESULT| line.
REMOTE = r'''
set -u
SRC=/opt/lightxray-src
CONF=/etc/lightxray/config.env
XRAY_JSON=/usr/local/etc/xray/config.json
TMPL="$SRC/deploy/xray-config.json.tmpl"
SNIP=/etc/nginx/snippets/lightxray-xhttp.conf
if [ ! -d "$SRC/.git" ] || [ ! -f "$CONF" ] || [ ! -x /usr/local/bin/lightxrayd ] || [ ! -f "$TMPL" ]; then
  echo "RESULT|skip|not-a-lightxray-node|||"; exit 0
fi
git -C "$SRC" fetch --quiet origin 2>/dev/null && git -C "$SRC" reset --hard origin/main --quiet 2>/dev/null || { echo "RESULT|fail|git-update-failed|||"; exit 0; }
SHA=$(git -C "$SRC" rev-parse --short HEAD 2>/dev/null)
TMPL="$SRC/deploy/xray-config.json.tmpl"
[ -f "$TMPL" ] || { echo "RESULT|fail|no-xray-template|$SHA||"; exit 0; }

set -a; . "$CONF" 2>/dev/null; set +a
CH=""

# 1. ensure LX_VLESS_XHTTP_PATH (install.sh shape) ---------------------------
if [ -z "${LX_VLESS_XHTTP_PATH:-}" ]; then
  LX_VLESS_XHTTP_PATH="/$(openssl rand -base64 22 | tr -d '=' | tr '+/' '-_' | head -c 22)"
  if grep -q '^LX_VLESS_XHTTP_PATH=' "$CONF"; then
    sed -i "s|^LX_VLESS_XHTTP_PATH=.*|LX_VLESS_XHTTP_PATH=${LX_VLESS_XHTTP_PATH}|" "$CONF"
  else
    echo "LX_VLESS_XHTTP_PATH=${LX_VLESS_XHTTP_PATH}" >> "$CONF"
  fi
  CH="${CH}path,"
fi

# 2. ensure vless-xhttp-in is in LX_XRAY_INBOUND_TAG -------------------------
case ",${LX_XRAY_INBOUND_TAG:-}," in
  *,vless-xhttp-in,*) : ;;
  *)
    NEWTAG="${LX_XRAY_INBOUND_TAG:-vless-ws-in},vless-xhttp-in"; NEWTAG="${NEWTAG#,}"
    if grep -q '^LX_XRAY_INBOUND_TAG=' "$CONF"; then
      sed -i "s|^LX_XRAY_INBOUND_TAG=.*|LX_XRAY_INBOUND_TAG=${NEWTAG}|" "$CONF"
    else
      echo "LX_XRAY_INBOUND_TAG=${NEWTAG}" >> "$CONF"
    fi
    LX_XRAY_INBOUND_TAG="$NEWTAG"; CH="${CH}tag,"
    ;;
esac

# re-source so renders use fresh values
set -a; . "$CONF" 2>/dev/null; set +a

# 3. re-render xray config from template; install + restart ONLY if changed --
sed -e "s|__LX_VLESS_WS_PATH__|${LX_VLESS_WS_PATH:-}|g" \
    -e "s|__LX_VLESS_XHTTP_PATH__|${LX_VLESS_XHTTP_PATH}|g" \
    -e "s|__LX_VLESS_GRPC_SERVICE__|${LX_VLESS_GRPC_SERVICE:-}|g" \
    -e "s|__LX_REALITY_TARGET__|${LX_REALITY_TARGET:-}|g" \
    -e "s|__LX_REALITY_PRIVKEY__|${LX_REALITY_PRIVKEY:-}|g" \
    -e "s|__LX_REALITY_SHORT_ID__|${LX_REALITY_SHORT_ID:-}|g" \
    "$TMPL" > "${XRAY_JSON}.new" 2>/dev/null
if grep -q '__LX_' "${XRAY_JSON}.new"; then echo "RESULT|fail|xray-render-leftover-placeholder|$SHA||"; rm -f "${XRAY_JSON}.new"; exit 0; fi
if command -v python3 >/dev/null 2>&1; then
  python3 -c "import json; json.load(open('${XRAY_JSON}.new'))" 2>/dev/null || { echo "RESULT|fail|xray-render-invalid-json|$SHA||"; rm -f "${XRAY_JSON}.new"; exit 0; }
fi
XRAY_RESTART=0
if [ ! -f "$XRAY_JSON" ] || ! cmp -s "${XRAY_JSON}.new" "$XRAY_JSON"; then
  chown xray:xray "${XRAY_JSON}.new" 2>/dev/null || true
  chmod 640 "${XRAY_JSON}.new"
  mv -f "${XRAY_JSON}.new" "$XRAY_JSON"
  XRAY_RESTART=1; CH="${CH}xray,"
else
  rm -f "${XRAY_JSON}.new"
fi

# 4. nginx XHTTP snippet — idempotent on the current path -------------------
NG=nochange
if [ ! -f "$SNIP" ] || ! grep -qF "location ${LX_VLESS_XHTTP_PATH} {" "$SNIP" || ! grep -qF "proxy_pass http://127.0.0.1:10002;" "$SNIP"; then
  cp -af "$SNIP" "${SNIP}.lxbak" 2>/dev/null || true
  mkdir -p /etc/nginx/snippets
  cat > "$SNIP" <<XEOF
# lightxray XHTTP (VLESS SplitHTTP) -> vless-xhttp-in. Managed by update_dataplane.py.
location ${LX_VLESS_XHTTP_PATH} {
    proxy_pass http://127.0.0.1:10002;
    proxy_http_version 1.1;
    proxy_set_header Host \$host;
    proxy_set_header X-Real-IP \$remote_addr;
    proxy_set_header X-Forwarded-For \$proxy_add_x_forwarded_for;
    proxy_set_header Connection "";
    proxy_buffering off;
    proxy_request_buffering off;
    client_max_body_size 0;
    proxy_read_timeout 86400s;
    proxy_send_timeout 86400s;
}
XEOF
  if nginx -t 2>/dev/null; then systemctl reload nginx; NG=reload; CH="${CH}nginx,"
  else
    if [ -f "${SNIP}.lxbak" ]; then mv -f "${SNIP}.lxbak" "$SNIP"; else rm -f "$SNIP"; fi
    nginx -t 2>/dev/null && systemctl reload nginx 2>/dev/null
    echo "RESULT|fail|nginx-test-failed-reverted|$SHA||"; exit 0
  fi
  rm -f "${SNIP}.lxbak"
fi

# 5. restart services as needed (xray before lightxrayd so the inbound exists
#    before lightxrayd hydrates users into it) -------------------------------
[ "$XRAY_RESTART" = "1" ] && systemctl restart xray
case "$CH" in *path,*|*tag,*) systemctl restart lightxrayd ;; esac
sleep 2
echo "RESULT|ok|$SHA|changed:${CH:-none}|xray:$(systemctl is-active xray),lxd:$(systemctl is-active lightxrayd)|nginx:$(systemctl is-active nginx),path:${LX_VLESS_XHTTP_PATH}"
'''


def load_servers(path: str) -> list[dict]:
    out = []
    for entry in json.loads(Path(path).read_text()):
        if not isinstance(entry, dict) or not entry:
            continue
        label, info = next(iter(entry.items()))
        ip = (info.get("ip") or "").strip()
        if not ip:
            continue
        out.append({"label": label, "ip": ip, "username": info.get("username", "root"),
                    "password": info.get("password"),
                    "key_file": info.get("key_file") or info.get("pem")})
    return out


def ssh_connect(srv: dict, timeout: int = 20) -> paramiko.SSHClient:
    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    kw = dict(hostname=srv["ip"], username=srv["username"],
              timeout=timeout, banner_timeout=timeout, auth_timeout=timeout)
    if srv.get("key_file"):
        kw["key_filename"] = os.path.expanduser(srv["key_file"])
    else:
        kw["password"] = srv["password"]
        kw["look_for_keys"] = False
        kw["allow_agent"] = False
    cli.connect(**kw)
    return cli


def run(cli: paramiko.SSHClient, cmd: str, timeout: int = 180) -> str:
    chan = cli.get_transport().open_session()
    chan.settimeout(timeout)
    chan.exec_command(cmd)
    buf = b""
    try:
        while True:
            data = chan.recv(65536)
            if not data:
                break
            buf += data
    except socket.timeout:
        return buf.decode("utf-8", "replace") + "\nRESULT|fail|timeout|||"
    return buf.decode("utf-8", "replace")


def update_one(srv: dict) -> dict:
    label, ip = srv["label"], srv["ip"]
    res = {"label": label, "ip": ip, "status": "fail", "detail": "", "a": "", "b": "", "c": ""}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["detail"] = f"ssh:{type(e).__name__}"
        return res
    try:
        out = run(cli, REMOTE)
    except Exception as e:  # noqa: BLE001
        res["detail"] = f"exec:{e}"
        return res
    finally:
        cli.close()
    line = next((l for l in out.splitlines() if l.startswith("RESULT|")), None)
    if not line:
        res["detail"] = "no-result (" + out.strip().replace("\n", " ")[-80:] + ")"
        return res
    parts = (line.split("|") + ["", "", "", "", "", ""])[:6]
    res["status"], res["detail"], res["a"], res["b"], res["c"] = parts[1], parts[2], parts[3], parts[4], parts[5]
    return res


def main() -> int:
    ap = argparse.ArgumentParser(description="Idempotently retrofit the XHTTP data plane onto lightxray nodes.")
    ap.add_argument("--servers", default="servers-install.json")
    ap.add_argument("--only", default="")
    ap.add_argument("--parallel", type=int, default=12)
    ap.add_argument("--expect", default="", help="expected short SHA (flag nodes that ended elsewhere)")
    args = ap.parse_args()

    servers = load_servers(args.servers)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]
    if not servers:
        sys.exit("no servers selected")

    print(f"# data-plane (XHTTP) retrofit on {len(servers)} node(s), parallel={args.parallel}", file=sys.stderr)
    order = [s["label"] for s in servers]
    results = []
    done = 0
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        futs = {ex.submit(update_one, s): s for s in servers}
        for fut in concurrent.futures.as_completed(futs):
            r = fut.result()
            results.append(r)
            done += 1
            tag = {"ok": "ok  ", "skip": "skip", "fail": "FAIL"}.get(r["status"], "????")
            extra = r["detail"] if r["status"] != "ok" else f'{r["detail"]} {r["a"]} {r["b"]} {r["c"]}'
            print(f"[{done:3}/{len(servers)}] {tag} {r['label']:6} {r['ip']:16} {extra}", file=sys.stderr)

    results.sort(key=lambda r: order.index(r["label"]))
    ok = [r for r in results if r["status"] == "ok"]
    skip = [r for r in results if r["status"] == "skip"]
    fail = [r for r in results if r["status"] not in ("ok", "skip")]

    print("\n================= SUMMARY =================")
    print(f"ok={len(ok)}  skipped(non-lightxray)={len(skip)}  failed={len(fail)}")
    changed = [r for r in ok if r["a"] and r["a"] != "changed:none"]
    print(f"changed (xray/config retrofit applied)={len(changed)}  already-correct={len(ok) - len(changed)}")
    if args.expect:
        off = [r["label"] for r in ok if r["detail"] != args.expect]
        if off:
            print(f"!! not on expected {args.expect}: {', '.join(off)}")
    if skip:
        print("\nSKIPPED (not lightxray):")
        for r in skip:
            print(f"  {r['label']:6} {r['ip']:16} {r['detail']}")
    if fail:
        print("\nFAILED:")
        for r in fail:
            print(f"  {r['label']:6} {r['ip']:16} {r['detail']}")
    return 0 if not fail else 1


if __name__ == "__main__":
    raise SystemExit(main())
