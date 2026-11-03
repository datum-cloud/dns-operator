#!/usr/bin/env bash
set -euo pipefail
jq -e 'length > 0 and all(.[]; .result == "success")' <<< "$JOB_RESULTS"
