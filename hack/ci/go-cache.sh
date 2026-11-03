#!/usr/bin/env bash
set -euo pipefail
mode=${1:?Expected inject or extract}
case "$mode" in
  inject) args=() ;;
  extract) args=(--extract) ;;
  *) echo "Unknown cache operation: $mode" >&2; exit 1 ;;
esac
node .ci-cache-dance/dist/index.js "${args[@]}" \
  --cache-map '{".ci-go-cache/modules":"/go/pkg/mod",".ci-go-cache/build":"/root/.cache/go-build"}' \
  --scratch-dir .ci-cache-dance-scratch
