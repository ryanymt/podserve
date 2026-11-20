// Package fakekube is a fake cluster for podserve's kube and CLI tests (spine helper, P01 T2; not
// parked). client-go's dynamic/fake alone can't test podserve's lifecycle: it never assigns UIDs,
// never deletes children with their owner, and fails a server-side apply of a new object. Cluster
// wraps it with reactors that do what the API server and garbage collector would:
//
//   - create and server-side apply: create the object if missing (assigning uid, generation and
//     creationTimestamp), otherwise replace its content, keeping uid, creationTimestamp and status;
//   - the /status subresource boundary: an update or apply on the main resource cannot change
//     status (it is kept as it was), and one on /status changes only status;
//   - delete: also deletes every object (any tracked resource) whose ownerReferences name the
//     deleted object's uid, recursively;
//   - AutoReady: an applied Deployment immediately gets one Ready pod per replica and
//     status.readyReplicas, and a created Job immediately completes;
//   - Deployments as GKE runs them (P02 T2): pods belong to a ReplicaSet named
//     <deployment>-<pod-template-hash>, labelled pod-template-hash and annotated with the
//     deployment.kubernetes.io/revision, owned by the Deployment; a new pod template gets a new
//     ReplicaSet and the old one's pods go (strategy Recreate); status carries observedGeneration,
//     replicas, updatedReplicas and readyReplicas;
//   - a JSON merge patch on a Deployment changes only the fields it names (D-027's stop); one that
//     sets spec.replicas to 0 removes the pods;
//   - events, and AddList to seed recorded `kubectl get -o json` lists (testdata/kube/failures/);
//
// plus helpers to drive the rest by hand (CompleteJob, FailJob, SetServingPods, ...).
package fakekube

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/yaml"
)

// Resources the fake knows, with their list kinds. Objects of other resources are rejected.
var Resources = map[schema.GroupVersionResource]string{
	{Group: "podserve.dev", Version: "v1alpha1", Resource: "modelservices"}:               "ModelServiceList",
	{Group: "apps", Version: "v1", Resource: "deployments"}:                               "DeploymentList",
	{Group: "apps", Version: "v1", Resource: "replicasets"}:                               "ReplicaSetList",
	{Version: "v1", Resource: "events"}:                                                   "EventList",
	{Version: "v1", Resource: "services"}:                                                 "ServiceList",
	{Group: "batch", Version: "v1", Resource: "jobs"}:                                     "JobList",
	{Version: "v1", Resource: "pods"}:                                                     "PodList",
	{Version: "v1", Resource: "configmaps"}:                                               "ConfigMapList",
	{Version: "v1", Resource: "namespaces"}:                                               "NamespaceList",
	{Version: "v1", Resource: "serviceaccounts"}:                                          "ServiceAccountList",
	{Group: "monitoring.googleapis.com", Version: "v1", Resource: "podmonitorings"}:       "PodMonitoringList",
	{Group: "apiextensions.k8s.io", Version: "v1", Resource: "customresourcedefinitions"}: "CustomResourceDefinitionList",
}

var (
	gvrPods        = schema.GroupVersionResource{Version: "v1", Resource: "pods"}
	gvrJobs        = schema.GroupVersionResource{Group: "batch", Version: "v1", Resource: "jobs"}
	gvrDeployments = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	gvrReplicaSets = schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "replicasets"}
)

// Cluster is a fake API server plus garbage collector.
type Cluster struct {
	*dynamicfake.FakeDynamicClient
	mu        sync.Mutex
	uids      int
	AutoReady bool
}

// New returns an empty cluster. Seed objects with Add.
func New() *Cluster {
	c := &Cluster{FakeDynamicClient: dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(), Resources)}
	c.PrependReactor("create", "*", c.onCreate)
	c.PrependReactor("patch", "*", c.onPatch)
	c.PrependReactor("update", "*", c.onUpdate)
	c.PrependReactor("delete", "*", c.onDelete)
	return c
}

func (c *Cluster) nextUID() types.UID {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.uids++
	return types.UID(fmt.Sprintf("00000000-0000-0000-0000-%012d", c.uids))
}

func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return u.DeepCopy(), nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

