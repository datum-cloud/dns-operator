#!/usr/bin/env bash
set -euo pipefail
jq -n --arg operator "${OPERATOR_DIGEST:-}" --arg runtime "${RUNTIME_DIGEST:-}" \
  --arg serving "${SERVING_DIGEST:-}" \
  '{operator: {"containerimage.digest": $operator}, runtime: {"containerimage.digest": $runtime}, serving: {"containerimage.digest": $serving}}' \
  > dev/publish-metadata.json
bash "$(dirname "${BASH_SOURCE[0]}")/publish-images.sh" digests
