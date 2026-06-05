#!/usr/bin/env python3
"""
Print the pool-add URLs for a LightXray fleet.

For each server in the servers JSON, SSHes in and reads the admin proxy path,
admin UUID, and management host straight from /etc/lightxray/config.env, then
prints the URL the pool's "Bulk add APIs" textarea accepts:

    https://<host>/<admin_proxy_path>/<admin_uuid>#<label>

<host> is the box's management domain (LX_PUBLIC_HOST) when set, else its IP.
Reading the live config means the output always matches the box — there's no
stale credentials file to keep in sync.

stdout is just the URLs (paste-ready / pipe-able); diagnostics go to stderr.

Usage:
    pip install paramiko
    python pool_urls.py servers-install.json
    python pool_urls.py servers.json --verify            # auth-check /me/ first
    python pool_urls.py servers.json --output pool-urls.txt
    python pool_urls.py servers.json --host ip           # force IPs, not domains
    python pool_urls.py servers.json --only k47,k51

Requires: Python 3.8+, paramiko.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import sys
from pathlib import Path

try:
    import paramiko
except ImportError:
    sys.exit("paramiko is required:  pip install paramiko")


def load_servers(path: str) -> list[dict]:
    data = json.loads(Path(path).read_text())
    out = []
    for entry in data:
        if not isinstance(entry, dict) or not entry:
            continue
        label, info = next(iter(entry.items()))
        ip = (info.get("ip") or "").strip()
        if not ip:
            continue
        out.append(
            {
                "label": label,
                "ip": ip,
                "username": info.get("username", "root"),
                "password": info.get("password"),
                "key_file": info.get("key_file") or info.get("pem"),
            }
        )
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


def run(cli: paramiko.SSHClient, cmd: str) -> str:
    _in, out, _err = cli.exec_command(cmd, get_pty=False)
    text = "".join(iter(out.readline, ""))
    out.channel.recv_exit_status()
    return text.strip()


# Read the three fields from config.env; optionally auth-check /me/.
REMOTE = r"""
ENV=/etc/lightxray/config.env
echo "HOST=$(grep -oP '^LX_PUBLIC_HOST=\K.*' "$ENV")"
echo "APATH=$(grep -oP '^LX_ADMIN_PROXY_PATH=\K.*' "$ENV")"
echo "UUID=$(grep -oP '^LX_ADMIN_UUID=\K.*' "$ENV")"
if [ -n "%VERIFY%" ]; then
  A=$(grep -oP '^LX_ADMIN_PROXY_PATH=\K.*' "$ENV")
  U=$(grep -oP '^LX_ADMIN_UUID=\K.*' "$ENV")
  echo "ME=$(curl -sk -o /dev/null -w '%{http_code}' --max-time 10 "https://127.0.0.1/$A/api/v2/admin/me/" -H "Hiddify-API-Key: $U")"
fi
"""


def fetch_one(srv: dict, host_pref: str, verify: bool) -> dict:
    label, ip = srv["label"], srv["ip"]
    res = {"label": label, "ip": ip, "url": None, "ok": False, "me": None}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["error"] = f"ssh: {e}"
        return res
    try:
        out = run(cli, REMOTE.replace("%VERIFY%", "1" if verify else ""))
    except Exception as e:  # noqa: BLE001
        res["error"] = f"exec: {e}"
        return res
    finally:
        cli.close()

    f = {}
    for line in out.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            f[k.strip()] = v.strip()
    apath, uuid, pub = f.get("APATH", ""), f.get("UUID", ""), f.get("HOST", "")
    if not (apath and uuid):
        res["error"] = "missing admin path/uuid in /etc/lightxray/config.env"
        return res
    host = ip if host_pref == "ip" else (pub or ip)
    res["host_kind"] = "domain" if (host_pref != "ip" and pub) else "ip"
    res["url"] = f"https://{host}/{apath}/{uuid}#{label}"
    res["me"] = f.get("ME")
    res["ok"] = (not verify) or (res["me"] == "200")
    return res


def main() -> int:
    ap = argparse.ArgumentParser(description="Print pool-add URLs for a LightXray fleet.")
    ap.add_argument("servers", nargs="?", default="servers-install.json")
    ap.add_argument("--host", choices=["auto", "ip"], default="auto",
                    help="auto = management domain when set, else IP (default); ip = always IP")
    ap.add_argument("--verify", action="store_true",
                    help="auth-check each box's /me/ returns 200 before listing it")
    ap.add_argument("--output", help="also write the URLs to this file")
    ap.add_argument("--only", default="", help="comma-separated labels to limit to")
    ap.add_argument("--parallel", type=int, default=8)
    args = ap.parse_args()

    servers = load_servers(args.servers)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]
    if not servers:
        sys.exit("no servers found")

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        futs = [ex.submit(fetch_one, s, args.host, args.verify) for s in servers]
        for fut in concurrent.futures.as_completed(futs):
            results.append(fut.result())
    order = [s["label"] for s in servers]
    results.sort(key=lambda r: order.index(r["label"]))

    urls = [r["url"] for r in results if r["url"] and r["ok"]]

    # stdout: the paste-ready URLs only.
    print("\n".join(urls))

    # stderr: per-box status + summary (keeps stdout clean for piping).
    for r in results:
        if r["url"] and r["ok"]:
            status = f"ok ({r['host_kind']}{', /me/=200' if args.verify else ''})"
        else:
            status = "FAIL: " + (r.get("error") or f"/me/={r.get('me')}")
        print(f"#  {r['label']:6} {r['ip']:16} {status}", file=sys.stderr)
    print(f"# {len(urls)}/{len(results)} URL(s) ready", file=sys.stderr)

    if args.output and urls:
        Path(args.output).write_text("\n".join(urls) + "\n")
        print(f"# written to {args.output}", file=sys.stderr)

    return 0 if len(urls) == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
