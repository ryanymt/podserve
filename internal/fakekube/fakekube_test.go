package fakekube

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

var (
	msGVR  = schema.GroupVersionResource{Group: "podserve.dev", Version: "v1alpha1", Resource: "modelservices"}
	depGVR = gvrDeployments
	svcGVR = schema.GroupVersionResource{Version: "v1", Resource: "services"}
)

func obj(apiVersion, kind, name string, spec map[string]any) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion, "kind": kind,
		"metadata": map[string]any{"name": name, "namespace": "podserve"},
		"spec":     spec,
	}}
}

func apply(t *testing.T, c *Cluster, gvr schema.GroupVersionResource, u *unstructured.Unstructured) *unstructured.Unstructured {
	t.Helper()
	out, err := c.Resource(gvr).Namespace("podserve").Apply(context.Background(), u.GetName(), u,
		metav1.ApplyOptions{FieldManager: "podserve", Force: true})
	if err != nil {
		t.Fatalf("apply %s: %v", u.GetName(), err)
	}
	return out
}

func TestApplyCreatesThenUpdatesKeepingIdentityAndStatus(t *testing.T) {
	c := New()
	ms := obj("podserve.dev/v1alpha1", "ModelService", "m", map[string]any{"replicas": int64(1)})
	ms.Object["status"] = map[string]any{"phase": "Ready"} // must be ignored on the main resource
	first := apply(t, c, msGVR, ms)
	if first.GetUID() == "" || first.GetGeneration() != 1 {
		t.Fatalf("created without uid/generation: %v", first.Object)
	}
	if _, found := first.Object["status"]; found {
		t.Errorf("apply on the main resource set status: %v", first.Object["status"])
	}

	// Write status through /status.
	withStatus := first.DeepCopy()
	withStatus.Object["status"] = map[string]any{"phase": "Caching"}
	if _, err := c.Resource(msGVR).Namespace("podserve").UpdateStatus(context.Background(), withStatus, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}

	// Re-apply a changed spec: same uid, generation+1, status kept.
	second := apply(t, c, msGVR, obj("podserve.dev/v1alpha1", "ModelService", "m", map[string]any{"replicas": int64(2)}))
	if second.GetUID() != first.GetUID() || second.GetGeneration() != 2 {
		t.Errorf("re-apply: uid %s->%s generation %d", first.GetUID(), second.GetUID(), second.GetGeneration())
	}
	if phase, _, _ := unstructured.NestedString(second.Object, "status", "phase"); phase != "Caching" {
		t.Errorf("re-apply lost status: %v", second.Object["status"])
	}

	// An update on the main resource can't change status; one on /status can't change spec.
	tamper := second.DeepCopy()
	tamper.Object["status"] = map[string]any{"phase": "Failed"}
	tamper.Object["spec"] = map[string]any{"replicas": int64(5)}
	if _, err := c.Resource(msGVR).Namespace("podserve").Update(context.Background(), tamper, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Get(msGVR, "podserve", "m")
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != "Caching" {
		t.Errorf("main-resource update changed status to %q", phase)
	}
	if _, err := c.Resource(msGVR).Namespace("podserve").UpdateStatus(context.Background(), tamper, metav1.UpdateOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ = c.Get(msGVR, "podserve", "m")
	if r, _, _ := unstructured.NestedInt64(got.Object, "spec", "replicas"); r != 5 {
		t.Errorf("spec replicas %d after the main update, want 5", r)
	}
	if phase, _, _ := unstructured.NestedString(got.Object, "status", "phase"); phase != "Failed" {
		t.Errorf("status update did not apply: %q", phase)
	}
}

func TestDeleteCascadesThroughOwners(t *testing.T) {
	c := New()
	rec := apply(t, c, msGVR, obj("podserve.dev/v1alpha1", "ModelService", "m", map[string]any{}))
	owner := []metav1.OwnerReference{{APIVersion: "podserve.dev/v1alpha1", Kind: "ModelService", Name: "m", UID: rec.GetUID()}}
	dep := obj("apps/v1", "Deployment", "m", map[string]any{"replicas": int64(2),
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "m"}}}})
	dep.SetOwnerReferences(owner)
	apply(t, c, depGVR, dep)
	svc := obj("v1", "Service", "m", map[string]any{})
	svc.SetOwnerReferences(owner)
	apply(t, c, svcGVR, svc)
	if err := c.SetServingPods("podserve", "m", StageLoading); err != nil {
		t.Fatal(err)
	}
	pods, err := c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{LabelSelector: "podserve.dev/model=m"})
	if err != nil || len(pods.Items) != 2 {
		t.Fatalf("want 2 pods by label, got %v (%v)", len(pods.Items), err)
	}

	if err := c.Resource(msGVR).Namespace("podserve").Delete(context.Background(), "m", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	for _, g := range []schema.GroupVersionResource{msGVR, depGVR, svcGVR} {
		if _, err := c.Get(g, "podserve", "m"); !apierrors.IsNotFound(err) {
			t.Errorf("%s still exists after the owner was deleted (%v)", g.Resource, err)
		}
	}
	pods, _ = c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("%d pods survived the cascade", len(pods.Items))
	}
}

