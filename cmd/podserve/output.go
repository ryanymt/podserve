package main

// output.go holds what the commands print: plan's text and JSON documents, the ModelService status
// built from a profile, YAML documents (dry-run), and deploy's derived names.

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"

	"github.com/ryanymt/podserve/catalog"
	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
	"github.com/ryanymt/podserve/pkg/profile"
)

// ---- plan -----------------------------------------------------------------------------------------

// modelJSON is plan's "model" object (docs/design/cli.md).
type modelJSON struct {
	Repo        string `json:"repo"`
	Revision    string `json:"revision"`
	PipelineTag string `json:"pipelineTag"`
	License     string `json:"license"`
	Gated       bool   `json:"gated"`
}

// refusalJSON is plan's "refusal" object.
type refusalJSON struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// planJSON is the -o json document: a model with either its profile or its refusal.
type planJSON struct {
	Model   modelJSON       `json:"model"`
	Profile *profile.Result `json:"profile,omitempty"`
	Refusal *refusalJSON    `json:"refusal,omitempty"`
}

// printPlanJSON writes plan's JSON document, indented by two spaces. A refusal also exits 3, with
// nothing on stderr (docs/design/cli.md).
func printPlanJSON(out io.Writer, m profile.Model, res *profile.Result, ref *refusal) error {
	doc := planJSON{
		Model: modelJSON{
			Repo: m.Repo, Revision: m.Revision, PipelineTag: m.PipelineTag,
			License: m.License, Gated: m.Gated,
		},
		Profile: res,
	}
	if ref != nil {
		doc.Profile = nil
		doc.Refusal = &refusalJSON{Code: ref.code, Message: ref.message}
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	fmt.Fprintln(out, string(b))
	if ref != nil {
		return &exitError{code: exitRefused}
	}
	return nil
}

// printPlanText writes plan's text document (docs/design/cli.md): labels padded to 14 columns, GiB
// sizes to one decimal, then each warning.
func printPlanText(out io.Writer, m profile.Model, res profile.Result) {
	tag := m.PipelineTag
	if tag == "" {
		tag = "no pipeline tag"
	}
	license := m.License
	if license == "" {
		license = "unknown licence"
	}
	maxPos := 0
	if m.Config != nil {
		maxPos = m.Config.MaxPositionEmbeddings
	}
	fmt.Fprintf(out, "%-14s%s @ %s (%s, %s)\n", "Model:", m.Repo, shortSHA(m.Revision), tag, license)
	fmt.Fprintf(out, "%-14s%s (supported by vLLM %s)\n", "Architecture:", firstArch(m), catalog.VLLMVersion)
	fmt.Fprintf(out, "%-14s%s GiB, served as %s\n", "Weights:", gib1(res.WeightsBytes), res.Dtype)
	fmt.Fprintf(out, "%-14s%dx %s on %s (%s CPU, %s)\n", "GPU:",
		res.GPUCount, res.GPUType, res.Shape.MachineType, res.Shape.CPU, res.Shape.Memory)
	fmt.Fprintf(out, "%-14stensor-parallel %d\n", "Parallelism:", res.TensorParallelSize)
	fmt.Fprintf(out, "%-14s%d tokens (model maximum %d)\n", "Context:", res.MaxModelLen, maxPos)
	fmt.Fprintf(out, "%-14s%s GiB, %d tokens, %d sequence(s) at full context\n", "KV cache:",
		gib1(res.KVCacheBytes), res.TokenCapacity, res.SeqsAtMaxLen)
	if res.Deployable {
		fmt.Fprintf(out, "%-14syes\n", "Deployable:")
	} else {
		fmt.Fprintf(out, "%-14sno (podserve v0.1 deploys nvidia-l4 only)\n", "Deployable:")
	}
	for _, w := range res.Warnings {
		fmt.Fprintf(out, "warning: %s\n", w)
	}
}

// firstArch is the model's first architecture, "" when config.json names none.
func firstArch(m profile.Model) string {
	if m.Config == nil {
		return ""
	}
	if len(m.Config.Architectures) == 0 {
		return ""
	}
	return m.Config.Architectures[0]
}

// gib1 renders bytes as GiB to one decimal place: round half away from zero on the second digit,
// like the settings engine's own messages (pkg/profile's gib1).
func gib1(b int64) string {
	const gib = float64(int64(1) << 30)
	return fmt.Sprintf("%.1f", math.Floor(float64(b)/gib*10+0.5)/10)
}

// shortSHA is a commit SHA's first 12 characters.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}

// cut trims a message to 200 characters (runes) for one line of output: a longer one keeps its
// first 199 and ends with "…" (docs/design/cli.md, deploy "Waiting reasons"; the failure output, the
// stored status and podserve status carry the whole message).
func cut(msg string) string {
	r := []rune(msg)
	if len(r) <= 200 {
		return msg
	}
	return string(r[:199]) + "…"
}

// ---- status ---------------------------------------------------------------------------------------

// statusProfile is a chosen profile as the ModelService records it.
func statusProfile(res profile.Result) *v1alpha1.ProfileStatus {
	return &v1alpha1.ProfileStatus{
		GPUType:            res.GPUType,
		GPUCount:           int32(res.GPUCount),
		MachineType:        res.Shape.MachineType,
		TensorParallelSize: int32(res.TensorParallelSize),
		Dtype:              res.Dtype,
		MaxModelLen:        int32(res.MaxModelLen),
		GPUMemoryPercent:   int32(res.GPUMemoryPercent),
		WeightsBytes:       res.WeightsBytes,
		KVCacheBytes:       res.KVCacheBytes,
		KVBytesPerToken:    res.KVBytesPerToken,
		TokenCapacity:      res.TokenCapacity,
	}
}

// deployStatus is the ModelService status a deploy writes: the resolved revision and the chosen
// profile throughout, and the phase the deploy has reached.
func deployStatus(m profile.Model, res profile.Result, phase v1alpha1.Phase) v1alpha1.ModelServiceStatus {
	return v1alpha1.ModelServiceStatus{
		Phase:            phase,
		ResolvedRevision: m.Revision,
		Profile:          statusProfile(res),
	}
}

// failedStatus is the status a failed deploy records.
func failedStatus(m profile.Model, res profile.Result, message string) v1alpha1.ModelServiceStatus {
	st := deployStatus(m, res, v1alpha1.PhaseFailed)
	st.Message = message
	return st
}

// ---- YAML documents (dry-run) ---------------------------------------------------------------------

// toUnstructured converts any Kubernetes object to an *unstructured.Unstructured: an
// *unstructured.Unstructured is its own content, anything typed goes through the unstructured
// converter (docs/design/cli.md).
func toUnstructured(obj runtime.Object) (*unstructured.Unstructured, error) {
	if u, ok := obj.(*unstructured.Unstructured); ok {
		return &unstructured.Unstructured{Object: u.Object}, nil
	}
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: m}, nil
}

