package profile

import (
	"fmt"
	"math"
	"strings"
)

// Byte arithmetic and the weight-file and KV-cache helpers of docs/design/profile.md. Bytes are
// int64; MiB = 1,048,576 bytes; every division is integer division (rounds down) unless stated.

const (
	mib = int64(1) << 20 // bytes in a MiB
	gib = int64(1) << 30 // bytes in a GiB
)

// gib1 renders bytes as GiB to one decimal place, the way the refusal messages print it: round
// half away from zero on the second digit (docs/design/profile.md, DOES_NOT_FIT).
func gib1(b int64) string {
	return fmt.Sprintf("%.1f", math.Floor(float64(b)/float64(gib)*10+0.5)/10)
}

// ceilDiv divides a by b, rounding up. a and b are positive.
func ceilDiv(a, b int64) int64 {
	return (a + b - 1) / b
}

// minI64 is the smaller of two int64s.
func minI64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

// weightFiles picks the weight files of one revision: with an index, exactly the indexed names (a
// name missing from the revision is INVALID_CONFIG); without one, every top-level .safetensors
// file. So a repo with both a consolidated.safetensors and an indexed model-*.safetensors set
// counts only the indexed files. The names may be empty; a non-nil Refusal is check 6 of
// docs/design/profile.md.
func weightFiles(m Model, sizes map[string]int64) ([]string, *Refusal) {
	if m.IndexWeightFiles != nil {
		for _, n := range m.IndexWeightFiles {
			if _, ok := sizes[n]; !ok {
				return nil, &Refusal{InvalidConfig, fmt.Sprintf("config.json is unreadable or incomplete: index lists %s, which is not in the repository", n)}
			}
		}
		return m.IndexWeightFiles, nil
	}
	var names []string
	for _, f := range m.Files {
		if !strings.Contains(f.Name, "/") && strings.HasSuffix(f.Name, ".safetensors") {
			names = append(names, f.Name)
		}
	}
	return names, nil
}

// kvCache is the KV-cache arithmetic of docs/design/profile.md for one model config.
type kvCache struct {
	perToken int64 // bytes per token for the whole model: every layer, all heads
	mla      bool  // DeepSeek-style MLA: one latent, not split by tensor parallelism
	heads    int   // num_attention_heads
	kvHeads  int   // num_key_value_heads, or num_attention_heads when absent
}

// newKVCache derives the KV numbers from the config. headDim falls back to
// hiddenSize/numAttentionHeads, kvHeads to numAttentionHeads; MLA (kv_lora_rank > 0) keeps one
// latent per layer instead of per-head caches.
func newKVCache(cfg *Config) kvCache {
	heads := cfg.NumAttentionHeads
	headDim := cfg.HeadDim
	if headDim == 0 {
		headDim = cfg.HiddenSize / heads
	}
	kvHeads := cfg.NumKeyValueHeads
	if kvHeads == 0 {
		kvHeads = heads
	}
	k := kvCache{mla: cfg.KVLoraRank > 0, heads: heads, kvHeads: kvHeads}
	if k.mla {
		k.perToken = int64(cfg.NumHiddenLayers) * int64(cfg.KVLoraRank+cfg.QKRopeHeadDim) * KVBytesPerElement
	} else {
		k.perToken = 2 * int64(cfg.NumHiddenLayers) * int64(kvHeads) * int64(headDim) * KVBytesPerElement
	}
	return k
}

// perGPU is the bytes a token costs on each GPU of a t-way tensor-parallel replica: KV heads split
// across the GPUs, replicated once there are more GPUs than KV heads; MLA never splits.
func (k kvCache) perGPU(t int) int64 {
	if k.mla {
		return k.perToken
	}
	split := t
	if k.kvHeads < split {
		split = k.kvHeads
	}
	return k.perToken / int64(split)
}

// valid says whether a GPU count t is valid for the model: attention heads must divide evenly
// across the GPUs and, for non-MLA models, the KV heads must too — or there must be at least as
// many GPUs as KV heads.
func (k kvCache) valid(t int) bool {
	if k.heads%t != 0 {
		return false
	}
	return k.mla || k.kvHeads%t == 0 || t%k.kvHeads == 0
}
