package main

// list.go is `podserve list`, and the two single-name commands that only need the record:
// `podserve delete` and `podserve port-forward` (docs/design/cli.md).

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/podserve/podserve/pkg/api/v1alpha1"
	"github.com/podserve/podserve/pkg/kube"
	"github.com/podserve/podserve/pkg/platform"
)

// ---- list -----------------------------------------------------------------------------------------

// listRow is one model as `podserve list -o json` reports it.
type listRow struct {
	Name     string `json:"name"`
	Source   string `json:"source"`
	Revision string `json:"revision"`
	GPUType  string `json:"gpuType"`
	GPUCount int32  `json:"gpuCount"`
	Phase    string `json:"phase"`
	Endpoint string `json:"endpoint"`
}

func listCmd(d deps) *cobra.Command {
	var output string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List the deployed models",
		RunE: func(cmd *cobra.Command, args []string) error {
			kc, err := clusterClient(d, 0, nil)
			if err != nil {
				return err
			}
			items, err := kc.List(cmd.Context())
			if err != nil {
				return failf(exitUnexpected, "%s", err)
			}
			rows := make([]listRow, 0, len(items))
			for i := range items {
				rows = append(rows, rowOf(&items[i]))
			}
			if output == "json" {
				b, err := json.MarshalIndent(rows, "", "  ")
				if err != nil {
					return failf(exitUnexpected, "%s", err)
				}
				fmt.Fprintln(d.Stdout, string(b))
				return nil
			}
			printListText(d.Stdout, rows)
			return nil
		},
	}
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}

// rowOf reads one record for display. REVISION is the resolved revision's first 12 characters, GPU
// the status profile's count and type (docs/design/cli.md).
func rowOf(ms *v1alpha1.ModelService) listRow {
	row := listRow{
		Name:     ms.Name,
		Source:   ms.Spec.Model.Source,
		Revision: shortSHA(ms.Status.ResolvedRevision),
		Phase:    string(ms.Status.Phase),
		Endpoint: ms.Status.Endpoint,
	}
	if p := ms.Status.Profile; p != nil {
		row.GPUType = p.GPUType
		row.GPUCount = p.GPUCount
	}
	return row
}

// printListText writes the table (tabwriter, columns as wide as their longest cell plus two spaces)
// or "No models deployed." when there is nothing to show. A missing value prints -.
func printListText(out io.Writer, rows []listRow) {
	if len(rows) == 0 {
		fmt.Fprintln(out, "No models deployed.")
		return
	}
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	defer tw.Flush()
	fmt.Fprintln(tw, "NAME\tMODEL\tREVISION\tGPU\tPHASE\tENDPOINT")
	for _, r := range rows {
		gpu := "-"
		if r.GPUType != "" {
			gpu = fmt.Sprintf("%dx %s", r.GPUCount, r.GPUType)
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n",
			orDash(r.Name), orDash(r.Source), orDash(r.Revision), gpu, orDash(r.Phase), orDash(r.Endpoint))
	}
}

// orDash is s, or "-" when s is empty.
func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// ---- delete ---------------------------------------------------------------------------------------

func deleteCmd(d deps) *cobra.Command {
	var timeout time.Duration
	cmd := &cobra.Command{
		Use:   "delete NAME",
		Short: "Delete a model and wait until its pods are gone",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			kc, err := clusterClient(d, timeout, nil)
			if err != nil {
				return err
			}
			start := d.Now()
			if err := kc.Delete(cmd.Context(), name); err != nil {
				if errors.Is(err, kube.ErrNotFound) {
					return failf(exitNotFound, "no model named %s", name)
				}
				var te *kube.TimeoutError
				if errors.As(err, &te) {
					return failf(exitTimeout, "%s's pods still running after %s", name, te.After)
				}
				return failf(exitUnexpected, "%s", err)
			}
			el := d.Now().Sub(start)
			fmt.Fprintf(d.Stdout, "Deleted %s (pods gone after %d:%02d)\n", name, int(el.Minutes()), int(el.Seconds())%60)
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 10*time.Minute, "how long to wait for the pods to be gone")
	return cmd
}

// ---- port-forward ---------------------------------------------------------------------------------

func portForwardCmd(d deps) *cobra.Command {
	var port int
	cmd := &cobra.Command{
		Use:   "port-forward NAME",
		Short: "Forward a local port to a model's service",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			kc, err := clusterClient(d, 0, nil)
			if err != nil {
				return err
			}
			if _, err := kc.Get(cmd.Context(), name); err != nil {
				return notFoundOrFail(name, err)
			}
			fmt.Fprintf(d.Stdout, "Forwarding localhost:%d -> %s (Ctrl-C to stop)\n", port, name)
			err = d.Exec(cmd.Context(), d.Stdout, d.Stderr, "kubectl", "port-forward",
				"-n", platform.Namespace, "svc/"+name, fmt.Sprintf("%d:%d", port, platform.ServingPort))
			if err == nil {
				return nil
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) && ee.ExitCode() >= 0 {
				return &exitError{code: ee.ExitCode()} // kubectl's own exit code (D-018)
			}
			return failf(exitUnexpected, "%s", err)
		},
	}
	cmd.Flags().IntVar(&port, "port", platform.ServingPort, "the local port to forward")
	return cmd
}

// notFoundOrFail maps a lookup miss to `no model named <NAME>`, exit 4 (docs/design/cli.md).
func notFoundOrFail(name string, err error) error {
	if errors.Is(err, kube.ErrNotFound) {
		return failf(exitNotFound, "no model named %s", name)
	}
	return failf(exitUnexpected, "%s", err)
}
