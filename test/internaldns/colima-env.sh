#!/usr/bin/env bash
# Keep every internal-DNS qualification resource in its suite-owned Colima VM.
# This deliberately does not activate or change the user's default Docker
# context. Start the profile separately with:
#   colima --profile internal-dns-e2e start --activate=false

set -euo pipefail

profile=${INTERNAL_DNS_COLIMA_PROFILE:-internal-dns-e2e}
socket=${INTERNAL_DNS_DOCKER_SOCKET:-"$HOME/.colima/$profile/docker.sock"}
expected="unix://$socket"

if [[ -n ${DOCKER_HOST:-} && $DOCKER_HOST != "$expected" ]]; then
  echo "internal DNS qualification requires DOCKER_HOST=$expected (got $DOCKER_HOST)" >&2
  exit 2
fi
if [[ ! -S $socket ]]; then
  echo "internal DNS Colima socket is unavailable: $socket" >&2
  echo "start it with: colima --profile $profile start --activate=false" >&2
  exit 2
fi

export INTERNAL_DNS_COLIMA_PROFILE=$profile
export DOCKER_HOST=$expected
