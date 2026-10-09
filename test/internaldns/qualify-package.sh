#!/usr/bin/env bash
# Qualify the shipped runtime artifact without claiming deployment parity.
set -euo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
cd "$root"
evidence_dir=${INTERNAL_DNS_PACKAGE_EVIDENCE:-test/internaldns/results/package}
mkdir -p "$evidence_dir"
evidence_dir=$(cd "$evidence_dir" && pwd)

# The RBAC test skips without a real API server. Require its binaries rather
# than accepting that skip as deployment qualification.
if [[ -z ${KUBEBUILDER_ASSETS:-} ]]; then
  make setup-envtest > "$evidence_dir/envtest.log" 2>&1
  k8s_version=$(go list -m -f '{{.Version}}' k8s.io/api | awk -F'[v.]' '{printf "1.%d", $3}')
  KUBEBUILDER_ASSETS=$(bin/setup-envtest use "$k8s_version" --bin-dir bin -p path)
  export KUBEBUILDER_ASSETS
fi
# go test runs from the package directory, not this script's repository root.
# setup-envtest can return a relative path when --bin-dir is relative.
KUBEBUILDER_ASSETS=$(cd "$KUBEBUILDER_ASSETS" && pwd)
export KUBEBUILDER_ASSETS
test -x "$KUBEBUILDER_ASSETS/kube-apiserver"
test -x "$KUBEBUILDER_ASSETS/etcd"
go test ./internal/internaldns/runtime \
  -run '^(TestDeploymentExampleConfigsValidate|TestDeploymentRolesAuthorizeWorkerAndDiscoveryBoundaries|TestNATSExampleUsesExactMemberAckSubjects|TestEveryMemberCanCreateAllRuntimeConsumers|TestReadOnlyServingImageHasWritableBINDWorkingDirectory)$' \
  -count=1 -v > "$evidence_dir/contracts.log" 2>&1
# Use the same pinned Kustomize binary as installation and the public E2E job.
make kustomize > "$evidence_dir/kustomize.log" 2>&1
bin/kustomize build config/crd > "$evidence_dir/crds.yaml"
crd_files=(config/crd/bases/*.yaml)
test "$(grep -c '^kind: CustomResourceDefinition$' "$evidence_dir/crds.yaml")" -eq "${#crd_files[@]}"
bin/kustomize build config/internal-dns > "$evidence_dir/deployment-examples.yaml"

# Run by immutable local image ID, so an unrelated tag cannot replace the
# artifact between build and verification. Docker selects the host architecture.
docker build --file config/internal-dns/Dockerfile \
  --iidfile "$evidence_dir/image.id" . > "$evidence_dir/build.log" 2>&1
image_id=$(cat "$evidence_dir/image.id")
docker image inspect "$image_id" > "$evidence_dir/image.json"
test "$(docker image inspect --format '{{.Config.User}}' "$image_id")" = '65532:65532'
docker run --rm --network none --read-only --cap-drop ALL \
  --security-opt no-new-privileges "$image_id" --help \
  > "$evidence_dir/help.log" 2>&1
# Confirm this is the internal DNS entry point rather than an image that only
# happens to exit successfully.
grep -q 'Process role: control-plane, agent, or watchdog' "$evidence_dir/help.log"
git rev-parse HEAD > "$evidence_dir/source.commit"
git status --porcelain > "$evidence_dir/source.status"
printf 'Internal DNS package qualification passed; evidence: %s\n' "$evidence_dir"
