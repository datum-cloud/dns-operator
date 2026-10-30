#!/usr/bin/env bash
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
test_dir="$(mktemp -d)"
trap 'rm -rf "$test_dir"' EXIT
jsonnet "$repo_root/observability/dashboards/private-dns/tests/latency.test.jsonnet" > "$test_dir/latency.test.yaml"
"${PROMTOOL:-promtool}" test rules "$test_dir/latency.test.yaml"
