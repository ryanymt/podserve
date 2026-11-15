package main

// Spine test (P01 T2), parked under testdata/spine/cli/. T10b moves it to cmd/podserve/ with
// `git mv`, unchanged. It runs the real commands through run() (the seam in main.go) against:
//   - a fake Hub built from testdata/hf/ (internal/hffixture),
//   - internal/fakekube (UIDs, server-side apply, /status, garbage collection, AutoReady),
//   - a clock whose Sleep does not advance time (so stage lines print [0:00]) unless a test says so,
//   - a recording Exec (gcloud, kubectl).
// Expected output: testdata/cli/*.golden (args, exit code, stdout, stderr; *-json goldens compare
// stdout as JSON) and the inline expectations below, all from docs/design/cli.md.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/yaml"

	"github.com/podserve/podserve/internal/fakekube"
	"github.com/podserve/podserve/internal/hffixture"
	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/hf"
	"github.com/podserve/podserve/pkg/kube"
	"github.com/podserve/podserve/pkg/platform"
	"github.com/podserve/podserve/pkg/profile"
)

// ---- fakes ---------------------------------------------------------------------------------------

type fakeHF struct{ token string }

func (f fakeHF) Model(ctx context.Context, repo, revision string) (profile.Model, error) {
	fx, err := hffixture.Load(repo)
	if err != nil {
		return profile.Model{}, hf.ErrNotFound
	}
	if revision != "main" && revision != fx.Info.SHA {
		return profile.Model{}, hf.ErrNotFound
	}
	return fx.Model(f.token != "")
}

type env struct {
	cluster *fakekube.Cluster
	stdout  bytes.Buffer
	stderr  bytes.Buffer
	execs   [][]string
	vars    map[string]string
	now     time.Time
	advance bool        // Sleep advances the clock
	onSleep func(n int) // called after every Sleep
	sleeps  int
}

var testPlatform = platform.Platform{
	Project: "my-project", Region: "us-central1", Bucket: "my-project-podserve-models",
	VLLMImage:     platform.VLLMImageRef("us-central1-docker.pkg.dev/my-project/podserve-dockerhub"),
	FetchImage:    "us-central1-docker.pkg.dev/my-project/podserve/fetch@sha256:0000000000000000000000000000000000000000000000000000000000000001",
	HFTokenSecret: "projects/my-project/secrets/podserve-hf-token",
}

// newEnv returns a cluster with the platform initialised (unless bare) and AutoReady on.
func newEnv(t *testing.T, bare bool) *env {
	t.Helper()
	e := &env{cluster: fakekube.New(), vars: map[string]string{}, now: time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC)}
	e.cluster.AutoReady = true
	if !bare {
		cm := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "v1", "kind": "ConfigMap",
			"metadata": map[string]any{"name": platform.ConfigMapName, "namespace": platform.Namespace}}}
		data := map[string]any{}
		for k, v := range testPlatform.Data() {
			data[k] = v
		}
		cm.Object["data"] = data
		if err := e.cluster.Add(kube.ConfigMaps, cm); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func (e *env) deps() deps {
	return deps{
		Stdout: &e.stdout, Stderr: &e.stderr,
		Getenv: func(k string) string { return e.vars[k] },
		NewHF:  func(token string) HFClient { return fakeHF{token: token} },
		NewDynamic: func() (dynamic.Interface, error) {
			return e.cluster, nil
		},
		Exec: func(ctx context.Context, stdout, stderr io.Writer, name string, args ...string) error {
			e.execs = append(e.execs, append([]string{name}, args...))
			if name == "gcloud" && len(args) > 2 && args[0] == "artifacts" {
				fmt.Fprintln(stdout, "sha256:2222222222222222222222222222222222222222222222222222222222222222")
			}
			return nil
		},
		Now: func() time.Time { return e.now },
		Sleep: func(d time.Duration) {
			if e.advance {
				e.now = e.now.Add(d)
			}
			e.sleeps++
			if e.onSleep != nil {
				e.onSleep(e.sleeps)
			}
			if e.sleeps > 10000 {
				panic("runaway wait loop")
			}
		},
		Version: "v0.1.0-test",
	}
}

