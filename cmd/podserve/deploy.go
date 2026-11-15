package main

// deploy.go is `podserve deploy`: build (or load) a ModelService, default and validate it, read the
// platform settings, plan the profile, and — unless --dry-run — walk the cluster through caching,
// applying and waiting (docs/design/cli.md, deploy).

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/hf"
	"github.com/podserve/podserve/pkg/kube"
	"github.com/podserve/podserve/pkg/platform"
	"github.com/podserve/podserve/pkg/profile"
	"github.com/podserve/podserve/pkg/render"
)

// specFlags are deploy's flags that map to spec fields; they cannot be combined with -f.
var specFlags = []string{
	"revision", "gpu", "gpus", "max-model-len", "gpu-memory-percent",
	"name", "served-model-name", "spot", "replicas",
}

type deployOpts struct {
	// plan's flags (without -o)
	revision    string
	gpu         string
	gpus        int
	maxModelLen int
	gpuMemPct   int
	// deploy's own
	name     string
	served   string
	spot     bool
	replicas int32
	timeout  time.Duration
	dryRun   bool
	file     string
}

func deployCmd(d deps) *cobra.Command {
	opts := &deployOpts{
		revision:  "main",
		gpu:       "auto",
		gpuMemPct: profile.DefaultGPUMemoryPercent,
		spot:      true,
		replicas:  1,
		timeout:   30 * time.Minute,
	}
	cmd := &cobra.Command{
		Use:   "deploy (SOURCE [flags] | -f FILE)",
		Short: "Deploy a Hugging Face model on the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDeploy(cmd, args, d, opts)
		},
	}
	cmd.Flags().StringVar(&opts.revision, "revision", opts.revision, "model revision (branch, tag or commit SHA)")
	cmd.Flags().StringVar(&opts.gpu, "gpu", opts.gpu, "GPU type from the catalogue, or auto")
	cmd.Flags().IntVar(&opts.gpus, "gpus", opts.gpus, "number of GPUs per replica (0 = choose)")
	cmd.Flags().IntVar(&opts.maxModelLen, "max-model-len", opts.maxModelLen, "context length cap in tokens (0 = choose)")
	cmd.Flags().IntVar(&opts.gpuMemPct, "gpu-memory-percent", opts.gpuMemPct, "share of each GPU's memory vLLM may use")
	cmd.Flags().StringVar(&opts.name, "name", opts.name, "name to deploy under (default: derived from the model)")
	cmd.Flags().StringVar(&opts.served, "served-model-name", opts.served, "the model name clients send (default: the repo id)")
	cmd.Flags().BoolVar(&opts.spot, "spot", opts.spot, "ask for spot GPU capacity")
	cmd.Flags().Int32Var(&opts.replicas, "replicas", opts.replicas, "number of serving pods")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", opts.timeout, "how long to wait for the model to become ready")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "print the rendered objects as YAML instead of deploying")
	cmd.Flags().StringVarP(&opts.file, "file", "f", opts.file, "load the ModelService from a YAML file")
	return cmd
}

// buildSpec assembles the ModelService from SOURCE and flags, or from -f FILE, and defaults and
// validates it (docs/design/cli.md). The parsed source comes back for the Hub and rendering calls.
func buildSpec(cmd *cobra.Command, args []string, o *deployOpts) (*v1alpha1.ModelService, modelSource, error) {
	var (
		ms  v1alpha1.ModelService
		src modelSource
	)
	if o.file != "" {
		if len(args) > 0 || anyChanged(cmd, specFlags...) {
			return nil, modelSource{}, failf(exitUsage, "-f FILE cannot be combined with SOURCE or spec flags")
		}
		b, err := os.ReadFile(o.file)
		if err != nil {
			return nil, modelSource{}, failf(exitUsage, "%s", err)
		}
		loaded, err := v1alpha1.Load(b)
		if err != nil {
			return nil, modelSource{}, failf(exitUsage, "%s", err)
		}
		ms = *loaded
		repo, err := platform.RepoFromSource(ms.Spec.Model.Source)
		if err != nil {
			return nil, modelSource{}, failf(exitUsage, "%s", err)
		}
		src = modelSource{repo: repo, rev: ms.Spec.Model.Revision}
	} else {
		if len(args) != 1 {
			return nil, modelSource{}, failf(exitUsage, "want a model source (hf://ORG/MODEL) or -f FILE")
		}
		parsed, err := parseModelSource(args[0])
		if err != nil {
			return nil, modelSource{}, err
		}
		if err := checkRevisionFlag(parsed, cmd.Flags().Changed("revision")); err != nil {
			return nil, modelSource{}, err
		}
		src = parsed
		if src.rev == "" {
			src.rev = o.revision
		}
		ms.Name = o.name
		if ms.Name == "" {
			ms.Name = derivedName(src.repo)
		}
		ms.Spec.Model.Source = "hf://" + src.repo
		ms.Spec.Model.Revision = src.rev
		if o.served != "" {
			ms.Spec.ServedModelName = o.served
		}
		ms.Spec.Accelerator.Type = o.gpu
		ms.Spec.Accelerator.Count = int32(o.gpus)
		ms.Spec.Accelerator.Spot = &o.spot
		ms.Spec.Engine.MaxModelLen = int32(o.maxModelLen)
		ms.Spec.Engine.GPUMemoryPercent = int32(o.gpuMemPct)
		ms.Spec.Replicas = &o.replicas
	}
	v1alpha1.Default(&ms)
	if errs := v1alpha1.Validate(&ms); len(errs) > 0 {
		lines := make([]string, len(errs))
		for i, e := range errs {
			lines[i] = "invalid spec: " + e.Error()
		}
		return nil, modelSource{}, failLines(exitUsage, lines...)
	}
	return &ms, src, nil
}

