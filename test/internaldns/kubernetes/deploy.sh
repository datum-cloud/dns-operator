#!/usr/bin/env bash
# Deploy onto the repository's shared test-infra clusters; never create a
# second Kind lifecycle or select the user's current Kubernetes context.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$root"
work="$root/.internal-dns-e2e/kubernetes"
mkdir -p "$work"
chmod 700 "$work"

for pair in 'dns-control platform' 'dns-upstream project-a' 'dns-edge project-b'; do
  read -r cluster file <<< "$pair"
  kind export kubeconfig --name "$cluster" --kubeconfig "$work/$file.kubeconfig"
  kind get kubeconfig --name "$cluster" --internal > "$work/$file-internal.kubeconfig"
  chmod 600 "$work/$file"*.kubeconfig
done
platform=(kubectl --kubeconfig "$work/platform.kubeconfig")
# Existing public-only clusters can use IPv4. Check the serving network before
# resetting any private fixtures; changing Kind networking requires fresh nodes.
pod_cidrs=$("${platform[@]}" get nodes -o jsonpath='{range .items[*]}{.spec.podCIDRs}{"\n"}{end}')
if ! grep -q ':' <<< "$pod_cidrs"; then
  echo 'Private DNS requires dual-stack dns-control nodes. Start env:internal-dns:up before env:stack-up on fresh shared clusters; existing fixtures were not reset.' >&2
  exit 2
fi
test/internaldns/kubernetes/reset.sh

make kustomize
bin/kustomize build config/crd > "$work/crds.yaml"
for file in platform project-a project-b; do
  kubectl --kubeconfig "$work/$file.kubeconfig" apply --server-side -f "$work/crds.yaml"
done

docker build -f test/internaldns/kubernetes/Dockerfile \
  --iidfile "$work/image.id" -t internal-dns-kubernetes:e2e .
kind load docker-image internal-dns-kubernetes:e2e --name dns-control
go run ./test/internaldns/kubernetes/render --phase bootstrap --out "$work"
"${platform[@]}" apply -f "$work/platform-bootstrap.json"
for project in project-a project-b; do
  kubectl --kubeconfig "$work/$project.kubeconfig" apply -f "$work/$project-bootstrap.json"
done
"${platform[@]}" -n internal-dns-system wait certificates --all --for=condition=Ready --timeout=180s
# Wait for network assignment before rendering concrete member addresses. The
# processes wait for ConfigMaps, so no placeholder serving configuration runs.
for pod in internal-dns-node internal-dns-regional-front internal-dns-regional-bind-0 internal-dns-regional-bind-1 internal-dns-probe; do
  "${platform[@]}" -n internal-dns-system wait --for=jsonpath='{.status.podIPs}' "pod/$pod" --timeout=180s
done
go run ./test/internaldns/kubernetes/render --phase configure --out "$work" \
  --platform "$work/platform.kubeconfig" \
  --source-a "$work/project-a.kubeconfig" --source-b "$work/project-b.kubeconfig" \
  --source-a-internal "$work/project-a-internal.kubeconfig" --source-b-internal "$work/project-b-internal.kubeconfig"
"${platform[@]}" apply -f "$work/platform-configure.json"
"${platform[@]}" -n internal-dns-system wait pod/internal-dns-network-fixture --for=jsonpath='{.status.phase}'=Succeeded --timeout=60s
"${platform[@]}" -n internal-dns-system rollout status statefulset/internal-dns-nats --timeout=180s
for controller in control-a control-b; do
  "${platform[@]}" -n internal-dns-system rollout status "deployment/$controller" --timeout=180s
done

# The source API servers reach a real TLS admission Service over the isolated
# Kind network. The broker and workers never use the development Python proxy.
ca_bundle=$("${platform[@]}" -n internal-dns-system get secret internal-dns-admission-tls -o jsonpath='{.data.ca\.crt}')
for project in project-a project-b; do
  sed -e "s|\${CA_BUNDLE}|$ca_bundle|g" \
      -e 's|host.docker.internal:9443|dns-control-control-plane:30443|g' \
      test/internaldns/fixtures/validating-webhook.yaml.tmpl > "$work/$project-webhook.yaml"
  kubectl --kubeconfig "$work/$project.kubeconfig" apply -f "$work/$project-webhook.yaml"
done

evidence="$root/test/internaldns/results/kubernetes-artifacts"
mkdir -p "$evidence"
cp "$work/crds.yaml" "$evidence/crds.yaml"
cp "$work/environment.json" "$evidence/environment.json"
cp "$work/image.id" "$evidence/image.id"
"${platform[@]}" -n internal-dns-system exec internal-dns-probe -- named -V > "$evidence/bind-version.txt"
"${platform[@]}" -n internal-dns-system exec internal-dns-probe -- dnsdist --version > "$evidence/dnsdist-version.txt"
git rev-parse HEAD > "$evidence/source.commit"
git status --short > "$evidence/source.status"
if [[ -s $evidence/source.status ]]; then
  printf 'dirty checkout; source.commit identifies the base, not the complete built source\n' > "$evidence/source.tree-state"
else
  printf 'clean checkout\n' > "$evidence/source.tree-state"
fi
printf 'Kubernetes private DNS environment ready: %s\n' "$work/environment.json"
