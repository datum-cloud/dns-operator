"""Exercise timing output and failure propagation without a cluster."""
import json
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

SCRIPT = Path(__file__).with_name("time-ci-phase.py")


class TimingTests(unittest.TestCase):
    def run_phase(self, code):
        with tempfile.TemporaryDirectory() as directory:
            output = Path(directory) / "timings.jsonl"
            summary = Path(directory) / "summary.md"
            result = subprocess.run(
                [sys.executable, str(SCRIPT), "foundation", sys.executable,
                 "-c", f"print('child output'); raise SystemExit({code})"],
                env={**os.environ, "CI_TIMINGS_FILE": str(output),
                     "GITHUB_STEP_SUMMARY": str(summary)}, capture_output=True, text=True)
            self.assertEqual(result.returncode, code)
            self.assertIn("child output", result.stdout)
            record = json.loads(output.read_text())
            self.assertEqual(record["exit_code"], code)
            self.assertEqual(record["phase"], "foundation")
            self.assertGreaterEqual(record["seconds"], 0)
            return summary.read_text()

    def test_success_reports_upper_bound(self):
        self.assertIn("upper bound", self.run_phase(0))

    def test_failure_is_not_a_savings_estimate(self):
        self.assertIn("partial duration", self.run_phase(23))


if __name__ == "__main__":
    unittest.main()
