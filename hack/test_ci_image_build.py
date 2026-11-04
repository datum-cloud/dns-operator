"""Image-cache orchestration with a fake Docker client."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).parent / 'ci/build-images.sh'


class ImageBuildTests(unittest.TestCase):
    def run_build(self, hit=False, complete=True, fail_build=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            for target in ['build', 'public', 'private'] if complete else ['public', 'private']:
                index = root / '.ci-buildkit-cache' / target / 'index.json'
                index.parent.mkdir(parents=True, exist_ok=True)
                index.write_text('{}')
            fake = root / 'docker'
            fake.write_text('''#!/bin/bash
set -eu
printf '%s|from=%s|to=%s\\n' "$*" "${CACHE_FROM:-}" "${CACHE_TO:-}" >> calls
if [[ "$1 $2" == 'buildx bake' ]]; then
  if [[ "${@: -1}" == build && "$FAIL_BUILD" == true ]]; then exit 42; fi
  if [[ -n "${CACHE_TO:-}" ]]; then
    for target in build public private; do
      mkdir -p "$CACHE_TO/$target"
      printf '{"fresh":true}' > "$CACHE_TO/$target/index.json"
    done
  fi
elif [[ "$1" == create ]]; then
  echo helper-container
elif [[ "$1" == cp ]]; then
  touch "$3"
fi
''')
            fake.chmod(0o755)
            result = subprocess.run(['bash', str(SCRIPT)], cwd=root, text=True, capture_output=True,
                env={**os.environ, 'PATH': str(root) + ':' + os.environ['PATH'],
                     'DNS_BUILD_CACHE_HIT': str(hit).lower(), 'FAIL_BUILD': str(fail_build).lower(),
                     'CACHE_TO': 'untrusted-inherited-export'})
            calls = (root / 'calls').read_text().splitlines() if (root / 'calls').exists() else []
            index = root / '.ci-buildkit-cache/public/index.json'
            return result, calls, index.read_text()

    def test_compilation_finishes_before_runtime_assembly(self):
        result, calls, index = self.run_build()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('images.hcl build|', calls[0])
        self.assertIn('images.hcl public private --load|', calls[1])
        self.assertIn('to=.ci-buildkit-cache-next', calls[0])
        self.assertIn('fresh', index)

    def test_exact_hit_imports_all_three_and_does_not_export(self):
        result, calls, index = self.run_build(hit=True)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertTrue(all('from=.ci-buildkit-cache|to=' in c for c in calls[:2]))
        self.assertTrue(all(c.endswith('|to=') for c in calls[:2]))
        self.assertEqual(index, '{}')

    def test_incomplete_exact_archive_fails_before_build(self):
        result, calls, _ = self.run_build(hit=True, complete=False)
        self.assertNotEqual(result.returncode, 0)
        self.assertEqual(calls, [])

    def test_legacy_fallback_does_not_import_partial_layout(self):
        result, calls, _ = self.run_build(complete=False)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('|from=|', calls[0])

    def test_compile_failure_prevents_assembly_and_cache_replacement(self):
        result, calls, index = self.run_build(fail_build=True)
        self.assertEqual(result.returncode, 42)
        self.assertEqual(len(calls), 1)
        self.assertEqual(index, '{}')