func (e *env) run(args ...string) int {
	e.stdout.Reset()
	e.stderr.Reset()
	return run(context.Background(), args, e.deps())
}

// ---- goldens -------------------------------------------------------------------------------------

type golden struct {
	args           []string
	exit           int
	stdout, stderr string
}

func readGolden(t *testing.T, path string) golden {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	var g golden
	head, rest, ok := strings.Cut(s, "--- stdout\n")
	if !ok {
		t.Fatalf("%s: no '--- stdout' line", path)
	}
	g.stdout, g.stderr, ok = strings.Cut(rest, "--- stderr\n")
	if !ok {
		t.Fatalf("%s: no '--- stderr' line", path)
	}
	for _, line := range strings.Split(strings.TrimSpace(head), "\n") {
		k, v, _ := strings.Cut(line, ": ")
		switch k {
		case "args":
			if err := json.Unmarshal([]byte(v), &g.args); err != nil {
				t.Fatalf("%s args: %v", path, err)
			}
		case "exit":
			fmt.Sscan(v, &g.exit)
		}
	}
	return g
}

func testdataDir(t *testing.T) string {
	t.Helper()
	root, err := hffixture.Root() // .../testdata/hf
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Dir(root)
}

func TestGoldens(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(testdataDir(t), "cli", "*.golden"))
	if len(files) < 10 {
		t.Fatalf("want the CLI goldens, found %d", len(files))
	}
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".golden")
		t.Run(name, func(t *testing.T) {
			g := readGolden(t, f)
			e := newEnv(t, false)
			code := e.run(g.args...)
			if code != g.exit {
				t.Errorf("exit %d, want %d\nstderr: %s", code, g.exit, e.stderr.String())
			}
			if strings.HasSuffix(name, "-json") {
				var got, want any
				if err := json.Unmarshal(e.stdout.Bytes(), &got); err != nil {
					t.Fatalf("stdout is not JSON: %v\n%s", err, e.stdout.String())
				}
				_ = json.Unmarshal([]byte(g.stdout), &want)
				if !reflect.DeepEqual(normaliseJSON(got), normaliseJSON(want)) {
					t.Errorf("stdout JSON\n got: %s\nwant: %s", e.stdout.String(), g.stdout)
				}
			} else if e.stdout.String() != g.stdout {
				t.Errorf("stdout\n got:\n%s\nwant:\n%s", e.stdout.String(), g.stdout)
			}
			if e.stderr.String() != g.stderr {
				t.Errorf("stderr\n got: %q\nwant: %q", e.stderr.String(), g.stderr)
			}
		})
	}
}

// normaliseJSON treats a null list as an empty one (Go marshals a nil slice as null).
func normaliseJSON(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, val := range x {
			if val == nil && (k == "warnings" || k == "indexWeightFiles") {
				x[k] = []any{}
			} else {
				x[k] = normaliseJSON(val)
			}
		}
	case []any:
		for i := range x {
			x[i] = normaliseJSON(x[i])
		}
	}
	return v
}

// ---- the deploy lifecycle ---------------------------------------------------------------------------

