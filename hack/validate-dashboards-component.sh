#!/usr/bin/env bash
# Check that the dashboards kustomize component builds. A component cannot be
# built on its own, so wrap it in a throwaway kustomization the way an
# installer would.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
check_dir=.dashboards-check
trap 'rm -rf "$check_dir"' EXIT
mkdir -p "$check_dir"
cat > "$check_dir/kustomization.yaml" <<'KUSTOMIZATION'
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: dashboards-check
components:
  - ../config/components/dashboards
KUSTOMIZATION
kubectl kustomize "$check_dir" > /dev/null
echo "OK: config/components/dashboards builds"
