#!/usr/bin/env bash
# Fail when two panels on a rendered Private DNS dashboard share grid cells.
set -euo pipefail
repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
status=0
for file in "$repo_root"/config/components/dashboards/private-dns-*.json; do
  count="$(jq '.panels | length' "$file")"
  if [ "$count" -eq 0 ]; then
    echo "FAIL: $file has no panels" >&2
    status=1
    continue
  fi
  overlaps="$(jq -r '
    [.panels[] | {t: .title, x: .gridPos.x, y: .gridPos.y, w: .gridPos.w, h: .gridPos.h}] as $p
    | range(0; $p | length) as $i
    | range($i + 1; $p | length) as $j
    | select($p[$i].x < $p[$j].x + $p[$j].w and $p[$j].x < $p[$i].x + $p[$i].w
        and $p[$i].y < $p[$j].y + $p[$j].h and $p[$j].y < $p[$i].y + $p[$i].h)
    | "\($p[$i].t) overlaps \($p[$j].t)"' "$file")"
  if [ -n "$overlaps" ]; then
    echo "FAIL: $file" >&2
    echo "$overlaps" >&2
    status=1
  else
    echo "OK: $(basename "$file") has $count panels and no overlaps"
  fi
done
exit "$status"
