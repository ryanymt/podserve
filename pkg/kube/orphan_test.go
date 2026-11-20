package kube_test

// Spine test (P01, T11 live fix): found on the live cluster, where `podserve delete` removed the
// fetch Job but the API's default for batch/v1 Jobs orphaned its pod (and the pod's volume), and a
// later deploy then read that stale pod's termination message ("done" instead of "cached").

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
	"github.com/ryanymt/podserve/pkg/kube"
	"github.com/ryanymt/podserve/pkg/platform"
)

func TestDeleteTakesTheFetchJobsPodsWithIt(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	jobName := platform.FetchJobName("Qwen/Qwen3-8B", sha)
	ref, _ := h.client.ApplyRecord(ctx, modelService("m", "hf://Qwen/Qwen3-8B"))
	_ = h.client.UpdateStatus(ctx, "m", readyStatus())
	_ = h.client.ApplyChildren(ctx, children("m", ref)...)
	_ = h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha))
	_ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done"}`)

	if err := h.client.Delete(ctx, "m"); err != nil {
		t.Fatal(err)
	}
	pods, _ := h.cluster.Resource(kube.Pods).Namespace(ns).List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + jobName})
	if len(pods.Items) != 0 {
		t.Errorf("the fetch Job's pod survived podserve delete (Job deleted without Background propagation?)")
	}
}

func TestOutcomeComesFromThisJobsPod(t *testing.T) {
	ctx := context.Background()
	h := newHarness()
	jobName := platform.FetchJobName("Qwen/Qwen3-8B", sha)
	// An earlier Job of the same name left an orphaned pod saying "done".
	_ = h.cluster.Add(kube.Jobs, fetchJob("Qwen/Qwen3-8B", sha))
	_ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done"}`)
	if err := h.cluster.Resource(kube.Jobs).Namespace(ns).Delete(ctx, jobName, metav1.DeleteOptions{}); err != nil {
		t.Fatal(err) // no policy: the API (and fakekube) orphan the pod
	}
	stale, _ := h.cluster.Resource(kube.Pods).Namespace(ns).Get(ctx, jobName+"-pod", metav1.GetOptions{})
	if stale == nil {
		t.Fatal("setup: expected an orphaned pod")
	}
	_ = h.cluster.Resource(kube.Pods).Namespace(ns).Delete(ctx, jobName+"-pod", metav1.DeleteOptions{})
	stale.SetName(jobName + "-old")
	stale.SetResourceVersion("")
	_ = h.cluster.Add(kube.Pods, stale)

	// This run finds the marker.
	h.clock.onSleep = func(int) { _ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"cached"}`) }
	out, err := h.client.EnsureFetchJob(ctx, fetchJob("Qwen/Qwen3-8B", sha))
	if err != nil || out != kube.FetchCached {
		t.Fatalf("outcome %q err %v; want cached (the stale pod's \"done\" must be ignored)", out, err)
	}
}

func readyStatus() v1alpha1.ModelServiceStatus {
	return v1alpha1.ModelServiceStatus{Phase: v1alpha1.PhaseReady, ResolvedRevision: sha}
}