func TestAutoReadyAndJobs(t *testing.T) {
	c := New()
	c.AutoReady = true
	dep := obj("apps/v1", "Deployment", "m", map[string]any{"replicas": int64(1),
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "m"}}}})
	got := apply(t, c, depGVR, dep)
	got, _ = c.Get(depGVR, "podserve", got.GetName())
	if r, _, _ := unstructured.NestedInt64(got.Object, "status", "readyReplicas"); r != 1 {
		t.Errorf("AutoReady: readyReplicas %d", r)
	}
	job := obj("batch/v1", "Job", "podserve-fetch-x", map[string]any{})
	if _, err := c.Resource(gvrJobs).Namespace("podserve").Create(context.Background(), job, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	j, _ := c.Get(gvrJobs, "podserve", "podserve-fetch-x")
	if s, _, _ := unstructured.NestedInt64(j.Object, "status", "succeeded"); s != 1 {
		t.Errorf("AutoReady: job not complete: %v", j.Object["status"])
	}
	if _, err := c.Resource(gvrJobs).Namespace("podserve").Create(context.Background(), job, metav1.CreateOptions{}); !apierrors.IsAlreadyExists(err) {
		t.Errorf("second create: want AlreadyExists, got %v", err)
	}

	c.AutoReady = false
	failing := obj("batch/v1", "Job", "podserve-fetch-y", map[string]any{})
	if _, err := c.Resource(gvrJobs).Namespace("podserve").Create(context.Background(), failing, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := c.FailJob("podserve", "podserve-fetch-y", `{"event":"error","code":"GATED_NO_TOKEN"}`); err != nil {
		t.Fatal(err)
	}
	pods, _ := c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=podserve-fetch-y"})
	if len(pods.Items) != 1 {
		t.Fatalf("FailJob: want 1 pod, got %d", len(pods.Items))
	}
	statuses, _, _ := unstructured.NestedSlice(pods.Items[0].Object, "status", "containerStatuses")
	msg, _, _ := unstructured.NestedString(statuses[0].(map[string]any), "state", "terminated", "message")
	if msg != `{"event":"error","code":"GATED_NO_TOKEN"}` {
		t.Errorf("termination message %q", msg)
	}
	// Like the API server, deleting a Job with no propagation policy orphans its pods...
	if err := c.Resource(gvrJobs).Namespace("podserve").Delete(context.Background(), "podserve-fetch-y", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	pods, _ = c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=podserve-fetch-y"})
	if len(pods.Items) != 1 {
		t.Errorf("a Job deleted without a propagation policy must orphan its pod (the API default); %d pods left", len(pods.Items))
	}
	// ...while Background cascades.
	again := obj("batch/v1", "Job", "podserve-fetch-z", map[string]any{})
	if _, err := c.Resource(gvrJobs).Namespace("podserve").Create(context.Background(), again, metav1.CreateOptions{}); err != nil {
		t.Fatal(err)
	}
	_ = c.FailJob("podserve", "podserve-fetch-z", `{"event":"error","code":"X"}`)
	bg := metav1.DeletePropagationBackground
	if err := c.Resource(gvrJobs).Namespace("podserve").Delete(context.Background(), "podserve-fetch-z", metav1.DeleteOptions{PropagationPolicy: &bg}); err != nil {
		t.Fatal(err)
	}
	pods, _ = c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{LabelSelector: "job-name=podserve-fetch-z"})
	if len(pods.Items) != 0 {
		t.Errorf("a Job deleted with Background propagation must take its pod with it")
	}
}

// D-027's stop: a JSON merge patch on spec.replicas changes nothing else (the template and selector
// survive, unlike a partial server-side apply), removes the pods, and the next forced apply of the
// full Deployment brings them back.
func TestMergePatchStopKeepsTheTemplate(t *testing.T) {
	c := New()
	c.AutoReady = true
	dep := obj("apps/v1", "Deployment", "m", map[string]any{"replicas": int64(1),
		"selector": map[string]any{"matchLabels": map[string]any{"podserve.dev/model": "m"}},
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "m"}}}})
	apply(t, c, depGVR, dep)
	gen := func() int64 { d, _ := c.Get(depGVR, "podserve", "m"); return d.GetGeneration() }
	before := gen()

	if _, err := c.Resource(depGVR).Namespace("podserve").Patch(context.Background(), "m", types.MergePatchType,
		[]byte(`{"spec":{"replicas":0}}`), metav1.PatchOptions{}); err != nil {
		t.Fatal(err)
	}
	got, _ := c.Get(depGVR, "podserve", "m")
	if r, _, _ := unstructured.NestedInt64(got.Object, "spec", "replicas"); r != 0 {
		t.Errorf("replicas %d after the stop", r)
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "template"); !found {
		t.Error("the merge patch dropped spec.template")
	}
	if _, found, _ := unstructured.NestedMap(got.Object, "spec", "selector"); !found {
		t.Error("the merge patch dropped spec.selector")
	}
	if got.GetGeneration() != before+1 {
		t.Errorf("generation %d, want %d", got.GetGeneration(), before+1)
	}
	pods, _ := c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 0 {
		t.Errorf("%d pods left after scaling to zero", len(pods.Items))
	}

	apply(t, c, depGVR, dep) // the next podserve deploy
	pods, _ = c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Errorf("%d pods after the re-apply, want 1", len(pods.Items))
	}
}

