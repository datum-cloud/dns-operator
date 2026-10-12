#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$root"
work="$root/.internal-dns-e2e/kubernetes"
evidence="$root/test/internaldns/results/kubernetes-artifacts"
mkdir -p "$evidence"
for file in platform project-a project-b; do
  [[ -f $work/$file.kubeconfig ]] || continue
  cli=(kubectl --kubeconfig "$work/$file.kubeconfig" --request-timeout=10s)
  namespace=project-e2e
  if [[ $file == platform ]]; then namespace=internal-dns-system; fi
  "${cli[@]}" -n "$namespace" get pods,deployments,statefulsets,pvc,services,configmaps -o yaml > "$evidence/$file-deployment.yaml" 2>&1 || true
  "${cli[@]}" -n "$namespace" get events -o yaml > "$evidence/$file-events.yaml" 2>&1 || true
  "${cli[@]}" -n "$namespace" get dnszones,dnszoneassociations,dnsresolvercontexts,dnsresolveraccessbindings,dnsnamingpolicies,dnsmanagednamespaces,dnsregistrations,dnscontributiongrants,dnsrecordcontributions,dnsresolverbindings,dnspublicationmanifests,dnspublicationchunks,dnspublicationownerships,dnstransportoutboxes -o yaml > "$evidence/$file-dns.yaml" 2>&1 || true
  while IFS= read -r pod; do
    [[ -z $pod ]] || "${cli[@]}" -n "$namespace" logs "$pod" --all-containers --prefix --tail=1000 > "$evidence/$file-${pod#pod/}.log" 2>&1 || true
  done < <("${cli[@]}" -n "$namespace" get pods -o name 2>/dev/null)
done
# Do not archive generated bootstrap manifests, Secrets, or kubeconfigs.
