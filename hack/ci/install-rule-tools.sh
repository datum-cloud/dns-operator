#!/usr/bin/env bash
set -euo pipefail
python -m pip install PyYAML==6.0.3
curl -fsSL https://github.com/prometheus/prometheus/releases/download/v3.15.0/prometheus-3.15.0.linux-amd64.tar.gz -o "$RUNNER_TEMP/prometheus.tgz"
tar -xzf "$RUNNER_TEMP/prometheus.tgz" -C "$RUNNER_TEMP" prometheus-3.15.0.linux-amd64/promtool
