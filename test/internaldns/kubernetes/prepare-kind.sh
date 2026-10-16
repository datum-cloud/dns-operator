#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)
cd "$root"
mkdir -p .internal-dns-e2e/kubernetes
# Kind runs against the selected Docker host. A remote VM may not share this
# checkout's filesystem, so stage the common audit policy on that Docker host.
docker run --rm -i \
  -v /tmp/datum-internal-dns-audit:/audit \
  alpine@sha256:5291449c3df73caf6ed85e649dec1b9e818b39a5d8c871e97afc13e9cd5e8fa8 \
  sh -ec 'cat > /audit/audit-policy.yaml; chmod 644 /audit/audit-policy.yaml' \
  < .test-infra/cluster/audit-policy.yaml
sed 's|${REPO_DIR}/cluster/audit-policy.yaml|/tmp/datum-internal-dns-audit/audit-policy.yaml|g' \
  .test-infra/cluster/kind-config.yaml > .internal-dns-e2e/kubernetes/kind-config.yaml
cat >> .internal-dns-e2e/kubernetes/kind-config.yaml <<'EOF'
networking:
  ipFamily: dual
EOF
