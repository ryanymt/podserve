package v1alpha1

import (
	"fmt"

	"sigs.k8s.io/yaml"
)

// Load parses one ModelService from YAML or JSON, rejecting unknown fields (the error names the
// field) and any apiVersion/kind other than podserve.dev/v1alpha1 ModelService
// (docs/design/modelservice.md).
func Load(data []byte) (*ModelService, error) {
	ms := &ModelService{}
	if err := yaml.UnmarshalStrict(data, ms); err != nil {
		return nil, err
	}
	if ms.APIVersion != Group+"/"+Version || ms.Kind != Kind {
		return nil, fmt.Errorf("want apiVersion podserve.dev/v1alpha1 and kind ModelService, got %s %s", ms.APIVersion, ms.Kind)
	}
	return ms, nil
}
