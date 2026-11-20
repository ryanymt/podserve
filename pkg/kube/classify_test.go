package kube_test

// Parked spine test (P02 T2) for T3: `git mv` it to pkg/kube/classify_test.go, unchanged.
// kube.Diagnose (D-025, D-026, D-036) on the states recorded live on GKE Autopilot in P02 T0
// (testdata/kube/failures/<state>/: kubectl's apps, pods and events lists, plus meta.json's time),
// and on a few states built from them. docs/design/cli.md ("Waiting reasons", "A model that can't
// start", "Readiness") is the contract.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/kube"
)

const recordedDir = "../../testdata/kube/failures"

// The registry's own errors, recorded live; the cause comes at the end, so they are never cut.
const (
	pullNotFound = "Failed to pull image \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": rpc error: code = NotFound desc = failed to pull and unpack image \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": failed to resolve reference \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001: not found"
	invalidImage = "Failed to apply default image tag \"us-central1-docker.pkg.dev/my-project/Bad_Repo/vLLM:Latest\": couldn't parse image name \"us-central1-docker.pkg.dev/my-project/Bad_Repo/vLLM:Latest\": invalid reference format: repository name (my-project/Bad_Repo/vLLM) must be lowercase"
)

// recordedObjects decodes a recorded `kubectl get -o json` list (integers stay int64).
func recordedObjects(t *testing.T, path string) []unstructured.Unstructured {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatal(err)
	}
	var out []unstructured.Unstructured
	for _, raw := range list.Items {
		var u unstructured.Unstructured
		if err := u.UnmarshalJSON(raw); err != nil {
			t.Fatal(err)
		}
		out = append(out, u)
	}
	return out
}

// recordedServing is a fixture as kube.Diagnose's input, and the time it was recorded.
func recordedServing(t *testing.T, state string) (kube.Serving, time.Time) {
	t.Helper()
	dir := filepath.Join(recordedDir, state)
	var s kube.Serving
	for _, o := range recordedObjects(t, filepath.Join(dir, "apps.json")) {
		o := o
		switch o.GetKind() {
		case "Deployment":
			s.Deployment = &o
		case "ReplicaSet":
			s.ReplicaSets = append(s.ReplicaSets, o)
		}
	}
	s.Pods = recordedObjects(t, filepath.Join(dir, "pods.json"))
	s.Events = recordedObjects(t, filepath.Join(dir, "events.json"))
	var meta struct {
		Now time.Time `json:"now"`
	}
	b, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &meta); err != nil {
		t.Fatal(err)
	}
	return s, meta.Now
}

func TestDiagnoseRecordedStates(t *testing.T) {
	for state, want := range map[string]kube.Diagnosis{
		// a pod waiting for its node while Autopilot adds one; FailedScheduling is never reported
		"scaleup": {Stage: kube.StageNode, Reason: kube.ReasonScaleUp, Message: "Autopilot is adding a node",
			Pod: "qwen3-8b-68496b54d4-f2wt9"},
		"pulling": {Stage: kube.StageImage},
		"loading": {Stage: kube.StageLoading},
		// the spot node went away while vLLM loaded; the ReplicaSet's new pod waits for a node
		"reclaimed": {Stage: kube.StageNode, Reason: kube.ReasonReclaimed,
			Message: "Pod was terminated in response to imminent node shutdown.", Pod: "qwen3-8b-68496b54d4-f2wt9"},
		// a FailedScaleUp that arrived after the pod was scheduled: stale, not reported
		"stale-scaleup": {Stage: kube.StageLoading},
		"ready":         {Stage: kube.StageReady, Ready: true},
		"crashloop": {Stage: kube.StageLoading, Failed: true, Reason: kube.ReasonCrashLoop,
			Message: "back-off 20s restarting failed container=vllm pod=qwen3-8b-745f4d9766-rtxmk_podserve(5d12aaf2-77a3-47c5-8aec-6845ee7f3f38)",
			Pod:     "qwen3-8b-745f4d9766-rtxmk", Last: &kube.Termination{Reason: "Error", ExitCode: 2}},
		// between two crashes the container runs: only CrashLoopBackOff itself fails the deploy
		"crashloop-between": {Stage: kube.StageLoading},
		// a missing image, 17 s after the first failed pull: still within ImagePullGrace
		"imagepull-early": {Stage: kube.StageImage, Reason: kube.ReasonPullRetry, Message: pullNotFound,
			Pod: "qwen3-8b-66c4d696c8-twg5q"},
		// the same, 2 min 7 s after the first failed pull
		"imagepull": {Stage: kube.StageImage, Failed: true, Reason: kube.ReasonImagePull, Message: pullNotFound,
			Pod: "qwen3-8b-66c4d696c8-twg5q"},
		"invalidimage": {Stage: kube.StageImage, Failed: true, Reason: kube.ReasonInvalidImage, Message: invalidImage,
			Pod: "qwen3-8b-65694ffb7f-x24mz"},
	} {
		s, now := recordedServing(t, state)
		if got := kube.Diagnose(s, now); !reflect.DeepEqual(got, want) {
			t.Errorf("%s:\n got %+v\nwant %+v", state, got, want)
		}
	}
}

