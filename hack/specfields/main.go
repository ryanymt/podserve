// Command specfields prints ModelService's spec fields as the generated CRD defines them: one
// markdown table row per leaf field, with its path, type and default. It is the code side of the
// SYSTEM.md design-vs-code pair for docs/design/modelservice.md (whose field table must list the
// same paths, types and defaults). Run: go run ./hack/specfields
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/ryanymt/podserve/config/crd"
)

func main() {
	var c apiextv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crd.ModelServicesYAML, &c); err != nil {
		fmt.Fprintln(os.Stderr, "specfields:", err)
		os.Exit(1)
	}
	if len(c.Spec.Versions) != 1 || c.Spec.Versions[0].Schema == nil {
		fmt.Fprintln(os.Stderr, "specfields: expected exactly one version with a schema")
		os.Exit(1)
	}
	spec := c.Spec.Versions[0].Schema.OpenAPIV3Schema.Properties["spec"]
	fmt.Println("| Path | Type | Default |")
	fmt.Println("|---|---|---|")
	walk("spec", spec)
}

func walk(path string, s apiextv1.JSONSchemaProps) {
	if s.Type == "object" && len(s.Properties) > 0 {
		names := make([]string, 0, len(s.Properties))
		for n := range s.Properties {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			walk(path+"."+n, s.Properties[n])
		}
		return
	}
	typ := s.Type
	if typ == "array" && s.Items != nil && s.Items.Schema != nil {
		typ = "[]" + s.Items.Schema.Type
	}
	def := ""
	if s.Default != nil {
		var v any
		if err := json.Unmarshal(s.Default.Raw, &v); err == nil {
			def = fmt.Sprint(v)
		}
	}
	fmt.Printf("| `%s` | %s | %s |\n", path, typ, def)
}