func TestDeployListDeleteLifecycle(t *testing.T) {
	e := newEnv(t, false)

	if code := e.run("deploy", "hf://Qwen/Qwen3-8B"); code != 0 {
		t.Fatalf("deploy exit %d\n%s", code, e.stderr.String())
	}
	wantDeploy := `Planned: 1x nvidia-l4, tensor-parallel 1, context 17408 (Qwen/Qwen3-8B @ b968826d9c46)
[0:00] record    applied podserve/qwen3-8b
[0:00] fetch     caching Qwen/Qwen3-8B@b968826d9c46 into gs://my-project-podserve-models/models/hf/Qwen/Qwen3-8B/b968826d9c46...
[0:00] fetch     done
[0:00] ready     qwen3-8b is ready
Endpoint (in cluster): http://qwen3-8b.podserve.svc:8000/v1
Try it: podserve port-forward qwen3-8b   then   curl localhost:8000/v1/models
`
	if e.stdout.String() != wantDeploy {
		t.Errorf("deploy stdout\n got:\n%s\nwant:\n%s", e.stdout.String(), wantDeploy)
	}

	// The record's status, written through /status.
	ms, err := e.cluster.Get(kube.ModelServices, "podserve", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	var rec v1alpha1.ModelService
	if err := runtimeFromUnstructured(ms, &rec); err != nil {
		t.Fatal(err)
	}
	st := rec.Status
	if st.Phase != v1alpha1.PhaseReady || st.ResolvedRevision != "b968826d9c46dd6066d109eabc6255188de91218" ||
		st.Endpoint != "http://qwen3-8b.podserve.svc:8000/v1" || st.Profile == nil || st.Profile.GPUCount != 1 ||
		st.Profile.MaxModelLen != 17408 || st.Profile.MachineType != "g2-standard-8" {
		t.Errorf("status after deploy: %+v profile %+v", st, st.Profile)
	}
	dep, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b")
	if err != nil {
		t.Fatal(err)
	}
	if refs := dep.GetOwnerReferences(); len(refs) != 1 || refs[0].UID != ms.GetUID() {
		t.Errorf("deployment owner references %+v, want the record (uid %s)", refs, ms.GetUID())
	}
	if _, err := e.cluster.Get(kube.Jobs, "podserve", platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")); err != nil {
		t.Errorf("fetch job: %v", err)
	}

	// Deploying again reuses the completed fetch Job.
	if code := e.run("deploy", "hf://Qwen/Qwen3-8B"); code != 0 {
		t.Fatalf("second deploy exit %d\n%s", code, e.stderr.String())
	}
	if !strings.Contains(e.stdout.String(), "[0:00] fetch     cached\n") || strings.Contains(e.stdout.String(), "fetch     done") {
		t.Errorf("second deploy should print 'fetch     cached' and no 'done':\n%s", e.stdout.String())
	}

	if code := e.run("list"); code != 0 {
		t.Fatalf("list exit %d", code)
	}
	wantList := "NAME      MODEL               REVISION      GPU           PHASE  ENDPOINT\n" +
		"qwen3-8b  hf://Qwen/Qwen3-8B  b968826d9c46  1x nvidia-l4  Ready  http://qwen3-8b.podserve.svc:8000/v1\n"
	if e.stdout.String() != wantList {
		t.Errorf("list\n got:\n%q\nwant:\n%q", e.stdout.String(), wantList)
	}

	if code := e.run("port-forward", "qwen3-8b"); code != 0 {
		t.Fatalf("port-forward exit %d", code)
	}
	if e.stdout.String() != "Forwarding localhost:8000 -> qwen3-8b (Ctrl-C to stop)\n" {
		t.Errorf("port-forward stdout %q", e.stdout.String())
	}
	if last := e.execs[len(e.execs)-1]; !reflect.DeepEqual(last, []string{"kubectl", "port-forward", "-n", "podserve", "svc/qwen3-8b", "8000:8000"}) {
		t.Errorf("port-forward exec %v", last)
	}

	if code := e.run("delete", "qwen3-8b"); code != 0 {
		t.Fatalf("delete exit %d\n%s", code, e.stderr.String())
	}
	if e.stdout.String() != "Deleted qwen3-8b (pods gone after 0:00)\n" {
		t.Errorf("delete stdout %q", e.stdout.String())
	}
	if _, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b"); err == nil {
		t.Error("deployment survived delete")
	}
	if _, err := e.cluster.Get(kube.Jobs, "podserve", platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")); err == nil {
		t.Error("fetch job survived the delete of its only user")
	}

	if code := e.run("list"); code != 0 || e.stdout.String() != "No models deployed.\n" {
		t.Errorf("empty list: exit %d %q", code, e.stdout.String())
	}
	if code := e.run("delete", "nope"); code != 4 || e.stderr.String() != "podserve: no model named nope\n" {
		t.Errorf("delete unknown: exit %d %q", code, e.stderr.String())
	}
	if code := e.run("port-forward", "nope"); code != 4 || e.stderr.String() != "podserve: no model named nope\n" {
		t.Errorf("port-forward unknown: exit %d %q", code, e.stderr.String())
	}
}

