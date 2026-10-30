#!/usr/bin/env bash
set -euo pipefail
# Cache mounts are injected/extracted separately by the pinned cache-dance action.
# Local BuildKit layer exports are saved with actions/cache after the job.
if [[ -f .ci-buildkit-cache/public/index.json && -f .ci-buildkit-cache/private/index.json ]]; then
  export CACHE_FROM=.ci-buildkit-cache
fi
export CACHE_TO=.ci-buildkit-cache-next
BUILDKIT_PROGRESS=plain docker buildx bake -f hack/ci/images.hcl --load
rm -rf .ci-buildkit-cache
mv .ci-buildkit-cache-next .ci-buildkit-cache
