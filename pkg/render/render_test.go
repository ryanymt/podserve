package render_test

// Spine test (P01 T2), parked under testdata/spine/render/. T6 moves it to pkg/render/ with
// `git mv`, unchanged. Each case renders the five objects and compares them with
// testdata/render/<case>.golden.yaml as parsed objects (null, empty-map, empty-list and empty-string
// values pruned on both sides), so serializer noise like `creationTimestamp: null` can't fail it but
// every real field, including false and 0, must match.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/yaml"

	"github.com/ryanymt/podserve/internal/hffixture"
	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
	"github.com/ryanymt/podserve/pkg/platform"
	"github.com/ryanymt/podserve/pkg/profile"
	"github.com/ryanymt/podserve/pkg/render"
)

var plat = platform.Platform{
	Project:       "my-project",
	Region:        "us-central1",
	Bucket:        "my-project-podserve-models",
	VLLMImage:     "us-central1-docker.pkg.dev/my-project/podserve-dockerhub/vllm/vllm-openai@sha256:befccf0e7e4089aa23926bb6f887a85c0d46c2a99e6de5c5ffdf5cb7957c5b34",
	FetchImage:    "us-central1-docker.pkg.dev/my-project/podserve/fetch@sha256:0000000000000000000000000000000000000000000000000000000000000001",
	HFTokenSecret: "projects/my-project/secrets/podserve-hf-token",
}

func ptr[T any](v T) *T { return &v }

func owner(name, uid string) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: "podserve.dev/v1alpha1", Kind: "ModelService", Name: name, UID: k8stypes.UID(uid),
		Controller: ptr(true), BlockOwnerDeletion: ptr(true),
	}
}

type cas struct {
	golden string
	in     render.Input
	owner  metav1.OwnerReference
}

func cases() []cas {
	ms8 := &v1alpha1.ModelService{}
	ms8.Name, ms8.Namespace = "qwen3-8b", "podserve"
	ms8.Spec = v1alpha1.ModelServiceSpec{
		Model:           v1alpha1.ModelSource{Source: "hf://Qwen/Qwen3-8B", Revision: "main"},
		ServedModelName: "Qwen/Qwen3-8B",
		Accelerator:     v1alpha1.AcceleratorSpec{Type: "auto", Spot: ptr(true)},
		Engine:          v1alpha1.EngineSpec{GPUMemoryPercent: 90},
		Replicas:        ptr(int32(1)),
	}
	ms14 := &v1alpha1.ModelService{}
	ms14.Name, ms14.Namespace = "qwen3-14b", "podserve"
	ms14.Spec = v1alpha1.ModelServiceSpec{
		Model:           v1alpha1.ModelSource{Source: "hf://Qwen/Qwen3-14B", Revision: "main"},
		ServedModelName: "qwen-14b",
		Accelerator:     v1alpha1.AcceleratorSpec{Type: "nvidia-l4", Count: 2, Spot: ptr(false)},
		Engine:          v1alpha1.EngineSpec{GPUMemoryPercent: 85, ExtraArgs: []string{"--enable-prefix-caching"}},
		Replicas:        ptr(int32(2)),
	}
	return []cas{
		{
			golden: "qwen3-8b.golden.yaml",
			owner:  owner("qwen3-8b", "11111111-2222-3333-4444-555555555555"),
			in: render.Input{
				Service: ms8, Repo: "Qwen/Qwen3-8B", Revision: "b968826d9c46dd6066d109eabc6255188de91218",
				Platform: plat, LargestFetchFileBytes: 3996250744,
				Profile: profile.Result{
					GPUType: "nvidia-l4", GPUCount: 1, TensorParallelSize: 1, Dtype: "bfloat16",
					Shape:       profile.Shape{GPUs: 1, MachineType: "g2-standard-8", CPU: "6", Memory: "24Gi"},
					MaxModelLen: 17408, GPUMemoryPercent: 90, Deployable: true,
				},
			},
		},
		{
			golden: "qwen3-14b.golden.yaml",
			owner:  owner("qwen3-14b", "66666666-7777-8888-9999-000000000000"),
			in: render.Input{
				Service: ms14, Repo: "Qwen/Qwen3-14B", Revision: "40c069824f4251a91eefaf281ebe4c544efd3e18",
				Platform: plat, LargestFetchFileBytes: 16_000_000_000,
				Profile: profile.Result{
					GPUType: "nvidia-l4", GPUCount: 2, TensorParallelSize: 2, Dtype: "bfloat16",
					Shape:       profile.Shape{GPUs: 2, MachineType: "g2-standard-24", CPU: "20", Memory: "80Gi"},
					MaxModelLen: 32768, GPUMemoryPercent: 85, Deployable: true,
				},
			},
		},
	}
}

