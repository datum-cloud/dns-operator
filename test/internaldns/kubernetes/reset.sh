#!/usr/bin/env bash
# Reset only owned private DNS fixtures; retain the shared bootstrap/public DNS.
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
work="$root/.internal-dns-e2e/kubernetes"
# Remove only the owned fixture's fail-closed webhook before stopping its
# service, otherwise admission could prevent namespace cleanup.
for file in project-a project-b; do
  cli=(kubectl --kubeconfig "$work/$file.kubeconfig")
  owner=$("${cli[@]}" get namespace project-e2e -o jsonpath='{.metadata.labels.internal-dns\.datum\.net/qualification}' 2>/dev/null || true)
  if [[ $owner == true ]]; then
    "${cli[@]}" delete clusterrolebinding internal-dns-control-plane --ignore-not-found
    webhook_owner=$("${cli[@]}" get validatingwebhookconfiguration internal-dns-e2e -o jsonpath='{.metadata.labels.internal-dns\.datum\.net/qualification}' 2>/dev/null || true)
    if [[ -n $webhook_owner && $webhook_owner != true ]]; then
      echo 'Refusing to delete an unowned admission webhook' >&2
      exit 1
    fi
    "${cli[@]}" delete validatingwebhookconfiguration internal-dns-e2e --ignore-not-found
  fi
done
for file in platform project-a project-b; do
  cli=(kubectl --kubeconfig "$work/$file.kubeconfig")
  namespace=project-e2e
  if [[ $file == platform ]]; then namespace=internal-dns-system; fi
  if "${cli[@]}" get namespace "$namespace" >/dev/null 2>&1; then
    owner=$("${cli[@]}" get namespace "$namespace" -o jsonpath='{.metadata.labels.internal-dns\.datum\.net/qualification}')
    if [[ $owner != true ]]; then
      echo "Refusing to reset unowned namespace $namespace on $file" >&2
      exit 1
    fi
    if [[ $file != platform ]]; then
      # The fixture may have deliberately stopped both controllers. Release
      # only the private-zone finalizers in this owned test namespace.
      while IFS= read -r zone; do
        [[ -z $zone ]] || "${cli[@]}" -n "$namespace" patch "$zone" --type merge -p '{"metadata":{"finalizers":[]}}'
      done < <("${cli[@]}" -n "$namespace" get dnszones -o name)
    fi
    "${cli[@]}" delete namespace "$namespace" --wait=true --timeout=90s
  fi
done
