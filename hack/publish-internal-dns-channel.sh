#!/usr/bin/env bash
set -euo pipefail

: "${RUNTIME_TAG:?runtime build tag is required}"
: "${SERVING_TAG:?serving build tag is required}"
: "${RUNNER_TEMP:?runner temporary directory is required}"
: "${GITHUB_REPOSITORY:?source repository is required}"
: "${GITHUB_SHA:?source revision is required}"
if [[ "$RUNTIME_TAG" != "$SERVING_TAG" || ! "$RUNTIME_TAG" =~ ^v[0-9]+\.[0-9]+\.[0-9]+[-a-zA-Z0-9.]*$ ]]; then
  echo 'Runtime and serving builds must share a valid version tag' >&2
  exit 1
fi

runtime_digest=$(docker buildx imagetools inspect "ghcr.io/datum-cloud/internal-dns:$RUNTIME_TAG" --format '{{.Manifest.Digest}}')
serving_digest=$(docker buildx imagetools inspect "ghcr.io/datum-cloud/internal-dns-serving:$SERVING_TAG" --format '{{.Manifest.Digest}}')
for digest in "$runtime_digest" "$serving_digest"; do
  if [[ ! "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo 'A published image manifest digest is required' >&2
    exit 1
  fi
done

channel_dir=$(mktemp -d "$RUNNER_TEMP/internal-dns-channel.XXXXXX")
trap 'rm -rf "$channel_dir"' EXIT
cat > "$channel_dir/kustomization.yaml" <<'EOF'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
resources:
  - images.yaml
EOF
cat > "$channel_dir/images.yaml" <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: internal-dns-images
  namespace: flux-system
  labels:
    reconcile.fluxcd.io/watch: Enabled
data:
  internal_dns_runtime_image: ghcr.io/datum-cloud/internal-dns@$runtime_digest
  internal_dns_serving_image: ghcr.io/datum-cloud/internal-dns-serving@$serving_digest
  source_revision: "$GITHUB_SHA"
EOF
flux push artifact "oci://ghcr.io/datum-cloud/internal-dns-builds:$RUNTIME_TAG" \
  --path="$channel_dir" \
  --source="https://github.com/$GITHUB_REPOSITORY" \
  --revision="$GITHUB_SHA"
