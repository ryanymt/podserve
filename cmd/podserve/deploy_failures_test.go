package main

// Parked spine test (P02 T2) for T4: `git mv` it to cmd/podserve/deploy_failures_test.go, unchanged.
// `podserve deploy` when a model can't start or times out, and its waiting-reason lines, with pod
// states recorded live on GKE Autopilot (testdata/kube/failures/, applied with
// fakekube.SetServingPodsFrom). Contract: docs/design/cli.md (`podserve deploy`: "Waiting reasons",
// "A model that can't start", "Timeout"), D-026, D-027, D-028. It replaces P01's timeout subtest of
// TestDeployFailures, whose expected output changed. Helpers here are prefixed p02 so they don't
// collide with other parked tests in this package.

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/internal/fakekube"
	"github.com/ryanymt/podserve/pkg/kube"
	"github.com/ryanymt/podserve/pkg/platform"
)

// p02Logs is a fake kube.LogReader.
type p02Logs struct {
	lines []string
	err   error
}

func (l *p02Logs) PreviousLog(context.Context, string, string, int64) ([]string, error) {
	return l.lines, l.err
}

func p02Recorded(t *testing.T, state string) string {
	return filepath.Join(testdataDir(t), "kube", "failures", state)
}

// p02Deploy runs `podserve deploy hf://Qwen/Qwen3-8B` with the fetch completing at once and the
// serving pods taking, on each poll, the next recorded state in states (the last one stays).
func p02Deploy(t *testing.T, e *env, states []string, args ...string) int {
	t.Helper()
	e.cluster.AutoReady = false
	e.advance = true
	job := platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")
	next := 0
	e.onSleep = func(int) {
		if _, err := e.cluster.Get(kube.Jobs, "podserve", job); err == nil {
			_ = e.cluster.CompleteJob("podserve", job)
		}
		if _, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b"); err != nil {
			return
		}
		if next < len(states) {
			if err := e.cluster.SetServingPodsFrom("podserve", "qwen3-8b", p02Recorded(t, states[next])); err != nil {
				t.Fatal(err)
			}
			next++
		}
	}
	return e.run(append([]string{"deploy", "hf://Qwen/Qwen3-8B"}, args...)...)
}

// p02Stopped checks the model was stopped (D-027): Failed with message, replicas 0, template kept.
func p02Stopped(t *testing.T, e *env, message string) {
	t.Helper()
	ms, err := e.cluster.Get(kube.ModelServices, "podserve", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	phase, _, _ := unstructured.NestedString(ms.Object, "status", "phase")
	msg, _, _ := unstructured.NestedString(ms.Object, "status", "message")
	if phase != "Failed" || msg != message {
		t.Errorf("status %s %q, want Failed %q", phase, msg, message)
	}
	dep, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if r, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); r != 0 {
		t.Errorf("replicas %d, want 0", r)
	}
	if _, found, _ := unstructured.NestedMap(dep.Object, "spec", "template"); !found {
		t.Error("the stop dropped the pod template")
	}
}

const p02CrashMessage = "back-off 20s restarting failed container=vllm pod=qwen3-8b-745f4d9766-rtxmk_podserve(5d12aaf2-77a3-47c5-8aec-6845ee7f3f38)"

func TestDeployFailsOnACrashLoop(t *testing.T) {
	e := newEnv(t, false)
	e.logs = &p02Logs{lines: []string{"usage: vllm [-h] [-v]", "vllm: error: unrecognized arguments: --no-such-flag"}}
	code := p02Deploy(t, e, []string{"crashloop"})
	want := "podserve: qwen3-8b failed to start: CrashLoopBackOff: " + p02CrashMessage + "\n" +
		"  last run: exit code 2 (Error)\n" +
		"  last lines of the vllm container's log:\n" +
		"    usage: vllm [-h] [-v]\n" +
		"    vllm: error: unrecognized arguments: --no-such-flag\n" +
		"podserve: stopped qwen3-8b (replicas 0) so its GPU is released; fix it and run podserve deploy again, or podserve delete qwen3-8b\n"
	if code != 7 || e.stderr.String() != want {
		t.Errorf("exit %d stderr\n%s\nwant exit 7 and\n%s", code, e.stderr.String(), want)
	}
	p02Stopped(t, e, "CrashLoopBackOff: "+p02CrashMessage)
	if strings.Contains(e.stdout.String(), "ready") {
		t.Errorf("stdout claims ready:\n%s", e.stdout.String())
	}
}

