#!/usr/bin/env bash
# Sends one run's results to a Prometheus import endpoint, such as
# VictoriaMetrics' /api/v1/import/prometheus, so dashboards can show them.
#
#   IMPORT_URL=... report.sh DIR [SCENARIO]
#
# DIR is a run.sh run or publish results directory. Every series carries
# run=<DIR's name>, scenario, and commit (COMMIT, default "unknown").
# idns_perf_run is 1 every 10 seconds while the run lasted, for dashboard
# annotations. Exits non-zero when DIR holds no results it recognises.
set -euo pipefail

: "${IMPORT_URL:?set IMPORT_URL to a Prometheus text import endpoint}"
dir=${1:?pass a results directory}
dir=${dir%/}
run=$(basename "$dir")
commit=${COMMIT:-unknown}
out=$(mktemp)
trap 'rm -f "$out"' EXIT

if [[ -f "$dir/publish.tsv" ]]; then
  scenario=${2:-publish}
  labels="run=\"$run\",scenario=\"$scenario\",commit=\"$commit\""
  awk -F'\t' -v l="$labels" 'NR > 1 {
      printf "idns_perf_publish_api_ms{%s,step=\"%s\"} %s %s\n", l, $1, $5, $6
      if ($8 >= 0) printf "idns_perf_publish_lag_ms{%s,step=\"%s\",resolver=\"%s\"} %s %s\n", l, $1, $2, $8, $7
      else printf "idns_perf_publish_missed{%s,step=\"%s\",resolver=\"%s\"} 1 %s\n", l, $1, $2, $6
    }' "$dir/publish.tsv" >>"$out"
  range=$(awk -F'\t' 'NR > 1 { if (!lo || $6 < lo) lo = $6; t = ($7 > $6 ? $7 : $6); if (t > hi) hi = t } END { if (lo) print lo, hi }' "$dir/publish.tsv")
elif [[ -f "$dir/timeline.tsv" ]]; then
  scenario=${2:-query}
  labels="run=\"$run\",scenario=\"$scenario\",commit=\"$commit\""
  awk -F'\t' -v l="$labels" 'NR > 1 {
      ts = $2 * 1000
      printf "idns_perf_query_p50_ms{%s,kind=\"%s\"} %s %s\n", l, $1, $3, ts
      printf "idns_perf_query_p99_ms{%s,kind=\"%s\"} %s %s\n", l, $1, $5, ts
      printf "idns_perf_query_max_ms{%s,kind=\"%s\"} %s %s\n", l, $1, $7, ts
      printf "idns_perf_query_qps{%s,kind=\"%s\"} %s %s\n", l, $1, $8, ts
      printf "idns_perf_query_failed{%s,kind=\"%s\"} %s %s\n", l, $1, $9, ts
    }' "$dir/timeline.tsv" >>"$out"
  range=$(awk -F'\t' 'NR > 1 { if (!lo || $2 < lo) lo = $2; if ($2 > hi) hi = $2 } END { if (lo) print lo * 1000, hi * 1000 }' "$dir/timeline.tsv")
else
  echo "$dir has neither publish.tsv nor timeline.tsv" >&2
  exit 1
fi
[[ -n "$range" ]] || { echo "$dir holds no samples" >&2; exit 1; }

read -r lo hi <<<"$range"
for ((t = lo; t <= hi + 10000; t += 10000)); do
  echo "idns_perf_run{$labels} 1 $t"
done >>"$out"

curl -fsS --data-binary "@$out" "$IMPORT_URL"
echo "sent $(wc -l <"$out" | tr -d ' ') samples for $run ($scenario, $commit)"
