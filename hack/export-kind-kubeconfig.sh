#!/usr/bin/env bash
# Export a kind cluster's kubeconfig with the server rewritten to the
# control-plane container's address on the kind docker network, so pods in
# other kind clusters can reach the API server.
#
# `kind get kubeconfig` reports the host-published port, which is only valid
# on the host. Inside the docker network the API server listens on 6443, so
# the port is rewritten too. Set KIND_KUBECONFIG_HOST and KIND_KUBECONFIG_PORT
# to point at a different endpoint, and KIND_KUBECONFIG_INSECURE=true to skip
# TLS verification (dev only).
set -euo pipefail

if [ $# -ne 2 ]; then
  echo "usage: $0 CLUSTER OUT" >&2
  exit 2
fi
cluster=$1
out=$2
kind=${KIND:-kind}
port=${KIND_KUBECONFIG_PORT:-6443}
host=${KIND_KUBECONFIG_HOST:-}
if [ -z "$host" ]; then
  host=$(docker inspect --format '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' \
    "$cluster-control-plane" 2>/dev/null | head -n 1)
fi
if [ -z "$host" ]; then
  echo "cannot find the docker network address of $cluster-control-plane; set KIND_KUBECONFIG_HOST" >&2
  exit 1
fi

mkdir -p "$(dirname "$out")"
"$kind" get kubeconfig --name "$cluster" \
  | sed -E "s#(server:[[:space:]]*https://)[^:]+:[0-9]+#\1$host:$port#g" > "$out"
if ! grep -q "server: https://$host:$port" "$out"; then
  echo "kubeconfig for $cluster has no server line to rewrite" >&2
  exit 1
fi

if [ "${KIND_KUBECONFIG_INSECURE:-}" = "true" ]; then
  echo "Rewriting kubeconfig to skip TLS verify (dev only)"
  sed -E "s#^([[:space:]]*)certificate-authority-data:.*#\1insecure-skip-tls-verify: true#" "$out" > "$out.tmp"
  mv "$out.tmp" "$out"
fi
