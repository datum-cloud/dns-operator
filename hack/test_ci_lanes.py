"""Verify lane overlap, deployment gating, and failure propagation without a cluster."""
import importlib.util
import json
import os
from pathlib import Path
import sys
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("lanes", Path(__file__).with_name("run-dns-lanes.py"))
lanes = importlib.util.module_from_spec(spec)
spec.loader.exec_module(lanes)
TIMER = Path(__file__).with_name("time-ci-phase.py")


class LaneTests(unittest.TestCase):
    def exercise(self, fail):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            child = root / "child.py"
            child.write_text('''import pathlib, sys, time
root, phase, fail = pathlib.Path(sys.argv[1]), sys.argv[2], sys.argv[3]
name, operation = phase.split("-")
if operation == "deploy":
    (root / (name + "-started")).touch()
    other = "public" if name == "private" else "private"
    deadline = time.monotonic() + 10
    while not (root / (other + "-started")).exists():
        if time.monotonic() > deadline: raise SystemExit(99)
        time.sleep(.01)
    if fail == name: raise SystemExit(23)
    (root / (name + "-ready")).touch()
else:
    assert (root / (name + "-ready")).exists()
    (root / (name + "-tested")).touch()
''')
            env = {"CI_TIMINGS_FILE": str(root / "combined.jsonl"),
                   "GITHUB_STEP_SUMMARY": str(root / "summary.md")}
            with patch.dict(os.environ, env):
                code = lanes.run_lanes(
                    {n: [(n + "-deploy", "unused"), (n + "-test", "unused")]
                     for n in ["private", "public"]}, root / "lanes",
                    lambda phase, task: [sys.executable, str(TIMER), phase,
                                         sys.executable, str(child), str(root), phase, fail])
            self.assertEqual(code, 1 if fail else 0)
            self.assertTrue((root / "public-tested").exists())
            self.assertEqual((root / "private-tested").exists(), not bool(fail))
            records = [json.loads(line) for line in (root / "combined.jsonl").read_text().splitlines()]
            statuses = {r["phase"]: r["exit_code"] for r in records}
            self.assertEqual(statuses["private-deploy"], 23 if fail else 0)
            self.assertEqual(statuses["public-lane"], 0)
            self.assertIn("Public lane wall time", (root / "summary.md").read_text())

    def test_lanes_overlap_and_tests_follow_deployment(self):
        self.exercise("")

    def test_failure_skips_own_tests_but_waits_for_other_lane(self):
        self.exercise("private")


if __name__ == "__main__":
    unittest.main()
