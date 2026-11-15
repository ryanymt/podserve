#!/usr/bin/env bash
# `terraform test` for the platform, offline: providers come from the local mirror (P01 T1), and the
# tests use mock providers, so nothing touches GCP. Skipped until the tests are in place (T8).
set -euo pipefail
cd "$(dirname "$0")/../deploy/terraform"
shopt -s nullglob
tests=(tests/*.tftest.hcl)
if (( ${#tests[@]} == 0 )); then
  echo "terraform: no tests in place yet (deploy/terraform/tests/*.tftest.hcl)"
  exit 0
fi
mirror="$HOME/.cache/podserve/tf-mirror"
terraform init -backend=false -plugin-dir="$mirror" -input=false -no-color >/dev/null
terraform test -no-color
