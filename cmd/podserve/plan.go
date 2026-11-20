package main

// plan.go is `podserve plan SOURCE [flags]`: profile a model's GPU needs without touching a cluster
// (docs/design/cli.md). The text and JSON renderers are in output.go.

import (
	"github.com/spf13/cobra"

	"github.com/ryanymt/podserve/pkg/profile"
)

func planCmd(d deps) *cobra.Command {
	var (
		revision    string
		gpu         string
		gpus        int
		maxModelLen int
		gpuMemPct   int
		output      string
	)
	cmd := &cobra.Command{
		Use:   "plan SOURCE",
		Short: "Plan the GPU profile for a Hugging Face model",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			src, err := parseModelSource(args[0])
			if err != nil {
				return err
			}
			if err := checkRevisionFlag(src, cmd.Flags().Changed("revision")); err != nil {
				return err
			}
			if src.rev == "" {
				src.rev = revision
			}
			m, res, ref, err := planRevision(cmd.Context(), d, src, profile.Request{
				GPUType:          gpu,
				GPUCount:         gpus,
				MaxModelLen:      maxModelLen,
				GPUMemoryPercent: gpuMemPct,
				HasToken:         d.Getenv("HF_TOKEN") != "",
			})
			if err != nil {
				return err
			}
			if ref != nil {
				if output == "json" {
					return printPlanJSON(d.Stdout, m, nil, ref)
				}
				return ref.error(src.repo)
			}
			if output == "json" {
				return printPlanJSON(d.Stdout, m, &res, nil)
			}
			printPlanText(d.Stdout, m, res)
			return nil
		},
	}
	cmd.Flags().StringVar(&revision, "revision", "main", "model revision (branch, tag or commit SHA)")
	cmd.Flags().StringVar(&gpu, "gpu", "auto", "GPU type from the catalogue, or auto")
	cmd.Flags().IntVar(&gpus, "gpus", 0, "number of GPUs per replica (0 = choose)")
	cmd.Flags().IntVar(&maxModelLen, "max-model-len", 0, "context length cap in tokens (0 = choose)")
	cmd.Flags().IntVar(&gpuMemPct, "gpu-memory-percent", profile.DefaultGPUMemoryPercent,
		"share of each GPU's memory vLLM may use")
	cmd.Flags().StringVarP(&output, "output", "o", "text", "output format: text or json")
	return cmd
}
