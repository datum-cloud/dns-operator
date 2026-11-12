#!/usr/bin/env bash
# Fail unless every share heatmap on the rendered Private DNS dashboards runs
# from blue (small share) to red (large share). Grafana's Spectral scheme is
# already blue-low; setting reverse flips it so slivers of traffic show red.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
status=0
checked=0
for file in "$repo_root"/config/components/dashboards/private-dns-*.json; do
  shares="$(jq '[.. | objects | select(.type? == "heatmap" and .options.cellValues.unit == "percentunit")] | length' "$file")"
  bad="$(jq -r '
    .. | objects
    | select(.type? == "heatmap" and .options.cellValues.unit == "percentunit")
    | .options.color as $c
    | select($c.mode != "scheme" or $c.scheme != "Spectral" or ($c.reverse // false) or ($c.min != 0) or ($c.steps != 128) or ((($c.max // 0) - 0.05) | fabs) > 1e-9)
    | "\(.title): mode=\($c.mode) scheme=\($c.scheme) reverse=\($c.reverse // false) steps=\($c.steps) min=\($c.min) max=\($c.max)"' "$file")"
  if [ -n "$bad" ]; then
    echo "FAIL: $(basename "$file") share heatmaps must use Spectral, unreversed, 0 to 5%, 128 steps" >&2
    echo "$bad" >&2
    status=1
  fi
  checked=$((checked + shares))
done
if [ "$checked" -eq 0 ]; then
  echo "FAIL: no share heatmaps found in the rendered dashboards" >&2
  exit 1
fi
[ "$status" -eq 0 ] && echo "OK: $checked share heatmaps use Spectral from blue to red"
exit "$status"
