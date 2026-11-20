package main

// status.go is `podserve status NAME [-o text|json]` (docs/design/cli.md): one model's record, and
// where its serving pods are now — the same diagnosis deploy waits on (kube.Diagnose, D-025). A
// stopped model (phase Failed, or scaled to zero) reports its stored message instead of pods.

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/ryanymt/podserve/pkg/api/v1alpha1"
	"github.com/ryanymt/podserve/pkg/kube"
)

// statusJSON is `status -o json`'s document: one object, no key omitted (docs/design/cli.md).
type statusJSON struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	GPUType  string `json:"gpuType"`
	GPUCount int32  `json:"gpuCount"`
	Spot     bool   `json:"spot"`
	Phase    string `json:"phase"`
	Stage    string `json:"stage"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Endpoint string `json:"endpoint"`
}

func statusCmd(d deps) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "status NAME",
		Short: "Show a model's phase, current stage and the reason it is waiting or failed",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			ctx := cmd.Context()
			kc, err := clusterClient(d, 0, nil)
			if err != nil {
				return err
			}
			if _, err := kc.PlatformData(ctx); err != nil {
				if errors.Is(err, kube.ErrPlatformNotInitialised) {
					return failf(exitNotFound, "%s", kube.ErrPlatformNotInitialised)
				}
				return failf(exitUnexpected, "%s", err)
			}
			ms, err := kc.Get(ctx, name)
			if err != nil {
				return notFoundOrFail(name, err)
			}
			serving, err := kc.Serving(ctx, name)
			if err != nil {
				return failf(exitUnexpected, "%s", err)
			}
			r := statusOf(ms, serving, d.Now())
			if output == "json" {
				return printStatusJSON(d.Stdout, r)
			}
			printStatusText(d.Stdout, r)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

// statusReport is one model as `podserve status` shows it: the record's display fields, plus the
// live diagnosis (stage, reason, message). For a stopped model the diagnosis stays at its zero
// values and the message is the stored one.
type statusReport struct {
	name, source, revision string // revision is the resolved revision's first 12 characters
	hasProfile             bool
	gpuType                string
	gpuCount               int32
	spot                   bool
	phase, endpoint        string
	deployed               bool // a Deployment exists: the stopped stage can name its replica count
	stopped                bool // phase Failed, or the Deployment scaled to zero
	stage                  kube.Stage
	reason                 kube.Reason
	message                string
}

// statusOf reads one model's record and diagnoses its serving pods as of now. A stopped model —
// phase Failed, or a Deployment with zero replicas — reports only its stored message: nothing is
// read from pods (docs/design/cli.md).
func statusOf(ms *v1alpha1.ModelService, serving kube.Serving, now time.Time) statusReport {
	r := statusReport{
		name:     ms.Name,
		source:   ms.Spec.Model.Source,
		revision: shortSHA(ms.Status.ResolvedRevision),
		phase:    string(ms.Status.Phase),
		endpoint: ms.Status.Endpoint,
		message:  ms.Status.Message,
		spot:     ms.Spec.Accelerator.Spot == nil || *ms.Spec.Accelerator.Spot, // nil is the spec default
	}
	if p := ms.Status.Profile; p != nil {
		r.hasProfile, r.gpuType, r.gpuCount = true, p.GPUType, p.GPUCount
	}
	replicas := int64(0)
	if serving.Deployment != nil {
		r.deployed = true
		if n, ok, _ := unstructured.NestedInt64(serving.Deployment.Object, "spec", "replicas"); ok {
			replicas = n
		}
	}
	r.stopped = ms.Status.Phase == v1alpha1.PhaseFailed || r.deployed && replicas == 0
	if r.stopped {
		return r
	}
	d := kube.Diagnose(serving, now)
	r.stage, r.reason, r.message = d.Stage, d.Reason, d.Message
	return r
}

// printStatusText writes the text document: label and value, the labels padded to 11 columns, a
// missing value as - (docs/design/cli.md).
func printStatusText(out io.Writer, r statusReport) {
	fmt.Fprintf(out, "%-11s%s\n", "Name:", orDash(r.name))
	fmt.Fprintf(out, "%-11s%s\n", "Model:", orDash(modelLine(r)))
	fmt.Fprintf(out, "%-11s%s\n", "GPU:", orDash(gpuLine(r)))
	fmt.Fprintf(out, "%-11s%s\n", "Phase:", orDash(r.phase))
	fmt.Fprintf(out, "%-11s%s\n", "Stage:", orDash(stageValue(r)))
	fmt.Fprintf(out, "%-11s%s\n", "Reason:", orDash(reasonValue(r)))
	fmt.Fprintf(out, "%-11s%s\n", "Endpoint:", orDash(r.endpoint))
}

// modelLine is the source, @, and the resolved revision's first 12 characters; the revision is left
// out when the record has none yet.
func modelLine(r statusReport) string {
	if r.revision == "" {
		return r.source
	}
	return r.source + " @ " + r.revision
}

// gpuLine is the recorded profile's GPU and capacity mode, "" when the record has no profile.
func gpuLine(r statusReport) string {
	if !r.hasProfile {
		return ""
	}
	mode := "on-demand"
	if r.spot {
		mode = "spot"
	}
	return fmt.Sprintf("%dx %s, %s", r.gpuCount, r.gpuType, mode)
}

// stageValue is where the model stands: the diagnosis's stage, or "stopped (replicas 0)" behind a
// stopped model's Deployment; with no Deployment there is no stage.
func stageValue(r statusReport) string {
	switch {
	case r.stopped && r.deployed:
		return "stopped (replicas 0)"
	case r.stopped:
		return ""
	default:
		return string(r.stage)
	}
}

// reasonValue is why the model is waiting or failed: the diagnosis's reason and message, or a
// stopped model's stored message. The message is never cut (docs/design/cli.md).
func reasonValue(r statusReport) string {
	if r.stopped {
		return r.message
	}
	if r.reason == "" {
		return ""
	}
	return fmt.Sprintf("%s: %s", r.reason, r.message)
}

// printStatusJSON writes one JSON object (docs/design/cli.md). A stopped model's stage is
// "stopped", or "" with no Deployment; its reason is "" and its message the stored one.
func printStatusJSON(out io.Writer, r statusReport) error {
	var stage string
	switch {
	case r.stopped && r.deployed:
		stage = "stopped"
	case r.stopped:
	default:
		stage = string(r.stage)
	}
	doc := statusJSON{
		Name:     r.name,
		Source:   r.source,
		Revision: r.revision,
		GPUType:  r.gpuType,
		GPUCount: r.gpuCount,
		Spot:     r.spot,
		Phase:    r.phase,
		Stage:    stage,
		Reason:   string(r.reason), // "" for a stopped model: its reason code is not stored
		Message:  r.message,
		Endpoint: r.endpoint,
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	fmt.Fprintln(out, string(b))
	return nil
}
