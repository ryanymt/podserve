#!/usr/bin/env bash
# Go unit tests. testdata/ (where parked spine tests live) is skipped by `go test ./...` by design.
set -euo pipefail
cd "$(dirname "$0")/.."
go test ./... 2>&1 | grep -v '^?' || exit "${PIPESTATUS[0]}"
