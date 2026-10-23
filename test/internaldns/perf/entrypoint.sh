#!/bin/sh
# Idle by default so runs can be started with exec. With K6_AUTORUN=1 the
# container runs dns.js once, prints the summary to its log, and exits with k6's
# status so a failed run marks the instance failed. A restart runs it again.
set -u
if [ "${K6_AUTORUN:-0}" = "1" ]; then
  exec k6 run --quiet /perf/dns.js
fi
exec sleep infinity
