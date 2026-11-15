package kube_test

// Spine test (P01 T2), parked under testdata/spine/kube/. T10a moves it to pkg/kube/ with `git mv`,
// unchanged. It drives pkg/kube against internal/fakekube (UIDs, server-side apply, the /status
// boundary, owner-reference garbage collection) with a fake clock whose Sleep advances the cluster.

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	k8stesting "k8s.io/client-go/testing"

	"github.com/podserve/podserve/config/crd"
	"github.com/podserve/podserve/internal/fakekube"
	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/kube"
	"github.com/podserve/podserve/pkg/platform"
)

const (
	ns  = "podserve"
	sha = "b968826d9c46dd6066d109eabc6255188de91218"
)

type fakeClock struct {
	now     time.Time
	sleeps  int
	onSleep func(n int)
}

func (f *fakeClock) Now() time.Time { return f.now }
func (f *fakeClock) Sleep(d time.Duration) {
	f.now = f.now.Add(d)
	f.sleeps++
	if f.onSleep != nil {
		f.onSleep(f.sleeps)
	}
}

type harness struct {
	cluster *fakekube.Cluster
	clock   *fakeClock
	stages  []kube.Stage
	client  *kube.Client
}

func newHarness() *harness {
	h := &harness{cluster: fakekube.New(), clock: &fakeClock{now: time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)}}
	h.client = &kube.Client{Dynamic: h.cluster, Options: kube.Options{
		PollInterval: 5 * time.Second, Timeout: time.Minute, Clock: h.clock,
		Progress: func(s kube.Stage) { h.stages = append(h.stages, s) },
	}}
	return h
}

func modelService(name, source string) *v1alpha1.ModelService {
	one, yes := int32(1), true
	ms := &v1alpha1.ModelService{}
	ms.APIVersion, ms.Kind = "podserve.dev/v1alpha1", "ModelService"
	ms.Name, ms.Namespace = name, ns
	ms.Spec = v1alpha1.ModelServiceSpec{
		Model:       v1alpha1.ModelSource{Source: source, Revision: "main"},
		Accelerator: v1alpha1.AcceleratorSpec{Type: "auto", Spot: &yes},
		Engine:      v1alpha1.EngineSpec{GPUMemoryPercent: 90},
		Replicas:    &one,
	}
	return ms
}

func fetchJob(repo, revision string) *batchv1.Job {
	j := &batchv1.Job{}
	j.APIVersion, j.Kind = "batch/v1", "Job"
	j.Name, j.Namespace = platform.FetchJobName(repo, revision), ns
	j.Labels = map[string]string{platform.RevisionLabel: revision, platform.ManagedByLabel: platform.ManagedByValue}
	j.Annotations = map[string]string{"podserve.dev/repo": repo}
	j.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	j.Spec.Template.Spec.Containers = []corev1.Container{{Name: "fetch", Image: "fetch@sha256:1"}}
	return j
}

func children(name string, owner metav1.OwnerReference) []*unstructured.Unstructured {
	mk := func(apiVersion, kind string, spec map[string]any) *unstructured.Unstructured {
		u := &unstructured.Unstructured{Object: map[string]any{"apiVersion": apiVersion, "kind": kind,
			"metadata": map[string]any{"name": name, "namespace": ns,
				"labels": map[string]any{platform.ModelLabel: name}},
			"spec": spec}}
		u.SetOwnerReferences([]metav1.OwnerReference{owner})
		return u
	}
	return []*unstructured.Unstructured{
		mk("apps/v1", "Deployment", map[string]any{"replicas": int64(1),
			"selector": map[string]any{"matchLabels": map[string]any{platform.ModelLabel: name}},
			"template": map[string]any{"metadata": map[string]any{"labels": map[string]any{platform.ModelLabel: name}}}}),
		mk("v1", "Service", map[string]any{"selector": map[string]any{platform.ModelLabel: name}}),
		mk("monitoring.googleapis.com/v1", "PodMonitoring", map[string]any{}),
	}
}

