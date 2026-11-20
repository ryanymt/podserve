package v1alpha1_test

// Spine test (P01 T2): the committed CRD's schema must describe exactly the Go types' JSON fields.
// controller-gen generates the CRD from types.go (tests/lint.sh checks it is fresh); this test also
// catches a hand edit to either side. Not parked: it tests spine files and passes on main.

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	apiextv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"

	"github.com/ryanymt/podserve/config/crd"
	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
)

// jsonFields lists the JSON paths of a struct type's exported fields, recursively (structs and
// pointers to structs), skipping embedded metadata.
func jsonFields(prefix string, t reflect.Type, out map[string]bool) {
	if t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "" || tag == "-" {
			continue
		}
		path := prefix + "." + tag
		ft := f.Type
		if ft.Kind() == reflect.Pointer {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.Struct && ft.PkgPath() == reflect.TypeOf(v1alpha1.ModelService{}).PkgPath() {
			jsonFields(path, ft, out)
			continue
		}
		out[path] = true
	}
}

func schemaFields(prefix string, s apiextv1.JSONSchemaProps, out map[string]bool) {
	if s.Type == "object" && len(s.Properties) > 0 {
		for name, p := range s.Properties {
			schemaFields(prefix+"."+name, p, out)
		}
		return
	}
	out[prefix] = true
}

func keys(m map[string]bool) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func TestCRDMatchesTypes(t *testing.T) {
	var c apiextv1.CustomResourceDefinition
	if err := yaml.Unmarshal(crd.ModelServicesYAML, &c); err != nil {
		t.Fatal(err)
	}
	if c.Spec.Group != v1alpha1.Group || c.Spec.Names.Kind != v1alpha1.Kind || c.Spec.Names.Plural != v1alpha1.Resource {
		t.Fatalf("CRD names %s/%s/%s", c.Spec.Group, c.Spec.Names.Kind, c.Spec.Names.Plural)
	}
	if len(c.Spec.Versions) != 1 || c.Spec.Versions[0].Name != v1alpha1.Version || c.Spec.Versions[0].Subresources == nil ||
		c.Spec.Versions[0].Subresources.Status == nil {
		t.Fatalf("want one version %s with a status subresource", v1alpha1.Version)
	}
	root := c.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, part := range []struct {
		name string
		typ  reflect.Type
	}{{"spec", reflect.TypeOf(v1alpha1.ModelServiceSpec{})}, {"status", reflect.TypeOf(v1alpha1.ModelServiceStatus{})}} {
		fromGo, fromCRD := map[string]bool{}, map[string]bool{}
		jsonFields(part.name, part.typ, fromGo)
		schemaFields(part.name, root.Properties[part.name], fromCRD)
		if !reflect.DeepEqual(keys(fromGo), keys(fromCRD)) {
			t.Errorf("%s fields differ\n Go: %v\nCRD: %v", part.name, keys(fromGo), keys(fromCRD))
		}
	}
	if req := root.Properties["spec"].Properties["model"].Required; !reflect.DeepEqual(req, []string{"source"}) {
		t.Errorf("spec.model required = %v, want [source]", req)
	}
}
