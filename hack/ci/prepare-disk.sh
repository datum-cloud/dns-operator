#!/usr/bin/env bash
set -euo pipefail

# Allow roughly 15 GiB for the observed foundation plus build caches and headroom.
minimum_kib=$((30 * 1024 * 1024))
docker_root=$(docker info --format '{{.DockerRootDir}}')
[[ "$docker_root" == /* ]] || { echo 'Docker storage path is unavailable' >&2; exit 1; }

check_space() {
  local path available report
  local sufficient=true
  for path in "$PWD" "$docker_root"; do
    report=$(df -Pk "$path")
    available=$(awk 'NR == 2 {print $4}' <<< "$report")
    [[ "$available" =~ ^[0-9]+$ ]] || { echo "Cannot read available space for $path" >&2; return 2; }
    printf 'Disk space: path=%s available_kib=%s required_kib=%s\n' "$path" "$available" "$minimum_kib"
    if (( available < minimum_kib )); then sufficient=false; fi
  done
  [[ "$sufficient" == true ]]
}

if check_space; then
  echo 'Skipping disk cleanup: checkout and Docker storage have at least 30 GiB available.'
  exit 0
else
  status=$?
  (( status == 1 )) || exit "$status"
fi

echo 'Reclaiming disk space for the E2E workload.'
sudo rm -rf /usr/share/dotnet /opt/ghc /usr/local/lib/android \
  /usr/local/share/boost /opt/hostedtoolcache/CodeQL /usr/local/share/powershell || true
sudo docker image prune --all --force || true
if ! check_space; then
  echo 'Insufficient or unreadable disk space after cleanup; refusing to start E2E.' >&2
  exit 1
fi
