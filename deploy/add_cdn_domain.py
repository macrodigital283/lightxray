#!/usr/bin/env python3
"""
Add LightXray CDN (orange-cloud) domains over SSH.

For each server in the servers JSON, looks up its CDN hostname(s) in a
Cloudflare-style domain map (keyed by label — the for_sub_link4.json shape),
SSHes in, and registers each hostname with LightXray by POSTing to the box's
own dashboard /ui/cdn/ endpoint (default transport = ws, enabled). The
subscription builder picks enabled CDN hosts up live — no restart, no cert
needed on the box (Cloudflare fronts the TLS; the origin uses the box's
existing cert).

The actual Cloudflare DNS (orange-cloud A record -> box origin) is set
separately by your Cloudflare script; this only tells LightXray to emit
VLESS+WS+TLS keys for those hostnames.

Domain map shape (for_sub_link4.json) — value is a [dict] list whose
record_names holds the CDN hostnames:
    {"k47": [{"proxied": true,
              "record_names": ["k47.aaa.store", "k47.bbb.store"]}], ...}
(a plain dict value works too.)

Servers JSON shape (servers-install.json):
    [{"k47": {"ip": "1.2.3.4", "username": "root", "password": "…"}}, ...]

Usage:
    pip install paramiko
    python add_cdn_domain.py servers-install.json \
        --cdn CloudflareApi_IP_proxied_change_for_sub_link4.json
    python add_cdn_domain.py servers.json --cdn map.json --only k47,k51 \
        --transport ws

Requires: Python 3.8+, paramiko.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import shlex
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


def load_cdn_map(path: str) -> dict:
    """Return by_label -> [hostname, ...] (all record_names across entries)."""
    data = json.loads(Path(path).read_text())
    by_label = {}
    for label, v in data.items():
        entries = v if isinstance(v, list) else [v]
        hosts = []
        for rec in entries:
            if isinstance(rec, dict):
                hosts += [n.strip() for n in (rec.get("record_names") or []) if n.strip()]
        if hosts:
            # de-dup, preserve order
            by_label[label] = list(dict.fromkeys(hosts))
    return by_label


def ssh_connect(srv: dict, timeout: int = 25) -> paramiko.SSHClient:
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
    tr = cli.get_transport()
    if tr is not None:
        tr.set_keepalive(30)
    return cli


def run(cli: paramiko.SSHClient, cmd: str) -> str:
    _in, out, _err = cli.exec_command(cmd, get_pty=False)
    text = "".join(iter(out.readline, ""))
    out.channel.recv_exit_status()
    return text


# Remote snippet: read admin creds from config.env, log into the local
# dashboard, POST each hostname to /ui/cdn/ (and optionally set transport).
REMOTE = r"""
set -e
ENV=/etc/lightxray/config.env
A=$(grep -oP '^LX_ADMIN_PROXY_PATH=\K.*' "$ENV")
U=$(grep -oP '^LX_ADMIN_UUID=\K.*' "$ENV")
B="https://127.0.0.1/$A/ui"
JAR=$(mktemp)
curl -sk -o /dev/null -c "$JAR" --data-urlencode "admin_uuid=$U" "$B/login"
for h in {hostlist}; do
  code=$(curl -sk -o /dev/null -w '%{{http_code}}' -b "$JAR" --data-urlencode "hostname=$h" "$B/cdn/")
  echo "CDN $h -> $code"
done
# Re-read the list and (optionally) pin transport on the freshly-added rows.
TR={transport}
if [ -n "$TR" ]; then
  for id in $(curl -sk -b "$JAR" "$B/cdn/" | grep -oE '/cdn/[0-9]+/transport' | grep -oE '[0-9]+' | sort -u); do
    curl -sk -o /dev/null -b "$JAR" --data-urlencode "transport=$TR" "$B/cdn/$id/transport"
  done
fi
rm -f "$JAR"
"""


def add_one(srv: dict, hosts: list[str], transport: str, verbose: bool) -> dict:
    label, ip = srv["label"], srv["ip"]
    tag = f"[{label} {ip}]"
    print(f"{tag} → CDN hosts: {', '.join(hosts)}")
    res = {"label": label, "ip": ip, "hosts": hosts, "added": 0, "ok": False}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["error"] = f"ssh: {e}"
        print(f"{tag} ✗ {res['error']}")
        return res
    try:
        hostlist = " ".join(shlex.quote(h) for h in hosts)
        cmd = REMOTE.format(hostlist=hostlist, transport=shlex.quote(transport or ""))
        out = run(cli, cmd)
    finally:
        cli.close()
    if verbose:
        for line in out.splitlines():
            print(f"{tag} {line}")
    # Each "CDN <host> -> 302" is a successful add (the handler redirects).
    added = len(re.findall(r"CDN \S+ -> 30[23]", out))
    res["added"] = added
    res["ok"] = added == len(hosts)
    mark = "✓" if res["ok"] else "✗"
    print(f"{tag} {mark} added {added}/{len(hosts)} CDN host(s)")
    return res


def main() -> int:
    ap = argparse.ArgumentParser(description="Add LightXray CDN domains over SSH.")
    ap.add_argument("servers", nargs="?", default="servers-install.json")
    ap.add_argument("--cdn", required=True, help="Cloudflare CDN map (for_sub_link4.json shape)")
    ap.add_argument("--only", default="", help="comma-separated labels to limit to")
    ap.add_argument("--transport", default="ws", choices=["ws", "grpc", "both", ""],
                    help="transport for added hosts (default ws; '' leaves the DB default)")
    ap.add_argument("--parallel", type=int, default=5)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    servers = load_servers(args.servers)
    by_label = load_cdn_map(args.cdn)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]

    jobs = []
    for s in servers:
        hosts = by_label.get(s["label"])
        if not hosts:
            print(f"[{s['label']} {s['ip']}] ! no CDN hosts in map — skipping")
            continue
        jobs.append((s, hosts))
    if not jobs:
        sys.exit("no servers matched CDN hosts in the map")

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        futs = [ex.submit(add_one, s, h, args.transport, args.verbose) for s, h in jobs]
        for f in concurrent.futures.as_completed(futs):
            results.append(f.result())

    total_added = sum(r["added"] for r in results)
    ok = sum(1 for r in results if r["ok"])
    print(f"\n{ok}/{len(results)} servers fully done; {total_added} CDN host(s) added:")
    for r in sorted(results, key=lambda r: r["label"]):
        print(f"  {'✓' if r['ok'] else '✗'} {r['label']:5} {r['added']}/{len(r['hosts'])}  {', '.join(r['hosts'])}")
    return 0 if ok == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
