#!/usr/bin/env bash
# Drives the internal DNS k6 load test in a Datum Cloud project.
#
#   run.sh up          create target and k6 workloads, wait for them
#   run.sh run [ENV=VALUE ...]
#                      run dns.js on every k6 instance at once
#   run.sh propagate   rewrite a private TXT record on a timer
#   run.sh publish     create, update and delete record sets; time each answer
#   run.sh down        delete everything run.sh created
#   run.sh analyze [DIR]
#                      per-step and per-second stats from a run's query records
#
# See README.md for the variables each command reads.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
if [[ ${1:-} == analyze ]]; then
  shift
  exec "$here/analyze.sh" "$@"
fi

: "${PROJECT:?set PROJECT to the Datum Cloud project to test in}"
LOCATION=${LOCATION:-us-central-1}
NETWORK=${NETWORK:-default}
TARGETS=${TARGETS:-3}
GENERATORS=${GENERATORS:-1}
IMAGE=${IMAGE:-ghcr.io/datum-cloud/internal-dns-perf:v0.0.0-main}
ZONE_DOMAIN=${ZONE_DOMAIN:-perf.internal}
INSTANCE_DOMAIN=${INSTANCE_DOMAIN:-instances.datum.internal}
ZONE_CLASS=${ZONE_CLASS:-private-coredns}
INTERVAL=${INTERVAL:-10}
DURATION=${DURATION:-2m}
RESULTS=${RESULTS:-$here/results/$(date -u +%Y%m%dT%H%M%SZ)}

dctl() { datumctl "$@" --project "$PROJECT"; }

# The load lands on a node resolver shared with every other instance on that
# node, so refuse anything that is not staging unless the caller insists.
guard() {
  [[ -n "${ALLOW_ANY_ENV:-}" ]] && return 0
  local endpoint
  # The API host the project's requests actually go to; whoami output varies
  # between datumctl releases.
  endpoint=$(dctl get namespace default -v=6 2>&1 | grep -oE 'https://[^/ "]+' | head -1)
  if [[ "$endpoint" != *staging* ]]; then
    echo "refusing: project $PROJECT is served by '$endpoint', which is not staging (set ALLOW_ANY_ENV=1 to override)" >&2
    exit 1
  fi
}

# Substitutes ${VAR} placeholders from the environment without needing envsubst.
render() {
  perl -pe 's/\$\{(\w+)\}/exists $ENV{$1} ? $ENV{$1} : die "unset: $1\n"/ge' "$1"
}

instances() {
  local w=$1 n=$2 i
  for ((i = 0; i < n; i++)); do echo "$w-default-$LOCATION-$i"; done
}

wait_ready() {
  local w=$1 want=$2 ready
  for _ in $(seq 1 90); do
    ready=$(dctl get workload "$w" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || true)
    if [[ "${ready:-0}" -ge "$want" ]]; then return 0; fi
    sleep 10
  done
  echo "workload $w has ${ready:-0}/$want ready instances after 15m" >&2
  return 1
}

resolver_context() {
  dctl get dnsresolvercontexts -o name | sed 's|.*/||' | grep -E "^dns-$NETWORK-[0-9a-f]+$" | head -1
}

up() {
  export LOCATION NETWORK TARGETS GENERATORS IMAGE
  render "$here/manifests/targets.yaml" | dctl apply -f -
  render "$here/manifests/loadgen.yaml" | dctl apply -f -
  wait_ready idns-perf-target "$TARGETS"
  wait_ready idns-perf-k6 "$GENERATORS"
  echo "Private names: $(instances idns-perf-target "$TARGETS" | sed "s/\$/.$INSTANCE_DOMAIN/" | paste -sd, -)"
}

