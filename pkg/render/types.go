// Package render turns a ModelService, its chosen profile and the platform settings into the
// Kubernetes objects podserve applies. Pure functions: no cluster access, no UID lookups (D-021's
// owner reference arrives as an input from pkg/kube, via the CLI). Spine file (P01 T1): types only;
// the functions are in render.go and fetch.go (T6). The exact objects are pinned by
// testdata/render/*.golden.yaml, compared as parsed objects.
package render

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/platform"
	"github.com/podserve/podserve/pkg/profile"
)

// Input is everything one ModelService renders from.
type Input struct {
	// Service is the ModelService, defaulted and validated (its Name is the object name).
	Service *v1alpha1.ModelService
	// Repo is ORG/MODEL; Revision the resolved commit SHA.
	Repo     string
	Revision string
	Profile  profile.Result
	Platform platform.Platform
	// LargestFetchFileBytes sizes the fetch Job's volume (hf.LargestFetchFile).
	LargestFetchFileBytes int64
}

// Fetch Job sizing (docs/design/platform.md): the volume holds one file at a time.
const (
	FetchVolumeMarginBytes = 2 << 30  // + 2 GiB on top of the largest file
	FetchVolumeMinBytes    = 10 << 30 // never below 10 GiB
)

// Owner is the owner reference the serving objects carry: the applied ModelService record.
type Owner = metav1.OwnerReference
