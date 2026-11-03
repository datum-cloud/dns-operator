#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$root"
qualify=(go run ./test/internaldns/kubernetes/qualify)
if [[ ${DNS_E2E_PREBUILT_IMAGES:-0} == 1 ]]; then
  test -x bin/dns-e2e-qualify
  qualify=(bin/dns-e2e-qualify)
fi
exec "${qualify[@]}" --environment .internal-dns-e2e/kubernetes/environment.json --results test/internaldns/results/kubernetes.json
