#!/usr/bin/env bash
#
# Fails when a committed generated file no longer matches what the generators
# produce. Run after `task generate` and `task manifests`; CI runs it the same
# way so an API type or marker change without regeneration cannot merge.

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
readonly REPO_ROOT
cd "$REPO_ROOT"

readonly GENERATED_PATHS=(
  ':(glob)**/zz_generated.*'
  config/crd/bases
  config/rbac/role.yaml
  config/webhook/manifests.yaml
)

readonly FIX="task generate manifests"

step_summary() {
  [[ -n "${GITHUB_STEP_SUMMARY:-}" ]] || return 0
  cat >>"${GITHUB_STEP_SUMMARY}"
}

fail() {
  local heading="$1"
  shift

  echo "FAIL: ${heading}"
  echo ""
  printf '%s\n' "$@"
  echo ""
  echo "To fix this, run \`${FIX}\` and commit the result."

  {
    echo "## ${heading}"
    echo ""
    printf '%s\n' "$@"
    echo ""
    echo "### How to fix"
    echo '```bash'
    echo "${FIX}"
    echo '```'
  } | step_summary

  exit 1
}

tracked="$(git ls-files -- "${GENERATED_PATHS[@]}" | wc -l | tr -d ' ')"
if [[ "${tracked}" -eq 0 ]]; then
  fail "Generated paths match no tracked file" \
    "Nothing was compared, so nothing is verified:" \
    "$(printf '  %s\n' "${GENERATED_PATHS[@]}")"
fi

if ! drift="$(git diff HEAD -- "${GENERATED_PATHS[@]}")" || [[ -n "${drift}" ]]; then
  echo "${drift}"
  fail "Generated files are out of sync with their source" \
    "These generated files differ from what is committed:" \
    "$(git diff HEAD --name-only -- "${GENERATED_PATHS[@]}")"
fi

untracked="$(git ls-files --others --exclude-standard -- "${GENERATED_PATHS[@]}")"
if [[ -n "${untracked}" ]]; then
  fail "Generation produced files that are not committed" \
    "${untracked}"
fi

echo "OK: ${tracked} generated file(s) match their source"
{
  echo "## Generated files are in sync"
  echo ""
  echo "All ${tracked} generated file(s) match their source."
} | step_summary
