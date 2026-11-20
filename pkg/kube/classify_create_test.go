package kube_test

// The two container-creation failures (D-026), which podserve's pod spec can't produce on purpose
// (it mounts no ConfigMap or Secret that could be missing). Hand-written per D11: the recorded
// invalidimage pod with its container's waiting reason and message replaced, in the shape the
// kubelet reports them (state.waiting.reason and .message). Contract: docs/design/cli.md.

import (
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/kube"
)

func TestDiagnoseContainerCreationFailures(t *testing.T) {
	cases := map[string]struct {
		message string
		want    kube.Reason
	}{
		"CreateContainerConfigError": {`secret "hf-token" not found`, kube.ReasonCreateConfig},
		"CreateContainerError": {`failed to generate container "vllm" spec: failed to apply OCI options: ` +
			`failed to mkdir "/var/lib/kubelet/pods/x/volumes": permission denied`, kube.ReasonCreateContainer},
	}
	for reason, c := range cases {
		t.Run(reason, func(t *testing.T) {
			s, now := recordedServing(t, "invalidimage")
			replaced := 0
			for i := range s.Pods {
				statuses, _, _ := unstructured.NestedSlice(s.Pods[i].Object, "status", "containerStatuses")
				for j, cs := range statuses {
					m := cs.(map[string]any)
					if _, ok, _ := unstructured.NestedMap(m, "state", "waiting"); !ok {
						continue
					}
					_ = unstructured.SetNestedField(m, reason, "state", "waiting", "reason")
					_ = unstructured.SetNestedField(m, c.message, "state", "waiting", "message")
					statuses[j] = m
					replaced++
				}
				_ = unstructured.SetNestedSlice(s.Pods[i].Object, statuses, "status", "containerStatuses")
			}
			if replaced == 0 {
				t.Fatal("the invalidimage fixture has no waiting container")
			}
			d := kube.Diagnose(s, now)
			if !d.Failed || d.Reason != c.want || d.Message != c.message || d.Pod == "" {
				t.Errorf("got %+v, want Failed %s with %q", d, c.want, c.message)
			}
		})
	}
}