func actions(h *harness, verb, resource string) []k8stesting.Action {
	var out []k8stesting.Action
	for _, a := range h.cluster.Actions() {
		if a.GetVerb() == verb && a.GetResource().Resource == resource {
			out = append(out, a)
		}
	}
	return out
}

func TestApplyRecordAndStatus(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	ref, err := h.client.ApplyRecord(ctx, modelService("qwen3-8b", "hf://Qwen/Qwen3-8B"))
	if err != nil {
		t.Fatal(err)
	}
	if ref.APIVersion != "podserve.dev/v1alpha1" || ref.Kind != "ModelService" || ref.Name != "qwen3-8b" || ref.UID == "" ||
		ref.Controller == nil || !*ref.Controller || ref.BlockOwnerDeletion == nil || !*ref.BlockOwnerDeletion {
		t.Fatalf("owner reference %+v", ref)
	}
	again, err := h.client.ApplyRecord(ctx, modelService("qwen3-8b", "hf://Qwen/Qwen3-8B"))
	if err != nil || again.UID != ref.UID {
		t.Fatalf("re-apply: uid %s -> %s (%v)", ref.UID, again.UID, err)
	}

	st := v1alpha1.ModelServiceStatus{Phase: v1alpha1.PhaseCaching, ResolvedRevision: sha, Message: "fetching"}
	if err := h.client.UpdateStatus(ctx, "qwen3-8b", st); err != nil {
		t.Fatal(err)
	}
	got, err := h.client.Get(ctx, "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if got.Status.Phase != v1alpha1.PhaseCaching || got.Status.ResolvedRevision != sha {
		t.Errorf("status not written: %+v (a status write to the main resource is dropped; use /status)", got.Status)
	}
	var viaStatus bool
	for _, a := range h.cluster.Actions() {
		if a.GetResource().Resource == "modelservices" && a.GetSubresource() == "status" {
			viaStatus = true
		}
	}
	if !viaStatus {
		t.Error("status was not written through the /status subresource")
	}
}

func TestApplyChildrenKeepsOwners(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	ref, err := h.client.ApplyRecord(ctx, modelService("m", "hf://Qwen/Qwen3-8B"))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.client.ApplyChildren(ctx, children("m", ref)...); err != nil {
		t.Fatal(err)
	}
	for _, gvr := range [...]struct {
		name string
		ok   func() (*unstructured.Unstructured, error)
	}{
		{"deployment", func() (*unstructured.Unstructured, error) { return h.cluster.Get(kube.Deployments, ns, "m") }},
		{"service", func() (*unstructured.Unstructured, error) { return h.cluster.Get(kube.Services, ns, "m") }},
		{"podmonitoring", func() (*unstructured.Unstructured, error) { return h.cluster.Get(kube.PodMonitorings, ns, "m") }},
	} {
		u, err := gvr.ok()
		if err != nil {
			t.Fatalf("%s: %v", gvr.name, err)
		}
		if refs := u.GetOwnerReferences(); len(refs) != 1 || refs[0].UID != ref.UID {
			t.Errorf("%s owner references %+v", gvr.name, refs)
		}
	}
	if len(actions(h, "patch", "deployments")) == 0 {
		t.Error("children were not server-side applied (no patch action)")
	}
}

