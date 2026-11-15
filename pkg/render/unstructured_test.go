package render_test

// Spine test (P01, after T6): the PodMonitoring is unstructured, so it must hold only JSON-shaped
// values; anything else makes unstructured.DeepCopy (used by client-go and internal/fakekube) panic.

import (
	"testing"

	"github.com/podserve/podserve/pkg/render"
)

func TestPodMonitoringIsDeepCopyable(t *testing.T) {
	c := cases()[0]
	_, _, pm, err := render.Serving(c.in, c.owner)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("unstructured.DeepCopy panicked: %v", r)
		}
	}()
	if cp := pm.DeepCopy(); cp.GetName() != pm.GetName() || len(cp.GetOwnerReferences()) != 1 {
		t.Errorf("copy lost data: %v", cp.Object)
	}
}
