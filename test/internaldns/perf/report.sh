#!/usr/bin/env bash
# Aggregate raw query samples before exporting; never average percentiles.
set -euo pipefail
here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
: "${1:?pass a completed results directory}"
"$here/analyze.sh" "$1"
exec python3 "$here/report.py" "$@"