func runtimeFromUnstructured(u *unstructured.Unstructured, into any) error {
	b, err := u.MarshalJSON()
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}

// ---- dry run: the same objects render produces, with a zero owner uid ------------------------------

func TestDeployDryRun(t *testing.T) {
	e := newEnv(t, false)
	if code := e.run("deploy", "hf://Qwen/Qwen3-8B", "--dry-run"); code != 0 {
		t.Fatalf("exit %d\n%s", code, e.stderr.String())
	}
	if n := len(e.cluster.Actions()); n > 2 {
		for _, a := range e.cluster.Actions() {
			if v := a.GetVerb(); v != "get" && v != "list" {
				t.Errorf("dry run wrote to the cluster: %s %s", v, a.GetResource().Resource)
			}
		}
	}
	out := e.stdout.String()
	docsOut := strings.Split(out[strings.Index(out, "apiVersion:"):], "\n---\n")
	want, err := os.ReadFile(filepath.Join(testdataDir(t), "render", "qwen3-8b.golden.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	wantText := strings.ReplaceAll(string(want), "11111111-2222-3333-4444-555555555555", "00000000-0000-0000-0000-000000000000")
	docsWant := strings.Split(wantText, "\n---\n")
	if len(docsOut) != len(docsWant) {
		t.Fatalf("dry run printed %d documents, want %d:\n%s", len(docsOut), len(docsWant), out)
	}
	for i := range docsWant {
		g, w := pruned(t, docsOut[i]), pruned(t, docsWant[i])
		if !reflect.DeepEqual(g, w) {
			gy, _ := yaml.Marshal(g)
			wy, _ := yaml.Marshal(w)
			t.Errorf("document %d differs\n--- got:\n%s\n--- want:\n%s", i, gy, wy)
		}
	}
}

func pruned(t *testing.T, doc string) any {
	t.Helper()
	j, err := yaml.YAMLToJSON([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	var v any
	_ = json.Unmarshal(j, &v)
	return prune(v)
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
	}
	return v
}

// ---- deploy failures --------------------------------------------------------------------------------

func TestDeployFailures(t *testing.T) {
	t.Run("not initialised", func(t *testing.T) {
		e := newEnv(t, true)
		if code := e.run("deploy", "hf://Qwen/Qwen3-8B"); code != 4 ||
			e.stderr.String() != "podserve: platform not initialised: ConfigMap podserve/podserve-platform not found; run podserve init\n" {
			t.Errorf("exit %d stderr %q", code, e.stderr.String())
		}
	})

	t.Run("not deployable", func(t *testing.T) {
		e := newEnv(t, false)
		code := e.run("deploy", "hf://Qwen/Qwen3-235B-A22B")
		want := "podserve: cannot serve Qwen/Qwen3-235B-A22B: it needs 8x nvidia-a100-80gb, which podserve v0.1 does not deploy (v0.1 deploys nvidia-l4 only) (NOT_DEPLOYABLE)\n"
		if code != 3 || e.stderr.String() != want || e.stdout.String() != "" {
			t.Errorf("exit %d\nstdout %q\nstderr %q\nwant   %q", code, e.stdout.String(), e.stderr.String(), want)
		}
	})

	t.Run("invalid spec file", func(t *testing.T) {
		e := newEnv(t, false)
		f := filepath.Join(t.TempDir(), "bad.yaml")
		os.WriteFile(f, []byte("apiVersion: podserve.dev/v1alpha1\nkind: ModelService\nmetadata: {name: Bad_Name}\nspec:\n  model: {source: hf://Qwen/Qwen3-8B}\n  replicas: 9\n"), 0o644)
		code := e.run("deploy", "-f", f)
		want := "podserve: invalid spec: metadata.name: must be a lowercase DNS label of at most 40 characters (a-z, 0-9, '-', starting with a letter)\n" +
			"podserve: invalid spec: spec.replicas: must be between 1 and 8\n"
		if code != 2 || e.stderr.String() != want {
			t.Errorf("exit %d stderr\n%s\nwant\n%s", code, e.stderr.String(), want)
		}
	})

	t.Run("fetch job fails", func(t *testing.T) {
		e := newEnv(t, false)
		e.cluster.AutoReady = false
		job := platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")
		e.onSleep = func(int) { _ = e.cluster.FailJob("podserve", job, `{"event":"error","code":"SIZE_MISMATCH"}`) }
		code := e.run("deploy", "hf://Qwen/Qwen3-8B")
		if code != 6 || e.stderr.String() != "podserve: fetching Qwen/Qwen3-8B failed: SIZE_MISMATCH\n" {
			t.Errorf("exit %d stderr %q", code, e.stderr.String())
		}
		ms, err := e.cluster.Get(kube.ModelServices, "podserve", "qwen3-8b")
		if err != nil {
			t.Fatalf("no ModelService after a failed fetch: %v", err)
		}
		if phase, _, _ := unstructured.NestedString(ms.Object, "status", "phase"); phase != "Failed" {
			t.Errorf("status phase %q, want Failed", phase)
		}
	})

	t.Run("timeout", func(t *testing.T) {
		e := newEnv(t, false)
		e.cluster.AutoReady = false
		e.advance = true
		job := platform.FetchJobName("Qwen/Qwen3-8B", "b968826d9c46dd6066d109eabc6255188de91218")
		e.onSleep = func(int) {
			if _, err := e.cluster.Get(kube.Jobs, "podserve", job); err == nil {
				_ = e.cluster.CompleteJob("podserve", job)
			}
			if _, err := e.cluster.Get(kube.Deployments, "podserve", "qwen3-8b"); err == nil {
				_ = e.cluster.SetServingPods("podserve", "qwen3-8b", fakekube.StageLoading)
			}
		}
		code := e.run("deploy", "hf://Qwen/Qwen3-8B", "--timeout", "2m")
		want := "podserve: qwen3-8b not ready after 2m0s (stage: loading); see: kubectl -n podserve logs deploy/qwen3-8b\n"
		if code != 5 || e.stderr.String() != want {
			t.Errorf("exit %d stderr %q\nwant %q", code, e.stderr.String(), want)
		}
		if !strings.Contains(e.stdout.String(), "] loading   loading weights and warming up\n") {
			t.Errorf("no loading stage line:\n%s", e.stdout.String())
		}
	})
}

// ---- init -------------------------------------------------------------------------------------------

func initDir(t *testing.T, dockerfile bool) {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join(testdataDir(t), "cli", "terraform-output.json"))
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "terraform-output.json"), b, 0o644)
	var outputs map[string]any
	_ = json.Unmarshal(b, &outputs)
	delete(outputs, "build_service_account")
	missing, _ := json.Marshal(outputs)
	os.WriteFile(filepath.Join(dir, "terraform-output-missing.json"), missing, 0o644)
	if dockerfile {
		os.MkdirAll(filepath.Join(dir, "images", "fetch"), 0o755)
		os.WriteFile(filepath.Join(dir, "images", "fetch", "Dockerfile"), []byte("FROM scratch\n"), 0o644)
	}
	t.Chdir(dir)
}