func (c *Cluster) stamp(u *unstructured.Unstructured) {
	if u.GetUID() == "" {
		u.SetUID(c.nextUID())
	}
	if ts := u.GetCreationTimestamp(); ts.IsZero() {
		u.SetCreationTimestamp(metav1.NewTime(time.Date(2026, 11, 11, 0, 0, 0, 0, time.UTC)))
	}
	if u.GetGeneration() == 0 {
		u.SetGeneration(1)
	}
}

// Add seeds an object (typed or unstructured), stamping a uid.
func (c *Cluster) Add(gvr schema.GroupVersionResource, obj runtime.Object) error {
	u, err := toUnstructured(obj)
	if err != nil {
		return err
	}
	c.stamp(u)
	return c.Tracker().Create(gvr, u, u.GetNamespace())
}

func (c *Cluster) get(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	obj, err := c.Tracker().Get(gvr, ns, name)
	if err != nil {
		return nil, err
	}
	return toUnstructured(obj)
}

// Get returns a copy of an object, or a NotFound error.
func (c *Cluster) Get(gvr schema.GroupVersionResource, ns, name string) (*unstructured.Unstructured, error) {
	return c.get(gvr, ns, name)
}

func (c *Cluster) onCreate(a k8stesting.Action) (bool, runtime.Object, error) {
	act := a.(k8stesting.CreateAction)
	if _, ok := Resources[act.GetResource()]; !ok {
		return true, nil, fmt.Errorf("fakekube: unknown resource %v", act.GetResource())
	}
	u, err := toUnstructured(act.GetObject())
	if err != nil {
		return true, nil, err
	}
	ns := act.GetNamespace()
	if _, err := c.get(act.GetResource(), ns, u.GetName()); err == nil {
		return true, nil, apierrors.NewAlreadyExists(act.GetResource().GroupResource(), u.GetName())
	}
	c.stamp(u)
	if err := c.Tracker().Create(act.GetResource(), u, ns); err != nil {
		return true, nil, err
	}
	c.after(act.GetResource(), u)
	out, _ := c.get(act.GetResource(), ns, u.GetName())
	return true, out, nil
}

func (c *Cluster) onPatch(a k8stesting.Action) (bool, runtime.Object, error) {
	act := a.(k8stesting.PatchAction)
	if act.GetPatchType() == types.MergePatchType && act.GetResource() == gvrDeployments && act.GetSubresource() == "" {
		return c.mergePatchDeployment(act)
	}
	if act.GetPatchType() != types.ApplyPatchType {
		return false, nil, nil // let the default tracker handle other merge/JSON patches
	}
	gvr, ns, name := act.GetResource(), act.GetNamespace(), act.GetName()
	if _, ok := Resources[gvr]; !ok {
		return true, nil, fmt.Errorf("fakekube: unknown resource %v", gvr)
	}
	// Decode like the API machinery does (integers stay int64), accepting YAML or JSON.
	data, err := yaml.YAMLToJSON(act.GetPatch())
	if err != nil {
		return true, nil, fmt.Errorf("fakekube: apply patch: %w", err)
	}
	applied := &unstructured.Unstructured{}
	if err := applied.UnmarshalJSON(data); err != nil {
		return true, nil, fmt.Errorf("fakekube: apply patch: %w", err)
	}
	existing, err := c.get(gvr, ns, name)
	if apierrors.IsNotFound(err) {
		if act.GetSubresource() == "status" {
			return true, nil, err
		}
		applied.SetNamespace(ns)
		delete(applied.Object, "status") // apply on the main resource cannot set status
		c.stamp(applied)
		if err := c.Tracker().Create(gvr, applied, ns); err != nil {
			return true, nil, err
		}
		c.after(gvr, applied)
		out, _ := c.get(gvr, ns, name)
		return true, out, nil
	}
	if err != nil {
		return true, nil, err
	}
	next := c.merge(existing, applied, act.GetSubresource() == "status")
	if err := c.Tracker().Update(gvr, next, ns); err != nil {
		return true, nil, err
	}
	if act.GetSubresource() != "status" {
		c.after(gvr, next)
	}
	out, _ := c.get(gvr, ns, name)
	return true, out, nil
}