// Pods belong to a ReplicaSet per pod template, as on GKE; a new template replaces the old pods.
func TestReplicaSetsFollowTheTemplate(t *testing.T) {
	c := New()
	c.AutoReady = true
	mk := func(arg string) *unstructured.Unstructured {
		return obj("apps/v1", "Deployment", "m", map[string]any{"replicas": int64(1),
			"template": map[string]any{
				"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "m"}},
				"spec":     map[string]any{"containers": []any{map[string]any{"name": "vllm", "args": []any{arg}}}}}})
	}
	apply(t, c, depGVR, mk("--a"))
	apply(t, c, depGVR, mk("--b"))
	pods, _ := c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	if len(pods.Items) != 1 {
		t.Fatalf("%d pods, want 1 (the old template's pod goes)", len(pods.Items))
	}
	rss, _ := c.Resource(gvrReplicaSets).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	if len(rss.Items) != 2 {
		t.Fatalf("%d ReplicaSets, want 2", len(rss.Items))
	}
	newest := ""
	for _, rs := range rss.Items {
		if rs.GetAnnotations()["deployment.kubernetes.io/revision"] == "2" {
			newest = rs.GetLabels()["pod-template-hash"]
		}
	}
	if h := pods.Items[0].GetLabels()["pod-template-hash"]; h == "" || h != newest {
		t.Errorf("pod hash %q, newest ReplicaSet's %q", h, newest)
	}
	d, _ := c.Get(depGVR, "podserve", "m")
	if og, _, _ := unstructured.NestedInt64(d.Object, "status", "observedGeneration"); og != d.GetGeneration() {
		t.Errorf("observedGeneration %d, generation %d", og, d.GetGeneration())
	}
}

// Recorded GKE states land on the fake's own pods: status copied, events re-pointed, integers int64.
func TestSetServingPodsFromARecording(t *testing.T) {
	c := New()
	dep := obj("apps/v1", "Deployment", "qwen3-8b", map[string]any{"replicas": int64(1),
		"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{"podserve.dev/model": "qwen3-8b"}}}})
	apply(t, c, depGVR, dep)
	if err := c.SetServingPodsFrom("podserve", "qwen3-8b", "../../testdata/kube/failures/crashloop"); err != nil {
		t.Fatal(err)
	}
	pods, _ := c.Resource(gvrPods).Namespace("podserve").List(context.Background(), metav1.ListOptions{LabelSelector: "podserve.dev/model=qwen3-8b"})
	if len(pods.Items) != 1 {
		t.Fatalf("%d pods, want 1", len(pods.Items))
	}
	p := pods.Items[0]
	if p.GetLabels()["pod-template-hash"] == "" {
		t.Error("the pod lacks the current template's pod-template-hash")
	}
	statuses, _, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
	cs := statuses[0].(map[string]any)
	if r, _, _ := unstructured.NestedString(cs, "state", "waiting", "reason"); r != "CrashLoopBackOff" {
		t.Errorf("waiting reason %q", r)
	}
	if code, found, _ := unstructured.NestedInt64(cs, "lastState", "terminated", "exitCode"); !found || code != 2 {
		t.Errorf("exit code %d (found %v), want int64 2", code, found)
	}
	events, _ := c.Resource(schema.GroupVersionResource{Version: "v1", Resource: "events"}).Namespace("podserve").List(context.Background(), metav1.ListOptions{})
	backoff := false
	for _, e := range events.Items {
		name, _, _ := unstructured.NestedString(e.Object, "involvedObject", "name")
		if e.Object["reason"] == "BackOff" && name == p.GetName() {
			backoff = true
		}
	}
	if !backoff {
		t.Error("the recorded BackOff event isn't pointed at the fake's pod")
	}
}
