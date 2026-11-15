#!/usr/bin/env bash
# The settings engine's expected values are generated, not hand-written: testdata/spine/profile/
# expected.py implements docs/design/profile.md independently of the Go code and writes
# testdata/profile/expected.json, which profile_test.go reads. This check regenerates the JSON and
# fails if the committed copy differs, so the formula, the reference script and the test can't drift.
set -euo pipefail
cd "$(dirname "$0")/.."
script=testdata/spine/profile/expected.py
want=testdata/profile/expected.json
if [[ ! -f $script ]]; then
  echo "profile-expected: no reference script yet ($script)"
  exit 0
fi
got=$(mktemp)
trap 'rm -f "$got"' EXIT
python3 -I "$script" --fixtures testdata/hf --catalog catalog/gpus.yaml --out "$got"
diff -u "$want" "$got"