// merge replaces existing's content with incoming's, keeping identity and enforcing the /status
// boundary: statusOnly changes only status; otherwise status is kept as it was.
func (c *Cluster) merge(existing, incoming *unstructured.Unstructured, statusOnly bool) *unstructured.Unstructured {
	if statusOnly {
		next := existing.DeepCopy()
		if st, ok := incoming.Object["status"]; ok {
			next.Object["status"] = st
		}
		return next
	}
	next := incoming.DeepCopy()
	next.SetNamespace(existing.GetNamespace())
	next.SetUID(existing.GetUID())
	next.SetCreationTimestamp(existing.GetCreationTimestamp())
	next.SetResourceVersion(existing.GetResourceVersion())
	gen := existing.GetGeneration()
	if specChanged(existing, incoming) {
		gen++
	}
	next.SetGeneration(gen)
	if st, ok := existing.Object["status"]; ok {
		next.Object["status"] = st
	} else {
		delete(next.Object, "status")
	}
	return next
}

func specChanged(a, b *unstructured.Unstructured) bool {
	x, _ := json.Marshal(a.Object["spec"])
	y, _ := json.Marshal(b.Object["spec"])
	return string(x) != string(y)
}

func (c *Cluster) onUpdate(a k8stesting.Action) (bool, runtime.Object, error) {
	act := a.(k8stesting.UpdateAction)
	gvr, ns := act.GetResource(), act.GetNamespace()
	incoming, err := toUnstructured(act.GetObject())
	if err != nil {
		return true, nil, err
	}
	existing, err := c.get(gvr, ns, incoming.GetName())
	if err != nil {
		return true, nil, err
	}
	next := c.merge(existing, incoming, act.GetSubresource() == "status")
	if err := c.Tracker().Update(gvr, next, ns); err != nil {
		return true, nil, err
	}
	out, _ := c.get(gvr, ns, incoming.GetName())
	return true, out, nil
}

func (c *Cluster) onDelete(a k8stesting.Action) (bool, runtime.Object, error) {
	act := a.(k8stesting.DeleteAction)
	obj, err := c.get(act.GetResource(), act.GetNamespace(), act.GetName())
	if err != nil {
		return true, nil, err
	}
	if err := c.Tracker().Delete(act.GetResource(), act.GetNamespace(), act.GetName()); err != nil {
		return true, nil, err
	}
	// Like the API server: an explicit propagation policy wins; without one, batch/v1 Jobs orphan
	// their pods (a long-standing API default; kubectl hides it by setting Background) and every
	// other kind cascades.
	policy := act.GetDeleteOptions().PropagationPolicy
	orphan := act.GetResource() == gvrJobs
	if policy != nil {
		orphan = *policy == metav1.DeletePropagationOrphan
	}
	if !orphan {
		c.cascade(obj.GetUID())
	}
	return true, nil, nil
}

// cascade deletes every tracked object owned by uid, recursively (the garbage collector).
func (c *Cluster) cascade(uid types.UID) {
	for gvr, listKind := range Resources {
		list, err := c.Tracker().List(gvr, gvr.GroupVersion().WithKind(strings.TrimSuffix(listKind, "List")), "")
		if err != nil {
			continue
		}
		ul, err := toUnstructuredList(list)
		if err != nil {
			continue
		}
		for _, item := range ul {
			for _, ref := range item.GetOwnerReferences() {
				if ref.UID == uid {
					if err := c.Tracker().Delete(gvr, item.GetNamespace(), item.GetName()); err == nil {
						c.cascade(item.GetUID())
					}
					break
				}
			}
		}
	}
}

