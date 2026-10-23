#!/usr/bin/env bash
# Turns the per-query records of one run into per-step and per-second stats.
#
#   analyze.sh [DIR]   DIR holds <generator>.csv.gz files from run.sh run;
#                      defaults to the newest directory under results/
#
# Prints one row per query kind and offered rate, and writes stages.tsv and
# timeline.tsv (one row per kind and second) next to the records. Exits
# non-zero when the records hold no queries.
set -euo pipefail

here=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)
dir=${1:-$(ls -d "$here"/results/*/ 2>/dev/null | sort | tail -1 || true)}
dir=${dir%/}
[[ -d "$dir" ]] || { echo "no results directory; pass one" >&2; exit 2; }
shopt -s nullglob
records=("$dir"/*.csv.gz)
(( ${#records[@]} > 0 )) || { echo "$dir has no query records (*.csv.gz)" >&2; exit 1; }

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT

# k6 CSV puts custom tags in extra_tags as key=value&key=value. A sample's
# timestamp is when the query finished, so the send time is that minus its
# latency. Reasons outside the known set count as Other.
for f in "${records[@]}"; do
  gzip -dc "$f" | awk -F, -v OFS='\t' '
    NR == 1 { for (i = 1; i <= NF; i++) col[$i] = i; next }
    BEGIN { split("ok ServerFailure Timeout Refused NonExistingDomain Answered Empty", k, " "); for (i in k) known[k[i]] = 1 }
    $col["metric_name"] == "idns_query_ms" {
      kind = stage = outcome = ""
      n = split($col["extra_tags"], tags, "&")
      for (i = 1; i <= n; i++) {
        split(tags[i], kv, "=")
        if (kv[1] == "kind") kind = kv[2]
        else if (kv[1] == "stage") stage = kv[2]
        else if (kv[1] == "outcome") outcome = kv[2]
      }
      if (!(outcome in known)) outcome = "Other"
      print kind, stage, $col["timestamp"] - $col["metric_value"], $col["metric_value"], outcome
    }'
done >"$work/queries.tsv"

total=$(wc -l <"$work/queries.tsv" | tr -d ' ')
if [[ "$total" -eq 0 ]]; then
  echo "no idns_query_ms samples in ${#records[@]} record file(s) under $dir" >&2
  exit 1
fi

# Shared by both tables: percentiles over a sorted group, failures by reason.
summarise='
  function flush() {
    if (n == 0) return
    printf "%s", key
    for (i = 1; i <= np; i++) {
      idx = int(p[i] * n + 0.999999); if (idx < 1) idx = 1
      printf "\t%s", v[idx]
    }
    printf "\t%d\t%d\t%.3f", n, n - ok, (n ? 100 * ok / n : 0)
    for (i = 1; i <= nr; i++) printf "\t%d", fails[r[i]] + 0
    if (rate) printf "\t%s", (key ~ /\tramping$/ ? "" : sprintf("%.1f", last > first ? n / ((last - first) / 1000) : n))
    printf "\n"
    n = ok = 0; delete fails
  }
  BEGIN { np = split("0.5 0.9 0.99 0.999 1", p, " "); nr = split("ServerFailure Timeout Refused NonExistingDomain Answered Empty Other", r, " ") }
'

{
  printf 'kind\tstage\tp50_ms\tp90_ms\tp99_ms\tp999_ms\tmax_ms\tqueries\tfailed\texpected_pct\tServerFailure\tTimeout\tRefused\tNonExistingDomain\tAnswered\tEmpty\tOther\tqps_all_generators\n'
  sort -t$'\t' -k1,1 -k2,2 -k4,4n "$work/queries.tsv" | awk -F'\t' -v rate=1 "$summarise"'
    { k = $1 "\t" $2 }
    k != key { flush(); key = k; first = last = $3 }
    { v[++n] = $4; if ($3 < first) first = $3; if ($3 > last) last = $3; if ($5 == "ok") ok++; else fails[$5]++ }
    END { flush() }'
} >"$dir/stages.tsv"

{
  printf 'kind\tsecond\tp50_ms\tp90_ms\tp99_ms\tp999_ms\tmax_ms\tqueries\tfailed\texpected_pct\tServerFailure\tTimeout\tRefused\tNonExistingDomain\tAnswered\tEmpty\tOther\n'
  awk -F'\t' -v OFS='\t' '{ print $1, int($3 / 1000), $4, $5 }' "$work/queries.tsv" |
    sort -t$'\t' -k1,1 -k2,2n -k3,3n | awk -F'\t' -v rate=0 "$summarise"'
    { k = $1 "\t" $2 }
    k != key { flush(); key = k }
    { v[++n] = $3; if ($4 == "ok") ok++; else fails[$4]++ }
    END { flush() }'
} >"$dir/timeline.tsv"

echo "$total queries from ${#records[@]} generator(s) in $dir"
echo "Latency is in whole milliseconds. Failed queries include timeouts at 5000 ms."
column -t -s$'\t' "$dir/stages.tsv" 2>/dev/null || cat "$dir/stages.tsv"
worst=$(awk -F'\t' 'NR > 1 && $9 > 0 { print $9, $1, $2 }' "$dir/timeline.tsv" | sort -rn | head -5)
if [[ -n "$worst" ]]; then
  echo "Seconds with the most failures (failed kind unix-second):"
  echo "$worst"
fi
echo "Per-step table: $dir/stages.tsv; per-second timeline: $dir/timeline.tsv"
