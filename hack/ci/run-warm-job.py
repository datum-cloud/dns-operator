#!/usr/bin/env python3
"""Overlap image compilation with claiming and preparing the DNS foundation."""
import importlib.util
import os
import json
import time
from pathlib import Path
import subprocess
import sys

ROOT = Path(__file__).resolve().parents[2]
spec = importlib.util.spec_from_file_location("dns_lanes", ROOT / "hack/run-dns-lanes.py")
lanes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lanes)


def main():
    timer = [sys.executable, "hack/time-ci-phase.py"]
    phases = {
        "images": [("dns-image-build", ["bash", "hack/ci/build-images.sh"])],
        "readiness": [
            ("foundation-claim", [sys.executable, "hack/ci/warm-foundation.py", "claim"]),
            ("duplicate-claim-rejection", ["bash", "-c",
                "if python3 hack/ci/warm-foundation.py claim > dev/repeated-claim.log 2>&1; then exit 1; fi; "
                "grep -q 'already been claimed' dev/repeated-claim.log"]),
            ("shared-prerequisites", ["task", "--yes", "env:shared-prerequisites"]),
        ],
    }
    start = time.monotonic()
    code = lanes.run_lanes(phases, Path("dev/ci-setup"),
                           lambda phase, command: timer + [phase] + command, lane_env={})
    with Path(os.environ.get("CI_TIMINGS_FILE", "dev/ci-timings.jsonl")).open("a") as stream:
        stream.write(json.dumps({"phase": "setup-parallel", "seconds": round(time.monotonic() - start, 3),
                                 "exit_code": code}) + "\n")
    if code:
        return code
    # Both images and all cluster prerequisites must succeed before DNS starts.
    return subprocess.call(timer + ["parallel-lanes", sys.executable, "hack/run-dns-lanes.py"],
                           env={**os.environ, "DNS_E2E_PREBUILT_IMAGES": "1",
                                "DNS_E2E_WARM_FOUNDATION": "1"})


if __name__ == "__main__":
    sys.exit(main())
