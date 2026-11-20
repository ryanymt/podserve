package v1alpha1_test

// Parked spine test (P02 T2) for T5: `git mv` it to pkg/api/v1alpha1/extraargs_test.go, unchanged.
// It pins D-032: an extraArgs item may not set an argument podserve owns in any form vLLM v0.32.0's
// parser accepts (docs/design/modelservice.md, "Validation beyond the schema", rule 4).

import (
	"reflect"
	"testing"

	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
)

func TestExtraArgsForms(t *testing.T) {
	allowed := []string{
		"--enable-prefix-caching",
		"--max-num-seqs", "64", // a value as its own item
		"--model-impl=vllm",               // starts with an owned name, but is a different flag
		"--config-format=auto",            // longer than --config, so not an abbreviation of it
		"--chat-template=/etc/chat.jinja", // the "." is in the value, after "="
		"--chat-template /etc/chat.jinja", // the "." is in the value, after the space
		"--hf-overrides.rope_scaling.factor=2",
		"-O3",
		"auto",
	}
	for _, arg := range allowed {
		ms := spec(func(m *v1alpha1.ModelService) { m.Spec.Engine.ExtraArgs = []string{arg} })
		if errs := v1alpha1.Validate(ms); errs != nil {
			t.Errorf("%q: got %v, want it allowed", arg, errs)
		}
	}

	refused := []struct{ arg, message string }{
		// exact matches keep P01's message, with the token as written
		{"--port 8001", "sets --port, which podserve owns"},
		{"--port\t8001", "sets --port, which podserve owns"},
		{"  --dtype  ", "sets --dtype, which podserve owns"},
		{"--max_model_len=4096", "sets --max_model_len, which podserve owns"},
		{"--gpu_memory_utilization", "sets --gpu_memory_utilization, which podserve owns"},
		{"--model-loader-extra-config.concurrency=8", "sets --model-loader-extra-config, which podserve owns"},
		{"--config=/etc/vllm.yaml", "sets --config, which podserve owns"},
		{"--config", "sets --config, which podserve owns"},
		{"-tp=2", "sets -tp, which podserve owns"},
		// abbreviations, which argparse accepts: name the shortest owned flag it could abbreviate
		{"--max-model=4096", "abbreviates --max-model-len, which podserve owns"},
		{"--max_model 4096", "abbreviates --max-model-len, which podserve owns"},
		{"--served", "abbreviates --served-model-name, which podserve owns"},
		{"--gpu-mem=0.5", "abbreviates --gpu-memory-utilization, which podserve owns"},
		{"--mod", "abbreviates --model, which podserve owns"},
		{"--conf=x.yaml", "abbreviates --config, which podserve owns"},
		{"-t", "abbreviates -tp, which podserve owns"},
	}
	for _, c := range refused {
		ms := spec(func(m *v1alpha1.ModelService) { m.Spec.Engine.ExtraArgs = []string{"--enable-prefix-caching", c.arg} })
		want := []v1alpha1.FieldError{{Path: "spec.engine.extraArgs[1]", Message: c.message}}
		if errs := v1alpha1.Validate(ms); !reflect.DeepEqual(errs, want) {
			t.Errorf("%q: got %v, want %v", c.arg, errs, want)
		}
	}
}
