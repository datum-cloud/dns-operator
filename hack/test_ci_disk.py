"""Exercise disk policy with fake commands; never remove files or invoke Docker."""
import os
from pathlib import Path
import subprocess
import tempfile
import unittest

SCRIPT = Path(__file__).parent / 'ci/prepare-disk.sh'


class DiskPolicyTests(unittest.TestCase):
    def run_policy(self, workspace='40000000', docker='40000000', after='40000000', fail_df=False):
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            commands = {
                'docker': "#!/bin/bash\nprintf '/fake/docker\\n'\n",
                'df': '''#!/bin/bash
if [[ "$FAIL_DF" == true ]]; then exit 1; fi
value=$WORKSPACE_FREE
[[ "${@: -1}" != /fake/docker ]] || value=$DOCKER_FREE
[[ ! -f "$STATE/cleaned" ]] || value=$AFTER_FREE
printf 'Filesystem 1024-blocks Used Available Capacity Mounted\\n'
printf 'fake 100000000 0 %s 0%% /\\n' "$value"
''',
                'sudo': '''#!/bin/bash
printf '%s\\n' "$*" >> "$STATE/actions"
touch "$STATE/cleaned"
''',
            }
            for name, body in commands.items():
                path = root / name
                path.write_text(body)
                path.chmod(0o755)
            result = subprocess.run(['bash', str(SCRIPT)], text=True, capture_output=True,
                env={**os.environ, 'PATH': str(root) + ':' + os.environ['PATH'], 'STATE': str(root),
                     'WORKSPACE_FREE': workspace, 'DOCKER_FREE': docker, 'AFTER_FREE': after,
                     'FAIL_DF': str(fail_df).lower()})
            actions = (root / 'actions').read_text() if (root / 'actions').exists() else ''
            return result, actions

    def test_enough_space_skips_all_cleanup(self):
        result, actions = self.run_policy()
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn('Skipping disk cleanup', result.stdout)
        self.assertEqual(actions, '')

    def test_threshold_is_inclusive(self):
        result, actions = self.run_policy(workspace=str(30 * 1024 * 1024), docker=str(30 * 1024 * 1024))
        self.assertEqual(result.returncode, 0)
        self.assertEqual(actions, '')

    def test_either_filesystem_can_require_cleanup(self):
        for free in [{'workspace': '14000000'}, {'docker': '14000000'}]:
            with self.subTest(free=free):
                result, actions = self.run_policy(**free)
                self.assertEqual(result.returncode, 0, result.stderr)
                self.assertIn('rm -rf /usr/share/dotnet', actions)
                self.assertIn('docker image prune --all --force', actions)

    def test_still_low_after_cleanup_fails(self):
        result, actions = self.run_policy(workspace='14000000', after='14000000')
        self.assertNotEqual(result.returncode, 0)
        self.assertTrue(actions)

    def test_unreadable_space_fails_without_cleanup(self):
        for args in [{'workspace': 'invalid'}, {'workspace': ''}, {'fail_df': True}]:
            with self.subTest(args=args):
                result, actions = self.run_policy(**args)
                self.assertNotEqual(result.returncode, 0)
                self.assertEqual(actions, '')
