// Package kube is podserve's cluster calls, through the dynamic client: apply the ModelService
// record and return its owner reference, write status through /status, server-side apply the
// serving objects, manage the per-revision fetch Job, wait for Ready, delete, and list. It never
// imports pkg/render; the CLI composes the two (D-021). Spine file (P01 T1): types only; the
// methods are in kube.go (T10a), pinned by testdata/spine/kube/kube_test.go on internal/fakekube.
package kube

import (
	"errors"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// Resources podserve touches.
var (
	ModelServices  = schema.GroupVersionResource{Group: "podserve.dev", Version: "v1alpha1", Resource: "modelservices"}
	Deployments    = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
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
	// Progress is called once per stage reached, in order. May be nil.
	Progress func(Stage)
}

const (
	DefaultPollInterval = 5 * time.Second
	DefaultTimeout      = 30 * time.Minute
)

// Client is bound to the podserve namespace.
type Client struct {
	Dynamic dynamic.Interface
	Options Options
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

// TimeoutError is returned when a wait runs out. Stage is the last stage reached.
type TimeoutError struct {
	Stage Stage
	After time.Duration
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
