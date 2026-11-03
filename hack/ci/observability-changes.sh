#!/usr/bin/env bash
set -euo pipefail
if [[ ${GITHUB_EVENT_NAME:-} != pull_request ]]; then
  echo 'run=true' >> "$GITHUB_OUTPUT"
  exit 0
fi
base=$(jq -er '.pull_request.base.sha' "$GITHUB_EVENT_PATH")
head=$(jq -er '.pull_request.head.sha' "$GITHUB_EVENT_PATH")
git fetch --no-tags --depth=1 origin "$base" "$head"
files=$(git diff --name-only "$base" "$head")
[[ -n "$files" ]] || { echo 'No changed paths found' >&2; exit 1; }
run=false
while IFS= read -r file; do
  case "$file" in
    observability/*|config/components/dashboards/*|hack/*|Taskfile.yaml|.github/*|go.mod|go.sum) run=true ;;
  esac
done <<< "$files"
echo "run=$run" >> "$GITHUB_OUTPUT"