func toUnstructuredList(list runtime.Object) ([]unstructured.Unstructured, error) {
	if ul, ok := list.(*unstructured.UnstructuredList); ok {
		return ul.Items, nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(list)
	if err != nil {
		return nil, err
	}
	var out []unstructured.Unstructured
	items, _ := m["items"].([]any)
	for _, it := range items {
		if mm, ok := it.(map[string]any); ok {
			out = append(out, unstructured.Unstructured{Object: mm})
		}
	}
	return out, nil
}

// after runs the AutoReady behaviour for a newly created or applied object.
func (c *Cluster) after(gvr schema.GroupVersionResource, u *unstructured.Unstructured) {
	if !c.AutoReady {
		return
	}
	switch gvr {
	case gvrDeployments:
		_ = c.SetServingPods(u.GetNamespace(), u.GetName(), StageReady)
	case gvrJobs:
		_ = c.CompleteJob(u.GetNamespace(), u.GetName())
	}
}

// CompleteJob marks a Job succeeded (status.succeeded = 1 and a Complete condition).
func (c *Cluster) CompleteJob(ns, name string) error {
	return c.setJobStatus(ns, name, map[string]any{
		"succeeded":  int64(1),
		"conditions": []any{map[string]any{"type": "Complete", "status": "True"}},
	})
}

// CompleteJobWithMessage marks a Job succeeded and adds a succeeded pod for it whose container's
// termination message is msg (fetch.py writes its last event there, e.g. {"event":"cached"}).
func (c *Cluster) CompleteJobWithMessage(ns, name, msg string) error {
	if err := c.CompleteJob(ns, name); err != nil {
		return err
	}
	return c.jobPod(ns, name, "Succeeded", 0, msg)
}

// RunJob marks a Job active (running, not finished).
func (c *Cluster) RunJob(ns, name string) error {
	return c.setJobStatus(ns, name, map[string]any{"active": int64(1)})
}

// FailJob marks a Job failed and adds a terminated pod for it whose container's termination
// message is terminationMessage (what fetch.py writes to /dev/termination-log).
func (c *Cluster) FailJob(ns, name, terminationMessage string) error {
	if err := c.setJobStatus(ns, name, map[string]any{
		"failed":     int64(1),
		"conditions": []any{map[string]any{"type": "Failed", "status": "True", "reason": "BackoffLimitExceeded"}},
	}); err != nil {
		return err
	}
	return c.jobPod(ns, name, "Failed", 3, terminationMessage)
}

func (c *Cluster) jobPod(ns, name, phase string, exitCode int64, msg string) error {
	job, err := c.get(gvrJobs, ns, name)
	if err != nil {
		return err
	}
	reason := "Completed"
	if exitCode != 0 {
		reason = "Error"
	}
	pod := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "v1", "kind": "Pod",
		"metadata": map[string]any{
			"name": name + "-pod", "namespace": ns,
			"labels": map[string]any{"job-name": name, "batch.kubernetes.io/job-name": name},
		},
		"status": map[string]any{
			"phase": phase,
			"containerStatuses": []any{map[string]any{
				"name": "fetch",
				"state": map[string]any{"terminated": map[string]any{
					"exitCode": exitCode, "reason": reason, "message": msg,
				}},
			}},
		},
	}}
	pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: name, UID: job.GetUID()}})
	c.stamp(pod)
	return c.Tracker().Create(gvrPods, pod, ns)
}

func (c *Cluster) setJobStatus(ns, name string, status map[string]any) error {
	job, err := c.get(gvrJobs, ns, name)
	if err != nil {
		return err
	}
	job.Object["status"] = status
	return c.Tracker().Update(gvrJobs, job, ns)
}

// Stage is how far a serving pod has got.
type Stage string

const (
	StageUnscheduled Stage = "unscheduled" // Pending, PodScheduled False (waiting for a node)
	StagePulling     Stage = "pulling"     // scheduled, container waiting (ContainerCreating)
	StageLoading     Stage = "loading"     // container running, not Ready
	StageReady       Stage = "ready"       // Ready
)

