package kube

// wait.go holds the poll loops: wait for the serving pods to be Ready (through Serving + Diagnose,
// so a failure is returned at once with its reason), and wait for a delete's garbage collection.

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ryanymt/podserve/pkg/platform"
)

// WaitReady waits until the ModelService's Deployment (same name) has all replicas Ready, or until
// Options.Timeout (then *TimeoutError with the last stage reached and, when one was seen, the last
// waiting reason and its message). It reports the stage through Options.Progress each time it
// changes, so pods that fall back to an earlier stage report it again, and always reports
// StageReady on success, even when the pods were Ready at the first check. When Diagnose says the
// model can't start (D-026) it returns *FailedError at once: a crash loop with the last lines of the
// vllm container's previous log, when they can be read. The waiting reason is reported through
// Options.Reason each time it or its message changes, and again for the first one after each change
// of stage.
func (c *Client) WaitReady(ctx context.Context, name string) error {
	clk, poll, timeout := c.waitParams()
	progress := newProgress(c)
	start := clk.Now()
	stage, reason, message := StageNode, Reason(""), ""
	// shown is the stage last reported, said the reason and message last reported since then.
	shown, said := Stage(""), ""
	for {
		s, err := c.Serving(ctx, name)
		if err != nil && !transient(err) {
			return err
		}
		if err != nil { // the API server was briefly unavailable: keep waiting, until the timeout
			if clk.Now().Sub(start) >= timeout {
				return &TimeoutError{Stage: stage, After: timeout, Reason: reason, Message: message}
			}
			clk.Sleep(poll)
			continue
		}
		d := Diagnose(s, clk.Now())
		if d.Ready {
			progress.reach(StageReady)
			return nil
		}
		if d.Failed {
			var fe FailedError
			fe.Diagnosis = d
			if d.Reason == ReasonCrashLoop && c.Logs != nil {
				fe.Log, fe.LogErr = c.Logs.PreviousLog(ctx, d.Pod, "vllm", 20)
				if fe.LogErr != nil {
					fe.Log = nil // a log that can't be read is reported, never fatal
				}
			}
			return &fe
		}
		if d.Stage != shown {
			progress.reach(d.Stage)
			shown, said = d.Stage, ""
		}
		if d.Reason != "" {
			if key := string(d.Reason) + "\x00" + d.Message; key != said {
				said = key
				if c.Options.Reason != nil {
					c.Options.Reason(d)
				}
			}
			reason, message = d.Reason, d.Message
		}
		stage = d.Stage
		if clk.Now().Sub(start) >= timeout {
			return &TimeoutError{Stage: stage, After: timeout, Reason: reason, Message: message}
		}
		clk.Sleep(poll)
	}
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
		if err != nil && !transient(err) {
			return err
		}
		if err == nil && len(pods.Items) == 0 {
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
