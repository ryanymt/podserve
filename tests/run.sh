#!/usr/bin/env bash
# Project test suite: runs every tests/**/*.test.sh and fails if any of them fails.
set -euo pipefail
cd "$(dirname "$0")/.."
shopt -s nullglob globstar
files=(tests/**/*.test.sh)
if (( ${#files[@]} == 0 )); then
  echo "tests: no test files yet (tests/**/*.test.sh)"
  exit 0
fi
rc=0
for f in "${files[@]}"; do
  if bash "$f" < /dev/null; then echo "  ok   $f"; else echo "  FAIL $f"; rc=1; fi
done
(( rc == 0 )) && echo "tests: ok (${#files[@]} files)"
exit $rc