// anyChanged reports whether any named flag was set on the command line.
func anyChanged(cmd *cobra.Command, names ...string) bool {
	for _, n := range names {
		if cmd.Flags().Changed(n) {
			return true
		}
	}
	return false
}

// runDeploy walks deploy's order: spec, platform settings, plan, then — unless --dry-run — the
// cluster (docs/design/cli.md).
func runDeploy(cmd *cobra.Command, args []string, d deps, o *deployOpts) error {
	ctx := cmd.Context()
	start := d.Now()

	ms, src, err := buildSpec(cmd, args, o)
	if err != nil {
		return err
	}

	// The stage lines of the wait; the profile they name is filled in by planning below.
	res := &profile.Result{}
	kc, err := clusterClient(d, o.timeout, deployProgress(d, start, ms, res))
	if err != nil {
		return err
	}

	data, err := kc.PlatformData(ctx)
	if err != nil {
		if errors.Is(err, kube.ErrPlatformNotInitialised) {
			return failf(exitNotFound, "%s", kube.ErrPlatformNotInitialised)
		}
		return failf(exitUnexpected, "%s", err)
	}
	plat, err := platform.FromData(data)
	if err != nil {
		return failf(exitNotFound, "%s", err)
	}

	m, planned, ref, err := planRevision(ctx, d, src, profile.Request{
		GPUType:          ms.Spec.Accelerator.Type,
		GPUCount:         int(ms.Spec.Accelerator.Count),
		MaxModelLen:      int(ms.Spec.Engine.MaxModelLen),
		GPUMemoryPercent: int(ms.Spec.Engine.GPUMemoryPercent),
		HasToken:         d.Getenv("HF_TOKEN") != "",
	})
	if err != nil {
		return err
	}
	if ref != nil {
		return ref.error(src.repo)
	}
	if !planned.Deployable {
		return refusal{
			code:    "NOT_DEPLOYABLE",
			message: fmt.Sprintf("it needs %dx %s, which podserve v0.1 does not deploy (v0.1 deploys nvidia-l4 only)", planned.GPUCount, planned.GPUType),
		}.error(src.repo)
	}
	*res = planned
	fmt.Fprintf(d.Stdout, "Planned: %dx %s, tensor-parallel %d, context %d (%s @ %s)\n",
		res.GPUCount, res.GPUType, res.TensorParallelSize, res.MaxModelLen, src.repo, shortSHA(m.Revision))

	in := render.Input{
		Service:               ms,
		Repo:                  src.repo,
		Revision:              m.Revision,
		Profile:               *res,
		Platform:              plat,
		LargestFetchFileBytes: hf.LargestFetchFile(m),
	}
	if o.dryRun {
		rec, err := render.Record(in)
		if err != nil {
			return failf(exitUnexpected, "%s", err)
		}
		job, err := render.FetchJob(in)
		if err != nil {
			return failf(exitUnexpected, "%s", err)
		}
		dep, svc, pm, err := render.Serving(in, dryRunOwner(ms.Name))
		if err != nil {
			return failf(exitUnexpected, "%s", err)
		}
		return printYAMLDocs(d.Stdout, []runtime.Object{rec, job, dep, svc, pm})
	}

	owner, err := kc.ApplyRecord(ctx, ms)
	if err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	stageLine(d.Stdout, start, d, "record", "applied podserve/"+ms.Name)

	if err := kc.UpdateStatus(ctx, ms.Name, deployStatus(m, *res, v1alpha1.PhaseCaching)); err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	stageLine(d.Stdout, start, d, "fetch", fmt.Sprintf("caching %s@%s into gs://%s/%s...",
		src.repo, shortSHA(m.Revision), plat.Bucket, platform.ModelPrefix(src.repo, shortSHA(m.Revision))))

	job, err := render.FetchJob(in)
	if err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	outcome, err := kc.EnsureFetchJob(ctx, job)
	if err != nil {
		return fetchFailed(ctx, kc, ms, src.repo, m, *res, plat, err)
	}
	if outcome == kube.FetchCopied {
		stageLine(d.Stdout, start, d, "fetch", "done")
	} else {
		stageLine(d.Stdout, start, d, "fetch", "cached")
	}

	if err := kc.UpdateStatus(ctx, ms.Name, deployStatus(m, *res, v1alpha1.PhaseDeploying)); err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	dep, svc, pm, err := render.Serving(in, owner)
	if err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	if err := kc.ApplyChildren(ctx, toUnstructuredObj(dep), toUnstructuredObj(svc), pm); err != nil {
		return failf(exitUnexpected, "%s", err)
	}

	if err := kc.WaitReady(ctx, ms.Name); err != nil {
		return waitFailed(ctx, kc, ms, m, *res, err)
	}

	endpoint := fmt.Sprintf("http://%s.podserve.svc:%d/v1", ms.Name, platform.ServingPort)
	st := deployStatus(m, *res, v1alpha1.PhaseReady)
	st.Endpoint = endpoint
	if err := kc.UpdateStatus(ctx, ms.Name, st); err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	fmt.Fprintf(d.Stdout, "Endpoint (in cluster): %s\n", endpoint)
	fmt.Fprintf(d.Stdout, "Try it: podserve port-forward %s   then   curl localhost:%d/v1/models\n",
		ms.Name, platform.ServingPort)
	return nil
}

