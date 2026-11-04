#!/usr/bin/env bash
set -euo pipefail
# Cache mounts are injected/extracted separately by the pinned cache utility.
# Local BuildKit layer exports are saved with actions/cache after the job.
unset CACHE_FROM CACHE_TO
cache_complete=true
for target in build public private; do
  if [[ ! -s .ci-buildkit-cache/$target/index.json ]]; then cache_complete=false; fi
done
if [[ ${DNS_BUILD_CACHE_HIT:-false} == true && "$cache_complete" != true ]]; then
  echo 'Exact archive hit is missing a required image cache index' >&2
  exit 1
fi
if [[ "$cache_complete" == true ]]; then
  export CACHE_FROM=.ci-buildkit-cache
fi
if [[ ${DNS_BUILD_CACHE_HIT:-false} != true ]]; then
  export CACHE_TO=.ci-buildkit-cache-next
fi
# Finish the shared solve before either runtime target can request its outputs.
BUILDKIT_PROGRESS=plain docker buildx bake -f hack/ci/images.hcl build
BUILDKIT_PROGRESS=plain docker buildx bake -f hack/ci/images.hcl public private --load
if [[ ${DNS_BUILD_CACHE_HIT:-false} != true ]]; then
  for target in build public private; do
    if [[ ! -s .ci-buildkit-cache-next/$target/index.json ]]; then
      echo "Missing exported cache index: $target" >&2
      exit 1
    fi
  done
  rm -rf .ci-buildkit-cache
  mv .ci-buildkit-cache-next .ci-buildkit-cache
fi
mkdir -p bin
container=$(docker create internal-dns-kubernetes:e2e)
trap 'docker rm -f "$container" >/dev/null' EXIT
docker cp "$container:/dns-render" bin/dns-e2e-render
docker cp "$container:/dns-qualify" bin/dns-e2e-qualify
chmod +x bin/dns-e2e-render bin/dns-e2e-qualify
