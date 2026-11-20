package kube

// install.go sets the platform up (podserve init) and reads its settings back.

import (
	"context"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"sigs.k8s.io/yaml"

	"github.com/ryanymt/podserve/pkg/platform"
)

// Install server-side applies, in order: the CRD (crdYAML), the podserve namespace, the
// podserve-fetch and podserve-model service accounts, and the podserve-platform ConfigMap with data.
func (c *Client) Install(ctx context.Context, crdYAML []byte, data map[string]string) error {
	crd := &unstructured.Unstructured{}
	if err := yaml.Unmarshal(crdYAML, &crd.Object); err != nil {
		return err
	}
	if _, err := c.apply(ctx, CRDs, "", crd); err != nil {
		return err
	}
	if _, err := c.apply(ctx, Namespaces, "", coreObject("v1", "Namespace", platform.Namespace, "")); err != nil {
		return err
	}
	for _, sa := range []string{platform.FetchSA, platform.ModelSA} {
		if _, err := c.apply(ctx, ServiceAccts, platform.Namespace, coreObject("v1", "ServiceAccount", sa, platform.Namespace)); err != nil {
			return err
		}
	}
	cm := coreObject("v1", "ConfigMap", platform.ConfigMapName, platform.Namespace)
	if len(data) > 0 {
		cm.Object["data"] = data
	}
	if _, err := c.apply(ctx, ConfigMaps, platform.Namespace, cm); err != nil {
		return err
	}
	return nil
}

// coreObject is a core/v1 object with podserve's managed-by label, in ns ("" for a cluster-scoped
// object).
func coreObject(apiVersion, kind, name, ns string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": apiVersion,
		"kind":       kind,
		"metadata": map[string]any{
			"name":   name,
			"labels": map[string]any{platform.ManagedByLabel: platform.ManagedByValue},
		},
	}}
	if ns != "" {
		u.SetNamespace(ns)
	}
	return u
}

// PlatformData returns the podserve/podserve-platform ConfigMap's data, or
// ErrPlatformNotInitialised.
func (c *Client) PlatformData(ctx context.Context) (map[string]string, error) {
	cm, err := c.configMaps().Get(ctx, platform.ConfigMapName, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, ErrPlatformNotInitialised
		}
		return nil, err
	}
	data, _, err := unstructured.NestedStringMap(cm.Object, "data")
	if err != nil {
		return nil, err
	}
	if data == nil {
		data = map[string]string{}
	}
	return data, nil
}
