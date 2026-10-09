#!/usr/bin/env bash
set -euo pipefail

ROOT=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
source "$ROOT/test/internaldns/colima-env.sh"
exec "$ROOT/test/internaldns/multi_control_plane.py" "$@"
