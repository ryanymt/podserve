package v1alpha1_test

// Spine test (P01 T2), parked under testdata/spine/api/. T4 moves it to pkg/api/v1alpha1/ with
// `git mv`, unchanged. It pins Load, Default and Validate to docs/design/modelservice.md, including
// every message (the CLI prints them as `invalid spec: PATH: MESSAGE`).

import (
	"reflect"
	"strings"
	"testing"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
)

const valid = `
apiVersion: podserve.dev/v1alpha1
kind: ModelService
metadata:
  name: qwen3-8b
spec:
  model:
    source: hf://Qwen/Qwen3-8B
`

func load(t *testing.T, y string) *v1alpha1.ModelService {
	t.Helper()
	ms, err := v1alpha1.Load([]byte(y))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return ms
}

func TestLoadAndDefault(t *testing.T) {
	ms := load(t, valid)
	v1alpha1.Default(ms)
	if ms.Namespace != "podserve" || ms.Spec.Model.Revision != "main" || ms.Spec.ServedModelName != "Qwen/Qwen3-8B" ||
		ms.Spec.Accelerator.Type != "auto" || ms.Spec.Accelerator.Count != 0 || ms.Spec.Accelerator.Spot == nil ||
		!*ms.Spec.Accelerator.Spot || ms.Spec.Engine.GPUMemoryPercent != 90 || ms.Spec.Engine.MaxModelLen != 0 ||
		ms.Spec.Replicas == nil || *ms.Spec.Replicas != 1 {
		t.Errorf("defaults not applied: %+v", ms)
	}
	if errs := v1alpha1.Validate(ms); len(errs) != 0 {
		t.Errorf("valid spec rejected: %v", errs)
	}
}

func TestDefaultKeepsExplicitValues(t *testing.T) {
	ms := load(t, `
apiVersion: podserve.dev/v1alpha1
kind: ModelService
metadata: {name: m, namespace: podserve}
spec:
  model: {source: hf://Qwen/Qwen3-14B, revision: v1.0}
  servedModelName: my-model
  accelerator: {type: nvidia-l4, count: 2, spot: false}
  engine: {maxModelLen: 8192, gpuMemoryPercent: 85, extraArgs: ["--enable-prefix-caching"]}
  replicas: 3
`)
	v1alpha1.Default(ms)
	s := ms.Spec
	if s.Model.Revision != "v1.0" || s.ServedModelName != "my-model" || s.Accelerator.Type != "nvidia-l4" ||
		s.Accelerator.Count != 2 || *s.Accelerator.Spot || s.Engine.MaxModelLen != 8192 ||
		s.Engine.GPUMemoryPercent != 85 || *s.Replicas != 3 {
		t.Errorf("explicit values overwritten: %+v", s)
	}
	if errs := v1alpha1.Validate(ms); len(errs) != 0 {
		t.Errorf("valid spec rejected: %v", errs)
	}
}

func TestLoadRejects(t *testing.T) {
	cases := map[string]struct{ yaml, want string }{
		"unknown field": {strings.Replace(valid, "  model:", "  modle: {}\n  model:", 1), "modle"},
		"wrong kind": {strings.Replace(valid, "kind: ModelService", "kind: Deployment", 1),
			"want apiVersion podserve.dev/v1alpha1 and kind ModelService, got podserve.dev/v1alpha1 Deployment"},
		"wrong group": {strings.Replace(valid, "podserve.dev/v1alpha1", "apps/v1", 1),
			"want apiVersion podserve.dev/v1alpha1 and kind ModelService, got apps/v1 ModelService"},
		"not yaml": {"{{{", ""},
	}
	for name, tc := range cases {
		_, err := v1alpha1.Load([]byte(tc.yaml))
		if err == nil {
			t.Errorf("%s: Load accepted it", name)
		} else if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q does not contain %q", name, err, tc.want)
		}
	}
}

func spec(mod func(*v1alpha1.ModelService)) *v1alpha1.ModelService {
	one := int32(1)
	yes := true
	ms := &v1alpha1.ModelService{}
	ms.Name = "qwen3-8b"
	ms.Namespace = "podserve"
	ms.Spec = v1alpha1.ModelServiceSpec{
		Model:           v1alpha1.ModelSource{Source: "hf://Qwen/Qwen3-8B", Revision: "main"},
		ServedModelName: "Qwen/Qwen3-8B",
		Accelerator:     v1alpha1.AcceleratorSpec{Type: "auto", Spot: &yes},
		Engine:          v1alpha1.EngineSpec{GPUMemoryPercent: 90},
		Replicas:        &one,
	}
	mod(ms)
	return ms
}

