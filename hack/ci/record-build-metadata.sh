#!/usr/bin/env bash
set -euo pipefail
printf '%s\n' "$BUILD_METADATA" > dev/publish-metadata.json
bash hack/ci/publish-images.sh digests
