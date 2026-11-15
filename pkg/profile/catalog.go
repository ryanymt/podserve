package profile

import (
	"bufio"
	"bytes"
	"fmt"
	"strings"

	"sigs.k8s.io/yaml"
)

// Spine file (P01 T1): loading the embedded data files is part of the contract, not of T3.

// LoadCatalog parses catalog/gpus.yaml.
func LoadCatalog(data []byte) (Catalog, error) {
	var c Catalog
	if err := yaml.UnmarshalStrict(data, &c); err != nil {
		return Catalog{}, fmt.Errorf("gpu catalog: %w", err)
	}
	if len(c.GPUs) == 0 {
		return Catalog{}, fmt.Errorf("gpu catalog: no gpus")
	}
	for _, g := range c.GPUs {
		if g.Type == "" || g.UsableMiB <= 0 || len(g.Shapes) == 0 {
			return Catalog{}, fmt.Errorf("gpu catalog: %q needs a type, usableMiB and shapes", g.Type)
		}
	}
	return c, nil
}

// LoadArchitectures parses catalog/vllm-<version>-architectures.txt: one name per line; blank
// lines and lines starting with # are ignored.
func LoadArchitectures(data []byte) Architectures {
	a := Architectures{}
	s := bufio.NewScanner(bytes.NewReader(data))
	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line != "" && !strings.HasPrefix(line, "#") {
			a[line] = true
		}
	}
	return a
}

// Find returns the catalog entry for a GPU type.
func (c Catalog) Find(gpuType string) (GPU, bool) {
	for _, g := range c.GPUs {
		if g.Type == gpuType {
			return g, true
		}
	}
	return GPU{}, false
}
