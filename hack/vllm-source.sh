#!/usr/bin/env bash
# Download the two vLLM source files podserve's data files are extracted from, at a release tag.
# Usage: hack/vllm-source.sh <tag> <outdir>        e.g. hack/vllm-source.sh v0.32.0 /tmp/vllm-src
# Then:
#   python3 -I hack/vllm-archs.py   <outdir>/registry.py > catalog/vllm-<tag>-architectures.txt
#   python3 -I hack/vllm-metrics.py <outdir>/loggers.py  > testdata/vllm-<tag>-metrics.txt
# The files are parsed, never executed (see the two .py scripts).
set -euo pipefail
tag=${1:?usage: vllm-source.sh <tag> <outdir>}
out=${2:?usage: vllm-source.sh <tag> <outdir>}
mkdir -p "$out"
base="https://raw.githubusercontent.com/vllm-project/vllm/$tag"
curl -sfL -o "$out/registry.py" "$base/vllm/model_executor/models/registry.py"
curl -sfL -o "$out/loggers.py" "$base/vllm/v1/metrics/loggers.py"
echo "downloaded vLLM $tag sources to $out"
