#!/usr/bin/env python3
"""Run independent DNS lanes; wait for every lane before returning failure."""
import argparse
from concurrent.futures import ThreadPoolExecutor
import json
import os
from pathlib import Path
import signal
import subprocess
import sys
import threading
import time

LANES = {
    "private": [("private-dns-deploy", "env:internal-dns:deploy"),
                ("private-dns-qualification", "env:internal-dns:qualify")],
    "public": [("public-dns-deploy", "env:stack-deploy"),
               ("chainsaw-tests", "env:chainsaw")],
}


def run_lanes(lanes, output, command, lane_env=None):
    if lane_env is None:
        lane_env = {"DNS_E2E_SHARED_PREREQUISITES": "1"}
    output.mkdir(parents=True, exist_ok=True)
    processes = set()
    lock = threading.Lock()
    cancelled = threading.Event()

    def cancel(signum, frame):
        cancelled.set()
        with lock:
            for process in processes:
                # Each phase owns a process group, including Task and its children.
                try:
                    os.killpg(process.pid, signal.SIGTERM)
                except ProcessLookupError:
                    pass

    previous = {sig: signal.signal(sig, cancel) for sig in (signal.SIGTERM, signal.SIGINT)}

    def lane(name, phases):
        started = time.monotonic()
        timings = output / f"{name}.jsonl"
        summary = output / f"{name}.md"
        timings.write_text("")
        summary.write_text("")
        env = {**os.environ, **lane_env,
               "CI_TIMINGS_FILE": str(timings), "GITHUB_STEP_SUMMARY": str(summary)}
        code = 0
        with (output / f"{name}.log").open("w") as log:
            for phase, task in phases:
                with lock:
                    if cancelled.is_set():
                        code = 130
                        break
                    process = subprocess.Popen(command(phase, task), env=env,
                                               stdout=log, stderr=subprocess.STDOUT,
                                               start_new_session=True)
                    processes.add(process)
                print(f"Starting {phase}; log: {log.name}", flush=True)
                code = process.wait()
                with lock:
                    processes.discard(process)
                print(f"Finished {phase}: exit {code}", flush=True)
                if code:
                    break
        elapsed = round(time.monotonic() - started, 3)
        with timings.open("a") as stream:
            stream.write(json.dumps({"phase": f"{name}-lane", "seconds": elapsed,
                                     "exit_code": code}) + "\n")
        with summary.open("a") as stream:
            stream.write(f"\n{name.title()} lane wall time: **{elapsed:.3f}s**, exit **{code}**.\n")
        return code

    try:
        with ThreadPoolExecutor(max_workers=len(lanes)) as executor:
            futures = [executor.submit(lane, name, phases) for name, phases in lanes.items()]
            # Collect all results before teardown, even when another lane fails.
            codes = [future.result() for future in futures]
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)
        combined = Path(os.environ.get("CI_TIMINGS_FILE", "dev/ci-timings.jsonl"))
        combined.parent.mkdir(parents=True, exist_ok=True)
        with combined.open("a") as stream:
            for name in lanes:
                path = output / f"{name}.jsonl"
                if path.exists():
                    stream.write(path.read_text())
        summary = os.environ.get("GITHUB_STEP_SUMMARY")
        if summary:
            with Path(summary).open("a") as stream:
                for name in lanes:
                    path = output / f"{name}.md"
                    if path.exists():
                        stream.write(path.read_text())
    return 1 if cancelled.is_set() or any(codes) else 0


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--include-harness", action="store_true")
    args = parser.parse_args()
    lanes = dict(LANES)
    if args.include_harness:
        lanes["harness"] = [("harness-tests", "test:internal-dns-kubernetes")]
    timer = Path(__file__).with_name("time-ci-phase.py")
    return run_lanes(lanes, Path("dev/ci-lanes"),
                     lambda phase, task: [sys.executable, str(timer), phase, "task", "--yes", task])


if __name__ == "__main__":
    sys.exit(main())
