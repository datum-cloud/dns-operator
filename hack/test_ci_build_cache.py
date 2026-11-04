import importlib.util
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
spec = importlib.util.spec_from_file_location('warm_job', ROOT / 'hack/ci/run-warm-job.py')
warm_job = importlib.util.module_from_spec(spec)
spec.loader.exec_module(warm_job)


class BuildInputKeyTests(unittest.TestCase):
    def setUp(self):
        self.directory = tempfile.TemporaryDirectory()
        self.addCleanup(self.directory.cleanup)
        self.root = Path(self.directory.name)
        subprocess.run(['git', 'init', '-q', str(self.root)], check=True)
        for name in ['Dockerfile.dockerignore', 'build-input-key.sh']:
            path = self.root / 'hack/ci' / name
            path.parent.mkdir(parents=True, exist_ok=True)
            shutil.copyfile(ROOT / 'hack/ci' / name, path)
        self.write('go.mod', 'module example')
        self.write('cmd/main.go', 'package main')

    def write(self, name, content):
        path = self.root / name
        path.parent.mkdir(parents=True, exist_ok=True)
        path.write_text(content)

    def key(self):
        return subprocess.check_output(['bash', 'hack/ci/build-input-key.sh'], cwd=self.root)

    def test_workflow_changes_preserve_key(self):
        before = self.key()
        self.write('.github/workflows/e2e.yml', 'changed workflow')
        self.assertEqual(before, self.key())

    def test_ci_cache_sources_preserve_key(self):
        before = self.key()
        self.write('.ci-go-cache/modules/example/module.go', 'package dependency')
        self.write('.ci-cache-dance/cmd/main.go', 'package main')
        self.assertEqual(before, self.key())

    def test_all_input_classes_invalidate_key(self):
        for name in ['cmd/main.go', 'go.mod', 'go.sum', 'docs/agent/llms-full.txt',
                     'docs/agent/skills/example.md', 'config/internal-dns/commands/start',
                     'hack/ci/Dockerfile', 'hack/ci/images.hcl', 'hack/ci/build-images.sh']:
            with self.subTest(name=name):
                before = self.key()
                self.write(name, 'changed input')
                self.assertNotEqual(before, self.key())

    def test_rename_deletion_and_mode_invalidate(self):
        before = self.key()
        path = self.root / 'cmd/main.go'
        path.chmod(0o755)
        self.assertNotEqual(before, self.key())
        before = self.key()
        path.rename(self.root / 'cmd/renamed.go')
        self.assertNotEqual(before, self.key())
        before = self.key()
        (self.root / 'cmd/renamed.go').unlink()
        self.assertNotEqual(before, self.key())

    def test_changed_context_policy_fails_closed(self):
        self.write('hack/ci/Dockerfile.dockerignore', '**\n!new-input\n')
        with self.assertRaises(subprocess.CalledProcessError):
            self.key()


class ExtractionOverlapTests(unittest.TestCase):
    def run_job(self, extract, setup_code=0, execution_code=0):
        with tempfile.TemporaryDirectory() as directory:
            timing = str(Path(directory) / 'timings.jsonl')
            with patch.dict(os.environ, {'CI_TIMINGS_FILE': timing,
                                         'DNS_E2E_EXTRACT_GO_CACHE': extract}), \
                 patch.object(warm_job.lanes, 'run_lanes', side_effect=[setup_code, execution_code]) as run:
                result = warm_job.main()
                return result, run.call_args_list

    def test_miss_extracts_alongside_tests_and_propagates_failure(self):
        result, calls = self.run_job('true', execution_code=1)
        self.assertEqual(result, 1)
        self.assertEqual(set(calls[1].args[0]), {'tests', 'cache'})
        self.assertEqual(calls[1].args[0]['cache'][0][1], ['bash', 'hack/ci/go-cache.sh', 'extract'])

    def test_hit_skips_extraction(self):
        result, calls = self.run_job('false')
        self.assertEqual(result, 0)
        self.assertEqual(set(calls[1].args[0]), {'tests'})

    def test_setup_failure_prevents_tests_and_extraction(self):
        result, calls = self.run_job('true', setup_code=1)
        self.assertEqual(result, 1)
        self.assertEqual(len(calls), 1)
