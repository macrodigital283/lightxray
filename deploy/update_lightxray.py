#!/usr/bin/env python3
"""
Roll the latest lightxray (origin/main) onto every lightxray node in
servers-install.json — binary + helpers + units + the worker_connections
baseline — with NO customer impact.

Per node it:
  - SKIPS anything that isn't a lightxray box (no /opt/lightxray-src + no
    /etc/lightxray/config.env) — so Hiddify servers are never touched
  - git-resets /opt/lightxray-src to origin/main and rebuilds lightxrayd
  - installs the 4 sudo helpers + sudoers grant + the systemd units +
    nginx CPUWeight drop-in (idempotent)
  - raises nginx worker_connections to the 16384 baseline (RAISE-ONLY, so a
    higher operator value is preserved) + worker_rlimit_nofile, nginx -t,
    graceful reload (auto-revert on failure)
  - applies CPUWeight live (set-property) and restarts ONLY lightxrayd
    (re-hydrates users; xray is NOT restarted, so live connections are fine)

It never restarts xray and never regenerates the customer-facing nginx/xray
config, so customers see at most a ~2s dashboard blip and nothing else.

Usage:
    pip install paramiko
    python update_lightxray.py                      # all nodes
    python update_lightxray.py --only k03,k64       # a subset (test first!)
    python update_lightxray.py --parallel 16
    python update_lightxray.py --servers servers-install.json
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

# The remote update. Prints exactly one machine-readable RESULT| line.
REMOTE = r'''
set -u
SRC=/opt/lightxray-src
if [ ! -d "$SRC/.git" ] || [ ! -f /etc/lightxray/config.env ] || [ ! -x /usr/local/bin/lightxrayd ]; then
  echo "RESULT|skip|not-a-lightxray-node|||"; exit 0
fi
git -C "$SRC" fetch --quiet origin 2>/dev/null && git -C "$SRC" reset --hard origin/main --quiet 2>/dev/null || { echo "RESULT|fail|git-update-failed|||"; exit 0; }
SHA=$(git -C "$SRC" rev-parse --short HEAD)
cd "$SRC"
if ! CGO_ENABLED=0 nice -n 19 /usr/local/bin/go build -trimpath -ldflags="-s -w -X main.version=$SHA" -o /usr/local/bin/lightxrayd.new ./cmd/lightxrayd 2>/tmp/lxbuild.err; then
  echo "RESULT|fail|build:$(tail -1 /tmp/lxbuild.err 2>/dev/null | tr '|' ' ' | cut -c1-80)|$SHA||"; exit 0
fi
chmod 755 /usr/local/bin/lightxrayd.new && mv -f /usr/local/bin/lightxrayd.new /usr/local/bin/lightxrayd
for h in applydomain applypaths applytuning applyadmin; do
  [ -f "$SRC/deploy/lightxray-$h" ] && install -m 0755 "$SRC/deploy/lightxray-$h" /usr/local/bin/lightxray-$h
done
printf 'lightxray ALL=(root) NOPASSWD: /usr/local/bin/lightxray-applydomain, /usr/local/bin/lightxray-applypaths, /usr/local/bin/lightxray-applytuning, /usr/local/bin/lightxray-applyadmin\n' > /etc/sudoers.d/lightxray
chmod 0440 /etc/sudoers.d/lightxray
install -m 0644 "$SRC/deploy/systemd/xray.service"      /etc/systemd/system/xray.service       2>/dev/null
install -m 0644 "$SRC/deploy/systemd/lightxrayd.service" /etc/systemd/system/lightxrayd.service 2>/dev/null
mkdir -p /etc/systemd/system/nginx.service.d
printf '[Service]\nCPUWeight=400\n' > /etc/systemd/system/nginx.service.d/lightxray-cpuweight.conf
systemctl daemon-reload
WC=$(sed -nE 's/^[[:space:]]*worker_connections[[:space:]]+([0-9]+);.*/\1/p' /etc/nginx/nginx.conf | head -1)
cp -af /etc/nginx/nginx.conf /etc/nginx/nginx.conf.lxbak
if [ -z "${WC:-}" ]; then
  sed -i -E 's/^([[:space:]]*events[[:space:]]*\{)/\1\n    worker_connections 16384;/' /etc/nginx/nginx.conf
elif [ "$WC" -lt 16384 ]; then
  sed -i -E 's/^([[:space:]]*)worker_connections[[:space:]]+[0-9]+;/\1worker_connections 16384;/' /etc/nginx/nginx.conf
fi
grep -qE '^[[:space:]]*worker_rlimit_nofile' /etc/nginx/nginx.conf || sed -i '1i worker_rlimit_nofile 65536;' /etc/nginx/nginx.conf
if nginx -t 2>/dev/null; then systemctl reload nginx; NG=ok; else cp -af /etc/nginx/nginx.conf.lxbak /etc/nginx/nginx.conf; systemctl reload nginx 2>/dev/null; NG=reverted; fi
systemctl set-property xray.service CPUWeight=400 2>/dev/null || true
systemctl set-property nginx.service CPUWeight=400 2>/dev/null || true
systemctl set-property lightxrayd.service CPUWeight=40 2>/dev/null || true
systemctl restart lightxrayd; sleep 3
WCF=$(sed -nE 's/^[[:space:]]*worker_connections[[:space:]]+([0-9]+);.*/\1/p' /etc/nginx/nginx.conf | head -1)
echo "RESULT|ok|$SHA|wc:${WC:-?}->$WCF,ng:$NG|lxd:$(systemctl is-active lightxrayd),nr:$(systemctl show -p NRestarts --value lightxrayd)|nginx:$(systemctl is-active nginx),xray:$(systemctl is-active xray)"
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


def run(cli: paramiko.SSHClient, cmd: str, timeout: int = 240) -> str:
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
    ap = argparse.ArgumentParser(description="Roll latest lightxray (origin/main) to all lightxray nodes.")
    ap.add_argument("--servers", default="servers-install.json")
    ap.add_argument("--only", default="")
    ap.add_argument("--parallel", type=int, default=16)
    ap.add_argument("--expect", default="", help="expected short SHA (flag nodes that ended elsewhere)")
    args = ap.parse_args()

    servers = load_servers(args.servers)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]
    if not servers:
        sys.exit("no servers selected")

    print(f"# updating {len(servers)} node(s), parallel={args.parallel}", file=sys.stderr)
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
    print(f"updated(ok)={len(ok)}  skipped(non-lightxray/other)={len(skip)}  failed={len(fail)}")
    shas = {}
    for r in ok:
        shas[r["detail"]] = shas.get(r["detail"], 0) + 1
    print("ended-on SHA:", ", ".join(f"{k}×{v}" for k, v in shas.items()) or "-")
    if args.expect:
        off = [r["label"] for r in ok if r["detail"] != args.expect]
        if off:
            print(f"!! not on expected {args.expect}: {', '.join(off)}")
    if skip:
        print("\nSKIPPED (not lightxray or other):")
        for r in skip:
            print(f"  {r['label']:6} {r['ip']:16} {r['detail']}")
    if fail:
        print("\nFAILED:")
        for r in fail:
            print(f"  {r['label']:6} {r['ip']:16} {r['detail']}")
    return 0 if not fail else 1


if __name__ == "__main__":
    raise SystemExit(main())
