package kube

// kube.go holds the record calls (apply, status, get, list), the serving-object apply, and the
// helpers every method shares. The waits are in wait.go, the fetch Job in fetch.go, the install
// in install.go.

import (
	"context"
	"fmt"
	"sort"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/platform"
)

// ApplyRecord server-side applies the ModelService (spec and metadata only) in the podserve
// namespace, reads it back, and returns the owner reference its children carry
// (controller: true, blockOwnerDeletion: true).
func (c *Client) ApplyRecord(ctx context.Context, ms *v1alpha1.ModelService) (metav1.OwnerReference, error) {
	u, err := toUnstructured(ms)
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	u.SetAPIVersion(v1alpha1.GroupVersion.String())
	u.SetKind(v1alpha1.Kind)
	if _, err := c.apply(ctx, ModelServices, platform.Namespace, u); err != nil {
		return metav1.OwnerReference{}, err
	}
	got, err := c.modelservices().Get(ctx, u.GetName(), metav1.GetOptions{})
	if err != nil {
		return metav1.OwnerReference{}, err
	}
	yes := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               v1alpha1.Kind,
		Name:               got.GetName(),
		UID:                got.GetUID(),
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}, nil
}

// UpdateStatus writes status through the /status subresource.
func (c *Client) UpdateStatus(ctx context.Context, name string, status v1alpha1.ModelServiceStatus) error {
	u, err := c.modelservices().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return err
	}
	st, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&status)
	if err != nil {
		return err
	}
	u.Object["status"] = st
	_, err = c.modelservices().UpdateStatus(ctx, u, metav1.UpdateOptions{})
	return err
}

// ApplyChildren server-side applies the serving objects (Deployment, Service, PodMonitoring).
func (c *Client) ApplyChildren(ctx context.Context, objs ...*unstructured.Unstructured) error {
	for _, u := range objs {
		gvr, ok := childResource(u.GetKind())
		if !ok {
			return fmt.Errorf("kube: podserve does not manage kind %q", u.GetKind())
		}
		if _, err := c.apply(ctx, gvr, platform.Namespace, u); err != nil {
			return err
		}
	}
	return nil
}

// Get returns one ModelService, or ErrNotFound.
func (c *Client) Get(ctx context.Context, name string) (*v1alpha1.ModelService, error) {
	u, err := c.modelservices().Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%s/%s: %w", platform.Namespace, name, ErrNotFound)
		}
		return nil, err
	}
	return asModelService(u)
}

// List returns every ModelService in the podserve namespace, sorted by name.
func (c *Client) List(ctx context.Context) ([]v1alpha1.ModelService, error) {
	list, err := c.modelservices().List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]v1alpha1.ModelService, 0, len(list.Items))
	for i := range list.Items {
		ms, err := asModelService(&list.Items[i])
		if err != nil {
			return nil, err
		}
		out = append(out, *ms)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Typed views of the podserve namespace's resources.
func (c *Client) modelservices() dynamic.ResourceInterface {
	return c.Dynamic.Resource(ModelServices).Namespace(platform.Namespace)
}

func (c *Client) deployments() dynamic.ResourceInterface {
	return c.Dynamic.Resource(Deployments).Namespace(platform.Namespace)
}

func (c *Client) pods() dynamic.ResourceInterface {
	return c.Dynamic.Resource(Pods).Namespace(platform.Namespace)
}

func (c *Client) jobs() dynamic.ResourceInterface {
	return c.Dynamic.Resource(Jobs).Namespace(platform.Namespace)
}

func (c *Client) configMaps() dynamic.ResourceInterface {
	return c.Dynamic.Resource(ConfigMaps).Namespace(platform.Namespace)
}

// apply server-side applies u with podserve's field manager, forcing the take-over of fields
// another manager may hold.
func (c *Client) apply(ctx context.Context, gvr schema.GroupVersionResource, ns string,
	u *unstructured.Unstructured,
) (*unstructured.Unstructured, error) {
	return c.Dynamic.Resource(gvr).Namespace(ns).Apply(ctx, u.GetName(), u,
		metav1.ApplyOptions{FieldManager: platform.FieldManager, Force: true})
}

// childResource maps a serving object's kind to the resource it applies to.
func childResource(kind string) (schema.GroupVersionResource, bool) {
	switch kind {
	case "Deployment":
		return Deployments, true
	case "Service":
		return Services, true
	case "PodMonitoring":
		return PodMonitorings, true
	}
	return schema.GroupVersionResource{}, false
}

// toUnstructured converts a typed object for applying. The caller sets apiVersion and kind, and
// status is dropped: a write to the main resource cannot set it.
func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	u := &unstructured.Unstructured{Object: m}
	delete(u.Object, "status")
	return u, nil
}

// asModelService converts a stored object into the typed ModelService.
func asModelService(u *unstructured.Unstructured) (*v1alpha1.ModelService, error) {
	ms := &v1alpha1.ModelService{}
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, ms); err != nil {
		return nil, err
	}
	return ms, nil
}

// conditionStatus returns the status ("True"/"False") of the condition of that type in an object's
// status.conditions, or "" when there is none.
func conditionStatus(obj map[string]any, kind string) string {
	conditions, found, _ := unstructured.NestedSlice(obj, "status", "conditions")
	if !found {
		return ""
	}
	for _, c := range conditions {
		cond, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if t, _ := cond["type"].(string); t == kind {
			return firstString(cond, "status")
		}
	}
	return ""
}

// firstString reads obj[field] as a string, "" when it isn't one.
func firstString(obj map[string]any, field string) string {
	s, _ := obj[field].(string)
	return s
}

// realClock is the default Options.Clock.
type realClock struct{}

func (realClock) Now() time.Time        { return time.Now() }
func (realClock) Sleep(d time.Duration) { time.Sleep(d) }

// waitParams resolves Options' zero values: the clock to wait on, the poll interval and the
// timeout.
func (c *Client) waitParams() (clk Clock, poll, timeout time.Duration) {
	clk = c.Options.Clock
	if clk == nil {
		clk = realClock{}
	}
	poll = c.Options.PollInterval
	if poll <= 0 {
		poll = DefaultPollInterval
	}
	timeout = c.Options.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return clk, poll, timeout
}

// progressReporter is Options.Progress made nil-safe and once-per-stage: each stage is reported
// the first time it is reached. The zero value never reports.
type progressReporter struct {
	progress func(Stage)
	seen     map[Stage]bool
}

func newProgress(c *Client) *progressReporter {
	if c.Options.Progress == nil {
		return nil
	}
	return &progressReporter{progress: c.Options.Progress, seen: map[Stage]bool{}}
}

func (p *progressReporter) reach(stage Stage) {
	if p == nil || p.seen[stage] {
		return
	}
	p.seen[stage] = true
	p.progress(stage)
}