// SetServingPods makes the Deployment's pods be at stage: one per replica, named
// <ReplicaSet>-<i>, owned by the ReplicaSet for the current pod template (created if needed, see
// ensureReplicaSet) and labelled like the template plus pod-template-hash. The Deployment's status
// records the rollout as observed: observedGeneration, replicas, updatedReplicas, and
// readyReplicas when the stage is Ready.
func (c *Cluster) SetServingPods(ns, deployment string, stage Stage) error {
	dep, err := c.get(gvrDeployments, ns, deployment)
	if err != nil {
		return err
	}
	replicas, found, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if !found {
		replicas = 1
	}
	rs, err := c.ensureReplicaSet(dep, replicas)
	if err != nil {
		return err
	}
	labels, _, _ := unstructured.NestedStringMap(rs.Object, "metadata", "labels")
	for i := int64(0); i < replicas; i++ {
		name := fmt.Sprintf("%s-%d", rs.GetName(), i)
		pod := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"status":   podStatus(stage),
		}}
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		pod.Object["metadata"].(map[string]any)["labels"] = l
		pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.GetName(),
			UID: rs.GetUID(), Controller: ptrTrue()}})
		if existing, err := c.get(gvrPods, ns, name); err == nil {
			pod.SetUID(existing.GetUID())
			pod.SetCreationTimestamp(existing.GetCreationTimestamp())
			if err := c.Tracker().Update(gvrPods, pod, ns); err != nil {
				return err
			}
		} else {
			c.stamp(pod)
			if err := c.Tracker().Create(gvrPods, pod, ns); err != nil {
				return err
			}
		}
	}
	ready := int64(0)
	if stage == StageReady {
		ready = replicas
	}
	rs.Object["status"] = map[string]any{"replicas": replicas, "readyReplicas": ready, "observedGeneration": rs.GetGeneration()}
	if err := c.Tracker().Update(gvrReplicaSets, rs, ns); err != nil {
		return err
	}
	dep.Object["status"] = map[string]any{"replicas": replicas, "updatedReplicas": replicas, "readyReplicas": ready,
		"observedGeneration": dep.GetGeneration()}
	return c.Tracker().Update(gvrDeployments, dep, ns)
}

func podStatus(stage Stage) map[string]any {
	switch stage {
	case StageUnscheduled:
		return map[string]any{"phase": "Pending", "conditions": []any{
			map[string]any{"type": "PodScheduled", "status": "False", "reason": "Unschedulable"}}}
	case StagePulling:
		return map[string]any{"phase": "Pending", "conditions": []any{
			map[string]any{"type": "PodScheduled", "status": "True"}},
			"containerStatuses": []any{map[string]any{"name": "vllm", "ready": false,
				"state": map[string]any{"waiting": map[string]any{"reason": "ContainerCreating"}}}}}
	case StageLoading:
		return map[string]any{"phase": "Running", "conditions": []any{
			map[string]any{"type": "PodScheduled", "status": "True"}, map[string]any{"type": "Ready", "status": "False"}},
			"containerStatuses": []any{map[string]any{"name": "vllm", "ready": false,
				"state": map[string]any{"running": map[string]any{}}}}}
	default:
		return map[string]any{"phase": "Running", "conditions": []any{
			map[string]any{"type": "PodScheduled", "status": "True"}, map[string]any{"type": "Ready", "status": "True"}},
			"containerStatuses": []any{map[string]any{"name": "vllm", "ready": true,
				"state": map[string]any{"running": map[string]any{}}}}}
	}
}

// DeleteServingPods removes a Deployment's pods (as when it scales to zero or is deleted): every
// pod owned by one of its ReplicaSets.
func (c *Cluster) DeleteServingPods(ns, deployment string) error {
	for _, rs := range c.replicaSetsOf(ns, deployment) {
		for _, p := range c.podsOf(ns, rs.GetName()) {
			if err := c.Tracker().Delete(gvrPods, ns, p.GetName()); err != nil {
				return err
			}
		}
	}
	return nil
}

// mergePatchDeployment applies a JSON merge patch (RFC 7386) to a Deployment: only the fields the
// patch names change, the generation goes up when the spec does, and spec.replicas 0 removes the
// pods (as the ReplicaSet controller would).
func (c *Cluster) mergePatchDeployment(act k8stesting.PatchAction) (bool, runtime.Object, error) {
	ns, name := act.GetNamespace(), act.GetName()
	existing, err := c.get(gvrDeployments, ns, name)
	if err != nil {
		return true, nil, err
	}
	var patch map[string]any
	if err := json.Unmarshal(act.GetPatch(), &patch); err != nil {
		return true, nil, fmt.Errorf("fakekube: merge patch: %w", err)
	}
	next := existing.DeepCopy()
	delete(patch, "status") // the main resource can't change status
	mergeJSON(next.Object, patch)
	if specChanged(existing, next) {
		next.SetGeneration(existing.GetGeneration() + 1)
	}
	if err := c.Tracker().Update(gvrDeployments, next, ns); err != nil {
		return true, nil, err
	}
	if r, found, _ := unstructured.NestedInt64(next.Object, "spec", "replicas"); found && r == 0 {
		if err := c.scaleToZero(ns, name); err != nil {
			return true, nil, err
		}
	} else if c.AutoReady {
		_ = c.SetServingPods(ns, name, StageReady)
	}
	out, _ := c.get(gvrDeployments, ns, name)
	return true, out, nil
}

