package profile

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/ryanymt/podserve/catalog"
)

// Plan picks a serving profile for one model revision, per docs/design/profile.md. A refusal is
// returned as a *Refusal error; any other error is a bug. It is a pure function: no network, no
// cluster, no clock. Zero-value request fields mean "choose": GPUType "" (or "auto") takes the
// catalogue's deployable types in order, GPUCount 0 tries 1, 2, 4, 8, MaxModelLen 0 picks a
// multiple of ContextStepTokens, GPUMemoryPercent 0 means DefaultGPUMemoryPercent.
func Plan(m Model, req Request, cat Catalog, archs Architectures) (Result, error) {
	pct := req.GPUMemoryPercent
	if pct == 0 {
		pct = DefaultGPUMemoryPercent
	}
	var warnings []string

	// The checks of docs/design/profile.md, in order: the first failure wins.
	if tag := m.PipelineTag; tag != "" && tag != "text-generation" {
		return Result{}, &Refusal{NotTextGeneration, fmt.Sprintf("its Hub pipeline tag is %s; podserve v0.1 serves text-generation models only", tag)}
	}
	if m.Gated && !req.HasToken {
		return Result{}, &Refusal{GatedNoToken, "the model is gated; set HF_TOKEN to a token whose owner has accepted its licence"}
	}
	if m.Config == nil {
		return Result{}, &Refusal{InvalidConfig, "config.json is unreadable or incomplete: config.json could not be read"}
	}
	cfg := m.Config
	if len(cfg.Architectures) == 0 {
		return Result{}, &Refusal{InvalidConfig, "config.json is unreadable or incomplete: no architectures"}
	}
	arch := cfg.Architectures[0]
	if !archs[arch] {
		return Result{}, &Refusal{UnsupportedArchitecture, fmt.Sprintf("architecture %s is not supported by vLLM %s", arch, catalog.VLLMVersion)}
	}
	// The architecture is checked before field completeness, so an unsupported model with
	// non-standard config keys gets the more useful UNSUPPORTED_ARCHITECTURE.
	for _, f := range []struct {
		have int
		name string
	}{
		{cfg.NumHiddenLayers, "num_hidden_layers"},
		{cfg.NumAttentionHeads, "num_attention_heads"},
		{cfg.HiddenSize, "hidden_size"},
		{cfg.MaxPositionEmbeddings, "max_position_embeddings"},
	} {
		if f.have <= 0 {
			return Result{}, &Refusal{InvalidConfig, "config.json is unreadable or incomplete: missing " + f.name}
		}
	}
	if m.PipelineTag == "" {
		warnings = append(warnings, fmt.Sprintf("model has no pipeline_tag; treating it as text-generation because its architecture is %s", arch))
	}

	// Weights: sum the chosen files; a float32 model without a quant method is served in 16-bit.
	sizes := make(map[string]int64, len(m.Files))
	for _, f := range m.Files {
		sizes[f.Name] = f.Size
	}
	names, ref := weightFiles(m, sizes)
	if ref != nil {
		return Result{}, ref
	}
	if len(names) == 0 {
		return Result{}, &Refusal{NoSafetensors, "the repository has no .safetensors weights; podserve does not load pickle (.bin) weights"}
	}
	var weights int64
	for _, n := range names {
		weights += sizes[n]
	}
	if cfg.Dtype == "float32" && cfg.QuantMethod == "" {
		weights /= 2
	}
	dtype := "bfloat16"
	if cfg.Dtype == "bfloat16" || cfg.Dtype == "float16" {
		dtype = cfg.Dtype
	} else if cfg.Dtype == "" {
		warnings = append(warnings, "config.json has no dtype; serving in bfloat16")
	}

	// The requested GPU type, if any, must be one the catalogue knows.
	byType := make(map[string]GPU, len(cat.GPUs))
	for _, g := range cat.GPUs {
		byType[g.Type] = g
	}
	gpuType := req.GPUType
	if gpuType == "" {
		gpuType = "auto"
	}
	if gpuType != "auto" {
		if _, ok := byType[gpuType]; !ok {
			known := make([]string, 0, len(cat.GPUs))
			for _, g := range cat.GPUs {
				known = append(known, g.Type)
			}
			return Result{}, &Refusal{UnknownGPUType, fmt.Sprintf("unknown GPU type %s; known types: %s", gpuType, strings.Join(known, ", "))}
		}
	}
	if req.MaxModelLen > cfg.MaxPositionEmbeddings {
		return Result{}, &Refusal{InvalidConfig, fmt.Sprintf("config.json is unreadable or incomplete: maxModelLen %d exceeds the model's maximum %d", req.MaxModelLen, cfg.MaxPositionEmbeddings)}
	}

	// KV cache per token.
	kv := newKVCache(cfg)
	maxPos := int64(cfg.MaxPositionEmbeddings)
	wantLen := int64(req.MaxModelLen)

	counts := GPUCounts
	if req.GPUCount != 0 {
		counts = []int{req.GPUCount}
	}
	var deployable, rest []GPU
	for _, g := range cat.GPUs {
		if g.Deployable {
			deployable = append(deployable, g)
		} else {
			rest = append(rest, g)
		}
	}

	// The largest option tried, for the DOES_NOT_FIT message: the last type and count that passed
	// the shape and validity checks, recorded before the free-memory check ("None" when no
	// option was even tried, which gets its own message).
	lastCount, lastType := "None", "None"
	lastRoom := int64(0)

	// search tries each type with each count, in order; the first fit wins. It returns nil when
	// nothing in types fits.
	search := func(types []GPU) *Result {
		for _, g := range types {
			shapes := make(map[int]Shape, len(g.Shapes))
			for _, s := range g.Shapes {
				shapes[s.GPUs] = s
			}
			for _, t := range counts {
				shape, ok := shapes[t]
				if !ok || !kv.valid(t) {
					continue
				}
				budget := g.UsableMiB * int64(pct) / 100
				reserve := int64(ReserveMiB)
				if t > 1 {
					reserve += ReserveTPMiB
				}
				lastCount, lastType, lastRoom = strconv.Itoa(t), g.Type, (budget-reserve)*mib*int64(t)
				free := (budget-reserve)*mib - ceilDiv(weights, int64(t)) // bytes per GPU for KV
				if free <= 0 {
					continue
				}
				capacity := free / kv.perGPU(t) // tokens, whole replica
				need := wantLen
				if need == 0 {
					need = minI64(maxPos, MinContextTokens)
				}
				if capacity < need {
					continue
				}
				maxLen := wantLen
				if maxLen == 0 {
					maxLen = minI64(maxPos, capacity/ContextStepTokens*ContextStepTokens)
					if maxLen < need { // a short model (maxPos < one step): need <= capacity, so serve need
						maxLen = need
					}
				}
				seqs := capacity / maxLen
				return &Result{
					GPUType:            g.Type,
					GPUCount:           t,
					Shape:              shape,
					TensorParallelSize: t,
					Dtype:              dtype,
					MaxModelLen:        int(maxLen),
					GPUMemoryPercent:   pct,
					Deployable:         g.Deployable,
					WeightsBytes:       weights,
					KVBytesPerToken:    kv.perToken,
					KVCacheBytes:       free * int64(t),
					TokenCapacity:      capacity,
					SeqsAtMaxLen:       seqs,
					Warnings:           warnings,
				}
			}
		}
		return nil
	}

	var res *Result
	if gpuType != "auto" {
		res = search([]GPU{byType[gpuType]})
	} else {
		res = search(deployable)
		if res == nil {
			// No deployable type fits: try the plan-only types before refusing. A fit there
			// returns its profile with Deployable false; deploy refuses it with NOT_DEPLOYABLE.
			res = search(rest)
		}
	}
	if res == nil && lastType == "None" {
		return Result{}, &Refusal{DoesNotFit, fmt.Sprintf("needs about %s GiB for weights; no catalogued GPU shape suits its %d attention heads",
			gib1(weights), cfg.NumAttentionHeads)}
	}
	if res == nil {
		return Result{}, &Refusal{DoesNotFit, fmt.Sprintf("needs about %s GiB for weights; the largest option tried (%sx %s) has room for %s GiB",
			gib1(weights), lastCount, lastType, gib1(lastRoom))}
	}
	return *res, nil
}
