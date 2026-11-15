#!/usr/bin/env bash
# Python unit tests for the fetch job. Skipped while none are in place: Python 3.13's
# `unittest discover` exits 5 when it finds no tests, which would turn main red before T7 merges.
set -euo pipefail
cd "$(dirname "$0")/.."
shopt -s nullglob
tests=(images/fetch/test_*.py)
if (( ${#tests[@]} == 0 )); then
  echo "fetch: no tests in place yet (images/fetch/test_*.py)"
  exit 0
fi
rc=0
python3 -m unittest discover -s images/fetch -p 'test_*.py' || rc=$?
find images/fetch -name __pycache__ -type d -prune -exec rm -rf {} +
exit $rc