func TestDeployCrashLoopWithoutItsLog(t *testing.T) {
	e := newEnv(t, false)
	e.logs = &p02Logs{err: errors.New(`previous terminated container "vllm" not found`)}
	code := p02Deploy(t, e, []string{"crashloop"})
	want := "podserve: qwen3-8b failed to start: CrashLoopBackOff: " + p02CrashMessage + "\n" +
		"  last run: exit code 2 (Error)\n" +
		"  vllm container's log unavailable: previous terminated container \"vllm\" not found\n" +
		"podserve: stopped qwen3-8b (replicas 0) so its GPU is released; fix it and run podserve deploy again, or podserve delete qwen3-8b\n"
	if code != 7 || e.stderr.String() != want {
		t.Errorf("exit %d stderr\n%s\nwant exit 7 and\n%s", code, e.stderr.String(), want)
	}
}

// P01's timeout case, with v0.1.1's behaviour: the model is stopped and status points at the record.
func TestDeployTimeoutStopsTheModel(t *testing.T) {
	e := newEnv(t, false)
	e.cluster.AutoReady = false
	e.advance = true
	job := platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")
	e.onSleep = func(int) {
		if _, err := e.cluster.Get(kube.Jobs, "podserve", job); err == nil {
			_ = e.cluster.CompleteJob("podserve", job)
		}
		if _, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b"); err == nil {
			_ = e.cluster.SetServingPods("podserve", "qwen3-8b", fakekube.StageLoading)
		}
	}
	code := e.run("deploy", "hf://Qwen/Qwen3-8B", "--timeout", "2m")
	want := "podserve: qwen3-8b not ready after 2m0s (stage: loading)\n" +
		"podserve: stopped qwen3-8b (replicas 0) so its GPU is released; see: podserve status qwen3-8b\n"
	if code != 5 || e.stderr.String() != want {
		t.Errorf("exit %d stderr %q\nwant %q", code, e.stderr.String(), want)
	}
	if !strings.Contains(e.stdout.String(), "] loading   loading weights and warming up\n") {
		t.Errorf("no loading stage line:\n%s", e.stdout.String())
	}
	p02Stopped(t, e, "not ready after 2m0s (stage: loading)")
}

func TestDeployTimeoutNamesTheLastReason(t *testing.T) {
	e := newEnv(t, false)
	code := p02Deploy(t, e, []string{"scaleup"}, "--timeout", "2m")
	want := "podserve: qwen3-8b not ready after 2m0s (stage: node); last reason: TriggeredScaleUp: Autopilot is adding a node\n" +
		"podserve: stopped qwen3-8b (replicas 0) so its GPU is released; see: podserve status qwen3-8b\n"
	if code != 5 || e.stderr.String() != want {
		t.Errorf("exit %d stderr %q\nwant %q", code, e.stderr.String(), want)
	}
	if n := strings.Count(e.stdout.String(), "] node      TriggeredScaleUp: Autopilot is adding a node\n"); n != 1 {
		t.Errorf("the TriggeredScaleUp line printed %d times, want once:\n%s", n, e.stdout.String())
	}
	p02Stopped(t, e, "not ready after 2m0s (stage: node); last reason: TriggeredScaleUp: Autopilot is adding a node")
}

// A reason line prints each time the reason changes, under the stage current at the time, and the
// deploy ends ready. (Until T9's live run this pinned "each reason once per deploy".)
func TestDeployPrintsEachChangeOfReason(t *testing.T) {
	e := newEnv(t, false)
	code := p02Deploy(t, e, []string{"scaleup", "reclaimed", "scaleup", "loading", "ready"})
	out := e.stdout.String()
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	p02InOrder(t, out, []string{
		"] node      waiting for a GPU node (1x nvidia-l4, spot)\n",
		"] node      TriggeredScaleUp: Autopilot is adding a node\n",
		"] node      TerminationByKubelet: Pod was terminated in response to imminent node shutdown.\n",
		"] node      TriggeredScaleUp: Autopilot is adding a node\n",
		"] loading   loading weights and warming up\n",
		"] ready     qwen3-8b is ready\n",
	})
	if n := strings.Count(out, "] node      waiting for a GPU node"); n != 1 {
		t.Errorf("the node line printed %d times, want once:\n%s", n, out)
	}
}

