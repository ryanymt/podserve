# The settings engine (`pkg/profile`)

Core document (SYSTEM.md). `profile.Plan(model, request, catalog, architectures)` picks the GPU type
and count, tensor-parallel size, dtype and context length for one model revision, or refuses with a
code.
- **Pure:** no network, no cluster, no clock.
- **Who implements it:** T3, in Go. `testdata/spine/profile/expected.py` implements it again,
  independently, to generate the expected values. `tests/profile-expected.test.sh` keeps the two in
  step.
- **Constants:** in `pkg/profile/types.go`. The reference script copies them.
- **Calibration:** all memory numbers were verified against live cluster runs.

Units: bytes are `int64`; `MiB` = 1,048,576 bytes; every division is integer division (round down)
unless stated.

## Inputs

- **`Model`:** repo, revision, `PipelineTag`, `Gated`, `Files` (name + size), `IndexWeightFiles`, and
  `Config`. `Config` is nil if `config.json` was unreadable.
- **`Request`:**
  - `GPUType`: `""`/`"auto"` or a catalogue type.
  - `GPUCount`: 0 or one of 1, 2, 4, 8.
  - `MaxModelLen`: 0 = choose.
  - `GPUMemoryPercent`: 0 = 90.
  - `HasToken`.
- **`Catalog`:** `catalog/gpus.yaml`, in preference order.
- **`Architectures`:** `catalog/vllm-v0.32.0-architectures.txt`. That's vLLM's `_TEXT_GENERATION_MODELS`
  table only, extracted by `hack/vllm-archs.py` (see `platform.md`).

## Checks, in this order (the first failure wins)

| # | Check | Refusal code |
|---|---|---|
| 1 | `PipelineTag` is non-empty and not `text-generation` | `NOT_TEXT_GENERATION` |
| 2 | `Gated` and not `HasToken` | `GATED_NO_TOKEN` |
| 3 | `Config` is nil, or `Config.Architectures` is empty | `INVALID_CONFIG` |
| 4 | `Config.Architectures[0]` is not in the architecture set | `UNSUPPORTED_ARCHITECTURE` |
| 5 | `NumHiddenLayers`, `NumAttentionHeads`, `HiddenSize` or `MaxPositionEmbeddings` is ≤ 0 | `INVALID_CONFIG` (`missing <json name>`: `num_hidden_layers`, `num_attention_heads`, `hidden_size`, `max_position_embeddings`, checked in that order) |
| 6 | No weight files (see below), or an index names a file not in the repo | `NO_SAFETENSORS` / `INVALID_CONFIG` |
| 7 | `Request.GPUType` is set (not `auto`) and not in the catalogue | `UNKNOWN_GPU_TYPE` |
| 8 | `Request.MaxModelLen` > `MaxPositionEmbeddings` | `INVALID_CONFIG` |
| 9 | Nothing fits (the search below) | `DOES_NOT_FIT` |

The architecture is checked before field completeness, so an unsupported model that also has
non-standard config keys (e.g. RWKV) gets the more useful `UNSUPPORTED_ARCHITECTURE`.

A missing `PipelineTag` (empty) isn't a refusal. It adds the warning
`model has no pipeline_tag; treating it as text-generation because its architecture is <ARCH>`.

## Weights

- **Weight files:**
  - With an index (`IndexWeightFiles` non-nil): exactly those names. A listed name missing from
    `Files` is `INVALID_CONFIG`.
  - Without an index: every top-level file (no `/` in the name) ending in `.safetensors`.
  - So a Mistral-style repo with both `consolidated.safetensors` and indexed `model-*.safetensors`
    counts only the indexed files.
- **`WeightsBytes`** = the sum of their sizes. If `Config.Dtype` is `float32` and `QuantMethod` is
  empty, halve it (the model is served in 16-bit).
- **Served dtype** (`Result.Dtype`, passed as `--dtype`):
  - `bfloat16` or `float16`: as is.
  - `float32`: `bfloat16`.
  - empty: `bfloat16`, with the warning `config.json has no dtype; serving in bfloat16`.

## KV cache per token

```
headDim  = Config.HeadDim                 if > 0, else HiddenSize / NumAttentionHeads
kvHeads  = Config.NumKeyValueHeads        if > 0, else NumAttentionHeads

if Config.KVLoraRank > 0:                 # MLA (DeepSeek-style): one latent, not split by TP
    KVBytesPerToken  = NumHiddenLayers * (KVLoraRank + QKRopeHeadDim) * KVBytesPerElement
    perGPUPerToken(t) = KVBytesPerToken
else:
    KVBytesPerToken  = 2 * NumHiddenLayers * kvHeads * headDim * KVBytesPerElement
    perGPUPerToken(t) = KVBytesPerToken / min(t, kvHeads)   # heads split across GPUs; replicated
                                                            # once there are more GPUs than KV heads
```

