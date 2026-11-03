#!/usr/bin/env bash
# Run the DNSdist UDP integration test inside the serving image, so the
# generated rules are checked against the dnsdist build that ships.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$repo_root"
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

CGO_ENABLED=0 go test -c -o "$work/serving.test" ./internal/internaldns/serving
image=$(sed -n 's/^FROM \(powerdns\/dnsdist[^ ]*\) AS serving$/\1/p' config/internal-dns/Dockerfile)
if [ -z "$image" ]; then
  echo "config/internal-dns/Dockerfile has no 'FROM powerdns/dnsdist... AS serving' stage" >&2
  exit 1
fi
docker run --rm --network none \
  --mount "type=bind,src=$work/serving.test,dst=/serving.test,readonly" \
  --entrypoint /serving.test \
  -e INTERNAL_DNS_TEST_DNSDIST=/usr/local/bin/dnsdist \
  "$image" -test.run '^TestDNSDistUDPIntegration$' -test.v -test.timeout 30s
