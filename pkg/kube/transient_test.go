package kube_test

// Live fix (P02 T9): the poll loops wait through a transient API error instead of failing on it.
// Live, one 503 while listing ReplicaSets ended a 17-minute Qwen3-14B deploy with exit 1.

import (
	"context"
	"errors"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stesting "k8s.io/client-go/testing"

	"github.com/ryanymt/podserve/pkg/kube"
	"github.com/ryanymt/podserve/pkg/platform"
)

// failOnce makes the first n calls of verb on resource fail with err.
func failOnce(h *harness, verb, resource string, n int, err error) *int {
	calls := 0
	h.cluster.PrependReactor(verb, resource, func(k8stesting.Action) (bool, runtime.Object, error) {
		calls++
		if calls <= n {
			return true, nil, err
		}
		return false, nil, nil
	})
	return &calls
}

var unavailable = apierrors.NewServiceUnavailable("the server is currently unable to handle the request")

func TestWaitReadyWaitsThroughATransientError(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	_ = h.cluster.SetServingPodsFrom(ns, "qwen3-8b", recorded("ready"))
	calls := failOnce(h, "list", "replicasets", 2, unavailable)
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); err != nil {
		t.Fatalf("got %v, want ready after the API server recovered", err)
	}
	if *calls < 3 {
		t.Errorf("replicasets listed %d times, want the two failures and a success", *calls)
	}
}

func TestWaitReadyFailsOnARealError(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "apps", Resource: "replicasets"}, "", errors.New("no"))
	failOnce(h, "list", "replicasets", 1, forbidden)
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); !apierrors.IsForbidden(err) {
		t.Errorf("got %v, want the Forbidden error at once", err)
	}
}

// A transient error that never clears ends in the ordinary timeout, not in the error.
func TestWaitReadyTimesOutThroughPersistentUnavailability(t *testing.T) {
	h := newHarness()
	servingDeployment(t, h, "qwen3-8b")
	failOnce(h, "list", "replicasets", 1000, unavailable)
	var te *kube.TimeoutError
	if err := h.client.WaitReady(context.Background(), "qwen3-8b"); !errors.As(err, &te) {
		t.Errorf("got %v, want *kube.TimeoutError", err)
	}
}

// The fetch Job's wait does the same: a 503 while the Job runs doesn't fail the deploy.
func TestFetchWaitWaitsThroughATransientError(t *testing.T) {
	h := newHarness()
	jobName := platform.FetchJobName("Qwen/Qwen3-8B", sha)
	gets := 0
	h.cluster.PrependReactor("get", "jobs", func(k8stesting.Action) (bool, runtime.Object, error) {
		gets++
		if gets == 2 { // the first get finds no Job; the second, inside the wait, fails
			return true, nil, unavailable
		}
		return false, nil, nil
	})
	h.clock.onSleep = func(n int) {
		if n == 2 {
			_ = h.cluster.CompleteJobWithMessage(ns, jobName, `{"event":"done","files":13,"bytes":1,"seconds":1}`)
		}
	}
	out, err := h.client.EnsureFetchJob(context.Background(), fetchJob("Qwen/Qwen3-8B", sha))
	if err != nil || out != kube.FetchCopied {
		t.Fatalf("outcome %q err %v, want copied after the API server recovered", out, err)
	}
}

// And Delete's wait for the pods to go.
func TestDeleteWaitsThroughATransientError(t *testing.T) {
	h := newHarness()
	ctx := context.Background()
	if _, err := h.client.ApplyRecord(ctx, modelService("qwen3-8b", "hf://Qwen/Qwen3-8B")); err != nil {
		t.Fatal(err)
	}
	failOnce(h, "list", "pods", 1, unavailable)
	if err := h.client.Delete(ctx, "qwen3-8b"); err != nil {
		t.Errorf("got %v, want the delete to finish after the API server recovered", err)
	}
}
