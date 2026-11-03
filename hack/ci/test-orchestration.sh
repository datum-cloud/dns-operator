#!/usr/bin/env bash
set -euo pipefail
python3 -m unittest discover -s hack -p 'test_*ci*.py'
python3 -m unittest discover -s hack/ci -p 'test_warm_foundation.py'
