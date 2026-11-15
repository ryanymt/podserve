"""Reference implementation of docs/design/profile.md, written independently of pkg/profile (Go).

Usage: python3 -I testdata/spine/profile/expected.py --fixtures testdata/hf \
           --catalog catalog/gpus.yaml --out testdata/profile/expected.json

Reads testdata/profile/cases.json (next to --out), loads each case's fixture the way pkg/hf would (the
alias table in pkg/hf/types.go), runs the formula and writes the expected Result or Refusal per case.
tests/profile-expected.test.sh regenerates this file and fails on any difference, so the formula,
this script and pkg/profile's test can't drift apart. Spine file (P01 T2); needs PyYAML (test only).
"""
import argparse
import json
import math
import os

import yaml

# Constants: copies of pkg/profile/types.go.
DEFAULT_GPU_MEMORY_PERCENT = 90
RESERVE_MIB = 2560
RESERVE_TP_MIB = 0
MIN_CONTEXT_TOKENS = 4096
CONTEXT_STEP_TOKENS = 1024
KV_BYTES_PER_ELEMENT = 2
GPU_COUNTS = [1, 2, 4, 8]
MIB = 1024 * 1024
GIB = 1024 * MIB
VLLM_VERSION = "v0.32.0"

ALIASES = {
    "hiddenSize": ["hidden_size", "n_embd", "d_model"],
    "numHiddenLayers": ["num_hidden_layers", "n_layer", "num_layers"],
    "numAttentionHeads": ["num_attention_heads", "n_head"],
    "numKeyValueHeads": ["num_key_value_heads"],
    "headDim": ["head_dim"],
    "maxPositionEmbeddings": ["max_position_embeddings", "n_positions", "max_sequence_length", "seq_length"],
    "dtype": ["dtype", "torch_dtype"],
    "kvLoraRank": ["kv_lora_rank"],
    "qkRopeHeadDim": ["qk_rope_head_dim"],
}


class Refusal(Exception):
    def __init__(self, code, message):
        super().__init__(code)
        self.code, self.message = code, message


def lookup(top, keys):
    for scope in (top, top.get("text_config") or {}):
        for k in keys:
            if scope.get(k) is not None:
                return scope[k]
    return None


def parse_config(raw):
    cfg = {}
    for field, keys in ALIASES.items():
        v = lookup(raw, keys)
        cfg[field] = (v or "") if field == "dtype" else int(v or 0)
    cfg["architectures"] = list(raw.get("architectures") or [])
    q = lookup(raw, ["quantization_config"]) or {}
    cfg["quantMethod"] = q.get("quant_method") or ""
    return cfg


def load_model(fixtures, repo, has_token):
    d = os.path.join(fixtures, repo.replace("/", "__"))
    info = json.load(open(os.path.join(d, "info.json")))
    gated = info.get("gated") in ("auto", "manual")
    model = {
        "pipelineTag": info.get("pipeline_tag") or "",
        "gated": gated,
        "files": {s["rfilename"]: s.get("size") or 0 for s in info["siblings"]},
        "index": None,
        "config": None,
    }
    if os.path.exists(os.path.join(d, "index.json")):
        wm = json.load(open(os.path.join(d, "index.json")))["weight_map"]
        model["index"] = sorted(set(wm.values()))
    cpath = os.path.join(d, "config.json")
    if os.path.exists(cpath) and (not gated or has_token):
        model["config"] = parse_config(json.load(open(cpath)))
    return model


def gib1(n):
    """GiB to 1 decimal place, as the messages print it (round half away from zero on the 2nd digit)."""
    return f"{math.floor(n / GIB * 10 + 0.5) / 10:.1f}"


