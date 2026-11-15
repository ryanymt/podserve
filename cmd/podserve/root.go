package main

// root.go builds the command tree and maps the errors the commands return to the exit codes of
// docs/design/cli.md. The commands live in plan.go, deploy.go, init.go and list.go; the output
// helpers they share are in output.go.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/podserve/podserve/catalog"
	"github.com/podserve/podserve/pkg/hf"
	"github.com/podserve/podserve/pkg/kube"
	"github.com/podserve/podserve/pkg/profile"
)

// Exit codes (docs/design/cli.md).
const (
	exitOK          = 0
	exitUnexpected  = 1
	exitUsage       = 2 // bad arguments or flags, or an invalid spec
	exitRefused     = 3 // the settings engine won't serve this model here
	exitNotFound    = 4 // not found or unreachable
	exitTimeout     = 5 // timed out waiting
	exitFetchFailed = 6 // the fetch Job failed
)

// exitError carries an exit code and the stderr lines printed for it, each prefixed "podserve: "
// (some errors print several lines, e.g. an invalid spec). An empty lines list exits silently,
// which is how a refusal reported as JSON (stdout) still exits 3.
type exitError struct {
	code  int
	lines []string
}

func (e *exitError) Error() string { return strings.Join(e.lines, "; ") }

// failf builds a one-line exitError.
func failf(code int, format string, a ...any) error {
	return &exitError{code: code, lines: []string{fmt.Sprintf(format, a...)}}
}

// failLines builds a multi-line exitError.
func failLines(code int, lines ...string) error {
	return &exitError{code: code, lines: lines}
}

// newRoot assembles the podserve command tree.
func newRoot(d deps) *cobra.Command {
	root := &cobra.Command{
		Use:   "podserve",
		Short: "Deploy Hugging Face models on GKE behind an OpenAI-compatible API.",
	}
	root.AddCommand(
		versionCmd(d),
		planCmd(d),
		initCmd(d),
		deployCmd(d),
		listCmd(d),
		deleteCmd(d),
		portForwardCmd(d),
	)
	return root
}

func versionCmd(d deps) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the podserve version",
		RunE: func(*cobra.Command, []string) error {
			fmt.Fprintln(d.Stdout, "podserve "+d.Version)
			return nil
		},
	}
}

// ---- shared helpers -------------------------------------------------------------------------------

// depsClock adapts deps.Now and deps.Sleep to kube.Clock.
type depsClock struct{ d deps }

func (c depsClock) Now() time.Time        { return c.d.Now() }
func (c depsClock) Sleep(d time.Duration) { c.d.Sleep(d) }

// loadCatalog parses the embedded GPU catalogue and vLLM architecture list. A parse error of the
// embedded files is a bug, so exit 1.
func loadCatalog() (profile.Catalog, profile.Architectures, error) {
	cat, err := profile.LoadCatalog(catalog.GPUsYAML)
	if err != nil {
		return profile.Catalog{}, nil, failf(exitUnexpected, "%s", err)
	}
	return cat, profile.LoadArchitectures(catalog.VLLMArchitectures), nil
}

// modelSource is a parsed SOURCE argument: hf://ORG/MODEL[@REVISION].
type modelSource struct {
	repo  string // ORG/MODEL
	rev   string // the revision as given; "" until resolved against the --revision flag
	hasAt bool   // SOURCE carried an explicit @REVISION
}

// parseModelSource splits a SOURCE argument (docs/design/cli.md, Model sources).
func parseModelSource(s string) (modelSource, error) {
	invalid := failf(exitUsage, "model source %s: want hf://ORG/MODEL", s)
	rest, ok := strings.CutPrefix(s, "hf://")
	if !ok {
		return modelSource{}, invalid
	}
	src := modelSource{}
	if i := strings.IndexByte(rest, '@'); i >= 0 {
		src.rev, rest, src.hasAt = rest[i+1:], rest[:i], true
		if src.rev == "" || strings.Contains(rest, "@") {
			return modelSource{}, invalid
		}
	}
	if strings.Count(rest, "/") != 1 {
		return modelSource{}, invalid
	}
	org, model, ok := strings.Cut(rest, "/")
	if !ok || org == "" || model == "" {
		return modelSource{}, invalid
	}
	src.repo = rest
	return src, nil
}