func TestInit(t *testing.T) {
	const digest = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	t.Run("builds the fetch image and installs", func(t *testing.T) {
		initDir(t, true)
		e := newEnv(t, true)
		if code := e.run("init", "--config", "terraform-output.json"); code != 0 {
			t.Fatalf("exit %d\n%s", code, e.stderr.String())
		}
		want := "Building the fetch image with Cloud Build...\n" +
			"Fetch image: us-central1-docker.pkg.dev/my-project/podserve/fetch@" + digest + "\n" +
			"Installed CRD modelservices.podserve.dev\n" +
			"Created namespace podserve and service accounts podserve-fetch, podserve-model\n" +
			"Saved platform settings to podserve/podserve-platform\n" +
			"podserve is ready. Next: podserve deploy hf://Qwen/Qwen3-8B\n"
		if e.stdout.String() != want {
			t.Errorf("stdout\n got:\n%s\nwant:\n%s", e.stdout.String(), want)
		}
		wantExecs := [][]string{
			{"gcloud", "builds", "submit", "images/fetch", "--project", "my-project", "--region", "us-central1",
				"--tag", "us-central1-docker.pkg.dev/my-project/podserve/fetch:v0.1.0-test",
				"--service-account", "projects/my-project/serviceAccounts/podserve-build@my-project.iam.gserviceaccount.com",
				"--gcs-source-staging-dir", "gs://my-project-podserve-models/_build/source",
				"--gcs-log-dir", "gs://my-project-podserve-models/_build/logs"},
			{"gcloud", "artifacts", "docker", "images", "describe", "us-central1-docker.pkg.dev/my-project/podserve/fetch:v0.1.0-test",
				"--project", "my-project", "--format", "value(image_summary.digest)"},
		}
		if !reflect.DeepEqual(e.execs, wantExecs) {
			t.Errorf("execs\n got: %v\nwant: %v", e.execs, wantExecs)
		}
		cm, err := e.cluster.Get(kube.ConfigMaps, "podserve", "podserve-platform")
		if err != nil {
			t.Fatal(err)
		}
		data, _, _ := unstructured.NestedStringMap(cm.Object, "data")
		wantData := testPlatform.Data()
		wantData["fetchImage"] = "us-central1-docker.pkg.dev/my-project/podserve/fetch@" + digest
		if !reflect.DeepEqual(data, wantData) {
			t.Errorf("ConfigMap data\n got: %v\nwant: %v", data, wantData)
		}
		if _, err := e.cluster.Get(kube.CRDs, "", "modelservices.podserve.dev"); err != nil {
			t.Errorf("CRD: %v", err)
		}
	})

	t.Run("given fetch image", func(t *testing.T) {
		initDir(t, false)
		e := newEnv(t, true)
		ref := "us-central1-docker.pkg.dev/my-project/podserve/fetch@sha256:1111111111111111111111111111111111111111111111111111111111111111"
		if code := e.run("init", "--config", "terraform-output.json", "--fetch-image", ref); code != 0 {
			t.Fatalf("exit %d\n%s", code, e.stderr.String())
		}
		if !strings.HasPrefix(e.stdout.String(), "Fetch image: "+ref+" (given)\nInstalled CRD") || len(e.execs) != 0 {
			t.Errorf("stdout %q execs %v", e.stdout.String(), e.execs)
		}
	})

	t.Run("missing output", func(t *testing.T) {
		initDir(t, true)
		e := newEnv(t, true)
		code := e.run("init", "--config", "terraform-output-missing.json")
		if code != 2 || e.stderr.String() != "podserve: terraform-output-missing.json has no output \"build_service_account\"\n" {
			t.Errorf("exit %d stderr %q", code, e.stderr.String())
		}
	})

	t.Run("not in the repository", func(t *testing.T) {
		initDir(t, false)
		e := newEnv(t, true)
		code := e.run("init", "--config", "terraform-output.json")
		if code != 2 || e.stderr.String() != "podserve: images/fetch not found; run init from the podserve repository root or pass --fetch-image\n" {
			t.Errorf("exit %d stderr %q", code, e.stderr.String())
		}
	})
}