// yamlDoc renders one Kubernetes object as a YAML document.
func yamlDoc(obj runtime.Object) ([]byte, error) {
	u, err := toUnstructured(obj)
	if err != nil {
		return nil, err
	}
	return yaml.Marshal(u.Object)
}

// printYAMLDocs writes objects as YAML documents separated by ---.
func printYAMLDocs(out io.Writer, objs []runtime.Object) error {
	for i, obj := range objs {
		if i > 0 {
			fmt.Fprint(out, "---\n")
		}
		b, err := yamlDoc(obj)
		if err != nil {
			return failf(exitUnexpected, "%s", err)
		}
		if _, err := out.Write(b); err != nil {
			return failf(exitUnexpected, "%s", err)
		}
	}
	return nil
}

// dryRunOwner is the owner reference dry-run renders with: no real ModelService uid exists, so a
// zero uuid stands in (docs/design/cli.md).
func dryRunOwner(name string) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion:         v1alpha1.GroupVersion.String(),
		Kind:               v1alpha1.Kind,
		Name:               name,
		UID:                "00000000-0000-0000-0000-000000000000",
		Controller:         &yes,
		BlockOwnerDeletion: &yes,
	}
}

// ---- names ----------------------------------------------------------------------------------------

// derivedName is deploy's default ModelService name (docs/design/cli.md): the model part of the
// repo, lowercased, reduced to [a-z0-9-], trimmed of '-', cut to 40 characters, and prefixed m-
// when it doesn't start with a letter. Qwen/Qwen3-8B -> qwen3-8b.
func derivedName(repo string) string {
	_, model, _ := strings.Cut(repo, "/")
	var b strings.Builder
	for _, r := range strings.ToLower(model) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	for strings.Contains(s, "--") {
		s = strings.ReplaceAll(s, "--", "-")
	}
	s = strings.Trim(s, "-")
	if len(s) > 40 {
		s = s[:40]
	}
	if s == "" || s[0] < 'a' || s[0] > 'z' {
		s = "m-" + s
	}
	return s
}
