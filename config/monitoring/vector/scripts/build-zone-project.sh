#!/bin/sh
# Build zone_project.csv from datum-downstream-dnszone-accounting ConfigMaps.
#
# Input file example:
#   namespace_<ns>.configmap_<zone>.owner  (content: /<project>/<...>)
#
# Output CSV:
#   zone,project
#   <zone>,<project>

set -eu

SRC_DIR="${ENRICHMENT_CONFIGMAP_DIR:-/tmp}"
OUT_FILE="${ENRICHMENT_OUTPUT_FILE:-/enrichment/zone_project.csv}"

mkdir -p "$(dirname "$OUT_FILE")"

tmp="$(mktemp)"
trap 'rm -f "$tmp"' EXIT

# Always produce a valid CSV (header at minimum).
printf 'zone,project\n' > "$tmp"

# Nothing to do if input directory doesn't exist.
[ -d "$SRC_DIR" ] || { mv "$tmp" "$OUT_FILE"; exit 0; }

# Collect rows, then sort+dedupe for stable output.
rows="$(mktemp)"
trap 'rm -f "$tmp" "$rows"' EXIT
: > "$rows"

for f in "$SRC_DIR"/*.owner; do
  [ -f "$f" ] || continue

  base="$(basename "$f")"
  # Expect: namespace_<ns>.configmap_<zone>.owner  -> extract <zone>
  zone="${base#*configmap_}"
  zone="${zone%.owner}"

  # Skip if we can't infer zone.
  [ -n "$zone" ] && [ "$zone" != "$base" ] || continue

  owner="$(tr -d '\r\n' < "$f")"
  owner="${owner#/}"                # trim leading /
  project="${owner%%/*}"            # first segment

  [ -n "$project" ] || continue

  printf '%s,%s\n' "$zone" "$project" >> "$rows"
done

# Append sorted unique rows.
if [ -s "$rows" ]; then
  sort "$rows" | uniq >> "$tmp"
fi

mv "$tmp" "$OUT_FILE"