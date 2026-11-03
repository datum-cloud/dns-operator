#!/usr/bin/env bash
# Validate the restored fleet and positive fixtures before an explicit cleanup.
set -euo pipefail
if [[ $# -ne 2 ]]; then
  echo "usage: $0 expectations.json NEW_EVIDENCE_DIR" >&2
  exit 2
fi
spec=$1
output=$2
for command in jq kubectl shasum awk sort diff; do command -v "$command" >/dev/null; done
jq -e '
  (.context | type == "string" and length > 0) and
  (.members | type == "array" and length > 0) and
  all(.members[];
    all([.namespace,.pod,.uid,.container,.config,.config_sha256,.configmap.name,.configmap.uid,.configmap.sha256][]; type == "string" and length > 0) and
    (.config | startswith("/")) and (.config_sha256 | test("^[a-f0-9]{64}$")) and
    (.images | type == "object" and length > 0) and all(.images[]; test("@sha256:[a-f0-9]{64}$"))) and
  (.probes | type == "array" and length > 0) and
  all(.probes[];
    all([.namespace,.pod,.uid,.container,.resolver,.name][];type == "string" and length > 0) and
    (.name | test("^[a-zA-Z0-9_.-]+[.]$")) and (.type == "A" or .type == "AAAA") and
    (.answers | type == "array" and length > 0) and all(.answers[];type == "string" and test("^[a-fA-F0-9:.]+$")))
' "$spec" >/dev/null
[[ ! -e $output ]] || { echo "refusing existing evidence directory: $output" >&2; exit 2; }
mkdir -p "$output"
cp "$spec" "$output/expectations.json"
context=$(jq -r .context "$spec")
cli=(kubectl --context "$context" --request-timeout=15s)
started=$(date -u +%Y-%m-%dT%H:%M:%SZ)
SECONDS=0
passed=false
watchdog=
finish() {
  [[ -z $watchdog ]] || kill "$watchdog" 2>/dev/null || true
  if [[ $passed != true ]]; then
    printf '%s\n' 'validation failed; no cleanup authorization produced' >&2
    printf '{"validation":"failed","cleanup_performed":false}\n' >"$output/result.json"
  fi
}
trap finish EXIT
trap 'exit 1' TERM INT
(
  timer=
  trap '[[ -z $timer ]] || kill "$timer" 2>/dev/null || true; exit' TERM INT
  sleep 60 &
  timer=$!
  wait "$timer"
  kill -TERM "$$" 2>/dev/null || true
) &
watchdog=$!

check_member() {
  local member=$1 label=$2 namespace pod uid container config cm images hash
  namespace=$(jq -r .namespace <<<"$member"); pod=$(jq -r .pod <<<"$member")
  uid=$(jq -r .uid <<<"$member"); container=$(jq -r .container <<<"$member")
  config=$(jq -r .config <<<"$member"); cm=$(jq -r .configmap.name <<<"$member")
  images=$(jq -c .images <<<"$member")
  "${cli[@]}" -n "$namespace" get pod "$pod" -o json >"$output/$label-pod.json"
  jq -e --arg uid "$uid" --arg cm "$cm" --arg container "$container" --argjson images "$images" '
    .metadata.uid == $uid and .metadata.deletionTimestamp == null and .status.phase == "Running" and
    (.spec.containers | map({key:.name,value:.image}) | from_entries) == $images and
    any(.spec.containers[]; .name == $container) and
    any(.spec.volumes[]; .configMap.name == $cm) and
    (.status.containerStatuses | length) == (.spec.containers | length) and
    all(.status.containerStatuses[]; .ready == true)
  ' "$output/$label-pod.json" >/dev/null
  "${cli[@]}" -n "$namespace" get configmap "$cm" -o json >"$output/$label-configmap.json"
  jq -e --arg uid "$(jq -r .configmap.uid <<<"$member")" '.metadata.uid == $uid' "$output/$label-configmap.json" >/dev/null
  hash=$(jq -S -c .data "$output/$label-configmap.json" | shasum -a 256 | awk '{print $1}')
  [[ $hash == "$(jq -r .configmap.sha256 <<<"$member")" ]]
  "${cli[@]}" -n "$namespace" exec "$pod" -c "$container" -- sha256sum "$config" >"$output/$label-config-sha256.txt"
  [[ $(awk '{print $1}' "$output/$label-config-sha256.txt") == "$(jq -r .config_sha256 <<<"$member")" ]]
  "${cli[@]}" -n "$namespace" exec "$pod" -c "$container" -- /internal-dns -role activation-check -config "$config" >"$output/$label-activation.txt" 2>"$output/$label-activation-stderr.txt"
  [[ $(cat "$output/$label-activation.txt") =~ ^[a-f0-9]{64}$ ]]
}

index=0
while IFS= read -r member; do check_member "$member" "before-$index"; index=$((index+1)); done < <(jq -c '.members[]' "$spec")
index=0
while IFS= read -r probe; do
  namespace=$(jq -r .namespace <<<"$probe"); pod=$(jq -r .pod <<<"$probe")
  uid=$(jq -r .uid <<<"$probe"); container=$(jq -r .container <<<"$probe")
  resolver=$(jq -r .resolver <<<"$probe"); name=$(jq -r .name <<<"$probe"); type=$(jq -r .type <<<"$probe")
  "${cli[@]}" -n "$namespace" get pod "$pod" -o json >"$output/probe-$index-pod.json"
  jq -e --arg uid "$uid" --arg container "$container" '.metadata.uid == $uid and .metadata.deletionTimestamp == null and any(.status.containerStatuses[]; .name == $container and .ready == true)' "$output/probe-$index-pod.json" >/dev/null
  jq -r '.answers[]' <<<"$probe" | LC_ALL=C sort -u >"$output/probe-$index-expected.txt"
  for protocol in udp tcp; do
    flag=+notcp; [[ $protocol != tcp ]] || flag=+tcp
    file="$output/probe-$index-$protocol.txt"
    "${cli[@]}" -n "$namespace" exec "$pod" -c "$container" -- dig "$flag" +ignore +nocookie +time=3 +tries=1 +noall +comments +answer "@$resolver" "$name" "$type" >"$file" 2>&1
    awk -v name="$name" -v type="$type" '
      /HEADER/ {if ($0 ~ /status: NOERROR,/) ok=1}
      /^;; flags:/ {for(i=3;i<=NF;i++){if($i=="aa" || $i=="aa;")aa=1;if($i=="tc" || $i=="tc;")bad=1}}
      /^[^;]/ && NF {if(tolower($1)!=tolower(name) || $3!="IN" || $4!=type || NF!=5)bad=1;else{print $5;count++}}
      END {if(!ok || !aa || bad || count==0)exit 1}
    ' "$file" | LC_ALL=C sort -u >"$output/probe-$index-$protocol-answers.txt"
    diff -u "$output/probe-$index-expected.txt" "$output/probe-$index-$protocol-answers.txt"
  done
  "${cli[@]}" -n "$namespace" get pod "$pod" -o json >"$output/probe-$index-after-pod.json"
  jq -e --arg uid "$uid" --arg container "$container" '.metadata.uid == $uid and .metadata.deletionTimestamp == null and any(.status.containerStatuses[]; .name == $container and .ready == true)' "$output/probe-$index-after-pod.json" >/dev/null
  index=$((index+1))
done < <(jq -c '.probes[]' "$spec")
index=0
while IFS= read -r member; do
  check_member "$member" "after-$index"
  diff -u "$output/before-$index-activation.txt" "$output/after-$index-activation.txt"
  index=$((index+1))
done < <(jq -c '.members[]' "$spec")
[[ $SECONDS -le 60 ]] || { echo 'validation exceeded 60s; rerun before cleanup' >&2; exit 1; }
ended=$(date -u +%Y-%m-%dT%H:%M:%SZ)
jq -n --arg started "$started" --arg finished "$ended" '{validation:"passed",started:$started,finished:$finished,cleanup_performed:false}' >"$output/result.json"
passed=true