// A pod that falls back from loading to waiting for a node (its spot node was reclaimed and the pod
// replaced) prints the node line again, with the reason it now waits for (T9's live run).
func TestDeployPrintsAStageAgainWhenThePodFallsBack(t *testing.T) {
	e := newEnv(t, false)
	code := p02Deploy(t, e, []string{"scaleup", "loading", "quota", "quota", "loading", "ready"})
	out := e.stdout.String()
	if code != 0 {
		t.Fatalf("exit %d, stderr %s", code, e.stderr.String())
	}
	p02InOrder(t, out, []string{
		"] node      waiting for a GPU node (1x nvidia-l4, spot)\n",
		"] node      TriggeredScaleUp: Autopilot is adding a node\n",
		"] loading   loading weights and warming up\n",
		"] node      waiting for a GPU node (1x nvidia-l4, spot)\n",
		"] node      NotTriggerScaleUp: Pod didn't trigger scale-up: 4 node(s) didn't match Pod's node affinity/selector, 3 in backoff after failed scale-up\n",
		"] loading   loading weights and warming up\n",
		"] ready     qwen3-8b is ready\n",
	})
	if n := strings.Count(out, "NotTriggerScaleUp"); n != 1 {
		t.Errorf("NotTriggerScaleUp printed %d times, want once:\n%s", n, out)
	}
}

// p02InOrder checks that each line appears in out, after the one before it.
func p02InOrder(t *testing.T, out string, lines []string) {
	t.Helper()
	at := 0
	for _, l := range lines {
		i := strings.Index(out[at:], l)
		if i < 0 {
			t.Fatalf("missing, or out of order: %q\nstdout:\n%s", l, out)
		}
		at += i + len(l)
	}
	if rest := out[at:]; strings.Contains(rest, "] node ") || strings.Contains(rest, "] loading ") {
		t.Errorf("unexpected lines after the last one expected:\n%s", rest)
	}
}

// Deploying a stopped model again brings its replicas back from the spec.
func TestRedeployAfterAStop(t *testing.T) {
	e := newEnv(t, false)
	if code := p02Deploy(t, e, []string{"crashloop"}); code != 7 {
		t.Fatalf("setup: exit %d, want 7", code)
	}
	e.onSleep = nil
	e.cluster.AutoReady = true
	if code := e.run("deploy", "hf://Qwen/Qwen3-8B"); code != 0 {
		t.Fatalf("redeploy: exit %d, stderr %s", code, e.stderr.String())
	}
	dep, _ := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b")
	if r, _, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas"); r != 1 {
		t.Errorf("replicas %d after the redeploy, want 1", r)
	}
}

// A reason line is cut at 200 characters (199 plus "…"); the timeout error carries the whole message.
func TestDeployCutsLongReasonLines(t *testing.T) {
	const pull = "Failed to pull image \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": rpc error: code = NotFound desc = failed to pull and unpack image \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": failed to resolve reference \"us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001\": us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000001: not found"
	e := newEnv(t, false)
	code := p02Deploy(t, e, []string{"imagepull-early"}, "--timeout", "1m")
	line := "] image     ErrImagePull: " + string([]rune(pull)[:199]) + "…\n"
	if !strings.Contains(e.stdout.String(), line) {
		t.Errorf("stdout lacks the cut reason line %q:\n%s", line, e.stdout.String())
	}
	want := "podserve: qwen3-8b not ready after 1m0s (stage: image); last reason: ErrImagePull: " + pull + "\n" +
		"podserve: stopped qwen3-8b (replicas 0) so its GPU is released; see: podserve status qwen3-8b\n"
	if code != 5 || e.stderr.String() != want {
		t.Errorf("exit %d stderr\n%s\nwant exit 5 and\n%s", code, e.stderr.String(), want)
	}
}
