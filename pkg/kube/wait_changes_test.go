package kube_test

// WaitReady reports a change, not just a first sighting (P02 T9 live fix). On T9's live run a
// Qwen3-14B pod got its spot node, started loading, and lost the node 27 s later; the pod garbage
// collector removed it and the ReplicaSet's new pod waited 35 minutes for a node while Autopilot's
// scale-ups failed zone after zone. `deploy` had already printed node and loading once, and each
// scale-up reason once, so it printed nothing more until the timeout. Contract:
// docs/design/cli.md (`podserve deploy`, "Steps" and "Waiting reasons").

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/kube"
)

// A pod that falls back from loading to waiting for a node reports the node stage again, and the
// reason it now waits for.
func TestWaitReportsAStageAgainWhenThePodFallsBack(t *testing.T) {
	h := newHarness()
	var reasons []kube.Reason
	h.client.Options.Reason = func(d kube.Diagnosis) { reasons = append(reasons, d.Reason) }
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("scaleup"))
	states := []string{"loading", "quota", "quota", "loading", "ready"}
	h.clock.onSleep = func(n int) {
		if n <= len(states) {
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded(states[n-1]))
		}
	}
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); err != nil {
		t.Fatal(err)
	}
	wantStages := []kube.Stage{kube.StageNode, kube.StageLoading, kube.StageNode, kube.StageLoading, kube.StageReady}
	if !reflect.DeepEqual(h.stages, wantStages) {
		t.Errorf("stages %v, want %v", h.stages, wantStages)
	}
	if want := []kube.Reason{kube.ReasonScaleUp, kube.ReasonNoScaleUp}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("reasons %v, want %v", reasons, want)
	}
}

// The same reason is reported again after a change of stage: a new pod's scale-up is news.
func TestWaitReportsAReasonAgainAfterAStageChange(t *testing.T) {
	h := newHarness()
	var reasons []kube.Reason
	h.client.Options.Reason = func(d kube.Diagnosis) { reasons = append(reasons, d.Reason) }
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("scaleup"))
	states := []string{"scaleup", "loading", "scaleup", "scaleup", "ready"}
	h.clock.onSleep = func(n int) {
		if n <= len(states) {
			_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded(states[n-1]))
		}
	}
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); err != nil {
		t.Fatal(err)
	}
	if want := []kube.Reason{kube.ReasonScaleUp, kube.ReasonScaleUp}; !reflect.DeepEqual(reasons, want) {
		t.Errorf("reasons %v, want %v", reasons, want)
	}
	wantStages := []kube.Stage{kube.StageNode, kube.StageLoading, kube.StageNode, kube.StageReady}
	if !reflect.DeepEqual(h.stages, wantStages) {
		t.Errorf("stages %v, want %v", h.stages, wantStages)
	}
}

// Within one stage, a reason whose message changes is reported again; an unchanged one is not.
func TestWaitReportsANewMessageForTheSameReason(t *testing.T) {
	const first = "Pod didn't trigger scale-up: 4 node(s) didn't match Pod's node affinity/selector, 3 in backoff after failed scale-up"
	const second = "Pod didn't trigger scale-up: 4 node(s) didn't match Pod's node affinity/selector"
	h := newHarness()
	var messages []string
	h.client.Options.Reason = func(d kube.Diagnosis) { messages = append(messages, string(d.Reason)+": "+d.Message) }
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("quota"))
	pods, err := h.cluster.Resource(kube.Pods).Namespace(ns).List(context.Background(), metav1.ListOptions{})
	if err != nil || len(pods.Items) != 1 {
		t.Fatalf("pods %v, err %v", pods, err)
	}
	h.clock.onSleep = func(n int) {
		if n != 3 {
			return
		}
		// Autopilot's next verdict on the same pod, five minutes after the recorded one.
		ev := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Event",
			"metadata":       map[string]any{"name": "qwen3-8b.later", "namespace": ns},
			"involvedObject": map[string]any{"kind": "Pod", "name": pods.Items[0].GetName(), "namespace": ns},
			"reason":         "NotTriggerScaleUp", "type": "Normal", "message": second,
			"lastTimestamp": "2026-11-19T04:34:36Z",
		}}
		if err := h.cluster.Add(kube.Events, ev); err != nil {
			t.Error(err)
		}
	}
	err = h.client.WaitReady(context.Background(), "qwen3-8b")
	var te *kube.TimeoutError
	if !errors.As(err, &te) {
		t.Fatalf("got %v, want *kube.TimeoutError", err)
	}
	want := []string{"NotTriggerScaleUp: " + first, "NotTriggerScaleUp: " + second}
	if !reflect.DeepEqual(messages, want) {
		t.Errorf("reported\n  %q\nwant\n  %q", messages, want)
	}
	if te.Reason != kube.ReasonNoScaleUp || te.Message != second || te.After != time.Minute {
		t.Errorf("timeout %+v", *te)
	}
}