func TestValidate(t *testing.T) {
	n := func(v int32) *int32 { return &v }
	cases := []struct {
		name string
		mod  func(*v1alpha1.ModelService)
		want []v1alpha1.FieldError
	}{
		{"valid", func(*v1alpha1.ModelService) {}, nil},
		{"no name", func(m *v1alpha1.ModelService) { m.Name = "" },
			[]v1alpha1.FieldError{{Path: "metadata.name", Message: "required"}}},
		{"bad name", func(m *v1alpha1.ModelService) { m.Name = "Qwen3_8B" },
			[]v1alpha1.FieldError{{Path: "metadata.name", Message: "must be a lowercase DNS label of at most 40 characters (a-z, 0-9, '-', starting with a letter)"}}},
		{"long name", func(m *v1alpha1.ModelService) { m.Name = strings.Repeat("a", 41) },
			[]v1alpha1.FieldError{{Path: "metadata.name", Message: "must be a lowercase DNS label of at most 40 characters (a-z, 0-9, '-', starting with a letter)"}}},
		{"other namespace", func(m *v1alpha1.ModelService) { m.Namespace = "default" },
			[]v1alpha1.FieldError{{Path: "metadata.namespace", Message: `must be "podserve" (podserve v0.1 uses one namespace)`}}},
		{"no source", func(m *v1alpha1.ModelService) { m.Spec.Model.Source = "" },
			[]v1alpha1.FieldError{{Path: "spec.model.source", Message: "required"}}},
		{"s3 source", func(m *v1alpha1.ModelService) { m.Spec.Model.Source = "s3://bucket/model" },
			[]v1alpha1.FieldError{{Path: "spec.model.source", Message: "must be hf://ORG/MODEL"}}},
		{"long source", func(m *v1alpha1.ModelService) { m.Spec.Model.Source = "hf://a/" + strings.Repeat("b", 200) },
			[]v1alpha1.FieldError{{Path: "spec.model.source", Message: "must be at most 200 characters"}}},
		{"bad revision", func(m *v1alpha1.ModelService) { m.Spec.Model.Revision = "main branch" },
			[]v1alpha1.FieldError{{Path: "spec.model.revision", Message: "must be a branch, tag or commit SHA (letters, digits, '.', '_', '-', '/'), at most 128 characters"}}},
		{"bad served name", func(m *v1alpha1.ModelService) { m.Spec.ServedModelName = "-x" },
			[]v1alpha1.FieldError{{Path: "spec.servedModelName", Message: "must match [A-Za-z0-9][A-Za-z0-9._/-]*, at most 128 characters"}}},
		{"bad gpu type", func(m *v1alpha1.ModelService) { m.Spec.Accelerator.Type = "tpu-v5e" },
			[]v1alpha1.FieldError{{Path: "spec.accelerator.type", Message: "must be one of auto, nvidia-l4, nvidia-tesla-a100, nvidia-a100-80gb, nvidia-h100-80gb"}}},
		{"bad count", func(m *v1alpha1.ModelService) { m.Spec.Accelerator.Type = "nvidia-l4"; m.Spec.Accelerator.Count = 3 },
			[]v1alpha1.FieldError{{Path: "spec.accelerator.count", Message: "must be one of 0, 1, 2, 4, 8"}}},
		{"count without type", func(m *v1alpha1.ModelService) { m.Spec.Accelerator.Count = 2 },
			[]v1alpha1.FieldError{{Path: "spec.accelerator.count", Message: "needs spec.accelerator.type set (not auto)"}}},
		{"context negative", func(m *v1alpha1.ModelService) { m.Spec.Engine.MaxModelLen = -1 },
			[]v1alpha1.FieldError{{Path: "spec.engine.maxModelLen", Message: "must be between 0 and 1048576"}}},
		{"context tiny", func(m *v1alpha1.ModelService) { m.Spec.Engine.MaxModelLen = 100 },
			[]v1alpha1.FieldError{{Path: "spec.engine.maxModelLen", Message: "must be 0 (choose) or at least 256"}}},
		{"memory", func(m *v1alpha1.ModelService) { m.Spec.Engine.GPUMemoryPercent = 99 },
			[]v1alpha1.FieldError{{Path: "spec.engine.gpuMemoryPercent", Message: "must be between 50 and 98"}}},
		{"reserved args", func(m *v1alpha1.ModelService) {
			m.Spec.Engine.ExtraArgs = []string{"--enable-prefix-caching", "--max-model-len=999", "-tp", "--port", "8001"}
		}, []v1alpha1.FieldError{
			{Path: "spec.engine.extraArgs[1]", Message: "sets --max-model-len, which podserve owns"},
			{Path: "spec.engine.extraArgs[2]", Message: "sets -tp, which podserve owns"},
			{Path: "spec.engine.extraArgs[3]", Message: "sets --port, which podserve owns"},
		}},
		{"too many args", func(m *v1alpha1.ModelService) { m.Spec.Engine.ExtraArgs = make([]string, 33) },
			[]v1alpha1.FieldError{{Path: "spec.engine.extraArgs", Message: "at most 32 items"}}},
		{"replicas", func(m *v1alpha1.ModelService) { m.Spec.Replicas = n(9) },
			[]v1alpha1.FieldError{{Path: "spec.replicas", Message: "must be between 1 and 8"}}},
		{"several, in field order", func(m *v1alpha1.ModelService) {
			m.Spec.Replicas = n(0)
			m.Name = ""
			m.Spec.Engine.GPUMemoryPercent = 10
		}, []v1alpha1.FieldError{
			{Path: "metadata.name", Message: "required"},
			{Path: "spec.engine.gpuMemoryPercent", Message: "must be between 50 and 98"},
			{Path: "spec.replicas", Message: "must be between 1 and 8"},
		}},
	}
	for _, tc := range cases {
		got := v1alpha1.Validate(spec(tc.mod))
		if len(got) == 0 && len(tc.want) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s:\n got %v\nwant %v", tc.name, got, tc.want)
		}
	}
}

func TestFieldErrorString(t *testing.T) {
	e := v1alpha1.FieldError{Path: "spec.replicas", Message: "must be between 1 and 8"}
	if e.Error() != "spec.replicas: must be between 1 and 8" {
		t.Errorf("Error() = %q", e.Error())
	}
}