run() {
  local names pod i failed=0
  local -a pods=() pids=()
  names=$(instances idns-perf-target "$TARGETS" | sed "s/\$/.$INSTANCE_DOMAIN/" | paste -sd, -)
  while IFS= read -r pod; do pods+=("$pod"); done < <(instances idns-perf-k6 "$GENERATORS")
  mkdir -p "$RESULTS"
  for pod in "${pods[@]}"; do
    datumctl compute exec --project "$PROJECT" "$pod" -c k6 -- \
      env PRIVATE_NAMES="$names" DURATION="$DURATION" SUMMARY_PATH=/tmp/k6-summary.json K6_CSV_TIME_FORMAT=unix_milli "$@" \
      k6 run --quiet --system-tags=scenario --out csv=/tmp/k6-queries.csv.gz /perf/dns.js >"$RESULTS/$pod.txt" 2>&1 &
    pids+=($!)
  done
  for i in "${!pods[@]}"; do
    pod=${pods[$i]}
    if ! wait "${pids[$i]}"; then
      echo "$pod: k6 failed (thresholds crossed, or the run did not start)" >&2
      failed=1
    fi
    echo "== $pod"
    if grep -q '^internal DNS load test' "$RESULTS/$pod.txt"; then
      sed -n '/^internal DNS load test/,$p' "$RESULTS/$pod.txt"
      # One exec for both files: the project allows only a few exec sessions.
      if datumctl compute exec --project "$PROJECT" "$pod" -c k6 -- tar -C /tmp -cf - k6-summary.json k6-queries.csv.gz >"$RESULTS/$pod.tar" &&
        tar -C "$RESULTS" -xf "$RESULTS/$pod.tar"; then
        mv "$RESULTS/k6-summary.json" "$RESULTS/$pod.json"
        mv "$RESULTS/k6-queries.csv.gz" "$RESULTS/$pod.csv.gz"
        rm "$RESULTS/$pod.tar"
      else
        echo "$pod: could not copy the query records back; copying the summary alone" >&2
        rm -f "$RESULTS/$pod.tar"
        datumctl compute exec --project "$PROJECT" "$pod" -c k6 -- cat /tmp/k6-summary.json >"$RESULTS/$pod.json" || true
        failed=1
      fi
    else
      echo "$pod: no summary; output follows" >&2
      cat "$RESULTS/$pod.txt" >&2
      failed=1
    fi
  done
  echo "Results in $RESULTS"
  if ls "$RESULTS"/*.csv.gz >/dev/null 2>&1; then
    "$here/analyze.sh" "$RESULTS" || failed=1
  fi
  return "$failed"
}

propagate() {
  local ctx end
  ctx=$(resolver_context)
  [[ -n "$ctx" ]] || { echo "no resolver context for network $NETWORK yet" >&2; exit 1; }
  export ZONE_DOMAIN ZONE_CLASS CONTEXT=$ctx
  end=$(( $(date +%s) + $(to_seconds "$DURATION") ))
  echo "Rewriting stamp.$ZONE_DOMAIN every ${INTERVAL}s; query it with PROPAGATION_NAME=stamp.$ZONE_DOMAIN"
  while (( $(date +%s) < end )); do
    STAMP=$(perl -MTime::HiRes=time -e 'printf "%d", time*1000') render "$here/manifests/propagation.yaml" | dctl apply -f - >/dev/null
    sleep "$INTERVAL"
  done
}

to_seconds() {
  case $1 in
    *h) echo $(( ${1%h} * 3600 )) ;;
    *m) echo $(( ${1%m} * 60 )) ;;
    *s) echo "${1%s}" ;;
    *) echo "$1" ;;
  esac
}

api_base() {
  local host
  host=$(dctl get namespace default -v=6 2>&1 | grep -oE 'https://[^/ "]+' | head -1)
  echo "$host/apis/resourcemanager.miloapis.com/v1alpha1/projects/$PROJECT/control-plane"
}

publish() {
  local pod prefix start probe_pid failed=0
  pod=$(instances idns-perf-k6 1)
  prefix=pub-$(date -u +%H%M%S)
  # The exec session takes a while to start k6, so schedule the writes ahead.
  start=$(( ($(date +%s) + ${LEAD_S:-45}) * 1000 ))
  mkdir -p "$RESULTS"
  if ! dctl get dnszone idns-perf >/dev/null 2>&1; then
    ctx=$(resolver_context)
    [[ -n "$ctx" ]] || { echo "no resolver context for network $NETWORK yet" >&2; exit 1; }
    export ZONE_DOMAIN ZONE_CLASS CONTEXT=$ctx STAMP=0
    render "$here/manifests/propagation.yaml" | dctl apply -f - >/dev/null
  fi
  local -a vars=(PREFIX="$prefix" START_MS="$start" COUNT="${COUNT:-20}" WRITE_RATE="${WRITE_RATE:-2}" ZONE_DOMAIN="$ZONE_DOMAIN")
  datumctl compute exec -i --project "$PROJECT" "$pod" -c k6 -- \
    env "${vars[@]}" RESOLVERS="${RESOLVERS:-[fd53::53]:53}" POLL_MS="${POLL_MS:-200}" \
    sh -c 'cat >/tmp/publish.js && k6 run --quiet /tmp/publish.js' <"$here/publish.js" >"$RESULTS/probe.log" 2>&1 &
  probe_pid=$!
  k6 run --quiet --console-output="$RESULTS/writer.log" \
    -e API="$(api_base)" -e TOKEN="$(datumctl auth get-token --output=token)" -e GAP_S="${GAP_S:-30}" -e TTL="${TTL:-1}" \
    $(printf -- '-e %s ' "${vars[@]}") "$here/publish-writer.js" >"$RESULTS/writer.txt" 2>&1 || failed=1
  wait "$probe_pid" || failed=1
  printf '%s\n' "${vars[@]}" >"$RESULTS/params"
  "$here/publish-analyze.sh" "$RESULTS" || failed=1
  return "$failed"
}

down() {
  # Only objects run.sh labelled, so a wrong PROJECT cannot lose anything else.
  dctl delete workloads,dnsrecordsets,dnszoneassociations,dnszones -l test.datum.net/owner=internal-dns-perf
}

cmd=${1:-}
shift || true
case $cmd in
  up | run | propagate | publish | down) guard; "$cmd" "$@" ;;
  *) sed -n '2,12p' "$0" >&2; exit 2 ;;
esac