// mergeJSON applies an RFC 7386 merge patch to target in place.
func mergeJSON(target, patch map[string]any) {
	for k, v := range patch {
		if v == nil {
			delete(target, k)
			continue
		}
		if pm, ok := v.(map[string]any); ok {
			if tm, ok := target[k].(map[string]any); ok {
				mergeJSON(tm, pm)
				continue
			}
		}
		if f, ok := v.(float64); ok && f == float64(int64(f)) {
			v = int64(f) // integers stay int64, as the API machinery decodes them
		}
		target[k] = v
	}
}

// scaleToZero removes a Deployment's pods and records zero replicas in its and its ReplicaSets'
// status.
func (c *Cluster) scaleToZero(ns, deployment string) error {
	if err := c.DeleteServingPods(ns, deployment); err != nil {
		return err
	}
	for _, rs := range c.replicaSetsOf(ns, deployment) {
		_ = unstructured.SetNestedField(rs.Object, int64(0), "spec", "replicas")
		rs.Object["status"] = map[string]any{"replicas": int64(0), "observedGeneration": rs.GetGeneration()}
		_ = c.Tracker().Update(gvrReplicaSets, &rs, ns)
	}
	dep, err := c.get(gvrDeployments, ns, deployment)
	if err != nil {
		return err
	}
	dep.Object["status"] = map[string]any{"replicas": int64(0), "readyReplicas": int64(0),
		"updatedReplicas": int64(0), "observedGeneration": dep.GetGeneration()}
	return c.Tracker().Update(gvrDeployments, dep, ns)
}

// replicaSetsOf lists the ReplicaSets a Deployment owns.
func (c *Cluster) replicaSetsOf(ns, deployment string) []unstructured.Unstructured {
	list, err := c.Tracker().List(gvrReplicaSets, gvrReplicaSets.GroupVersion().WithKind("ReplicaSet"), ns)
	if err != nil {
		return nil
	}
	items, _ := toUnstructuredList(list)
	var out []unstructured.Unstructured
	for _, rs := range items {
		for _, ref := range rs.GetOwnerReferences() {
			if ref.Kind == "Deployment" && ref.Name == deployment {
				out = append(out, rs)
			}
		}
	}
	return out
}

// templateHash is the fake's pod-template-hash: the first 10 hex characters of the sha256 of the
// pod template's JSON (the real controller uses a different hash; tests compare labels, never values).
func templateHash(dep *unstructured.Unstructured) string {
	b, _ := json.Marshal(dep.Object["spec"].(map[string]any)["template"])
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])[:10]
}

