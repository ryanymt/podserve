// Package deps pins every third-party package podserve's build tasks may import, so go.mod and
// go.sum stay complete and workers (who cannot run `go get`) never hit "no required module provides
// package". A brief lists which of these imports a task may use. Adding a dependency is a spine
// change: add the blank import here, run `go mod tidy` with network access, commit both files.
package deps

import (
	// Kubernetes API types and machinery (one v0.35.x line).
	_ "k8s.io/api/apps/v1"
	_ "k8s.io/api/batch/v1"
	_ "k8s.io/api/core/v1"
	_ "k8s.io/apimachinery/pkg/api/errors"
	_ "k8s.io/apimachinery/pkg/api/resource"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1"
	_ "k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	_ "k8s.io/apimachinery/pkg/runtime"
	_ "k8s.io/apimachinery/pkg/runtime/schema"
	_ "k8s.io/apimachinery/pkg/types"
	_ "k8s.io/apimachinery/pkg/util/intstr"
	_ "k8s.io/apimachinery/pkg/util/yaml"
	_ "k8s.io/apimachinery/pkg/watch"

	// CRD schema types, for the CRD-matches-types test.
	_ "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"

	// client-go: dynamic client, discovery, REST mapping, kubeconfig, and the fakes for tests.
	_ "k8s.io/client-go/discovery"
	_ "k8s.io/client-go/discovery/cached/memory"
	_ "k8s.io/client-go/dynamic"
	_ "k8s.io/client-go/dynamic/fake"
	_ "k8s.io/client-go/rest"
	_ "k8s.io/client-go/restmapper"
	_ "k8s.io/client-go/testing"
	_ "k8s.io/client-go/tools/clientcmd"

	// YAML <-> JSON for Kubernetes objects, and the CLI framework.
	_ "github.com/spf13/cobra"
	_ "sigs.k8s.io/yaml"
)
