#!/usr/bin/env python3
"""
LightXray fleet installer.

Installs LightXray on a list of servers over SSH by running the canonical
deploy/install.sh on each one. This script is ONLY an orchestrator — it never
reimplements the install logic; install.sh stays the single source of truth.
Hosts run in parallel; each box's generated credentials are captured and
written to a results file.

Servers JSON (same shape as Hiddify_Server_Setup/servers-install.json):

    [
      {"k47": {"ip": "178.128.80.96", "username": "root", "password": "secret"}},
      {"k51": {"ip": "188.166.232.177", "username": "root",
               "key_file": "~/keys/do.pem", "domain": "k51.example.com"}}
    ]

  label    : the dict key — used for logs + the results file
  ip       : host to SSH to                              (required)
  username : SSH user                                    (default "root")
  password : SSH password                                (use this OR key_file)
  key_file : path to a private key / PEM                 (use this OR password)
  domain   : OPTIONAL management domain. When set, install.sh issues a Let's
             Encrypt cert and goes live on it (point a grey-cloud / DNS-only
             A record at this IP FIRST). Omit it for a bare install
             (self-signed, reachable by IP; attach the domain later from the
             dashboard's Domains page).
  any other keys (e.g. config_file) are ignored.

Usage:
    pip install paramiko
    python install.py                         # uses ./servers-install.json
    python install.py path/to/servers.json
    python install.py --ref main --parallel 5 --output creds.json
    python install.py --reset                 # RESET=1: wipe DB + regen secrets
    python install.py --verbose               # stream every box's output

Requires: Python 3.8+, paramiko.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import json
import os
import re
import sys
import time
from pathlib import Path

try:
    import paramiko
except ImportError:
    sys.exit("paramiko is required:  pip install paramiko")

REPO = "macrodigital283/lightxray"
RAW_URL = "https://raw.githubusercontent.com/{repo}/{ref}/deploy/install.sh"

# Credential lines emitted by install.sh's closing banner. Pulled out so the
# operator gets a tidy per-box record without scraping logs by hand.
FIELDS = {
    "management_domain": r"Management domain:\s*(.+?)\s*$",
    "reality_target":    r"Reality target:\s*(\S+)",
    "admin_proxy_path":  r"Admin proxy path:\s*(\S+)",
    "client_proxy_path": r"Client proxy path:\s*(\S+)",
    "admin_uuid":        r"Admin UUID:\s*(\S+)",
    "ws_token":          r"WS token:\s*(\S+)",
    "reality_pubkey":    r"Reality public key:\s*(\S+)",
    "reality_short_id":  r"Reality short id:\s*(\S+)",
    "dashboard_login":   r"Dashboard login:\s*(\S+)",
}


def load_servers(path: Path) -> list[dict]:
    """Parse the [{label: {...}}, ...] servers file into a flat list."""
    data = json.loads(path.read_text())
    servers = []
    for entry in data:
        if not isinstance(entry, dict) or not entry:
            continue
        label, info = next(iter(entry.items()))
        ip = (info.get("ip") or "").strip()
        if not ip:
            print(f"  ! skipping {label!r}: no ip", file=sys.stderr)
            continue
        servers.append(
            {
                "label": label,
                "ip": ip,
                "username": info.get("username", "root"),
                "password": info.get("password"),
                "key_file": info.get("key_file") or info.get("pem"),
                "domain": (info.get("domain") or "").strip(),
            }
        )
    return servers


def parse_credentials(output: str) -> dict:
    creds = {}
    for key, pat in FIELDS.items():
        m = re.search(pat, output, re.MULTILINE)
        if m:
            creds[key] = m.group(1).strip()
    return creds


def ssh_connect(srv: dict, timeout: int = 25) -> paramiko.SSHClient:
    cli = paramiko.SSHClient()
    cli.set_missing_host_key_policy(paramiko.AutoAddPolicy())
    kwargs = dict(
        hostname=srv["ip"],
        username=srv["username"],
        timeout=timeout,
        banner_timeout=timeout,
        auth_timeout=timeout,
    )
    if srv.get("key_file"):
        kwargs["key_filename"] = os.path.expanduser(srv["key_file"])
    else:
        kwargs["password"] = srv["password"]
        kwargs["look_for_keys"] = False
        kwargs["allow_agent"] = False
    cli.connect(**kwargs)
    # Keep the connection alive across the multi-minute install so an idle
    # NAT / firewall doesn't drop us mid-build.
    tr = cli.get_transport()
    if tr is not None:
        tr.set_keepalive(30)
    return cli


def install_one(srv: dict, ref: str, reset: bool, verbose: bool,
                logdir: Path) -> dict:
    label, ip = srv["label"], srv["ip"]
    url = RAW_URL.format(repo=REPO, ref=ref)

    env_parts = []
    if srv["domain"]:
        env_parts.append(f'DOMAIN={srv["domain"]}')
    if reset:
        env_parts.append("RESET=1")
    env_prefix = (" ".join(env_parts) + " ") if env_parts else ""

    # Fetch install.sh fresh (a commit SHA in --ref dodges the raw.github CDN
    # cache), then run it as root. install.sh is idempotent / re-run-safe.
    remote_cmd = (
        f"curl -fsSL {url} -o /root/lx-install.sh && "
        f"{env_prefix}bash /root/lx-install.sh 2>&1; echo LX_INSTALL_EXIT=$?"
    )

    result = {"label": label, "ip": ip, "ok": False, "exit": None,
              "domain": srv["domain"] or None, "seconds": 0, "credentials": {}}
    started = time.time()
    tag = f"[{label} {ip}]"
    print(f"{tag} installing…" + (f" (domain {srv['domain']})" if srv["domain"] else " (bare)"))

    try:
        cli = ssh_connect(srv)
    except Exception as e:  # noqa: BLE001 — report any connect failure verbatim
        result["error"] = f"ssh connect failed: {e}"
        print(f"{tag} ✗ {result['error']}")
        return result

    try:
        _stdin, stdout, _stderr = cli.exec_command(remote_cmd, get_pty=False)
        chunks = []
        # readline() blocks until EOF (command finished). Drain fully BEFORE
        # asking for the exit status to avoid a buffer-full deadlock.
        for line in iter(stdout.readline, ""):
            chunks.append(line)
            if verbose:
                print(f"{tag} {line.rstrip()}")
        stdout.channel.recv_exit_status()  # whole pipeline; the real code is below
        output = "".join(chunks)
    except Exception as e:  # noqa: BLE001
        result["error"] = f"exec failed: {e}"
        print(f"{tag} ✗ {result['error']}")
        cli.close()
        return result
    finally:
        cli.close()

    logdir.mkdir(parents=True, exist_ok=True)
    (logdir / f"{label}.log").write_text(output)

    # The channel's exit code is `echo`'s (always 0); the real install exit is
    # in the LX_INSTALL_EXIT marker we appended.
    m = re.search(r"LX_INSTALL_EXIT=(\d+)", output)
    exit_code = int(m.group(1)) if m else None
    result["exit"] = exit_code
    result["credentials"] = parse_credentials(output)
    result["seconds"] = round(time.time() - started, 1)
    result["ok"] = exit_code == 0 and "all services active" in output

    if result["ok"]:
        print(f"{tag} ✓ done in {result['seconds']}s")
    else:
        # Surface the tail so a failure is debuggable from the console.
        tail = "\n    ".join(output.strip().splitlines()[-4:])
        print(f"{tag} ✗ FAILED (exit {exit_code}) in {result['seconds']}s\n    {tail}")
        print(f"{tag}   full log: {logdir / (label + '.log')}")
    return result


def print_summary(results: list[dict]) -> None:
    ok = [r for r in results if r["ok"]]
    bad = [r for r in results if not r["ok"]]
    print("\n" + "=" * 78)
    print(f"  LightXray fleet install: {len(ok)}/{len(results)} succeeded")
    print("=" * 78)
    for r in results:
        c = r.get("credentials", {})
        mark = "✓" if r["ok"] else "✗"
        print(f"\n  {mark} {r['label']}  ({r['ip']})"
              + (f"   FAILED: {r.get('error') or 'exit ' + str(r['exit'])}" if not r["ok"] else ""))
        if c:
            print(f"      dashboard : {c.get('dashboard_login', '?')}")
            print(f"      admin uuid: {c.get('admin_uuid', '?')}")
            print(f"      admin path: {c.get('admin_proxy_path', '?')}"
                  f"   client path: {c.get('client_proxy_path', '?')}")
    print()


def main() -> int:
    ap = argparse.ArgumentParser(description="Install LightXray across a fleet over SSH.")
    ap.add_argument("servers", nargs="?", default="servers-install.json",
                    help="servers JSON (default: ./servers-install.json)")
    ap.add_argument("--ref", default="main",
                    help="git ref of lightxray to install (branch, tag, or commit SHA; default main)")
    ap.add_argument("--reset", action="store_true",
                    help="pass RESET=1 to install.sh (WIPES the DB + regenerates all secrets)")
    ap.add_argument("--parallel", type=int, default=5,
                    help="max concurrent installs (default 5)")
    ap.add_argument("--output", default="lightxray-credentials.json",
                    help="where to write captured credentials (default lightxray-credentials.json)")
    ap.add_argument("--logdir", default="lightxray-install-logs",
                    help="directory for per-host install logs (default ./lightxray-install-logs)")
    ap.add_argument("--verbose", action="store_true", help="stream every host's output")
    args = ap.parse_args()

    spath = Path(args.servers)
    if not spath.is_file():
        sys.exit(f"servers file not found: {spath}")
    servers = load_servers(spath)
    if not servers:
        sys.exit("no servers with an ip found in the file")

    print(f"Installing LightXray (ref={args.ref}) on {len(servers)} host(s), "
          f"{'RESET=1, ' if args.reset else ''}up to {args.parallel} at a time.\n")
    logdir = Path(args.logdir)

    results: list[dict] = []
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, args.parallel)) as ex:
        futs = {
            ex.submit(install_one, s, args.ref, args.reset, args.verbose, logdir): s
            for s in servers
        }
        for fut in concurrent.futures.as_completed(futs):
            results.append(fut.result())

    results.sort(key=lambda r: [s["label"] for s in servers].index(r["label"]))
    print_summary(results)

    # Credentials file: everything install.sh generated, minus SSH secrets.
    creds_out = [
        {"label": r["label"], "ip": r["ip"], "ok": r["ok"],
         "domain": r.get("domain"), **r.get("credentials", {})}
        for r in results
    ]
    Path(args.output).write_text(json.dumps(creds_out, indent=2))
    print(f"  Credentials written to {args.output}")
    print(f"  Per-host logs in {logdir}/\n")

    return 0 if all(r["ok"] for r in results) else 1


if __name__ == "__main__":
    raise SystemExit(main())
