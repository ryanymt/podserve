package main

// Spine tests (P02 T2) for the CLI's shared client config and D-033's warning filter.

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// The filter is installed on the config both clients are built from, so a test of the filter alone
// can't pass while the CLI still prints Autopilot's warnings.
func TestRestConfigInstallsTheWarningFilter(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	os.WriteFile(kubeconfig, []byte(`apiVersion: v1
kind: Config
clusters: [{name: c, cluster: {server: "https://127.0.0.1:1"}}]
users: [{name: u, user: {token: t}}]
contexts: [{name: x, context: {cluster: c, user: u}}]
current-context: x
`), 0o600)
	t.Setenv("KUBECONFIG", kubeconfig)
	var stderr bytes.Buffer
	cfg, err := restConfig(&stderr)
	if err != nil {
		t.Fatal(err)
	}
	h, ok := cfg.WarningHandler.(autopilotWarnings)
	if !ok {
		t.Fatalf("WarningHandler is %T, want autopilotWarnings", cfg.WarningHandler)
	}
	if h.w != &stderr {
		t.Error("the filter doesn't write to the given stderr")
	}
}

func TestAutopilotWarningsAreDropped(t *testing.T) {
	var out bytes.Buffer
	h := autopilotWarnings{w: &out}
	// recorded live on GKE Autopilot in P01 T11 (t11-live/deploy-stage-lines.txt)
	h.HandleWarningHeader(299, "", "autopilot-workload-defaulter:Autopilot added tolerations matching: cloud.google.com/gke-spot")
	h.HandleWarningHeader(299, "", "autopilot-default-resources-mutator:Autopilot updated Deployment podserve/qwen3-8b: defaulted unspecified 'nvidia.com/gpu' resource for containers [vllm] (see http://g.co/gke/autopilot-defaults).")
	if out.Len() != 0 {
		t.Errorf("Autopilot's warnings got through:\n%s", out.String())
	}
	h.HandleWarningHeader(299, "", "apps/v1beta1 Deployment is deprecated")
	if out.String() != "Warning: apps/v1beta1 Deployment is deprecated\n" {
		t.Errorf("other warnings must still print, got %q", out.String())
	}
}
