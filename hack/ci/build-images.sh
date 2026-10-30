#!/usr/bin/env bash
set -euo pipefail
# Cache mounts are injected/extracted separately by the pinned cache-dance action.
# Local BuildKit layer exports are saved with actions/cache after the job.
if [[ -f .ci-buildkit-cache/public/index.json && -f .ci-buildkit-cache/private/index.json ]]; then
  export CACHE_FROM=.ci-buildkit-cache
fi
if [[ ${DNS_BUILD_CACHE_HIT:-false} != true ]]; then
  export CACHE_TO=.ci-buildkit-cache-next
fi
BUILDKIT_PROGRESS=plain docker buildx bake -f hack/ci/images.hcl --load
if [[ ${DNS_BUILD_CACHE_HIT:-false} != true ]]; then
  rm -rf .ci-buildkit-cache
  mv .ci-buildkit-cache-next .ci-buildkit-cache
fi
mkdir -p bin
container=$(docker create internal-dns-kubernetes:e2e)
trap 'docker rm -f "$container" >/dev/null' EXIT
docker cp "$container:/dns-render" bin/dns-e2e-render
chmod +x bin/dns-e2e-render