// D-036: a Deployment whose new generation the controller hasn't observed yet isn't ready, even
// though the previous template's pods are.
func TestDiagnoseUnobservedRedeploy(t *testing.T) {
	s, now := recordedServing(t, "ready")
	s.Deployment.SetGeneration(s.Deployment.GetGeneration() + 1)
	got := kube.Diagnose(s, now)
	if got.Ready || got.Failed || got.Stage != kube.StageNode {
		t.Errorf("got %+v, want not ready, stage node", got)
	}
}

// D-036: a crash-looping pod of the previous template that is still terminating doesn't fail a
// redeploy whose new pod is loading.
func TestDiagnoseIgnoresTheOldTemplatesPods(t *testing.T) {
	s, now := recordedServing(t, "crashloop") // two ReplicaSets: revision 1 (gone) and 2 (crash-looping)
	oldHash := s.Pods[0].GetLabels()["pod-template-hash"]
	var oldRS unstructured.Unstructured
	for _, rs := range s.ReplicaSets {
		if rs.GetLabels()["pod-template-hash"] == oldHash {
			oldRS = rs
		}
	}

	newRS := *oldRS.DeepCopy()
	newRS.SetName("qwen3-8b-5c9d7f6b88")
	newRS.SetLabels(map[string]string{"pod-template-hash": "5c9d7f6b88", "podserve.dev/model": "qwen3-8b"})
	newRS.SetAnnotations(map[string]string{"deployment.kubernetes.io/revision": "3"})
	s.ReplicaSets = append(s.ReplicaSets, newRS)

	old := s.Pods[0]
	if oldRS.GetName() == "" {
		t.Fatalf("fixture: no ReplicaSet with the crashing pod's hash %q", oldHash)
	}
	gone := metav1Now(now)
	old.SetDeletionTimestamp(&gone)
	s.Pods[0] = old

	loading, _ := recordedServing(t, "loading")
	fresh := loading.Pods[0]
	fresh.SetName("qwen3-8b-5c9d7f6b88-k2x4p")
	labels := fresh.GetLabels()
	labels["pod-template-hash"] = "5c9d7f6b88"
	fresh.SetLabels(labels)
	s.Pods = append(s.Pods, fresh)

	got := kube.Diagnose(s, now)
	if got.Failed || got.Stage != kube.StageLoading {
		t.Errorf("got %+v, want stage loading and not failed", got)
	}
}

// Events about other pods (another model's, or a deleted one's) don't count: the fake ignores field
// selectors, so Diagnose filters by involvedObject itself.
func TestDiagnoseIgnoresOtherPodsEvents(t *testing.T) {
	s, now := recordedServing(t, "scaleup")
	other := s.Events[0].DeepCopy()
	_ = unstructured.SetNestedField(other.Object, "llama-8b-7d4f9c6b5d-zz9qq", "involvedObject", "name")
	other.Object["reason"] = "FailedScaleUp"
	other.Object["message"] = "Node scale up in zones us-central1-a associated with this pod failed: GCE quota exceeded."
	other.Object["lastTimestamp"] = now.Format(time.RFC3339)
	s.Events = append(s.Events, *other)
	if got := kube.Diagnose(s, now); got.Reason != kube.ReasonScaleUp {
		t.Errorf("reason %q, want %q (another pod's FailedScaleUp must not count)", got.Reason, kube.ReasonScaleUp)
	}
}

// FailedScaleUp outranks TriggeredScaleUp, and messages are collapsed to one line but never cut
// (the CLI cuts its one-line reasons; the diagnosis keeps the whole message).
func TestDiagnoseFailedScaleUpAndLongMessages(t *testing.T) {
	s, now := recordedServing(t, "scaleup")
	ev := s.Events[0].DeepCopy()
	_ = unstructured.SetNestedField(ev.Object, "qwen3-8b-68496b54d4-f2wt9", "involvedObject", "name")
	ev.SetName("qwen3-8b-68496b54d4-f2wt9.failedscaleup")
	ev.Object["reason"] = "FailedScaleUp"
	long := "Node scale up in zones us-central1-a\nassociated with this pod failed:   GCE out of resources. " +
		strings.Repeat("Pod is at risk of not being scheduled. ", 6)
	ev.Object["message"] = long
	ev.Object["type"] = "Warning"
	ev.Object["lastTimestamp"] = now.Format(time.RFC3339)
	s.Events = append(s.Events, *ev)
	got := kube.Diagnose(s, now)
	want := strings.Join(strings.Fields(long), " ")
	if got.Reason != kube.ReasonScaleUpFailed || got.Message != want || got.Failed {
		t.Errorf("got %+v\nwant reason %q, not failed, message %q", got, kube.ReasonScaleUpFailed, want)
	}
}

func TestDiagnoseWithoutADeployment(t *testing.T) {
	got := kube.Diagnose(kube.Serving{}, time.Date(2026, 11, 19, 4, 0, 0, 0, time.UTC))
	if !reflect.DeepEqual(got, kube.Diagnosis{Stage: kube.StageNode}) {
		t.Errorf("got %+v, want stage node and nothing else", got)
	}
}

func metav1Now(t time.Time) metav1.Time { return metav1.NewTime(t) }
