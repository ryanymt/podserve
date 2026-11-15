package fakekube

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