// toUnstructuredObj converts a typed object for ApplyChildren.
func toUnstructuredObj(obj runtime.Object) *unstructured.Unstructured {
	u, err := toUnstructured(obj)
	if err != nil {
		panic("render: cannot convert to unstructured: " + err.Error()) // podserve's own types: cannot fail
	}
	return u
}

// fetchFailed writes Failed and maps a fetch Job failure to its exit code and hint (docs/design/cli.md).
func fetchFailed(ctx context.Context, kc *kube.Client, ms *v1alpha1.ModelService, repo string,
	m profile.Model, res profile.Result, plat platform.Platform, err error,
) error {
	var fe *kube.FetchError
	if !errors.As(err, &fe) {
		return waitFailed(ctx, kc, ms, m, res, err)
	}
	message := fmt.Sprintf("fetching %s failed: %s", repo, fe.Code)
	_ = kc.UpdateStatus(ctx, ms.Name, failedStatus(m, res, message))
	lines := []string{message}
	if fe.Code == string(profile.GatedNoToken) {
		lines = append(lines, "add a token with: gcloud secrets versions add "+plat.HFTokenSecret+" --data-file=-")
	}
	return failLines(exitFetchFailed, lines...)
}

// waitFailed writes Failed and maps a wait failure to its exit code (docs/design/cli.md).
func waitFailed(ctx context.Context, kc *kube.Client, ms *v1alpha1.ModelService,
	m profile.Model, res profile.Result, err error,
) error {
	var te *kube.TimeoutError
	if !errors.As(err, &te) {
		return failf(exitUnexpected, "%s", err)
	}
	message := fmt.Sprintf("not ready after %s (stage: %s)", te.After, te.Stage)
	_ = kc.UpdateStatus(ctx, ms.Name, failedStatus(m, res, message))
	return failf(exitTimeout, "%s not ready after %s (stage: %s); see: kubectl -n podserve logs deploy/%s",
		ms.Name, te.After, te.Stage, ms.Name)
}

// deployProgress builds kube.Options.Progress: the stage lines of WaitReady, printed as each stage
// is reached. The fetch stage's lines are printed around EnsureFetchJob instead (docs/design/cli.md).
func deployProgress(d deps, start time.Time, ms *v1alpha1.ModelService, res *profile.Result) func(kube.Stage) {
	return func(stage kube.Stage) {
		var text string
		switch stage {
		case kube.StageFetch:
			return
		case kube.StageNode:
			mode := "spot"
			if ms.Spec.Accelerator.Spot != nil && !*ms.Spec.Accelerator.Spot {
				mode = "on-demand"
			}
			text = fmt.Sprintf("waiting for a GPU node (%dx %s, %s)", res.GPUCount, res.GPUType, mode)
		case kube.StageImage:
			text = "pulling the vLLM image"
		case kube.StageLoading:
			text = "loading weights and warming up"
		case kube.StageReady:
			text = ms.Name + " is ready"
		default:
			return
		}
		stageLine(d.Stdout, start, d, string(stage), text)
	}
}
