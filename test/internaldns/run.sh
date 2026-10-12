#!/usr/bin/env bash
# Task entry point for the shared Kubernetes qualification.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"
if (( $# )); then
  echo 'Use Task env:internal-dns:up / env:internal-dns:qualify; this wrapper takes no arguments.' >&2
  exit 2
fi
task --yes env:internal-dns:up
task --yes env:internal-dns:qualify
