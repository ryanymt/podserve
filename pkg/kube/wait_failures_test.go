package kube_test

// Parked spine test (P02 T2) for T3: `git mv` it to pkg/kube/wait_failures_test.go, unchanged.
// WaitReady, Serving and Stop against internal/fakekube, with pod states recorded live on GKE
// Autopilot (testdata/kube/failures/, applied with fakekube.SetServingPodsFrom). It uses the harness
// in kube_test.go (newHarness, fakeClock, ns). Contract: docs/design/cli.md (`podserve deploy`), and
// D-025, D-026, D-027, D-029, D-036.

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/kube"
)

// recordingLogs is a kube.LogReader that remembers its calls.
type recordingLogs struct {
	lines []string
	err   error
	calls []string // "<pod> <container> <lines>"
}

func (r *recordingLogs) PreviousLog(_ context.Context, pod, container string, lines int64) ([]string, error) {
	r.calls = append(r.calls, pod+" "+container+" "+strconv.FormatInt(lines, 10))
	return r.lines, r.err
}

// servingDeployment seeds the Deployment a ModelService named name would have, without its pods.
func servingDeployment(t *testing.T, h *harness, name string) {
	t.Helper()
	dep := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "Deployment",
		"metadata": map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{"podserve.dev/model": name}},
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": name}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "vllm", "image": "vllm"}}},
			},
		},
	}}
	if err := h.cluster.Add(kube.Deployments, dep); err != nil {
		t.Fatal(err)
	}
}

func recorded(state string) string { return filepath.Join(recordedDir, state) }

func TestWaitFailsOnACrashLoopWithTheLog(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	if err := h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("crashloop")); err != nil {
		t.Fatal(err)
	}
	logs := &recordingLogs{lines: []string{"usage: vllm [-h] [-v]", "vllm: error: unrecognized arguments: --no-such-flag"}}
	h.client.Logs = logs
	err := h.client.WaitReady(context.Background(), "qwen3-8b")
	var fe *kube.FailedError
	if !errors.As(err, &fe) {
		t.Fatalf("got %v, want *kube.FailedError", err)
	}
	d := fe.Diagnosis
	if !d.Failed || d.Reason != kube.ReasonCrashLoop || d.Last == nil || *d.Last != (kube.Termination{Reason: "Error", ExitCode: 2}) {
		t.Errorf("diagnosis %+v", d)
	}
	pods, _ := h.cluster.Resource(kube.Pods).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	pod := pods.Items[0].GetName()
	if d.Pod != pod {
		t.Errorf("diagnosis pod %q, want %q", d.Pod, pod)
	}
	if !reflect.DeepEqual(logs.calls, []string{pod + " vllm 20"}) {
		t.Errorf("log reads %v, want one: %s vllm 20", logs.calls, pod)
	}
	if !reflect.DeepEqual(fe.Log, logs.lines) || fe.LogErr != nil {
		t.Errorf("log %v (err %v), want %v", fe.Log, fe.LogErr, logs.lines)
	}
}

// A log that can't be read is reported, not fatal; no reader at all means no log and no error.
func TestWaitFailsEvenWithoutALog(t *testing.T) {
	for _, logs := range []*recordingLogs{{err: errors.New(`previous terminated container "vllm" not found`)}, nil} {
		h := newHarness()
		servingDeployment(t, h, "qwen3-8b")
		_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("crashloop"))
		if logs != nil {
			h.client.Logs = logs
		}
		var fe *kube.FailedError
		if err := h.client.WaitReady(context.Background(), "qwen3-8b"); !errors.As(err, &fe) {
			t.Fatalf("got %v, want *kube.FailedError", err)
		}
		if fe.Log != nil {
			t.Errorf("log %v, want none", fe.Log)
		}
		if (logs == nil) != (fe.LogErr == nil) {
			t.Errorf("LogErr %v with reader %v", fe.LogErr, logs != nil)
		}
	}
}

