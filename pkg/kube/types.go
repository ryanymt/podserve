// Package kube is podserve's cluster calls, through the dynamic client: apply the ModelService
// record and return its owner reference, write status through /status, server-side apply the
// serving objects, manage the per-revision fetch Job, wait for Ready, delete, and list. It never
// imports pkg/render; the CLI composes the two (D-021). Spine file (P01 T1): types only; the
// methods are in kube.go (T10a), pinned by testdata/spine/kube/kube_test.go on internal/fakekube.
package kube

import (
	"context"
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Resources podserve touches.
var (
	ModelServices  = schema.GroupVersionResource{Group: "podserve.dev", Version: "v1alpha1", Resource: "modelservices"}
	Deployments    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	ReplicaSets    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
	Events         = schema.GroupVersionResource{Version: "v1", Resource: "events"}
	Services       = schema.GroupVersionResource{Version: "v1", Resource: "services"}
	Jobs           = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	Pods           = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	ConfigMaps     = schema.GroupVersionResource{Version: "v1", Resource: "configmaps"}
	Namespaces     = schema.GroupVersionResource{Version: "v1", Resource: "namespaces"}
	ServiceAccts   = schema.GroupVersionResource{Version: "v1", Resource: "serviceaccounts"}
	PodMonitorings = schema.GroupVersionResource{Group: "monitoring.googleapis.com", Version: "v1", Resource: "podmonitorings"}
	CRDs           = schema.GroupVersionResource{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}
)

// Stage is a step of `podserve deploy`'s wait, printed as it is reached (docs/design/cli.md).
type Stage string

const (
	StageFetch   Stage = "fetch"   // fetch Job running
	StageNode    Stage = "node"    // serving pod waiting for a node (Autopilot provisioning)
	StageImage   Stage = "image"   // pulling the vLLM image
	StageLoading Stage = "loading" // container started, not yet Ready (weights + CUDA graphs)
	StageReady   Stage = "ready"
)

// Clock is injected so tests run in milliseconds.
type Clock interface {
	Now() time.Time
	Sleep(d time.Duration)
}

// Options tune waiting. Zero values mean the defaults below.
type Options struct {
	PollInterval time.Duration // default 5s
	Timeout      time.Duration // default 30m (an Autopilot GPU cold start can take 7-12 min)
	Clock        Clock         // default: the real clock
	// Progress is called each time the stage changes: once per stage, in order, on a normal start,
	// and again for an earlier stage when the pods fall back to it (a pod replaced after its spot
	// node was reclaimed). May be nil.
	Progress func(Stage)
	// Reason is called while WaitReady waits, each time the waiting reason or its message changes,
	// and for the first reason after each change of stage; the same reason and message don't repeat
	// while they stay current. Never for a failure, which WaitReady returns. May be nil.
	Reason func(Diagnosis)
}

const (
	DefaultPollInterval = 5 * time.Second
	DefaultTimeout      = 30 * time.Minute
)

// Client is bound to the podserve namespace.
type Client struct {
	Dynamic dynamic.Interface
	Options Options
	// Logs reads a crash-looping container's previous log (D-029). May be nil: the failure is then
	// reported without log lines.
	Logs LogReader
}

// LogReader reads the last lines of a container's previous run (D-029):
// GET /api/v1/namespaces/podserve/pods/<POD>/log?container=<C>&previous=true&tailLines=<N>.
type LogReader interface {
	PreviousLog(ctx context.Context, pod, container string, lines int64) ([]string, error)
}

// Reason says why a model is failing to start or still waiting (D-026). The values are Kubernetes'
// and Autopilot's own reason strings, so a user can search for them.
type Reason string

const (
	// Failures: the deploy stops, scales the model to zero and exits 7 (D-027, D-028).
	ReasonCrashLoop       Reason = "CrashLoopBackOff"
	ReasonImagePull       Reason = "ImagePullBackOff" // ErrImagePull or ImagePullBackOff for ImagePullGrace or longer
	ReasonInvalidImage    Reason = "InvalidImageName"
	ReasonCreateConfig    Reason = "CreateContainerConfigError"
	ReasonCreateContainer Reason = "CreateContainerError"

	// Waiting: reported once each through Options.Reason; the deploy keeps waiting.
	ReasonPullRetry     Reason = "ErrImagePull"         // an image pull failing for less than ImagePullGrace
	ReasonScaleUp       Reason = "TriggeredScaleUp"     // Autopilot is adding a node
	ReasonScaleUpFailed Reason = "FailedScaleUp"        // Autopilot tried to add a node and couldn't
	ReasonNoScaleUp     Reason = "NotTriggerScaleUp"    // Autopilot won't add a node for this pod
	ReasonReclaimed     Reason = "TerminationByKubelet" // a pod ended before Ready (spot reclaim); its ReplicaSet starts another
)

// ImagePullGrace is how long an image pull may keep failing before the deploy fails: a cold pull
// through the Artifact Registry mirror can fail once and then succeed (D-026).
const ImagePullGrace = 2 * time.Minute

// Serving is one model's serving objects as read from the cluster: the input to Diagnose.
type Serving struct {
	Deployment  *unstructured.Unstructured  // nil when there is none (yet)
	ReplicaSets []unstructured.Unstructured // owned by the Deployment
	Pods        []unstructured.Unstructured // labelled podserve.dev/model=<NAME>
	Events      []unstructured.Unstructured // the namespace's events; Diagnose keeps those about Pods
}

// Diagnosis is where a model's serving pods are and why (D-025). Diagnose (classify.go) computes it,
// purely, from a Serving and the time; deploy, status and the v0.2 operator all use it.
type Diagnosis struct {
	Stage   Stage  // node, image or loading; ready when Ready
	Ready   bool   // every replica of the newest pod template is Ready (D-036)
	Failed  bool   // Reason is a failure
	Reason  Reason // "" when there is nothing to report
	Message string // Kubernetes' message for Reason, whitespace collapsed to one line
	Pod     string // the pod Reason is about, "" for none
	// Last is the crash-looping container's last termination (CrashLoopBackOff only).
	Last *Termination
}

// Termination is how a container's previous run ended.
type Termination struct {
	Reason   string // e.g. "Error", "OOMKilled"
	ExitCode int32
}

// FailedError is returned by WaitReady when the model can't start (Diagnosis.Failed). Log holds the
// last lines of the container's previous run for a crash loop; LogErr says why there are none.
type FailedError struct {
	Diagnosis Diagnosis
	Log       []string
	LogErr    error
}

func (e *FailedError) Error() string {
	return fmt.Sprintf("failed to start: %s: %s", e.Diagnosis.Reason, e.Diagnosis.Message)
}

// Errors the CLI maps to exit codes (docs/design/cli.md).
var (
	// ErrNotFound: no ModelService with that name in the podserve namespace.
	ErrNotFound = errors.New("model service not found")
	// ErrPlatformNotInitialised: the podserve/podserve-platform ConfigMap doesn't exist.
	ErrPlatformNotInitialised = errors.New("platform not initialised: ConfigMap podserve/podserve-platform not found; run podserve init")
)

// FetchError is returned when the fetch Job failed. Code is the "code" of the JSON error event the
// fetch job wrote to its termination log (platform.md), or "UNKNOWN" when there is none.
type FetchError struct{ Code string }

func (e *FetchError) Error() string { return "fetch job failed: " + e.Code }

// TimeoutError is returned when a wait runs out. Stage is the last stage reached; Reason and
// Message are the last waiting reason seen, if any.
type TimeoutError struct {
	Stage   Stage
	After   time.Duration
	Reason  Reason
	Message string
}

func (e *TimeoutError) Error() string {
	return fmt.Sprintf("not ready after %s (stage: %s)", e.After, e.Stage)
}

// FetchOutcome says how the weights got (or already were) into GCS.
type FetchOutcome string

const (
	FetchReused FetchOutcome = "reused" // a completed fetch Job for the revision already existed
	FetchCached FetchOutcome = "cached" // the Job ran and found the GCS completion marker
	FetchCopied FetchOutcome = "copied" // the Job ran and copied the files
)
