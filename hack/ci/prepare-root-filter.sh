#!/usr/bin/env bash
set -euo pipefail
mv "$GITHUB_WORKSPACE/.root-filter/scripts/drop-distrusted-roots" "$RUNNER_TEMP/drop-distrusted-roots"
rm -rf "$GITHUB_WORKSPACE/.root-filter"
