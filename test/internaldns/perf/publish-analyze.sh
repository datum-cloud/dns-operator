#!/usr/bin/env bash
# Joins one publish run's writer and prober logs into per-change timings.
#
#   publish-analyze.sh DIR   DIR holds writer.log and probe.log from run.sh publish
#
# Writes publish.tsv (one row per change and resolver) and summary.tsv (per step
# and resolver), and prints the summary. Exits non-zero when either log holds no
# events, or when a change was never answered or a write failed.
set -euo pipefail

dir=${1:?pass a results directory}
cd "$dir"
grep -ho 'WRITE {.*}' writer.log | sed 's/^WRITE //; s/\\"/"/g; s/}".*/}/' >writes.jsonl || true
grep -ho 'PUBLISH {.*}' probe.log | sed 's/^PUBLISH //; s/\\"/"/g; s/}".*/}/' >seen.jsonl || true
[[ -s writes.jsonl ]] || { echo "no writes in $dir/writer.log" >&2; exit 1; }
[[ -s seen.jsonl ]] || { echo "no prober events in $dir/probe.log" >&2; exit 1; }

# seen is 0 when the prober gave up on that change; status is 0 when the writer
# never sent it.
jq -rn --slurpfile w writes.jsonl --slurpfile s seen.jsonl '
  ($w | map({key: "\(.index)/\(.step)", value: .}) | from_entries) as $by
  | ["step", "resolver", "index", "status", "api_ms", "sent", "seen", "lag_ms"],
    ($s[] | . as $e | $by["\($e.index)/\($e.step)"] as $x
      | if $x == null then [$e.step, $e.resolver, $e.index, 0, 0, 0, $e.seen, -1]
        else [$e.step, $e.resolver, $e.index, $x.status, $x.done - $x.sent, $x.sent, $e.seen,
          (if $e.seen > 0 then $e.seen - $x.sent else -1 end)] end)
  | @tsv' >publish.tsv

{
  printf 'step\tresolver\tchanges\tmissed\tp50_ms\tp90_ms\tp99_ms\tmax_ms\tapi_p50_ms\tapi_p99_ms\twrite_errors\n'
  tail -n +2 publish.tsv | sort -t$'\t' -k1,1 -k2,2 -k8,8n | awk -F'\t' -v OFS='\t' '
    function pct(a, n, p,   i) { i = int(p * n + 0.999999); if (i < 1) i = 1; return n ? a[i] : "-" }
    function flush(   n2, i, t, j) {
      if (key == "") return
      n2 = 0; for (i = 1; i <= c; i++) { api[i] = apis[i] }
      for (i = 2; i <= c; i++) { t = api[i]; for (j = i - 1; j >= 1 && api[j] > t; j--) api[j + 1] = api[j]; api[j + 1] = t }
      print key, c, miss + 0, pct(lag, n, 0.5), pct(lag, n, 0.9), pct(lag, n, 0.99), pct(lag, n, 1), pct(api, c, 0.5), pct(api, c, 0.99), errs + 0
      c = n = miss = errs = 0; delete lag; delete apis; delete api
    }
    { k = $1 "\t" $2 }
    k != key { flush(); key = k }
    { c++; apis[c] = $5; if ($4 < 200 || $4 > 299) errs++; if ($8 < 0) miss++; else lag[++n] = $8 }
    END { flush() }'
} >summary.tsv

echo "Lag is from the write request to the first matching answer, in ms."
column -t -s$'\t' summary.tsv
awk -F'\t' 'NR > 1 && ($4 > 0 || $11 > 0) { bad = 1 } END { exit bad }' summary.tsv
