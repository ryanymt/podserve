package kube

// wait.go holds the poll loops: wait for the serving pods to be Ready, and wait for a delete's
// garbage collection.

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/podserve/podserve/pkg/platform"
)

// WaitReady waits until the ModelService's Deployment (same name) has all replicas Ready, or until
// Options.Timeout (then *TimeoutError with the last stage reached). It reports each stage through
// Options.Progress the first time it observes it, in order, and always reports StageReady on
// success, even when the pods were Ready at the first check. Stages come from the pods labelled
// podserve.dev/model=NAME: PodScheduled False -> node; scheduled, container waiting -> image;
// running, not Ready -> loading; all replicas Ready -> ready.
func (c *Client) WaitReady(ctx context.Context, name string) error {
	clk, poll, timeout := c.waitParams()
	progress := newProgress(c)
	selector := platform.ModelLabel + "=" + name
	start := clk.Now()
	for {
		ready, stage, err := c.servingState(ctx, name, selector)
		if err != nil {
			return err
		}
		if ready {
			progress.reach(StageReady)
			return nil
		}
		progress.reach(stage)
		if clk.Now().Sub(start) >= timeout {
			return &TimeoutError{Stage: stage, After: timeout}
		}
		clk.Sleep(poll)
	}
}

// servingState says whether the Deployment NAME has all its replicas Ready, and if not, which
// stage its pods are waiting at: the earliest across the pods, node when there are none yet.
func (c *Client) servingState(ctx context.Context, name, selector string) (bool, Stage, error) {
	dep, err := c.deployments().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return false, StageNode, err
	}
	replicas := int64(1)
	if n, ok, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); ok && n > 0 {
		replicas = n
	}
	ready, _, _ := unstructured.NestedInt64(dep.Object, "status", "readyReplicas")
	if ready >= replicas {
		return true, StageReady, nil
	}
	pods, err := c.pods().List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return false, StageNode, err
	}
	stage := StageNode // no pods yet: still waiting for a node
	if len(pods.Items) > 0 {
		stage = podStage(&pods.Items[0])
		for i := 1; i < len(pods.Items); i++ {
			if s := podStage(&pods.Items[i]); stageRank[s] < stageRank[stage] {
				stage = s
			}
		}
	}
	return false, stage, nil
}

// stageRank orders the stages by how early in a pod's start-up they are.
var stageRank = map[Stage]int{StageNode: 0, StageImage: 1, StageLoading: 2, StageReady: 3}

// podStage says how far one serving pod has got: node while it has nowhere to run, image while its
// container is waiting to start, loading once it runs (not Ready, or we wouldn't be polling).
func podStage(p *unstructured.Unstructured) Stage {
	if conditionStatus(p.Object, "PodScheduled") != "True" {
		return StageNode
	}
	statuses, found, _ := unstructured.NestedSlice(p.Object, "status", "containerStatuses")
	if !found {
		return StageImage
	}
	for _, s := range statuses {
		cs, ok := s.(map[string]any)
		if !ok {
			continue
		}
		if _, waiting, _ := unstructured.NestedMap(cs, "state", "waiting"); waiting {
			return StageImage
		}
	}
	return StageLoading
}

// Delete deletes the ModelService (its children go with it through owner references), waits until
// its pods are gone, and deletes the revision's finished fetch Job when no other ModelService uses
// that revision.
func (c *Client) Delete(ctx context.Context, name string) error {
	ms, err := c.Get(ctx, name)
	if err != nil {
		return err
	}
	repo, _ := platform.RepoFromSource(ms.Spec.Model.Source)
	revision := ms.Status.ResolvedRevision
	if err := c.modelservices().Delete(ctx, name, deleteBackground()); err != nil {
		return err
	}

	// The Deployment, Service, PodMonitoring and pods go with it through owner references: wait
	// until the garbage collector has taken the pods.
	clk, poll, timeout := c.waitParams()
	selector := platform.ModelLabel + "=" + name
	start := clk.Now()
	for {
		pods, err := c.pods().List(ctx, metav1.ListOptions{LabelSelector: selector})
		if err != nil {
			return err
		}
		if len(pods.Items) == 0 {
			break
		}
		if clk.Now().Sub(start) >= timeout {
			return &TimeoutError{After: timeout}
		}
		clk.Sleep(poll)
	}

	// The fetch Job caches one revision for every ModelService that uses it: only when this was
	// the last one does its Job go too.
	if repo == "" || revision == "" {
		return nil
	}
	others, err := c.List(ctx)
	if err != nil {
		return err
	}
	for i := range others {
		if others[i].Spec.Model.Source == ms.Spec.Model.Source && others[i].Status.ResolvedRevision == revision {
			return nil
		}
	}
	if err := c.jobs().Delete(ctx, platform.FetchJobName(repo, revision), deleteBackground()); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}
