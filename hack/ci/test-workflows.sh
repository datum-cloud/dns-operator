#!/usr/bin/env bash
set -euo pipefail
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT
cd "$work"
export RUNNER_TEMP="$work" GITHUB_OUTPUT="$work/output" GITHUB_SHA=0123456789012345678901234567890123456789
export OPERATOR_METADATA='{"tags":["ghcr.io/datum-cloud/dns-operator:v0.0.0-test"],"labels":{"test":"operator"}}'
export RUNTIME_METADATA='{"tags":["ghcr.io/datum-cloud/internal-dns:v0.0.0-test"],"labels":{"test":"runtime"}}'
export SERVING_METADATA='{"tags":["ghcr.io/datum-cloud/internal-dns-serving:v0.0.0-test"],"labels":{"test":"serving"}}'
bash "$root/hack/ci/publish-images.sh" prepare
jq -e '.group.default.targets | length == 3' dev/publish-bake.json
jq -e '[.target[] | .["cache-to"][0]] | unique | length == 3' dev/publish-bake.json
jq -e '.target.runtime.args == .target.serving.args' dev/publish-bake.json
jq -e 'all(.target[]; .platforms == ["linux/amd64", "linux/arm64"] and (.output[0] | startswith("type=oci,")))' dev/publish-bake.json
if OPERATOR_METADATA='{"tags":[],"labels":{}}' bash "$root/hack/ci/publish-images.sh" prepare; then
  echo 'Empty image metadata passed' >&2
  exit 1
fi
printf '{}\n' > dev/publish-metadata.json
if bash "$root/hack/ci/publish-images.sh" digests; then
  echo 'Missing build digests passed' >&2
  exit 1
fi
JOB_RESULTS='{"tests":{"result":"success"},"image":{"result":"success"}}' bash "$root/hack/ci/check-validation.sh"
for result in failure cancelled skipped; do
  if JOB_RESULTS="{\"tests\":{\"result\":\"$result\"},\"image\":{\"result\":\"success\"}}" bash "$root/hack/ci/check-validation.sh"; then
    echo "Invalid validation result passed: $result" >&2
    exit 1
  fi
done
if JOB_RESULTS='{}' bash "$root/hack/ci/check-validation.sh"; then
  echo 'Missing validation results passed' >&2
  exit 1
fi
mkdir fake-bin
cat > fake-bin/git <<'MOCK'
#!/usr/bin/env bash
set -euo pipefail
case "$1" in
  fetch) ;;
  diff) printf '%s' "$CHANGED_FILES" ;;
  *) exit 1 ;;
esac
MOCK
chmod +x fake-bin/git
export PATH="$work/fake-bin:$PATH"
export GITHUB_EVENT_PATH="$work/event.json"
printf '{"pull_request":{"base":{"sha":"base"},"head":{"sha":"head"}}}\n' > "$GITHUB_EVENT_PATH"
export GITHUB_EVENT_NAME=pull_request
for file in observability/alerts.yaml config/components/dashboards/test.yaml hack/test-observability-rules.sh Taskfile.yaml .github/workflows/test.yml go.mod; do
  : > "$GITHUB_OUTPUT"
  CHANGED_FILES="$file" bash "$root/hack/ci/observability-changes.sh"
  grep -qx 'run=true' "$GITHUB_OUTPUT"
done
: > "$GITHUB_OUTPUT"
CHANGED_FILES=README.md bash "$root/hack/ci/observability-changes.sh"
grep -qx 'run=false' "$GITHUB_OUTPUT"
if CHANGED_FILES='' bash "$root/hack/ci/observability-changes.sh"; then
  echo 'Empty change list passed' >&2
  exit 1
fi
: > "$GITHUB_OUTPUT"
GITHUB_EVENT_NAME=push bash "$root/hack/ci/observability-changes.sh"
grep -qx 'run=true' "$GITHUB_OUTPUT"