// revision returns src's revision, defaulting to main.
func (s modelSource) revision() string {
	if s.rev == "" {
		return "main"
	}
	return s.rev
}

// hubModel resolves one revision through the Hub, mapping the Hub errors to their exit codes and
// texts (docs/design/cli.md).
func hubModel(ctx context.Context, d deps, src modelSource) (profile.Model, error) {
	m, err := d.NewHF(d.Getenv("HF_TOKEN")).Model(ctx, src.repo, src.revision())
	if errors.Is(err, hf.ErrNotFound) {
		return profile.Model{}, failf(exitNotFound, "%s@%s not found on the Hugging Face Hub", src.repo, src.revision())
	}
	if errors.Is(err, hf.ErrAccessDenied) {
		return profile.Model{}, failf(exitRefused, "the Hugging Face Hub refused access to %s with this token; accept the model's licence on huggingface.co", src.repo)
	}
	if err != nil {
		return profile.Model{}, failf(exitUnexpected, "%s", err)
	}
	return m, nil
}

// refusal is why a model won't be served: any profile.Refusal, or the CLI's own NOT_DEPLOYABLE when
// only a GPU type v0.1 doesn't deploy fits.
type refusal struct {
	code    string
	message string
}

// error is the stderr line a refusal maps to: `podserve: cannot serve <ORG/MODEL>: <MESSAGE> (<CODE>)`,
// exit 3 (docs/design/cli.md).
func (r refusal) error(repo string) error {
	return failf(exitRefused, "cannot serve %s: %s (%s)", repo, r.message, r.code)
}

// planRevision resolves the revision on the Hub and plans a profile for it. A refusal comes back as
// ref rather than an error, so the text and JSON outputs can each report it their own way.
func planRevision(ctx context.Context, d deps, src modelSource, req profile.Request,
) (profile.Model, profile.Result, *refusal, error) {
	cat, archs, err := loadCatalog()
	if err != nil {
		return profile.Model{}, profile.Result{}, nil, err
	}
	m, err := hubModel(ctx, d, src)
	if err != nil {
		return profile.Model{}, profile.Result{}, nil, err
	}
	res, err := profile.Plan(m, req, cat, archs)
	if err != nil {
		var r *profile.Refusal
		if !errors.As(err, &r) {
			return profile.Model{}, profile.Result{}, nil, failf(exitUnexpected, "%s", err)
		}
		return m, profile.Result{}, &refusal{code: string(r.Code), message: r.Message}, nil
	}
	return m, res, nil, nil
}

// checkRevisionFlag is the usage error for giving the revision both as @REVISION and --revision.
func checkRevisionFlag(src modelSource, changed bool) error {
	if src.hasAt && changed {
		return failf(exitUsage, "give the revision either as @REVISION or with --revision, not both")
	}
	return nil
}

// stageLine prints one stage line of deploy: `[m:ss] <stage padded to 10> <text>`, with the time
// elapsed since the command started (docs/design/cli.md).
func stageLine(out io.Writer, start time.Time, d deps, stage, text string) {
	el := d.Now().Sub(start)
	fmt.Fprintf(out, "[%d:%02d] %-10s%s\n", int(el.Minutes()), int(el.Seconds())%60, stage, text)
}

// clusterClient builds the kube client for the current context, with the injected clock and the
// given wait timeout. A kubeconfig that cannot be loaded means the cluster is unreachable.
func clusterClient(d deps, timeout time.Duration, progress func(kube.Stage)) (*kube.Client, error) {
	dyn, err := d.NewDynamic()
	if err != nil {
		return nil, failf(exitNotFound, "%s", err)
	}
	return &kube.Client{Dynamic: dyn, Options: kube.Options{
		Clock:    depsClock{d},
		Timeout:  timeout,
		Progress: progress,
	}}, nil
}
