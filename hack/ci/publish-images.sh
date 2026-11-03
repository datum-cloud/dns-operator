#!/usr/bin/env bash
set -euo pipefail
case "${IMAGE_GROUP:?required image group}" in
  operator) targets='["operator"]' ;;
  internal) targets='["runtime","serving"]' ;;
  *) echo 'Unknown image group' >&2; exit 2 ;;
esac
case "${1:-}" in
  prepare)
    mkdir -p dev
    jq -n -e \
      --argjson targets "$targets" --argjson operator "$OPERATOR_METADATA" \
      --argjson runtime "$RUNTIME_METADATA" \
      --argjson serving "$SERVING_METADATA" \
      --arg sha "$GITHUB_SHA" --arg directory "$RUNNER_TEMP" \
      --arg date "$(date -u +'%Y-%m-%dT%H:%M:%SZ')" '
      def target($name; $metadata; $file; $stage):
        if ($metadata.tags | length) == 0 then error("image has no tags") else
        {context: ".", dockerfile: $file, target: $stage,
         platforms: ["linux/amd64", "linux/arm64"],
         tags: $metadata.tags, labels: $metadata.labels,
         args: {VERSION: ($metadata.tags[0] | split(":")[-1]), GIT_COMMIT: $sha,
                GIT_TREE_STATE: "clean", BUILD_DATE: $date},
         output: ["type=oci,dest=" + $directory + "/" + $name + ",tar=false"],
         "cache-from": ["type=gha,scope=publish-" + $name],
         "cache-to": ["type=gha,scope=publish-" + $name + ",mode=max"]} end;
      {group: {default: {targets: $targets}},
       target: {operator: target("operator"; $operator; "Dockerfile"; ""),
                runtime: target("runtime"; $runtime; "config/internal-dns/Dockerfile"; "runtime"),
                serving: target("serving"; $serving; "config/internal-dns/Dockerfile"; "serving")}}
      ' > dev/publish-bake.json
    tag=$(jq -er '.tags[0] | split(":")[-1]' <<< "$OPERATOR_METADATA")
    echo "tag=$tag" >> "$GITHUB_OUTPUT"
    ;;
  digests)
    for name in $(jq -er 'join(" ")' <<< "$targets"); do
      digest=$(jq -er --arg name "$name" '.[$name]["containerimage.digest"] | select(test("^sha256:[0-9a-f]{64}$"))' dev/publish-metadata.json)
      echo "$name=$digest" >> "$GITHUB_OUTPUT"
    done
    ;;
  *) exit 2 ;;
esac
