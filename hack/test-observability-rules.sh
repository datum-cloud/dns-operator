#!/usr/bin/env bash
# Requires Python 3 + PyYAML and promtool >= 3 (UTF-8 OTel label names).
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
python3 - "$repo_root" "$test_dir" <<'PY'
import pathlib
import sys
import yaml
root, output = map(pathlib.Path, sys.argv[1:])
rules = yaml.safe_load((root / 'config/observability/rules/dns-drift-rules.yaml').read_text())
(output / 'rules.yaml').write_text(yaml.safe_dump(rules['spec']))
tests = yaml.safe_load((root / 'test/observability/dns-drift.test.yaml').read_text())
tests['rule_files'] = [str(output / 'rules.yaml')]
(output / 'tests.yaml').write_text(yaml.safe_dump(tests))
PY
"${PROMTOOL:-promtool}" check rules "$test_dir/rules.yaml"
"${PROMTOOL:-promtool}" test rules "$test_dir/tests.yaml"
