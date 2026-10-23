#!/usr/bin/env bash
# Drives the internal DNS k6 load test in a Datum Cloud project.
#
#   run.sh up          create target and k6 workloads, wait for them
#   run.sh run [ENV=VALUE ...]
#                      run dns.js on every k6 instance at once
#   run.sh propagate   rewrite a private TXT record on a timer
#   run.sh down        delete everything run.sh created
#
# See README.md for the variables each command reads.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

: "${PROJECT:?set PROJECT to the Datum Cloud project to test in}"
LOCATION=${LOCATION:-us-central-1}
NETWORK=${NETWORK:-default}
TARGETS=${TARGETS:-3}
GENERATORS=${GENERATORS:-1}
IMAGE=${IMAGE:-ghcr.io/datum-cloud/internal-dns-perf:v0.0.0-main}
ZONE_DOMAIN=${ZONE_DOMAIN:-perf.internal}
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
  echo "Private names: $(instances idns-perf-target "$TARGETS" | sed 's/$/.datum.internal/' | paste -sd, -)"
}

run() {
  local names pod i failed=0
  local -a pods=() pids=()
  names=$(instances idns-perf-target "$TARGETS" | sed 's/$/.datum.internal/' | paste -sd, -)
  while IFS= read -r pod; do pods+=("$pod"); done < <(instances idns-perf-k6 "$GENERATORS")
  mkdir -p "$RESULTS"
  for pod in "${pods[@]}"; do
    datumctl compute exec --project "$PROJECT" "$pod" -c k6 -- \
      env PRIVATE_NAMES="$names" DURATION="$DURATION" SUMMARY_PATH=/tmp/k6-summary.json "$@" \
      k6 run --quiet /perf/dns.js >"$RESULTS/$pod.txt" 2>&1 &
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
      datumctl compute exec --project "$PROJECT" "$pod" -c k6 -- cat /tmp/k6-summary.json >"$RESULTS/$pod.json" || failed=1
    else
      echo "$pod: no summary; output follows" >&2
      cat "$RESULTS/$pod.txt" >&2
      failed=1
    fi
  done
  echo "Results in $RESULTS"
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

down() {
  # Only objects run.sh labelled, so a wrong PROJECT cannot lose anything else.
  dctl delete workloads,dnsrecordsets,dnszoneassociations,dnszones -l test.datum.net/owner=internal-dns-perf
}

cmd=${1:-}
shift || true
case $cmd in
  up | run | propagate | down) guard; "$cmd" "$@" ;;
  *) sed -n '2,10p' "$0" >&2; exit 2 ;;
esac
