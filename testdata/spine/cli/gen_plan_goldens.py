"""Generate the `podserve plan` goldens in testdata/cli/ from docs/design/cli.md's output rules
(spine tool, P01 T2). Independent of the Go code: it reads the recorded fixtures and the expected
profiles (testdata/profile/expected.json, itself generated from docs/design/profile.md).

Usage: python3 -I testdata/spine/cli/gen_plan_goldens.py   (from the repo root)
Run it again whenever expected.json changes; review the diff; commit.
"""
import json
import math
import os
import subprocess
import sys

ROOT = os.getcwd()
sys.path.insert(0, os.path.join(ROOT, "testdata", "spine", "profile"))
import expected as ref  # noqa: E402  (the reference formula, for running plan cases)

import yaml  # noqa: E402

GIB = 1 << 30
VLLM = "v0.32.0"


def gib1(n):
    return f"{math.floor(n / GIB * 10 + 0.5) / 10:.1f}"


def fixture(repo):
    d = os.path.join(ROOT, "testdata", "hf", repo.replace("/", "__"))
    info = json.load(open(os.path.join(d, "info.json")))
    cfg = os.path.join(d, "config.json")
    raw = json.load(open(cfg)) if os.path.exists(cfg) else None
    return info, raw


def plan(repo, request):
    catalog = yaml.safe_load(open(os.path.join(ROOT, "catalog", "gpus.yaml")))["gpus"]
    archs = {l.strip() for l in open(os.path.join(ROOT, "catalog", f"vllm-{VLLM}-architectures.txt")) if l.strip()}
    model = ref.load_model(os.path.join(ROOT, "testdata", "hf"), repo, bool(request.get("hasToken")))
    try:
        return ref.plan(model, request, catalog, archs), None
    except ref.Refusal as r:
        return None, r


def text(repo, request):
    info, raw = fixture(repo)
    result, refusal = plan(repo, request)
    if refusal:
        return 3, "", f"podserve: cannot serve {repo}: {refusal.message} ({refusal.code})\n"
    cfg = ref.parse_config(raw)
    tag = info.get("pipeline_tag")
    lic = (info.get("cardData") or {}).get("license") or "unknown licence"
    head = f"{tag}, {lic}" if tag else f"no pipeline tag, {lic}"
    r = result
    dep = "yes" if r["deployable"] else "no (podserve v0.1 deploys nvidia-l4 only)"
    lines = [
        f"Model:        {repo} @ {info['sha'][:12]} ({head})",
        f"Architecture: {cfg['architectures'][0]} (supported by vLLM {VLLM})",
        f"Weights:      {gib1(r['weightsBytes'])} GiB, served as {r['dtype']}",
        f"GPU:          {r['gpuCount']}x {r['gpuType']} on {r['shape']['machineType']} ({r['shape']['cpu']} CPU, {r['shape']['memory']})",
        f"Parallelism:  tensor-parallel {r['tensorParallelSize']}",
        f"Context:      {r['maxModelLen']} tokens (model maximum {cfg['maxPositionEmbeddings']})",
        f"KV cache:     {gib1(r['kvCacheBytes'])} GiB, {r['tokenCapacity']} tokens, {r['seqsAtMaxLen']} sequence(s) at full context",
        f"Deployable:   {dep}",
    ] + [f"warning: {w}" for w in r["warnings"]]
    return 0, "\n".join(lines) + "\n", ""


def as_json(repo, request):
    info, _ = fixture(repo)
    result, refusal = plan(repo, request)
    model = {"repo": repo, "revision": info["sha"], "pipelineTag": info.get("pipeline_tag") or "",
             "license": (info.get("cardData") or {}).get("license") or "",
             "gated": info.get("gated") in ("auto", "manual")}
    if refusal:
        return 3, json.dumps({"model": model, "refusal": {"code": refusal.code, "message": refusal.message}}, indent=2) + "\n", ""
    return 0, json.dumps({"model": model, "profile": result}, indent=2) + "\n", ""


def write(name, args, code, out, err):
    path = os.path.join(ROOT, "testdata", "cli", name + ".golden")
    with open(path, "w") as f:
        f.write(f"args: {json.dumps(args)}\nexit: {code}\n--- stdout\n{out}--- stderr\n{err}")


CASES = [
    ("plan-qwen3-8b", ["plan", "hf://Qwen/Qwen3-8B"], "Qwen/Qwen3-8B", {}, text),
    ("plan-qwen3-8b-json", ["plan", "hf://Qwen/Qwen3-8B", "-o", "json"], "Qwen/Qwen3-8B", {}, as_json),
    ("plan-qwen3-8b-two-gpus", ["plan", "hf://Qwen/Qwen3-8B", "--gpu", "nvidia-l4", "--gpus", "2"], "Qwen/Qwen3-8B",
     {"gpuType": "nvidia-l4", "gpuCount": 2}, text),
    ("plan-ministral-no-tag", ["plan", "hf://mistralai/Ministral-8B-Instruct-2410"], "mistralai/Ministral-8B-Instruct-2410", {}, text),
    ("plan-gpt2-legacy", ["plan", "hf://openai-community/gpt2"], "openai-community/gpt2", {}, text),
    ("plan-qwen3-235b-not-deployable", ["plan", "hf://Qwen/Qwen3-235B-A22B"], "Qwen/Qwen3-235B-A22B", {}, text),
    ("plan-vision-refused", ["plan", "hf://Qwen/Qwen2.5-VL-7B-Instruct"], "Qwen/Qwen2.5-VL-7B-Instruct", {}, text),
    ("plan-gated-no-token", ["plan", "hf://meta-llama/Llama-3.1-8B-Instruct"], "meta-llama/Llama-3.1-8B-Instruct", {}, text),
    ("plan-deepseek-v3-does-not-fit", ["plan", "hf://deepseek-ai/DeepSeek-V3"], "deepseek-ai/DeepSeek-V3", {}, text),
    ("plan-embedding-refused-json", ["plan", "hf://Qwen/Qwen3-Embedding-0.6B", "-o", "json"], "Qwen/Qwen3-Embedding-0.6B", {}, as_json),
]

if __name__ == "__main__":
    os.makedirs(os.path.join(ROOT, "testdata", "cli"), exist_ok=True)
    for name, args, repo, request, fn in CASES:
        write(name, args, *fn(repo, request))
    print(f"wrote {len(CASES)} plan goldens")
