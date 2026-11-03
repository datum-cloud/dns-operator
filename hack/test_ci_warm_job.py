"""Failure gating for the warm setup/build overlap."""
import importlib.util
import os
from pathlib import Path
import tempfile
import unittest
from unittest.mock import patch

spec = importlib.util.spec_from_file_location("warm_job", Path(__file__).parent / "ci/run-warm-job.py")
job = importlib.util.module_from_spec(spec)
spec.loader.exec_module(job)


class WarmJobTests(unittest.TestCase):
    def test_failed_setup_does_not_deploy(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"CI_TIMINGS_FILE": str(Path(directory) / "times.jsonl")}), patch.object(job.lanes, "run_lanes", return_value=1), patch.object(job.subprocess, "call") as deploy:
            self.assertEqual(job.main(), 1)
            deploy.assert_not_called()

    def test_success_enables_consumption_only_after_setup(self):
        with tempfile.TemporaryDirectory() as directory, patch.dict(os.environ, {"CI_TIMINGS_FILE": str(Path(directory) / "times.jsonl")}), patch.object(job.lanes, "run_lanes", return_value=0), patch.object(job.subprocess, "call", return_value=0) as deploy:
            self.assertEqual(job.main(), 0)
            env = deploy.call_args.kwargs["env"]
            self.assertEqual(env["DNS_E2E_PREBUILT_IMAGES"], "1")
            self.assertEqual(env["DNS_E2E_WARM_FOUNDATION"], "1")
