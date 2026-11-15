package hffixture

import "testing"

func TestEveryFixtureLoads(t *testing.T) {
	repos, err := Repos()
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) < 12 {
		t.Fatalf("want at least 12 fixtures (P01 AC2), have %d", len(repos))
	}
	for _, r := range repos {
		f, err := Load(r)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		m, err := f.Model(true)
		if err != nil {
			t.Fatalf("%s: %v", r, err)
		}
		if m.Repo != r || len(m.Revision) != 40 || len(m.Files) == 0 {
			t.Errorf("%s: repo %q revision %q files %d", r, m.Repo, m.Revision, len(m.Files))
		}
	}
}

func TestAliasesAndShapes(t *testing.T) {
	cases := []struct {
		repo                        string
		layers, heads, kv, hd, maxp int
		hidden                      int
		dtype, quant                string
		lora                        int
		gated, config, index        bool
	}{
		{"Qwen/Qwen3-8B", 36, 32, 8, 128, 40960, 4096, "bfloat16", "", 0, false, true, true},
		{"openai-community/gpt2", 12, 12, 0, 0, 1024, 768, "", "", 0, false, true, false},
		{"deepseek-ai/DeepSeek-V2-Lite-Chat", 27, 16, 16, 0, 163840, 2048, "bfloat16", "", 512, false, true, true},
		{"Qwen/Qwen3-8B-FP8", 36, 32, 8, 128, 40960, 4096, "bfloat16", "fp8", 0, false, true, true},
		{"JackFram/llama-160m", 12, 12, 12, 0, 2048, 768, "float32", "", 0, false, true, false},
		{"meta-llama/Llama-3.1-8B-Instruct", 0, 0, 0, 0, 0, 0, "", "", 0, true, false, false},
	}
	for _, tc := range cases {
		f, err := Load(tc.repo)
		if err != nil {
			t.Fatal(err)
		}
		m, err := f.Model(true)
		if err != nil {
			t.Fatal(err)
		}
		if m.Gated != tc.gated || (m.Config != nil) != tc.config || (m.IndexWeightFiles != nil) != tc.index {
			t.Errorf("%s: gated %v config %v index %v", tc.repo, m.Gated, m.Config != nil, m.IndexWeightFiles != nil)
			continue
		}
		if m.Config == nil {
			continue
		}
		c := m.Config
		got := []int{c.NumHiddenLayers, c.NumAttentionHeads, c.NumKeyValueHeads, c.HeadDim, c.MaxPositionEmbeddings, c.HiddenSize, c.KVLoraRank}
		want := []int{tc.layers, tc.heads, tc.kv, tc.hd, tc.maxp, tc.hidden, tc.lora}
		for i := range got {
			if got[i] != want[i] {
				t.Errorf("%s: field %d = %d, want %d (%+v)", tc.repo, i, got[i], want[i], c)
				break
			}
		}
		if c.Dtype != tc.dtype || c.QuantMethod != tc.quant {
			t.Errorf("%s: dtype %q quant %q", tc.repo, c.Dtype, c.QuantMethod)
		}
	}
}