def plan(model, req, catalog, archs):
    pct = req.get("gpuMemoryPercent") or DEFAULT_GPU_MEMORY_PERCENT
    warnings = []
    tag = model["pipelineTag"]
    if tag and tag != "text-generation":
        raise Refusal("NOT_TEXT_GENERATION", f"its Hub pipeline tag is {tag}; podserve v0.1 serves text-generation models only")
    if model["gated"] and not req.get("hasToken"):
        raise Refusal("GATED_NO_TOKEN", "the model is gated; set HF_TOKEN to a token whose owner has accepted its licence")
    cfg = model["config"]
    if cfg is None:
        raise Refusal("INVALID_CONFIG", "config.json is unreadable or incomplete: config.json could not be read")
    if not cfg["architectures"]:
        raise Refusal("INVALID_CONFIG", "config.json is unreadable or incomplete: no architectures")
    arch = cfg["architectures"][0]
    if arch not in archs:
        raise Refusal("UNSUPPORTED_ARCHITECTURE", f"architecture {arch} is not supported by vLLM {VLLM_VERSION}")
    for field, name in (("numHiddenLayers", "num_hidden_layers"), ("numAttentionHeads", "num_attention_heads"),
                        ("hiddenSize", "hidden_size"), ("maxPositionEmbeddings", "max_position_embeddings")):
        if cfg[field] <= 0:
            raise Refusal("INVALID_CONFIG", f"config.json is unreadable or incomplete: missing {name}")
    if not tag:
        warnings.append(f"model has no pipeline_tag; treating it as text-generation because its architecture is {arch}")

    # Weights.
    if model["index"] is not None:
        names = model["index"]
        for n in names:
            if n not in model["files"]:
                raise Refusal("INVALID_CONFIG", f"config.json is unreadable or incomplete: index lists {n}, which is not in the repository")
    else:
        names = [n for n in model["files"] if "/" not in n and n.endswith(".safetensors")]
    if not names:
        raise Refusal("NO_SAFETENSORS", "the repository has no .safetensors weights; podserve does not load pickle (.bin) weights")
    weights = sum(model["files"][n] for n in names)
    if cfg["dtype"] == "float32" and not cfg["quantMethod"]:
        weights //= 2
    if cfg["dtype"] in ("bfloat16", "float16"):
        dtype = cfg["dtype"]
    else:
        dtype = "bfloat16"
        if not cfg["dtype"]:
            warnings.append("config.json has no dtype; serving in bfloat16")

    gpu_type = req.get("gpuType") or "auto"
    by_type = {g["type"]: g for g in catalog}
    if gpu_type != "auto" and gpu_type not in by_type:
        raise Refusal("UNKNOWN_GPU_TYPE", f"unknown GPU type {gpu_type}; known types: " + ", ".join(g["type"] for g in catalog))
    max_pos = cfg["maxPositionEmbeddings"]
    want_len = req.get("maxModelLen") or 0
    if want_len > max_pos:
        raise Refusal("INVALID_CONFIG", f"config.json is unreadable or incomplete: maxModelLen {want_len} exceeds the model's maximum {max_pos}")

    # KV cache.
    heads = cfg["numAttentionHeads"]
    head_dim = cfg["headDim"] or cfg["hiddenSize"] // heads
    kv_heads = cfg["numKeyValueHeads"] or heads
    mla = cfg["kvLoraRank"] > 0
    if mla:
        kv_per_token = cfg["numHiddenLayers"] * (cfg["kvLoraRank"] + cfg["qkRopeHeadDim"]) * KV_BYTES_PER_ELEMENT
    else:
        kv_per_token = 2 * cfg["numHiddenLayers"] * kv_heads * head_dim * KV_BYTES_PER_ELEMENT

    def per_gpu_per_token(t):
        return kv_per_token if mla else kv_per_token // min(t, kv_heads)

    def valid(t):
        if heads % t:
            return False
        return mla or kv_heads % t == 0 or t % kv_heads == 0

    counts = [req["gpuCount"]] if req.get("gpuCount") else GPU_COUNTS
    last = {}

    def search(types):
        for g in types:
            shapes = {s["gpus"]: s for s in g["shapes"]}
            for t in counts:
                if t not in shapes or not valid(t):
                    continue
                budget = g["usableMiB"] * pct // 100
                reserve = RESERVE_MIB + (RESERVE_TP_MIB if t > 1 else 0)
                last.update(type=g["type"], count=t, room=(budget - reserve) * MIB * t)
                free = (budget - reserve) * MIB - (-(-weights // t))
                if free <= 0:
                    continue
                capacity = free // per_gpu_per_token(t)
                need = want_len or min(max_pos, MIN_CONTEXT_TOKENS)
                if capacity < need:
                    continue
                max_len = want_len or max(need, min(max_pos, capacity // CONTEXT_STEP_TOKENS * CONTEXT_STEP_TOKENS))
                s = shapes[t]
                return {
                    "gpuType": g["type"], "gpuCount": t,
                    "shape": {"gpus": s["gpus"], "machineType": s["machineType"], "cpu": str(s["cpu"]), "memory": str(s["memory"])},
                    "tensorParallelSize": t, "dtype": dtype, "maxModelLen": max_len, "gpuMemoryPercent": pct,
                    "deployable": bool(g["deployable"]),
                    "weightsBytes": weights, "kvBytesPerToken": kv_per_token, "kvCacheBytes": free * t,
                    "tokenCapacity": capacity, "seqsAtMaxLen": capacity // max_len,
                    "warnings": warnings,
                }
        return None

    if gpu_type != "auto":
        result = search([by_type[gpu_type]])
    else:
        result = search([g for g in catalog if g["deployable"]])
        if result is None:
            result = search([g for g in catalog if not g["deployable"]])
    if result is None and not last:
        raise Refusal("DOES_NOT_FIT", f"needs about {gib1(weights)} GiB for weights; no catalogued GPU shape suits its {heads} attention heads")
    if result is None:
        raise Refusal("DOES_NOT_FIT", f"needs about {gib1(weights)} GiB for weights; the largest option tried "
                                      f"({last.get('count')}x {last.get('type')}) has room for {gib1(last.get('room', 0))} GiB")
    return result


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--fixtures", required=True)
    ap.add_argument("--catalog", required=True)
    ap.add_argument("--out", required=True)
    a = ap.parse_args()
    root = os.path.dirname(os.path.dirname(os.path.abspath(a.fixtures)))
    catalog = yaml.safe_load(open(a.catalog))["gpus"]
    archs = {l.strip() for l in open(os.path.join(root, "catalog", f"vllm-{VLLM_VERSION}-architectures.txt")) if l.strip() and not l.startswith("#")}
    cases = json.load(open(os.path.join(root, "testdata", "profile", "cases.json")))["cases"]
    out = []
    for c in cases:
        model = load_model(a.fixtures, c["repo"], bool(c["request"].get("hasToken")))
        try:
            out.append({"name": c["name"], "result": plan(model, c["request"], catalog, archs)})
        except Refusal as r:
            out.append({"name": c["name"], "refusal": {"code": r.code, "message": r.message}})
    with open(a.out, "w") as f:
        json.dump({"cases": out}, f, indent=1, sort_keys=True)
        f.write("\n")


if __name__ == "__main__":
    main()