// ensureReplicaSet returns the ReplicaSet for the Deployment's current pod template, creating it
// (with the next revision) if needed. Every older ReplicaSet is scaled to zero and loses its pods,
// as strategy Recreate does.
func (c *Cluster) ensureReplicaSet(dep *unstructured.Unstructured, replicas int64) (*unstructured.Unstructured, error) {
	ns, hash := dep.GetNamespace(), templateHash(dep)
	name := dep.GetName() + "-" + hash
	revision := 0
	for _, rs := range c.replicaSetsOf(ns, dep.GetName()) {
		if r, err := strconv.Atoi(rs.GetAnnotations()["deployment.kubernetes.io/revision"]); err == nil && r > revision {
			revision = r
		}
		if rs.GetName() == name {
			continue
		}
		for _, p := range c.podsOf(ns, rs.GetName()) {
			_ = c.Tracker().Delete(gvrPods, ns, p.GetName())
		}
		_ = unstructured.SetNestedField(rs.Object, int64(0), "spec", "replicas")
		rs.Object["status"] = map[string]any{"replicas": int64(0), "observedGeneration": rs.GetGeneration()}
		_ = c.Tracker().Update(gvrReplicaSets, &rs, ns)
	}
	labels, _, _ := unstructured.NestedStringMap(dep.Object, "spec", "template", "metadata", "labels")
	l := map[string]any{"pod-template-hash": hash}
	for k, v := range labels {
		l[k] = v
	}
	if rs, err := c.get(gvrReplicaSets, ns, name); err == nil {
		_ = unstructured.SetNestedField(rs.Object, replicas, "spec", "replicas")
		return rs, c.Tracker().Update(gvrReplicaSets, rs, ns)
	}
	tmpl, _, _ := unstructured.NestedMap(dep.Object, "spec", "template")
	rs := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "apps/v1", "kind": "ReplicaSet",
		"metadata": map[string]any{"name": name, "namespace": ns, "labels": l,
			"annotations": map[string]any{"deployment.kubernetes.io/revision": strconv.Itoa(revision + 1)}},
		"spec": map[string]any{"replicas": replicas, "template": tmpl},
	}}
	rs.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: dep.GetName(),
		UID: dep.GetUID(), Controller: ptrTrue()}})
	c.stamp(rs)
	return rs, c.Tracker().Create(gvrReplicaSets, rs, ns)
}

func ptrTrue() *bool { t := true; return &t }

// podsOf lists the pods a ReplicaSet owns.
func (c *Cluster) podsOf(ns, replicaSet string) []unstructured.Unstructured {
	list, err := c.Tracker().List(gvrPods, gvrPods.GroupVersion().WithKind("Pod"), ns)
	if err != nil {
		return nil
	}
	items, _ := toUnstructuredList(list)
	var out []unstructured.Unstructured
	for _, p := range items {
		for _, ref := range p.GetOwnerReferences() {
			if ref.Kind == "ReplicaSet" && ref.Name == replicaSet {
				out = append(out, p)
			}
		}
	}
	return out
}

// AddList seeds every item of a recorded `kubectl get ... -o json` list (a List, PodList, EventList,
// ...), keeping the items as recorded (names, uids, labels, status). Items of kinds the fake doesn't
// know are an error.
func (c *Cluster) AddList(path string) error {
	items, err := readList(path)
	if err != nil {
		return err
	}
	for _, u := range items {
		gvr, ok := resourceFor(u.GetAPIVersion(), u.GetKind())
		if !ok {
			return fmt.Errorf("fakekube: %s: unknown kind %s/%s", path, u.GetAPIVersion(), u.GetKind())
		}
		c.stamp(u)
		if err := c.Tracker().Create(gvr, u, u.GetNamespace()); err != nil {
			return fmt.Errorf("fakekube: %s: %s %s: %w", path, u.GetKind(), u.GetName(), err)
		}
	}
	return nil
}

// resourceFor maps an object's apiVersion and kind to a resource the fake knows.
func resourceFor(apiVersion, kind string) (schema.GroupVersionResource, bool) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, false
	}
	for gvr, listKind := range Resources {
		if gvr.Group == gv.Group && gvr.Version == gv.Version && listKind == kind+"List" {
			return gvr, true
		}
	}
	return schema.GroupVersionResource{}, false
}

