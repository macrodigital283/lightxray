#!/usr/bin/env python3
"""
Add LightXray management domains over SSH.

For each server in the servers JSON, looks up its management domain in a
Cloudflare-style domain map (keyed by label — the greentech4.json shape),
then SSHes in and runs `lightxray-applydomain <domain>`, which issues a
Let's Encrypt cert, points nginx at it, sets LX_PUBLIC_HOST, and restarts
lightxrayd. Pure orchestrator — the helper on the box does the real work.

For certbot's HTTP-01 to succeed the domain MUST be grey-cloud / DNS-only,
already resolve to the box, and :80 must be reachable from the internet.

Domain map shape (greentech4.json) — value may be a dict OR a [dict] list:
    {"k47": {"new_ip": "1.2.3.4", "proxied": false,
             "record_names": ["k47.example.com"]}, ...}

Servers JSON shape (servers-install.json):
    [{"k47": {"ip": "1.2.3.4", "username": "root", "password": "…"}}, ...]

A server is matched to a domain by label first, then by IP.

Usage:
    pip install paramiko
    python add_management_domain.py servers-install.json \
        --domains CloudflareApi_IP_proxied_change_greentech4.json
    python add_management_domain.py servers.json --domains map.json --only k47,k51

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


def load_domain_map(path: str) -> tuple[dict, dict]:
    """Return (by_label, by_ip) -> the management domain (first record_name)."""
    data = json.loads(Path(path).read_text())
    by_label, by_ip = {}, {}
    for label, v in data.items():
        rec = v[0] if isinstance(v, list) and v else v
        if not isinstance(rec, dict):
            continue
        names = rec.get("record_names") or []
        if not names:
            continue
        domain = names[0].strip()
        by_label[label] = domain
        ip = (rec.get("new_ip") or "").strip()
        if ip:
            by_ip[ip] = domain
    return by_label, by_ip


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


def apply_one(srv: dict, domain: str, verbose: bool) -> dict:
    label, ip = srv["label"], srv["ip"]
    tag = f"[{label} {ip}]"
    print(f"{tag} → management domain {domain} (issuing cert…)")
    res = {"label": label, "ip": ip, "domain": domain, "ok": False, "exit": None}
    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001
        res["error"] = f"ssh: {e}"
        print(f"{tag} ✗ {res['error']}")
        return res
    try:
        cmd = f"/usr/local/bin/lightxray-applydomain {shlex.quote(domain)} 2>&1; echo LX_APPLY_EXIT=$?"
        out = run(cli, cmd)
    finally:
        cli.close()
    if verbose:
        for line in out.splitlines():
            print(f"{tag} {line}")
    m = re.search(r"LX_APPLY_EXIT=(\d+)", out)
    res["exit"] = int(m.group(1)) if m else None
    res["ok"] = res["exit"] == 0 and "OK: management domain set" in out
    if res["ok"]:
        print(f"{tag} ✓ {domain} live (LE cert issued, PublicHost set)")
    else:
        tail = " | ".join(out.strip().splitlines()[-3:])
        print(f"{tag} ✗ FAILED (exit {res['exit']}): {tail}")
    return res


def main() -> int:
    ap = argparse.ArgumentParser(description="Add LightXray management domains over SSH.")
    ap.add_argument("servers", nargs="?", default="servers-install.json")
    ap.add_argument("--domains", required=True, help="Cloudflare domain map (greentech4.json shape)")
    ap.add_argument("--only", default="", help="comma-separated labels to limit to")
    ap.add_argument("--parallel", type=int, default=5)
    ap.add_argument("--verbose", action="store_true")
    args = ap.parse_args()

    servers = load_servers(args.servers)
    by_label, by_ip = load_domain_map(args.domains)
    only = {s.strip() for s in args.only.split(",") if s.strip()}
    if only:
        servers = [s for s in servers if s["label"] in only]

    jobs = []
    for s in servers:
        domain = by_label.get(s["label"]) or by_ip.get(s["ip"])
        if not domain:
            print(f"[{s['label']} {s['ip']}] ! no management domain in map — skipping")
            continue
        jobs.append((s, domain))
    if not jobs:
        sys.exit("no servers matched a domain in the map")

    results = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        futs = [ex.submit(apply_one, s, d, args.verbose) for s, d in jobs]
        for f in concurrent.futures.as_completed(futs):
            results.append(f.result())

    ok = sum(1 for r in results if r["ok"])
    print(f"\n{ok}/{len(results)} management domains set:")
    for r in sorted(results, key=lambda r: r["label"]):
        print(f"  {'✓' if r['ok'] else '✗'} {r['label']:5} {r['domain']}")
    return 0 if ok == len(results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
