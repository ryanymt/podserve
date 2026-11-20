package main

// Parked spine test (P02 T2) for T11: `git mv` it to cmd/podserve/status_test.go, unchanged.
// `podserve status NAME [-o text|json]` (docs/design/cli.md, AC4): the stored status for a stopped
// model, the live diagnosis (kube.Diagnose, D-025) otherwise. Records are seeded directly, so this
// doesn't depend on `deploy`. Helpers are prefixed p02s.

import (
	"context"
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
	"github.com/ryanymt/podserve/pkg/kube"
)

// p02sModel seeds a qwen3-8b record with phase and message, and its Deployment with replicas.
func p02sModel(t *testing.T, e *env, phase v1alpha1.Phase, message string, replicas int64) {
	t.Helper()
	one, yes := int32(1), true
	ms := &v1alpha1.ModelService{}
	ms.APIVersion, ms.Kind = "podserve.dev/v1alpha1", "ModelService"
	ms.Name, ms.Namespace = "qwen3-8b", "podserve"
	ms.Spec = v1alpha1.ModelServiceSpec{
		Model:           v1alpha1.ModelSource{Source: "hf://Qwen/Qwen3-8B", Revision: "main"},
		ServedModelName: "Qwen/Qwen3-8B",
		Accelerator:     v1alpha1.AcceleratorSpec{Type: "auto", Spot: &yes},
		Engine:          v1alpha1.EngineSpec{GPUMemoryPercent: 90},
		Replicas:        &one,
	}
	kc := &kube.Client{Dynamic: e.cluster}
	ctx := context.Background()
	if _, err := kc.ApplyRecord(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if err := kc.UpdateStatus(ctx, "qwen3-8b", v1alpha1.ModelServiceStatus{
		Phase:            phase,
		Message:          message,
		ResolvedRevision: "b968826d9c46dd6066d109eabc6255188de91218",
		Profile:          &v1alpha1.ProfileStatus{GPUType: "nvidia-l4", GPUCount: 1, MachineType: "g2-standard-8", TensorParallelSize: 1},
		Endpoint:         "http://qwen3-8b.podserve.svc:8000/v1",
	}); err != nil {
		t.Fatal(err)
	}
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": "qwen3-8b", "namespace": "podserve"},
		"spec": map[string]any{"replicas": replicas,
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "qwen3-8b"}}}},
	}}
	if err := e.cluster.Add(kube.Deployments, dep); err != nil {
		t.Fatal(err)
	}
}

func TestStatusOfAWaitingModel(t *testing.T) {
	e := newEnv(t, false)
	p02sModel(t, e, v1alpha1.PhaseDeploying, "", 1)
	if err := e.cluster.SetServingPodsFrom("podserve", "qwen3-8b", filepath.Join(testdataDir(t), "kube", "failures", "scaleup")); err != nil {
		t.Fatal(err)
	}
	if code := e.run("status", "qwen3-8b"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	want := "Name:      qwen3-8b\n" +
		"Model:     hf://Qwen/Qwen3-8B @ b968826d9c46\n" +
		"GPU:       1x nvidia-l4, spot\n" +
		"Phase:     Deploying\n" +
		"Stage:     node\n" +
		"Reason:    TriggeredScaleUp: Autopilot is adding a node\n" +
		"Endpoint:  http://qwen3-8b.podserve.svc:8000/v1\n"
	if e.stdout.String() != want {
		t.Errorf("stdout\n%s\nwant\n%s", e.stdout.String(), want)
	}

	if code := e.run("status", "qwen3-8b", "-o", "json"); code != 0 {
		t.Fatalf("json: exit %d", code)
	}
	var got map[string]any
	if err := json.Unmarshal(e.stdout.Bytes(), &got); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, e.stdout.String())
	}
	wantJSON := map[string]any{"name": "qwen3-8b", "source": "hf://Qwen/Qwen3-8B", "revision": "b968826d9c46",
		"gpuType": "nvidia-l4", "gpuCount": float64(1), "spot": true, "phase": "Deploying", "stage": "node",
		"reason": "TriggeredScaleUp", "message": "Autopilot is adding a node", "endpoint": "http://qwen3-8b.podserve.svc:8000/v1"}
	if !reflect.DeepEqual(got, wantJSON) {
		t.Errorf("json\n got %v\nwant %v", got, wantJSON)
	}
}

// A Failed model scaled to zero: nothing is read from pods; the stored message is the reason.
func TestStatusOfAStoppedModel(t *testing.T) {
	e := newEnv(t, false)
	msg := "CrashLoopBackOff: back-off 20s restarting failed container=vllm pod=qwen3-8b-745f4d9766-rtxmk_podserve(5d12aaf2-77a3-47c5-8aec-6845ee7f3f38)"
	p02sModel(t, e, v1alpha1.PhaseFailed, msg, 0)
	if code := e.run("status", "qwen3-8b"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	want := "Name:      qwen3-8b\n" +
		"Model:     hf://Qwen/Qwen3-8B @ b968826d9c46\n" +
		"GPU:       1x nvidia-l4, spot\n" +
		"Phase:     Failed\n" +
		"Stage:     stopped (replicas 0)\n" +
		"Reason:    " + msg + "\n" +
		"Endpoint:  http://qwen3-8b.podserve.svc:8000/v1\n"
	if e.stdout.String() != want {
		t.Errorf("stdout\n%s\nwant\n%s", e.stdout.String(), want)
	}
	if code := e.run("status", "qwen3-8b", "-o", "json"); code != 0 {
		t.Fatalf("json: exit %d", code)
	}
	var got map[string]any
	_ = json.Unmarshal(e.stdout.Bytes(), &got)
	if got["stage"] != "stopped" || got["reason"] != "" || got["message"] != msg || got["phase"] != "Failed" {
		t.Errorf("json %v", got)
	}
}

// A ready model: stage ready, no reason.
func TestStatusOfAReadyModel(t *testing.T) {
	e := newEnv(t, false)
	p02sModel(t, e, v1alpha1.PhaseReady, "", 1)
	if err := e.cluster.SetServingPodsFrom("podserve", "qwen3-8b", filepath.Join(testdataDir(t), "kube", "failures", "ready")); err != nil {
		t.Fatal(err)
	}
	if code := e.run("status", "qwen3-8b"); code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	for _, line := range []string{"Phase:     Ready\n", "Stage:     ready\n", "Reason:    -\n"} {
		if !strings.Contains(e.stdout.String(), line) {
			t.Errorf("stdout lacks %q:\n%s", line, e.stdout.String())
		}
	}
}

func TestStatusOfAnUnknownModel(t *testing.T) {
	e := newEnv(t, false)
	if code := e.run("status", "nope"); code != 4 || e.stderr.String() != "podserve: no model named nope\n" {
		t.Errorf("exit %d stderr %q", code, e.stderr.String())
	}
}