// SetServingPodsFrom gives a Deployment's pods the statuses recorded in a fixture directory
// (testdata/kube/failures/<state>/: pods.json and events.json from `kubectl get -o json`). It makes
// one pod per recorded pod, owned by the ReplicaSet of the Deployment's current template and named
// <ReplicaSet>-<i>, with the recorded status (phase, conditions, container states, deletion), and
// re-points the recorded events at those pods. So CLI tests see what GKE reported, on objects the
// fake owns. The Deployment's status counts the pods that are Ready.
func (c *Cluster) SetServingPodsFrom(ns, deployment, dir string) error {
	pods, err := readList(dir + "/pods.json")
	if err != nil {
		return err
	}
	events, err := readList(dir + "/events.json")
	if err != nil {
		return err
	}
	dep, err := c.get(gvrDeployments, ns, deployment)
	if err != nil {
		return err
	}
	replicas, found, _ := unstructured.NestedInt64(dep.Object, "spec", "replicas")
	if !found {
		replicas = 1
	}
	rs, err := c.ensureReplicaSet(dep, replicas)
	if err != nil {
		return err
	}
	for _, p := range c.podsOf(ns, rs.GetName()) {
		_ = c.Tracker().Delete(gvrPods, ns, p.GetName())
	}
	labels, _, _ := unstructured.NestedStringMap(rs.Object, "metadata", "labels")
	renamed := map[string]string{}
	ready := int64(0)
	for i, recorded := range pods {
		rec := recorded.Object
		name := fmt.Sprintf("%s-%d", rs.GetName(), i)
		renamed[recorded.GetName()] = name
		pod := &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "v1", "kind": "Pod",
			"metadata": map[string]any{"name": name, "namespace": ns},
			"status":   rec["status"],
		}}
		l := map[string]any{}
		for k, v := range labels {
			l[k] = v
		}
		pod.Object["metadata"].(map[string]any)["labels"] = l
		if ts := recorded.GetDeletionTimestamp(); ts != nil {
			pod.SetDeletionTimestamp(ts)
		}
		pod.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: rs.GetName(),
			UID: rs.GetUID(), Controller: ptrTrue()}})
		c.stamp(pod)
		if err := c.Tracker().Create(gvrPods, pod, ns); err != nil {
			return err
		}
		if conditionTrue(rec, "Ready") {
			ready++
		}
	}
	// a recording replaces the previous one's events for this Deployment's pods
	gvrEvents := schema.GroupVersionResource{Version: "v1", Resource: "events"}
	if list, err := c.Tracker().List(gvrEvents, gvrEvents.GroupVersion().WithKind("Event"), ns); err == nil {
		old, _ := toUnstructuredList(list)
		for _, e := range old {
			if strings.HasPrefix(e.GetName(), deployment+".") {
				_ = c.Tracker().Delete(gvrEvents, ns, e.GetName())
			}
		}
	}
	for i, ev := range events {
		rec := ev.Object
		involved, _, _ := unstructured.NestedString(rec, "involvedObject", "name")
		if to, ok := renamed[involved]; ok {
			_ = unstructured.SetNestedField(ev.Object, to, "involvedObject", "name")
		} else if kind, _, _ := unstructured.NestedString(rec, "involvedObject", "kind"); kind == "Pod" {
			continue // an event about a pod this fixture doesn't carry
		}
		ev.SetName(fmt.Sprintf("%s.%d", deployment, i))
		ev.SetNamespace(ns)
		ev.SetResourceVersion("")
		ev.SetUID("")
		c.stamp(ev)
		if err := c.Tracker().Create(gvrEvents, ev, ns); err != nil {
			return err
		}
	}
	rs.Object["status"] = map[string]any{"replicas": replicas, "readyReplicas": ready, "observedGeneration": rs.GetGeneration()}
	if err := c.Tracker().Update(gvrReplicaSets, rs, ns); err != nil {
		return err
	}
	dep.Object["status"] = map[string]any{"replicas": replicas, "updatedReplicas": replicas, "readyReplicas": ready,
		"observedGeneration": dep.GetGeneration()}
	return c.Tracker().Update(gvrDeployments, dep, ns)
}

func conditionTrue(obj map[string]any, kind string) bool {
	conds, _, _ := unstructured.NestedSlice(obj, "status", "conditions")
	for _, c := range conds {
		if m, ok := c.(map[string]any); ok && m["type"] == kind && m["status"] == "True" {
			return true
		}
	}
	return false
}

// readList decodes a recorded `kubectl get -o json` list the way the API machinery does (integers
// stay int64), so code reading counts, exit codes and restart counts sees what it would live.
func readList(path string) ([]*unstructured.Unstructured, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var list struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, fmt.Errorf("fakekube: %s: %w", path, err)
	}
	out := make([]*unstructured.Unstructured, 0, len(list.Items))
	for _, raw := range list.Items {
		u := &unstructured.Unstructured{}
		if err := u.UnmarshalJSON(raw); err != nil {
			return nil, fmt.Errorf("fakekube: %s: %w", path, err)
		}
		out = append(out, u)
	}
	return out, nil
}
