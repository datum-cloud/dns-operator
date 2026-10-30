#!/usr/bin/env bash
# Fail when `go mod tidy` would change go.mod or go.sum in any module given
# as an argument.
set -euo pipefail

if [ $# -eq 0 ]; then
  echo "usage: $0 MODULE_DIR..." >&2
  exit 2
fi

failed=0
for dir in "$@"; do
  if [ ! -f "$dir/go.mod" ]; then
    echo "$dir: no go.mod" >&2
    exit 2
  fi
  if ! (cd "$dir" && go mod tidy -diff); then
    echo "$dir: go.mod or go.sum is not tidy; run 'go mod tidy' in $dir and commit the result" >&2
    failed=1
  fi
done
exit "$failed"
