#!/usr/bin/env bash
set -euo pipefail
jq -e '.tests.result == "success" and .image.result == "success" and all(.[]; .result == "success")' <<< "$JOB_RESULTS"
