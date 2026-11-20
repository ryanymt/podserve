// Package hffixture loads the Hugging Face Hub responses recorded under testdata/hf/ (by
// hack/record-hf-fixture.py) for tests. Spine helper (P01 T2), not parked: the profile, hf and CLI
// tests share it. It maps config.json to profile.Config with the alias table in pkg/hf/types.go,
// implemented here independently of pkg/hf so the two can be checked against each other.
package hffixture

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ryanymt/podserve/pkg/profile"
)

// Info is the trimmed model-info response.
type Info struct {
	ID          string `json:"id"`
	SHA         string `json:"sha"`
	Gated       any    `json:"gated"` // false, "auto" or "manual"
	PipelineTag string `json:"pipeline_tag"`
	CardData    struct {
		License string `json:"license"`
	} `json:"cardData"`
	Siblings []struct {
		RFilename string `json:"rfilename"`
		Size      int64  `json:"size"`
	} `json:"siblings"`
}

// Meta is meta.json.
type Meta struct {
	Repo         string `json:"repo"`
	SHA          string `json:"sha"`
	ConfigStatus int    `json:"configStatus"`
	IndexStatus  *int   `json:"indexStatus"`
}

// Fixture is one recorded model. Raw bytes are kept so an HTTP fake can serve them verbatim.
type Fixture struct {
	Dir        string
	Meta       Meta
	Info       Info
	InfoJSON   []byte
	ConfigJSON []byte // nil when the Hub refused config.json
	IndexJSON  []byte // nil when the repo has no index
}

// Root is the repo's testdata/hf directory, found by walking up from the working directory to go.mod.
func Root() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return filepath.Join(dir, "testdata", "hf"), nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("hffixture: go.mod not found above the working directory")
		}
		dir = parent
	}
}

// Load reads the fixture for repo ("ORG/MODEL").
func Load(repo string) (*Fixture, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	d := filepath.Join(root, strings.ReplaceAll(repo, "/", "__"))
	f := &Fixture{Dir: d}
	if f.InfoJSON, err = os.ReadFile(filepath.Join(d, "info.json")); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(f.InfoJSON, &f.Info); err != nil {
		return nil, fmt.Errorf("%s/info.json: %w", d, err)
	}
	b, err := os.ReadFile(filepath.Join(d, "meta.json"))
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &f.Meta); err != nil {
		return nil, fmt.Errorf("%s/meta.json: %w", d, err)
	}
	if b, err := os.ReadFile(filepath.Join(d, "config.json")); err == nil {
		f.ConfigJSON = b
	}
	if b, err := os.ReadFile(filepath.Join(d, "index.json")); err == nil {
		f.IndexJSON = b
	}
	return f, nil
}

// Repos lists every recorded repo, sorted.
func Repos() ([]string, error) {
	root, err := Root()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, strings.Replace(e.Name(), "__", "/", 1))
		}
	}
	sort.Strings(out)
	return out, nil
}

// Model is what a correct pkg/hf client returns for this fixture. withToken says whether the
// caller has an HF token: a gated model's config is only read with one (and these anonymous
// recordings have no config for gated models, so Config stays nil either way).
func (f *Fixture) Model(withToken bool) (profile.Model, error) {
	m := profile.Model{
		Repo:        f.Info.ID,
		Revision:    f.Info.SHA,
		PipelineTag: f.Info.PipelineTag,
		Gated:       f.Info.Gated == "auto" || f.Info.Gated == "manual",
		License:     f.Info.CardData.License,
	}
	for _, s := range f.Info.Siblings {
		m.Files = append(m.Files, profile.File{Name: s.RFilename, Size: s.Size})
	}
	if f.IndexJSON != nil {
		var idx struct {
			WeightMap map[string]string `json:"weight_map"`
		}
		if err := json.Unmarshal(f.IndexJSON, &idx); err != nil {
			return m, fmt.Errorf("%s/index.json: %w", f.Dir, err)
		}
		set := map[string]bool{}
		for _, file := range idx.WeightMap {
			set[file] = true
		}
		m.IndexWeightFiles = []string{}
		for file := range set {
			m.IndexWeightFiles = append(m.IndexWeightFiles, file)
		}
		sort.Strings(m.IndexWeightFiles)
	}
	if f.ConfigJSON != nil && (!m.Gated || withToken) {
		c, err := ParseConfig(f.ConfigJSON)
		if err != nil {
			return m, fmt.Errorf("%s/config.json: %w", f.Dir, err)
		}
		m.Config = c
	}
	return m, nil
}

// ParseConfig maps config.json to profile.Config with the alias table (pkg/hf/types.go).
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