func TestEnsureFetchJob(t *testing.T) {
	ctx := context.Background()
	jobName := platform.FetchJobName("Qwen/Qwen3-8B", sha)

	t.Run("absent: create, wait, copied", func(t *testing.T) {
		h := newHarness()
		h.clock.onSleep = func(n int) {
			if n == 2 {
				_ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done","files":13,"bytes":1,"seconds":1}`)
			}
		}
		out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		if err != nil || out != kube.FetchCopied {
			t.Fatalf("outcome %q err %v", out, err)
		}
		if len(actions(h, "create", "jobs"))+len(actions(h, "patch", "jobs")) != 1 {
			t.Error("want exactly one create or apply of the Job")
		}
		if !reflect.DeepEqual(h.stages, []kube.Stage{kube.StageFetch}) {
			t.Errorf("stages %v, want [fetch]", h.stages)
		}
	})

	t.Run("absent: the job finds the marker", func(t *testing.T) {
		h := newHarness()
		h.clock.onSleep = func(n int) { _ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"cached"}`) }
		out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		if err != nil || out != kube.FetchCached {
			t.Fatalf("outcome %q err %v", out, err)
		}
	})

	t.Run("completed job exists: reused, nothing created", func(t *testing.T) {
		h := newHarness()
		if err := h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha)); err != nil {
			t.Fatal(err)
		}
		_ = h.cluster.CompleteJob(ns, jobName)
		out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		if err != nil || out != kube.FetchReused {
			t.Fatalf("outcome %q err %v", out, err)
		}
		if n := len(actions(h, "create", "jobs")) + len(actions(h, "patch", "jobs")) + len(actions(h, "delete", "jobs")); n != 0 {
			t.Errorf("%d job writes for a reusable job", n)
		}
	})

	t.Run("running job exists: wait for it", func(t *testing.T) {
		h := newHarness()
		_ = h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha))
		_ = h.cluster.RunJob(ns, jobName)
		h.clock.onSleep = func(n int) {
			if n == 3 {
				_ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done"}`)
			}
		}
		out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		if err != nil || out != kube.FetchCopied {
			t.Fatalf("outcome %q err %v", out, err)
		}
		if len(actions(h, "create", "jobs")) != 0 {
			t.Error("created a second job while one was running")
		}
	})

	t.Run("failed job exists: delete, then create again", func(t *testing.T) {
		h := newHarness()
		_ = h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha))
		_ = h.cluster.FailJob(ns, jobName, `{"event":"error","code":"UNEXPECTED"}`)
		h.clock.onSleep = func(n int) { _ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done"}`) }
		out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		if err != nil || out != kube.FetchCopied {
			t.Fatalf("outcome %q err %v", out, err)
		}
		if len(actions(h, "delete", "jobs")) != 1 {
			t.Error("the failed job was not deleted first")
		}
	})

	t.Run("job fails: FetchError with the code", func(t *testing.T) {
		h := newHarness()
		h.clock.onSleep = func(n int) {
			_ = h.cluster.FailJob(ns, jobName, `{"event":"error","code":"GATED_NO_TOKEN"}`)
		}
		_, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		var fe *kube.FetchError
		if !errors.As(err, &fe) || fe.Code != "GATED_NO_TOKEN" {
			t.Fatalf("want FetchError GATED_NO_TOKEN, got %v", err)
		}
	})

	t.Run("job never finishes: timeout", func(t *testing.T) {
		h := newHarness()
		_, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
		var te *kube.TimeoutError
		if !errors.As(err, &te) || te.Stage != kube.StageFetch {
			t.Fatalf("want TimeoutError at fetch, got %v", err)
		}
	})
}