A GPU count `t` is *valid* for the model when `NumAttentionHeads % t == 0`, and, for non-MLA models,
`kvHeads % t == 0 || t % kvHeads == 0`.

## Search

```
pct    = Request.GPUMemoryPercent, or DefaultGPUMemoryPercent (90)
types  = [Request.GPUType] if set, else the catalogue's deployable types in order
counts = [Request.GPUCount] if set, else GPUCounts (1, 2, 4, 8)

for g in types:
  for t in counts:
    skip unless g has a shape with t GPUs and t is valid for the model
    budgetMiB  = g.UsableMiB * pct / 100
    reserveMiB = ReserveMiB + (ReserveTPMiB if t > 1 else 0)
    weightsGPU = ceil(WeightsBytes / t)
    free       = (budgetMiB - reserveMiB) * MiB - weightsGPU          # bytes per GPU for KV
    skip if free <= 0
    capacity   = free / perGPUPerToken(t)                           # tokens (whole replica)
    need       = Request.MaxModelLen if set, else min(MaxPositionEmbeddings, MinContextTokens)
    skip if capacity < need
    maxLen     = Request.MaxModelLen if set,
                 else max(need, min(MaxPositionEmbeddings, capacity / ContextStepTokens * ContextStepTokens))
                 (the max() only matters for models shorter than one step, e.g. a 512-token model)
    -> Result{GPUType: g.Type, GPUCount: t, Shape: g's t-GPU shape, TensorParallelSize: t,
              MaxModelLen: maxLen, Deployable: g.Deployable,
              WeightsBytes, KVBytesPerToken, KVCacheBytes: free * t, TokenCapacity: capacity,
              SeqsAtMaxLen: capacity / maxLen, GPUMemoryPercent: pct, Dtype, Warnings}
```

The first fit wins: the cheapest GPU type in catalogue order, with the fewest GPUs.

**Not deployable.** If `Request.GPUType` was `auto` and no deployable type fits, run the same search
over the catalogue's non-deployable types, in order. A fit there returns its profile with
`Deployable: false`. `podserve plan` shows it; `podserve deploy` refuses it with `NOT_DEPLOYABLE`
(`cli.md`).

**Nothing fits:** `DOES_NOT_FIT`. If no type and count even passed the shape and head-divisibility
checks, the message is `needs about <W> GiB for weights; no catalogued GPU shape suits its <H>
attention heads`. Otherwise it is
`needs about <W> GiB for weights; the largest option tried (<N>x <TYPE>) has room for <R> GiB`.
- `W` = `WeightsBytes` in GiB, to 1 decimal place.
- `R` = (budget − reserve) × N, in GiB, to 1 decimal place, for the last type and count tried.

## Worked example (checked live in T0)

**Qwen3-8B** (`Qwen3ForCausalLM`): 36 layers, 32 attention heads, 8 KV heads, head dim 128, bf16,
weights 16,382,… bytes in 5 indexed shards.
- KV per token = 2 × 36 × 8 × 128 × 2 = 147,456 B (144 KiB).
- **1× L4:** budget = 23034 × 90 / 100 = 20,730 MiB. Free = (20,730 − 2,560) MiB − weights ≈ 2.67 GB,
  for a capacity of about 18,100 tokens. Max context = min(40,960, 17,408) = **17,408**.
- **Live:** vLLM reported 2.71 GiB and 19,760 tokens, so the formula is about 8% conservative.

**Qwen3-14B:** about 29.5 GB of weights.
- **1× L4:** free < 0.
- **2× L4:** weights 14.8 GB per GPU. Free ≈ 4.3 GB per GPU. Per GPU per token = 163,840 / 2 =
  81,920 B, for a capacity of about 52,000 tokens. Max context = min(40,960, …) = **40,960**.
- Tested live in T11.

## Not modelled in v0.1 (conservative by design)

- **Sliding-window and hybrid attention.** Gemma-style local layers and linear-attention layers
  (Qwen3-Next) are counted as full attention, which underestimates the capacity.
- **Prefix caching, chunked prefill and speculative decoding** don't change the capacity estimate.
- **FP8 KV cache:** `KVBytesPerElement` is fixed at 2.
