package kube

// fetch.go manages the per-revision fetch Job.

import (
	"context"
	"encoding/json"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"

	"github.com/podserve/podserve/pkg/platform"
)

// fetchEvent is the fetch Job's final event, written as one JSON line to the pod's termination log
// (platform.md): {"event":"done"|"cached"|...}, with "code" only on errors.
type fetchEvent struct {
	Event string `json:"event"`
	Code  string `json:"code"`
}

// jobPhase says where a fetch Job is. A Job without status yet counts as running.
type jobPhase int

const (
	jobRunning jobPhase = iota
	jobComplete
	jobFailed
)

// phaseOfJob classifies a Job: succeeded (a succeeded count or a Complete condition) wins, then a
// Failed condition.
func phaseOfJob(u *unstructured.Unstructured) jobPhase {
	if conditionStatus(u.Object, "Complete") == string(corev1.ConditionTrue) {
		return jobComplete
	}
	if n, _, _ := unstructured.NestedInt64(u.Object, "status", "succeeded"); n > 0 {
		return jobComplete
	}
	if conditionStatus(u.Object, "Failed") == string(corev1.ConditionTrue) {
		return jobFailed
	}
	return jobRunning
}

// EnsureFetchJob makes sure the revision's fetch Job has completed: a completed Job with this name
// already there -> FetchReused, without creating anything; a running one -> wait for it; a failed
// one -> delete it, then create it again; none -> create it. While waiting it reports StageFetch
// once. When the Job succeeds, the outcome comes from its pod's termination message
// ({"event":"cached"} -> FetchCached, anything else -> FetchCopied). When it fails, the error is a
// *FetchError with the code from the failed pod's termination message.
func (c *Client) EnsureFetchJob(ctx context.Context, job *batchv1.Job) (FetchOutcome, error) {
	clk, poll, timeout := c.waitParams()
	progress := newProgress(c)
	name := job.GetName()

	// First make sure a Job that isn't already finished exists under this name: a completed one is
	// reused as it is, a failed one is deleted so that the next pass can create it again.
	for {
		got, err := c.jobs().Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			u, err := toUnstructured(job)
			if err != nil {
				return "", err
			}
			u.SetAPIVersion("batch/v1")
			u.SetKind("Job")
			if _, err := c.apply(ctx, Jobs, platform.Namespace, u); err != nil {
				return "", err
			}
			break
		}
		if err != nil {
			return "", err
		}
		switch phaseOfJob(got) {
		case jobComplete:
			return FetchReused, nil
		case jobFailed:
			if err := c.jobs().Delete(ctx, name, deleteBackground()); err != nil && !apierrors.IsNotFound(err) {
				return "", err
			}
			continue
		}
		break
	}
	progress.reach(StageFetch)

	// Then wait for the Job to finish; its pod's termination message says how it went.
	start := clk.Now()
	for {
		got, err := c.jobs().Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", err
		}
		switch phaseOfJob(got) {
		case jobComplete:
			ev, err := c.terminationEvent(ctx, got)
			if err != nil {
				return "", err
			}
			if ev != nil && ev.Event == "cached" {
				return FetchCached, nil
			}
			return FetchCopied, nil
		case jobFailed:
			code := "UNKNOWN"
			ev, err := c.terminationEvent(ctx, got)
			if err != nil {
				return "", err
			}
			if ev != nil && ev.Code != "" {
				code = ev.Code
			}
			return "", &FetchError{Code: code}
		}
		if clk.Now().Sub(start) >= timeout {
			return "", &TimeoutError{Stage: StageFetch, After: timeout}
		}
		clk.Sleep(poll)
	}
}

// terminationEvent reads the fetch Job's pod termination message (the dynamic client cannot read
// logs; platform.md), parsed as the fetch job's final event. Only pods owned by this Job (its UID)
// count: an earlier Job of the same name may have left pods behind. Of those, the one that
// terminated last wins. It returns nil when no such pod carries a parsable message.
func (c *Client) terminationEvent(ctx context.Context, job *unstructured.Unstructured) (*fetchEvent, error) {
	pods, err := c.pods().List(ctx, metav1.ListOptions{LabelSelector: "job-name=" + job.GetName()})
	if err != nil {
		return nil, err
	}
	var best *fetchEvent
	var bestAt string
	for i := range pods.Items {
		if !ownedBy(&pods.Items[i], job.GetUID()) {
			continue
		}
		statuses, found, _ := unstructured.NestedSlice(pods.Items[i].Object, "status", "containerStatuses")
		if !found {
			continue
		}
		for _, s := range statuses {
			cs, ok := s.(map[string]any)
			if !ok {
				continue
			}
			terminated, found, _ := unstructured.NestedMap(cs, "state", "terminated")
			if !found {
				continue
			}
			var ev fetchEvent
			if json.Unmarshal([]byte(firstString(terminated, "message")), &ev) != nil {
				continue
			}
			if at := firstString(terminated, "finishedAt"); best == nil || at > bestAt { // RFC 3339 sorts as text
				best, bestAt = &ev, at
			}
		}
	}
	return best, nil
}

// ownedBy reports whether obj has an owner reference to uid.
func ownedBy(obj *unstructured.Unstructured, uid types.UID) bool {
	for _, ref := range obj.GetOwnerReferences() {
		if ref.UID == uid {
			return true
		}
	}
	return false
}

// deleteBackground deletes dependents too. The API default for batch/v1 Jobs is to orphan their pods
// (and with them the fetch Job's volume): found live in P01 T11.
func deleteBackground() metav1.DeleteOptions {
	p := metav1.DeletePropagationBackground
	return metav1.DeleteOptions{PropagationPolicy: &p}
}