func TestWaitReadyReportsStages(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	ref, _ := h.client.ApplyRecord(ctx, modelService("m", "hf://Qwen/Qwen3-8B"))
	if err := h.client.ApplyChildren(ctx, children("m", ref)...); err != nil {
		t.Fatal(err)
	}
	_ = h.cluster.SetServingPods(ns, "m", fakekube.StageUnscheduled)
	h.clock.onSleep = func(n int) {
		switch n {
		case 2:
			_ = h.cluster.SetServingPods(ns, "m", fakekube.StagePulling)
		case 3:
			_ = h.cluster.SetServingPods(ns, "m", fakekube.StageLoading)
		case 6:
			_ = h.cluster.SetServingPods(ns, "m", fakekube.StageReady)
		}
	}
	if err := h.client.WaitReady(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	want := []kube.Stage{kube.StageNode, kube.StageImage, kube.StageLoading, kube.StageReady}
	if !reflect.DeepEqual(h.stages, want) {
		t.Errorf("stages %v, want %v", h.stages, want)
	}
}

func TestWaitReadyTimesOut(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	ref, _ := h.client.ApplyRecord(ctx, modelService("m", "hf://Qwen/Qwen3-8B"))
	_ = h.client.ApplyChildren(ctx, children("m", ref)...)
	_ = h.cluster.SetServingPods(ns, "m", fakekube.StageLoading)
	err := h.client.WaitReady(ctx, "m")
	var te *kube.TimeoutError
	if !errors.As(err, &te) || te.Stage != kube.StageLoading || te.After != time.Minute {
		t.Fatalf("want TimeoutError{loading, 1m}, got %v", err)
	}
	if h.clock.sleeps > 13 {
		t.Errorf("slept %d times for a 1m timeout at 5s intervals", h.clock.sleeps)
	}
}

func TestDelete(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	jobName := platform.FetchJobName("Qwen/Qwen3-8B", sha)
	for _, name := range []string{"a", "b"} {
		ref, err := h.client.ApplyRecord(ctx, modelService(name, "hf://Qwen/Qwen3-8B"))
		if err != nil {
			t.Fatal(err)
		}
		_ = h.client.UpdateStatus(ctx, name, v1alpha1.ModelServiceStatus{Phase: v1alpha1.PhaseReady, ResolvedRevision: sha})
		_ = h.client.ApplyChildren(ctx, children(name, ref)...)
		_ = h.cluster.SetServingPods(ns, name, fakekube.StageReady)
	}
	_ = h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha))
	_ = h.cluster.CompleteJob(ns, jobName)

	if err := h.client.Delete(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.client.Get(ctx, "a"); !errors.Is(err, kube.ErrNotFound) {
		t.Errorf("a still there: %v", err)
	}
	if _, err := h.cluster.Get(kube.Deployments, ns, "a"); !apierrors.IsNotFound(err) {
		t.Errorf("a's deployment survived: %v", err)
	}
	if _, err := h.cluster.Get(kube.Jobs, ns, jobName); err != nil {
		t.Errorf("fetch job deleted while b still uses the revision: %v", err)
	}

	if err := h.client.Delete(ctx, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.cluster.Get(kube.Jobs, ns, jobName); !apierrors.IsNotFound(err) {
		t.Errorf("fetch job kept after its last user was deleted: %v", err)
	}
	if err := h.client.Delete(ctx, "nope"); !errors.Is(err, kube.ErrNotFound) {
		t.Errorf("delete unknown: want ErrNotFound, got %v", err)
	}
}

func TestListGetPlatformInstall(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	if _, err := h.client.PlatformData(ctx); !errors.Is(err, kube.ErrPlatformNotInitialised) {
		t.Errorf("PlatformData before init: %v", err)
	}
	data := map[string]string{"project": "p", "region": "r", "bucket": "b", "vllmImage": "v", "fetchImage": "f", "hfTokenSecret": "s"}
	if err := h.client.Install(ctx, crd.ModelServicesYAML, data); err != nil {
		t.Fatal(err)
	}
	got, err := h.client.PlatformData(ctx)
	if err != nil || !reflect.DeepEqual(got, data) {
		t.Errorf("PlatformData %v %v", got, err)
	}
	if _, err := h.cluster.Get(kube.CRDs, "", "modelservices.podserve.dev"); err != nil {
		t.Errorf("CRD not installed: %v", err)
	}
	if _, err := h.cluster.Get(kube.Namespaces, "", ns); err != nil {
		t.Errorf("namespace not created: %v", err)
	}
	for _, sa := range []string{platform.FetchSA, platform.ModelSA} {
		if _, err := h.cluster.Get(kube.ServiceAccts, ns, sa); err != nil {
			t.Errorf("service account %s: %v", sa, err)
		}
	}

	for _, name := range []string{"zeta", "alpha"} {
		if _, err := h.client.ApplyRecord(ctx, modelService(name, "hf://Qwen/Qwen3-8B")); err != nil {
			t.Fatal(err)
		}
	}
	list, err := h.client.List(ctx)
	if err != nil || len(list) != 2 || list[0].Name != "alpha" || list[1].Name != "zeta" {
		t.Errorf("List: %v %v", list, err)
	}
	if _, err := h.client.Get(ctx, "nope"); !errors.Is(err, kube.ErrNotFound) {
		t.Errorf("Get unknown: %v", err)
	}
}
