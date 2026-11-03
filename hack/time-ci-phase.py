#!/usr/bin/env python3
"""Time a CI command without swallowing its output or exit status."""
import json
import os
from pathlib import Path
import subprocess
import sys
import time


def main():
    if len(sys.argv) < 3:
        sys.exit("usage: time-ci-phase.py PHASE COMMAND [ARG ...]")
    phase, command = sys.argv[1], sys.argv[2:]
    start = time.monotonic()
    try:
        result = subprocess.run(command, check=False)
        code = result.returncode
        if code < 0:
            code = 128 - code
    except OSError as error:
        print(error, file=sys.stderr)
        code = 127
    elapsed = round(time.monotonic() - start, 3)
    record = {"phase": phase, "seconds": elapsed, "exit_code": code}
    output = Path(os.environ.get("CI_TIMINGS_FILE", "dev/ci-timings.jsonl"))
    output.parent.mkdir(parents=True, exist_ok=True)
    with output.open("a") as stream:
        stream.write(json.dumps(record) + "\n")
    print(f"CI phase {phase}: {elapsed:.3f}s (exit {code})", flush=True)
    summary = os.environ.get("GITHUB_STEP_SUMMARY")
    if summary:
        with Path(summary).open("a") as stream:
            stream.write(f"### {phase}\n\nDuration: **{elapsed:.3f}s**. Exit code: **{code}**.\n\n")
            if phase == "foundation":
                if code == 0:
                    stream.write("This is the cold foundation preparation cost a ready warm pool could avoid. "
                                 "It is an upper bound on savings: pool acquisition and readiness checks still take time. "
                                 "DNS build, deployment, and qualification are measured separately.\n\n")
                else:
                    stream.write("Foundation preparation failed; this partial duration is not a warm-pool savings estimate.\n\n")
    return code


if __name__ == "__main__":
    sys.exit(main())