// The waiting reason is reported each time it changes, in the order seen, and the wait ends Ready.
// (Until T9's live run this test pinned "each reason once per deploy"; see wait_changes_test.go.)
func TestWaitReportsEachChangeOfReason(t *testing.T) {
	h := newHarness()
	var reasons []kube.Reason
	h.client.Options.Reason = func(d kube.Diagnosis) { reasons = append(reasons, d.Reason) }
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("scaleup"))
	h.clock.onSleep = func(n int) {
		switch n {
		case 1:
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("reclaimed"))
		case 2:
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("scaleup"))
		case 3:
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("loading"))
		case 4:
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("ready"))
		}
	}
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); err != nil {
		t.Fatal(err)
	}
	if want := []kube.Reason{kube.ReasonScaleUp, kube.ReasonReclaimed, kube.ReasonScaleUp}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("reasons %v, want %v", reasons, want)
	}
	if want := []kube.Stage{kube.StageNode, kube.StageLoading, kube.StageReady}; !reflect.DeepEqual(h.stages, want) {
		t.Errorf("stages %v, want %v", h.stages, want)
	}
}

// A timeout names the last reason seen.
func TestWaitTimeoutCarriesTheLastReason(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("scaleup"))
	err := h.client.WaitReady(context.Background(), "qwen3-8b")
	var te *kube.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *kube.TimeoutError", err)
	}
	if te.Stage != kube.StageNode || te.Reason != kube.ReasonScaleUp || te.Message != "Autopilot is adding a node" || te.After != time.Minute {
		t.Errorf("got %+v", *te)
	}
}

// D-027: Stop scales the Deployment to zero with a merge patch; the template survives and the pods go.
func TestStopScalesTheModelToZero(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("crashloop"))
	if err := h.client.Stop(context.Background(), "qwen3-8b"); err != nil {
		t.Fatal(err)
	}
	dep, err := h.cluster.Get(kube.Deployments, ns, "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); r != 0 {
		t.Errorf("replicas %d, want 0", r)
	}
	if _, found, _ := unstructured.NestedMap(dep.Object, "spec", "template"); !found {
		t.Error("Stop dropped the pod template")
	}
	pods, _ := h.cluster.Resource(kube.Pods).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("%d pods left", len(pods.Items))
	}
	if err := h.client.Stop(context.Background(), "no-such-model"); !errors.Is(err, kube.ErrNotFound) {
		t.Errorf("Stop of a missing model: %v, want kube.ErrNotFound", err)
	}
}

// Serving reads one model's Deployment, its ReplicaSets and pods, and the namespace's events.
func TestServingReadsOneModel(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	servingDeployment(t, h, "llama-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("crashloop"))
	_ = h.cluster.SetServingPodsFrom(ns, "llama-8b", recorded("scaleup"))
	s, err := h.client.Serving(context.Background(), "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if s.Deployment == nil || s.Deployment.GetName() != "qwen3-8b" {
		t.Fatalf("deployment %v", s.Deployment)
	}
	if len(s.ReplicaSets) != 1 || len(s.Pods) != 1 {
		t.Errorf("%d ReplicaSets and %d pods, want 1 and 1 (llama-8b's are not qwen3-8b's)", len(s.ReplicaSets), len(s.Pods))
	}
	for _, p := range s.Pods {
		if p.GetLabels()["podserve.dev/model"] != "qwen3-8b" {
			t.Errorf("pod %s belongs to another model", p.GetName())
		}
	}
	if len(s.Events) == 0 {
		t.Error("no events")
	}
	if d := kube.Diagnose(s, h.clock.Now()); d.Reason != kube.ReasonCrashLoop {
		t.Errorf("diagnosis %+v: llama-8b's events must not change qwen3-8b's", d)
	}
	none, err := h.client.Serving(context.Background(), "no-such-model")
	if err != nil || none.Deployment != nil {
		t.Errorf("a model with no Deployment: %+v, %v (want an empty Serving, no error)", none, err)
	}
}
