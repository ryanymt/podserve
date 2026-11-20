package profile_test

// Spine test (P01, after T3): two edge cases no recorded fixture reaches. Kept in step with
// testdata/spine/profile/expected.py and docs/design/profile.md.

import (
	"errors"
	"testing"

	"github.com/ryanymt/podserve/catalog"
	"github.com/ryanymt/podserve/pkg/profile"
)

func edgeModel(heads, maxPos int) profile.Model {
	return profile.Model{
		Repo: "x/y", Revision: "0123456789012345678901234567890123456789", PipelineTag: "text-generation",
		Files: []profile.File{{Name: "model.safetensors", Size: 300 << 20}},
		Config: &profile.Config{Architectures: []string{"LlamaForCausalLM"}, HiddenSize: 768, NumHiddenLayers: 12,
			NumAttentionHeads: heads, NumKeyValueHeads: heads, HeadDim: 64, MaxPositionEmbeddings: maxPos, Dtype: "bfloat16"},
	}
}

func TestShortModelGetsItsWholeContext(t *testing.T) {
	cat, _ := profile.LoadCatalog(catalog.GPUsYAML)
	r, err := profile.Plan(edgeModel(12, 512), profile.Request{}, cat, profile.LoadArchitectures(catalog.VLLMArchitectures))
	if err != nil {
		t.Fatal(err)
	}
	if r.MaxModelLen != 512 || r.SeqsAtMaxLen < 1 {
		t.Errorf("a 512-token model: MaxModelLen %d, SeqsAtMaxLen %d; want 512 and at least 1", r.MaxModelLen, r.SeqsAtMaxLen)
	}
}

func TestNoShapeSuitsTheHeads(t *testing.T) {
	cat, _ := profile.LoadCatalog(catalog.GPUsYAML)
	// 3 heads with 3 KV heads: only 1 GPU is valid, so force 2 GPUs to leave no valid option.
	_, err := profile.Plan(edgeModel(3, 2048), profile.Request{GPUCount: 2, GPUType: "nvidia-l4"}, cat,
		profile.LoadArchitectures(catalog.VLLMArchitectures))
	var r *profile.Refusal
	if !errors.As(err, &r) || r.Code != profile.DoesNotFit ||
		r.Message != "needs about 0.3 GiB for weights; no catalogued GPU shape suits its 3 attention heads" {
		t.Fatalf("got %v", err)
	}
}
