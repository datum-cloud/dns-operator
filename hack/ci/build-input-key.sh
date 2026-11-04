#!/usr/bin/env bash
set -euo pipefail

# Keep the input selection conservative and fail closed if the context policy changes.
expected=$(cat <<'POLICY'
**
!**/*.go
**/*_test.go
!go.mod
!go.sum
!docs/agent/llms-full.txt
!docs/agent/skills/*.md
!config/internal-dns/commands/**
.ci-*/**
.ci-*
POLICY
)
if [[ $(sed '/^#/d; /^[[:space:]]*$/d' hack/ci/Dockerfile.dockerignore) != "$expected" ]]; then
  echo 'Update build-input-key.sh for the changed Docker context policy' >&2
  exit 1
fi
manifest=$(mktemp)
trap 'rm -f "$manifest"' EXIT
count=0
while IFS= read -r -d '' path; do
  [[ -f "$path" && ! -L "$path" ]] || { echo "Unsupported build input: $path" >&2; exit 1; }
  mode=644
  [[ ! -x "$path" ]] || mode=755
  printf '%s\0%s\0' "$mode" "$path" >> "$manifest"
  sha256sum < "$path" >> "$manifest"
  count=$((count + 1))
done < <(git ls-files --cached --others --exclude-standard -z -- '*.go' go.mod go.sum \
  docs/agent/llms-full.txt 'docs/agent/skills/*.md' 'config/internal-dns/commands/**' \
  hack/ci/Dockerfile hack/ci/Dockerfile.dockerignore hack/ci/images.hcl hack/ci/build-images.sh \
  ':(exclude,glob).ci-*/**' | sort -zu)
(( count > 0 )) || { echo 'No build inputs found' >&2; exit 1; }
printf 'key=%s\n' "$(sha256sum < "$manifest" | cut -d ' ' -f 1)"
