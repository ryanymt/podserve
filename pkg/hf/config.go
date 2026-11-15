package hf

// config.json -> profile.Config, with the alias table from types.go: for each field the first key
// present (non-null) at the top level wins; if none is, the same keys are tried inside
// "text_config". Architectures is read from the top level only. Absent means zero / "".

import (
	"encoding/json"

	"github.com/podserve/podserve/pkg/profile"
)

// ParseConfig maps a config.json to profile.Config. Numbers are JSON numbers.
func ParseConfig(data []byte) (*profile.Config, error) {
	var top map[string]any
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, err
	}
	text, _ := top["text_config"].(map[string]any)
	lookup := func(keys ...string) any {
		for _, scope := range []map[string]any{top, text} {
			if scope == nil {
				continue
			}
			for _, k := range keys {
				if v, ok := scope[k]; ok && v != nil {
					return v
				}
			}
		}
		return nil
	}
	num := func(keys ...string) int {
		if f, ok := lookup(keys...).(float64); ok {
			return int(f)
		}
		return 0
	}
	str := func(keys ...string) string {
		s, _ := lookup(keys...).(string)
		return s
	}
	c := &profile.Config{
		HiddenSize:            num("hidden_size", "n_embd", "d_model"),
		NumHiddenLayers:       num("num_hidden_layers", "n_layer", "num_layers"),
		NumAttentionHeads:     num("num_attention_heads", "n_head"),
		NumKeyValueHeads:      num("num_key_value_heads"),
		HeadDim:               num("head_dim"),
		MaxPositionEmbeddings: num("max_position_embeddings", "n_positions", "max_sequence_length", "seq_length"),
		Dtype:                 str("dtype", "torch_dtype"),
		KVLoraRank:            num("kv_lora_rank"),
		QKRopeHeadDim:         num("qk_rope_head_dim"),
	}
	if archs, ok := top["architectures"].([]any); ok {
		for _, a := range archs {
			if s, ok := a.(string); ok {
				c.Architectures = append(c.Architectures, s)
			}
		}
	}
	if q, ok := lookup("quantization_config").(map[string]any); ok {
		c.QuantMethod, _ = q["quant_method"].(string)
	}
	return c, nil
}
