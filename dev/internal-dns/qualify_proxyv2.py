#!/usr/bin/env python3
"""Qualify shared dnsdist -> isolated BIND context views and replica failover.

This component check uses static zone files in two independent regional BIND
members. The full test under test/internaldns renders those files only from
Kubernetes API publications delivered through JetStream.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import sys
import time


HERE = Path(__file__).resolve().parent
COMPOSE = HERE / "compose.yaml"
DEFAULT_RUNTIME = HERE / "runtime"
RESULTS: list[dict[str, object]] = []
COLIMA_PROFILE = "internal-dns-e2e"


def run(*args: str, check: bool = True) -> str:
    process = subprocess.run(args, text=True, capture_output=True, env=os.environ)
    if check and process.returncode:
        raise RuntimeError(
            f"command failed ({process.returncode}): {' '.join(args)}\n"
            f"stdout:\n{process.stdout}\nstderr:\n{process.stderr}"
        )
    return process.stdout.strip()


def compose(*args: str, check: bool = True) -> str:
    return run("docker", "compose", "-f", str(COMPOSE), *args, check=check)


def require_suite_docker_host() -> None:
    profile = os.environ.get("INTERNAL_DNS_COLIMA_PROFILE", COLIMA_PROFILE)
    expected = f"unix://{Path.home()}/.colima/{profile}/docker.sock"
    if os.environ.get("DOCKER_HOST") != expected:
        raise RuntimeError(f"set DOCKER_HOST={expected}; the default Docker context is not used")


def source_hash() -> str:
    digest = hashlib.sha256()
    paths = [HERE / "compose.yaml", HERE / "qualify_proxyv2.py", *sorted((HERE / "runtime").rglob("*"))]
    for path in paths:
        if path.is_file():
            digest.update(str(path.relative_to(HERE)).encode() + b"\0")
            digest.update(path.read_bytes())
    return digest.hexdigest()


def query(server: str, name: str, tcp: bool = False) -> tuple[str, list[str]]:
    flags = ["+tcp"] if tcp else []
    comments = compose("exec", "-T", "probe", "dig", f"@{server}", name, "A", "+noall", "+comments", *flags)
    status = "UNKNOWN"
    for line in comments.splitlines():
        if "status:" in line:
            status = line.split("status:", 1)[1].split(",", 1)[0].strip()
            break
    answer = compose("exec", "-T", "probe", "dig", f"@{server}", name, "A", "+short", *flags)
    return status, [line.strip() for line in answer.splitlines() if line.strip()]


def expect(label: str, server: str, name: str, status: str, answers: list[str], tcp: bool = False) -> None:
    actual_status, actual_answers = query(server, name, tcp=tcp)
    result = {
        "label": label,
        "server": server,
        "name": name,
        "transport": "TCP" if tcp else "UDP",
        "status": actual_status,
        "answers": actual_answers,
    }
    RESULTS.append(result)
    print(f"PASS {label}: {actual_status} {actual_answers}" if (actual_status, actual_answers) == (status, answers) else f"FAIL {label}: {actual_status} {actual_answers}", flush=True)
    if (actual_status, actual_answers) != (status, answers):
        raise AssertionError(f"{label}: expected {(status, answers)}, got {(actual_status, actual_answers)}")


def main() -> int:
    parser = argparse.ArgumentParser()
    parser.add_argument("--keep", action="store_true", help="leave the disposable Compose environment running")
    parser.add_argument("--results", type=Path, help="write machine-readable evidence to this path")
    args = parser.parse_args()
    require_suite_docker_host()
    os.environ.setdefault("INTERNAL_DNS_RUNTIME", str(DEFAULT_RUNTIME))

    compose("down", "--volumes", "--remove-orphans", check=False)
    try:
        compose("up", "-d", "--wait")
        time.sleep(1.5)

        for tcp in (False, True):
            expect(f"VPC A overlap {'TCP' if tcp else 'UDP'}", "10.253.0.51", "common.prod.internal", "NOERROR", ["10.0.1.10"], tcp)
            expect(f"VPC B overlap {'TCP' if tcp else 'UDP'}", "10.253.0.52", "common.prod.internal", "NOERROR", ["10.0.1.20"], tcp)
            expect(f"regional VPC A {'TCP' if tcp else 'UDP'}", "10.253.0.41", "common.prod.internal", "NOERROR", ["10.0.1.10"], tcp)

        expect("VPC B positive before VPC A denial", "10.253.0.52", "only-b.prod.internal", "NOERROR", ["10.0.1.20"])
        expect("VPC A private NXDOMAIN", "10.253.0.51", "only-b.prod.internal", "NXDOMAIN", [])
        expect("VPC B positive after VPC A denial UDP", "10.253.0.52", "only-b.prod.internal", "NOERROR", ["10.0.1.20"])
        expect("VPC B positive after VPC A denial TCP", "10.253.0.52", "only-b.prod.internal", "NOERROR", ["10.0.1.20"], True)
        expect("VPC A NODATA", "10.253.0.51", "type-split.prod.internal", "NOERROR", [])
        expect("VPC B positive after VPC A NODATA", "10.253.0.52", "type-split.prod.internal", "NOERROR", ["10.0.1.20"])
        expect("second zone in VPC A", "10.253.0.51", "tool.apps.internal", "NOERROR", ["10.0.2.10"])
        # VPC B deliberately has no attached private zone for this suffix. The
        # known context must fail closed rather than recurse to public DNS.
        expect("second zone unavailable in VPC B", "10.253.0.52", "tool.apps.internal", "SERVFAIL", [])
        expect("unknown node destination rejected", "10.253.0.53", "common.prod.internal", "REFUSED", [])
        expect("unknown cluster destination rejected", "10.253.0.43", "common.prod.internal", "REFUSED", [])

        for stopped in ("regional-0-bind", "regional-1-bind"):
            compose("stop", stopped)
            compose("exec", "-T", "node-bind", "rndc", "-s", "127.0.0.1", "-p", "9953", "-k", "/etc/bind/rndc.key", "flush")
            time.sleep(1.5)
            expect(f"regional member failover with {stopped} stopped", "10.253.0.51", "common.prod.internal", "NOERROR", ["10.0.1.10"], True)
            compose("start", stopped)
            time.sleep(1.5)

        evidence = {
            "dnsdistImage": "powerdns/dnsdist-20@sha256:4ed9af56729c7021795dc478421237ab9cf6c7ceec8734677405e7617f06d52f",
            "dnsdistVersion": "2.0.1",
            "bindImage": "internetsystemsconsortium/bind9@sha256:071465f88068854d0ceadd9b985fb1acae4071e80754e545cd811fde57541ad0",
            "bindVersion": "9.20.29",
            "sourceSHA256": source_hash(),
            "topology": {
                "nodeMembers": 1,
                "regionalDNSDistFrontends": 1,
                "regionalBINDMembers": 2,
                "perVPCProcesses": 0,
            },
            "results": RESULTS,
        }
        if args.results:
            args.results.parent.mkdir(parents=True, exist_ok=True)
            args.results.write_text(json.dumps(evidence, indent=2) + "\n")
        print(f"qualified {len(RESULTS)} PROXYv2/view checks")
        return 0
    finally:
        if not args.keep:
            compose("down", "--volumes", "--remove-orphans", check=False)


if __name__ == "__main__":
    sys.exit(main())
