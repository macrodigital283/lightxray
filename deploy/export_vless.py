#!/usr/bin/env python3
"""
Export one VLESS key from each LightXray server.

For each server in the servers JSON this SSHes in, ensures a small "sample"
user exists (idempotent — a fixed UUID that lightxray honors, so re-runs
reuse the same user instead of piling up), fetches that user's subscription,
and prints ONE vless:// key per server. The key's #fragment is set to the
server label so they're easy to tell apart once imported into a client.
Reality is preferred by default (direct to the box — best for testing).

The sample user persists so the key keeps working. To revoke it, delete the
"sample" user from that node's dashboard. Defaults: 50 GB / 365 days.

stdout = the keys (paste-ready / pipe-able); diagnostics go to stderr.

Usage:
    pip install paramiko
    python export_vless.py servers-install.json
    python export_vless.py servers.json --prefer ws --output keys.txt
    python export_vless.py servers.json --only k47,k51 --name probe

Requires: Python 3.8+, paramiko.
"""

from __future__ import annotations

import argparse
import base64
import concurrent.futures
import json
import os
import sys
from pathlib import Path
from urllib.parse import quote

try:
    import paramiko
except ImportError:
    sys.exit("paramiko is required:  pip install paramiko")

# Stable UUID for the sample user (valid v4 shape). lightxray honors a posted
# uuid, so re-runs reuse the same user/key. Override with --uuid.
DEFAULT_UUID = "5a3f1e57-0000-4000-8000-000000000001"

# Runs on the box: ensure the sample user (idempotent), then emit its sub.
REMOTE = '''
set -a; . /etc/lightxray/config.env; set +a
A=$LX_ADMIN_PROXY_PATH; U=$LX_ADMIN_UUID; C=$LX_CLIENT_PROXY_PATH
POST=$(curl -sk -o /dev/null -w '%{http_code}' -X POST "https://127.0.0.1/$A/api/v2/admin/user/" \
  -H "Hiddify-API-Key: $U" -H "Content-Type: application/json" \
  -d '{"uuid":"%UUID%","name":"%NAME%","package_days":365,"usage_limit_GB":50}')
echo "POST=$POST"
echo "SUB<<<"
curl -sk "https://127.0.0.1/$C/%UUID%/sub"
'''


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
    return text


def pick_key(decoded: str, prefer: str, label: str) -> str | None:
    lines = [l.strip() for l in decoded.splitlines() if l.strip().startswith("vless://")]
    if not lines:
        return None
    chosen = None
    if prefer == "reality":
        chosen = next((l for l in lines if "security=reality" in l), None)
    elif prefer == "ws":
        chosen = next((l for l in lines if "type=ws" in l), None)
    chosen = chosen or lines[0]
    base = chosen.split("#", 1)[0]
    return f"{base}#{quote(label)}"  # relabel by server


def export_one(srv: dict, uuid: str, name: str, prefer: str) -> dict:
    label, ip = srv["label"], srv["ip"]
    res = {"label": label, "ip": ip, "key": None}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["error"] = f"ssh: {e}"
        return res
    try:
        out = run(cli, REMOTE.replace("%UUID%", uuid).replace("%NAME%", name))
    except Exception as e:  # noqa: BLE001
        res["error"] = f"exec: {e}"
        return res
    finally:
        cli.close()

    if "POST=" in out:
        code = out.split("POST=", 1)[1].splitlines()[0].strip()
        if code != "200":
            res["error"] = f"create sample user failed (HTTP {code})"
            return res
    if "SUB<<<" not in out:
        res["error"] = "no sub output — config.env unreadable or dashboard down"
        return res
    b64 = out.split("SUB<<<", 1)[1].strip()
    try:
        decoded = base64.b64decode(b64 + "=" * (-len(b64) % 4)).decode("utf-8", "replace")
    except Exception as e:  # noqa: BLE001
        res["error"] = f"decode: {e}"
        return res
    key = pick_key(decoded, prefer, label)
    if not key:
        res["error"] = "no vless key in sub (no CDN host / Reality configured?)"
        return res
    res["key"] = key
    return res


def main() -> int:
    ap = argparse.ArgumentParser(description="Export one VLESS key per LightXray server.")
    ap.add_argument("servers", nargs="?", default="servers-install.json")
    ap.add_argument("--prefer", choices=["reality", "ws", "first"], default="reality",
                    help="which key to export when the sub has several (default reality)")
    ap.add_argument("--uuid", default=DEFAULT_UUID, help="sample user UUID (kept idempotent across runs)")
    ap.add_argument("--name", default="sample", help="sample user name (default 'sample')")
    ap.add_argument("--only", default="", help="comma-separated labels to limit to")
    ap.add_argument("--output", help="also write the keys to this file")
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
        futs = [ex.submit(export_one, s, args.uuid, args.name, args.prefer) for s in servers]
        for fut in concurrent.futures.as_completed(futs):
            results.append(fut.result())
    order = [s["label"] for s in servers]
    results.sort(key=lambda r: order.index(r["label"]))

    keys = [r["key"] for r in results if r["key"]]
    print("\n".join(keys))  # stdout: the keys only

    for r in results:
        status = "ok" if r["key"] else "FAIL: " + r.get("error", "")
        print(f"#  {r['label']:6} {r['ip']:16} {status}", file=sys.stderr)
    print(f"# {len(keys)}/{len(results)} keys exported (prefer={args.prefer})", file=sys.stderr)

    if args.output and keys:
        Path(args.output).write_text("\n".join(keys) + "\n")
        print(f"# written to {args.output}", file=sys.stderr)

    return 0 if len(keys) == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