func prune(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := map[string]any{}
		for k, val := range x {
			if p := prune(val); p != nil {
				out[k] = p
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case []any:
		var out []any
		for _, val := range x {
			if p := prune(val); p != nil {
				out = append(out, p)
			}
		}
		if len(out) == 0 {
			return nil
		}
		return out
	case string:
		if x == "" {
			return nil
		}
		return x
	default:
		return v
	}
}

func toGeneric(t *testing.T, obj any) any {
	t.Helper()
	b, err := json.Marshal(obj)
	if err != nil {
		t.Fatal(err)
	}
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		t.Fatal(err)
	}
	return prune(v)
}

func goldenDocs(t *testing.T, name string) []any {
	t.Helper()
	root, err := hffixture.Root() // .../testdata/hf
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(filepath.Dir(root), "render", name))
	if err != nil {
		t.Fatal(err)
	}
	var docs []any
	for _, d := range strings.Split(string(b), "\n---\n") {
		j, err := yaml.YAMLToJSON([]byte(d))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var v any
		if err := json.Unmarshal(j, &v); err != nil {
			t.Fatal(err)
		}
		docs = append(docs, prune(v))
	}
	if len(docs) != 5 {
		t.Fatalf("%s: want 5 documents, have %d", name, len(docs))
	}
	return docs
}

func diff(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		g, _ := yaml.Marshal(got)
		w, _ := yaml.Marshal(want)
		t.Errorf("%s differs from the golden\n--- got:\n%s\n--- want:\n%s", what, g, w)
	}
}

func TestRenderMatchesGoldens(t *testing.T) {
	for _, c := range cases() {
		t.Run(c.golden, func(t *testing.T) {
			want := goldenDocs(t, c.golden)

			rec, err := render.Record(c.in)
			if err != nil {
				t.Fatalf("Record: %v", err)
			}
			diff(t, "Record", toGeneric(t, rec), want[0])

			job, err := render.FetchJob(c.in)
			if err != nil {
				t.Fatalf("FetchJob: %v", err)
			}
			diff(t, "FetchJob", toGeneric(t, job), want[1])
			if job.Name != platform.FetchJobName(c.in.Repo, c.in.Revision) {
				t.Errorf("fetch Job name %q, want %q", job.Name, platform.FetchJobName(c.in.Repo, c.in.Revision))
			}

			dep, svc, pm, err := render.Serving(c.in, c.owner)
			if err != nil {
				t.Fatalf("Serving: %v", err)
			}
			diff(t, "Deployment", toGeneric(t, dep), want[2])
			diff(t, "Service", toGeneric(t, svc), want[3])
			diff(t, "PodMonitoring", toGeneric(t, pm), want[4])
		})
	}
}

func TestRenderDoesNotMutateInput(t *testing.T) {
	c := cases()[1]
	before := toGeneric(t, c.in.Service)
	if _, _, _, err := render.Serving(c.in, c.owner); err != nil {
		t.Fatal(err)
	}
	if _, err := render.Record(c.in); err != nil {
		t.Fatal(err)
	}
	if after := toGeneric(t, c.in.Service); !reflect.DeepEqual(before, after) {
		t.Error("render changed its input ModelService")
	}
}
