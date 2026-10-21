#!/usr/bin/env bash
set -euo pipefail
umask 077

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

free -h || true
df -h || true
sysctl fs.inotify.max_user_instances fs.inotify.max_user_watches || true

while IFS= read -r cluster; do
  [[ $cluster == dns-* ]] || continue
  echo "::group::$cluster bootstrap"
  if kind get kubeconfig --name "$cluster" > "$work/$cluster.kubeconfig"; then
    cli=(kubectl --kubeconfig "$work/$cluster.kubeconfig" --request-timeout=10s)
    "${cli[@]}" get nodes,pods -A -o wide || true
    "${cli[@]}" get events -A --sort-by=.lastTimestamp | tail -60 || true
  fi
  docker exec "$cluster-control-plane" journalctl -u kubelet --no-pager -n 120 || true
  echo "::endgroup::"
done < <(kind get clusters)
