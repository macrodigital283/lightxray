#!/usr/bin/env python3
"""
Change the client path + WS path on LightXray servers.

Reads SSH targets from servers-install.json and the desired paths from
lightxray_ws_client_paths.json (both keyed by the SAME server label), then runs
each node's `lightxray-applypaths` helper to rotate its client/WS path.

  servers-install.json          lightxray_ws_client_paths.json
  [                             {
    {"h01": {                     "h01": {
       "ip": "1.2.3.4",             "ws_path":     "/BcF12S1ZvOBjYC2",
       "username": "root",          "client_path": "USyWOuHYfiUtVYDeiWkX"
       "password": "..." }},      },
    ...                           ...
  ]                             }

The label (the dict key) in servers-install.json must match a key in the paths
file. Servers with no matching paths entry are skipped (reported, not changed).

⚠  DISRUPTIVE. Rotating these paths INVALIDATES every existing customer key/sub
   on that node until the client refetches, and the pool must re-discover the
   client path. Changing the WS path also restarts xray (a brief data-plane
   blip). The client-path-only change does not restart xray.

Safe by default: with no flags it only PREVIEWS (reads current paths over SSH
and shows what would change). Pass --apply to actually rotate; it asks you to
type APPLY to confirm unless you also pass --yes.

Usage:
    pip install paramiko
    python change_paths.py                       # preview every server in servers-install.json
    python change_paths.py --only h01,h02        # preview a subset
    python change_paths.py --apply               # rotate (asks to confirm)
    python change_paths.py --apply --yes         # rotate, no prompt (automation)
    python change_paths.py --servers servers-install.json --paths lightxray_ws_client_paths.json

Requires: Python 3.8+, paramiko. Needs each node on a build that has the
lightxray-applypaths helper (the "Paths" feature / v1+); nodes without it are
reported so you can update them first.
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

# Read current client/WS path + whether the helper is present. Read-only.
REMOTE_READ = r'''
set -u
if [ -x /usr/local/bin/lightxray-applypaths ]; then echo "HELPER=yes"; else echo "HELPER=no"; fi
set -a; . /etc/lightxray/config.env 2>/dev/null; set +a
echo "CUR_CLIENT=${LX_CLIENT_PROXY_PATH:-?}"
echo "CUR_WS=${LX_VLESS_WS_PATH:-?}"
'''

# Apply: invoke the privileged helper directly as root (servers-install creds
# are root). The helper validates the paths, rewrites config.env, regenerates
# nginx (+ xray when the WS path changed), updates the DB ws_path_token, and
# reloads/restarts out-of-band. %CLIENT% / %WS% are substituted per server.
REMOTE_APPLY = r'''
set -u
H=/usr/local/bin/lightxray-applypaths
if [ ! -x "$H" ]; then echo "HELPER_MISSING"; exit 9; fi
"$H" "%CLIENT%" "%WS%"
'''


def load_servers(path: str) -> list[dict]:
    """servers-install.json -> [{label, ip, username, password, key_file}]."""
    data = json.loads(Path(path).read_text())
    out = []
    for entry in data:
        if not isinstance(entry, dict) or not entry:
            continue
        label, info = next(iter(entry.items()))
        ip = (info.get("ip") or "").strip()
        if not ip:
            continue
        out.append({
            "label": label,
            "ip": ip,
            "username": info.get("username", "root"),
            "password": info.get("password"),
            "key_file": info.get("key_file") or info.get("pem"),
        })
    return out


def load_paths(path: str) -> dict[str, dict]:
    """lightxray_ws_client_paths.json -> {label: {ws_path, client_path}}."""
    data = json.loads(Path(path).read_text())
    if not isinstance(data, dict):
        sys.exit(f"{path}: expected a JSON object keyed by server label")
    return data


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


def run(cli: paramiko.SSHClient, cmd: str) -> tuple[str, int]:
    _in, out, err = cli.exec_command(cmd, get_pty=False)
    text = out.read().decode("utf-8", "replace") + err.read().decode("utf-8", "replace")
    code = out.channel.recv_exit_status()
    return text, code


def parse_kv(text: str) -> dict[str, str]:
    kv = {}
    for line in text.splitlines():
        if "=" in line:
            k, v = line.split("=", 1)
            kv[k.strip()] = v.strip()
    return kv


def norm_ws(s: str) -> str:
    s = (s or "").strip()
    return s if s.startswith("/") else "/" + s


def plan_one(srv: dict, paths_map: dict) -> dict:
    """Read-only pass: classify each server (no changes made)."""
    label, ip = srv["label"], srv["ip"]
    res = {"label": label, "ip": ip, "status": "?", "msg": ""}

    entry = paths_map.get(label)
    if not entry:
        res["status"] = "skip"
        res["msg"] = "no entry in paths file for this label"
        return res
    client = (entry.get("client_path") or "").strip()
    ws = norm_ws(entry.get("ws_path") or "")
    if not client or ws == "/":
        res["status"] = "skip"
        res["msg"] = "paths entry missing client_path or ws_path"
        return res
    res["new_client"], res["new_ws"] = client, ws

    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["status"] = "error"
        res["msg"] = f"ssh: {e}"
        return res
    try:
        text, _ = run(cli, REMOTE_READ)
    except Exception as e:  # noqa: BLE001
        res["status"] = "error"
        res["msg"] = f"read: {e}"
        return res
    finally:
        cli.close()

    kv = parse_kv(text)
    res["cur_client"] = kv.get("CUR_CLIENT", "?")
    res["cur_ws"] = norm_ws(kv.get("CUR_WS", "?")) if kv.get("CUR_WS", "?") != "?" else "?"
    if kv.get("HELPER") != "yes":
        res["status"] = "error"
        res["msg"] = "lightxray-applypaths not installed (update this node to the Paths build first)"
        return res
    if res["cur_client"] == client and res["cur_ws"] == ws:
        res["status"] = "nochange"
        res["msg"] = "already set to the target paths"
    else:
        res["status"] = "would-change"
    return res


def apply_one(srv: dict, client: str, ws: str) -> dict:
    label, ip = srv["label"], srv["ip"]
    res = {"label": label, "ip": ip}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["status"] = "error"
        res["msg"] = f"ssh: {e}"
        return res
    try:
        out, code = run(cli, REMOTE_APPLY.replace("%CLIENT%", client).replace("%WS%", ws))
    except Exception as e:  # noqa: BLE001
        res["status"] = "error"
        res["msg"] = f"exec: {e}"
        return res
    finally:
        cli.close()
    res["output"] = out.strip()
    if code == 0 and "OK:" in out:
        res["status"] = "applied"
    else:
        res["status"] = "failed"
        res["msg"] = f"helper exit {code}"
    return res


def parallel_map(fn, items, workers):
    out = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, workers)) as ex:
        futs = {ex.submit(fn, it): it for it in items}
        for fut in concurrent.futures.as_completed(futs):
            out.append(fut.result())
    return out


def main() -> int:
    ap = argparse.ArgumentParser(description="Rotate client + WS path on LightXray servers.")
    ap.add_argument("--servers", default="servers-install.json")
    ap.add_argument("--paths", default="lightxray_ws_client_paths.json")
    ap.add_argument("--only", default="", help="comma-separated labels to limit to")
    ap.add_argument("--apply", action="store_true", help="actually rotate (default is preview only)")
    ap.add_argument("--yes", action="store_true", help="skip the confirmation prompt when applying")
    ap.add_argument("--parallel", type=int, default=6)
    args = ap.parse_args()

    servers = load_servers(args.servers)
    paths_map = load_paths(args.paths)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]
    if not servers:
        sys.exit("no servers selected")

    order = [s["label"] for s in servers]
    by_label = {s["label"]: s for s in servers}

    # ---- pass 1: read-only plan -------------------------------------------
    print(f"# reading current paths from {len(servers)} server(s)...", file=sys.stderr)
    plan = parallel_map(lambda s: plan_one(s, paths_map), servers, args.parallel)
    plan.sort(key=lambda r: order.index(r["label"]))

    for r in plan:
        head = f"{r['label']:5} {r['ip']:16} [{r['status']}]"
        if r["status"] == "would-change":
            print(head)
            print(f"      client: {r.get('cur_client','?')}  ->  {r['new_client']}")
            print(f"      ws:     {r.get('cur_ws','?')}  ->  {r['new_ws']}")
        elif r["status"] == "nochange":
            print(f"{head}  ({r['msg']})")
        else:  # skip / error
            print(f"{head}  {r['msg']}")

    changers = [r for r in plan if r["status"] == "would-change"]
    n_skip = sum(1 for r in plan if r["status"] == "skip")
    n_err = sum(1 for r in plan if r["status"] == "error")
    n_nc = sum(1 for r in plan if r["status"] == "nochange")
    print(f"\n# would-change={len(changers)}  nochange={n_nc}  skip={n_skip}  error={n_err}")

    if not args.apply:
        print("# preview only — re-run with --apply to rotate the would-change servers.")
        return 0
    if not changers:
        print("# nothing to change.")
        return 0

    # ---- confirm ----------------------------------------------------------
    print("\n⚠  APPLYING will invalidate existing customer keys on the above servers")
    print("   until each client refetches its subscription, and the pool must")
    print("   re-discover the client path. WS-path changes also restart xray.")
    if not args.yes:
        if not sys.stdin.isatty():
            sys.exit("refusing to apply non-interactively without --yes")
        if input(f"Type APPLY to rotate {len(changers)} server(s): ").strip() != "APPLY":
            sys.exit("aborted")

    # ---- pass 2: apply ----------------------------------------------------
    results = parallel_map(
        lambda r: apply_one(by_label[r["label"]], r["new_client"], r["new_ws"]),
        changers, args.parallel,
    )
    results.sort(key=lambda r: order.index(r["label"]))

    print()
    ok = 0
    for r in results:
        if r["status"] == "applied":
            ok += 1
            print(f"{r['label']:5} {r['ip']:16} APPLIED")
        else:
            print(f"{r['label']:5} {r['ip']:16} {r['status'].upper()}: {r.get('msg','')}")
            if r.get("output"):
                print("    " + r["output"].replace("\n", "\n    "))
    print(f"\n# applied {ok}/{len(results)}.  Customers on changed nodes must refetch their sub;")
    print("# update the pool so it re-discovers the new client paths.")
    return 0 if ok == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
