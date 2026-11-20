package kube_test

// Spine test (P02 T0/T2, added after T3 merged): with both regional L4 quotas (on-demand and
// preemptible) capped at zero, Autopilot never names the quota in the pod's events. Its verdict is
// NotTriggerScaleUp ("... in backoff after failed scale-up"), which outranks its own earlier
// TriggeredScaleUp. Recorded live in T0 (testdata/kube/failures/quota/).

import (
	"reflect"
	"testing"

	"github.com/ryanymt/podserve/pkg/kube"
)

func TestDiagnoseRecordedQuotaFailure(t *testing.T) {
	s, now := recordedServing(t, "quota")
	want := kube.Diagnosis{Stage: kube.StageNode, Reason: kube.ReasonNoScaleUp,
		Message: "Pod didn't trigger scale-up: 4 node(s) didn't match Pod's node affinity/selector, 3 in backoff after failed scale-up",
		Pod:     "qwen3-8b-68496b54d4-5gcgc"}
	if got := kube.Diagnose(s, now); !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v\nwant %+v", got, want)
	}
}
