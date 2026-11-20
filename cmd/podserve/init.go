package main

// init.go is `podserve init --config FILE`: read the Terraform outputs, build the fetch image with
// Cloud Build (unless --fetch-image), and install the platform on the cluster (docs/design/cli.md).

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ryanymt/podserve/config/crd"
	"github.com/ryanymt/podserve/pkg/platform"
)

// initOutputs are the Terraform outputs `podserve init --config` needs, in docs/design/platform.md's
// order: the first missing one names itself in the error.
var initOutputs = []string{
	"project_id", "region", "model_bucket", "dockerhub_mirror",
	"images_repo", "hf_token_secret", "build_service_account", "cluster_name",
}

func initCmd(d deps) *cobra.Command {
	var (
		config     string
		fetchImage string
	)
	cmd := &cobra.Command{
		Use:   "init --config FILE",
		Short: "Install the podserve platform on the cluster",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runInit(cmd.Context(), d, config, fetchImage)
		},
	}
	cmd.Flags().StringVar(&config, "config", "", "path of a `terraform output -json` file")
	_ = cmd.MarkFlagRequired("config")
	cmd.Flags().StringVar(&fetchImage, "fetch-image", "", "use this fetch image instead of building one")
	return cmd
}

// runInit installs the platform (docs/design/cli.md, init).
func runInit(ctx context.Context, d deps, configFile, fetchImageFlag string) error {
	b, err := os.ReadFile(configFile)
	if err != nil {
		return failf(exitUsage, "%s", err)
	}
	out, err := readTerraformOutputs(configFile, b)
	if err != nil {
		return err
	}
	get := func(name string) string { return out[name].Value }

	fetchImage := fetchImageFlag
	if fetchImage != "" {
		fmt.Fprintf(d.Stdout, "Fetch image: %s (given)\n", fetchImage)
	} else {
		ref, err := buildFetchImage(ctx, d, get, d.Version)
		if err != nil {
			return err
		}
		fetchImage = ref
		fmt.Fprintf(d.Stdout, "Fetch image: %s\n", fetchImage)
	}

	kc, err := clusterClient(d, 0, nil)
	if err != nil {
		return err
	}
	plat := platform.Platform{
		Project:       get("project_id"),
		Region:        get("region"),
		Bucket:        get("model_bucket"),
		VLLMImage:     platform.VLLMImageRef(get("dockerhub_mirror")),
		FetchImage:    fetchImage,
		HFTokenSecret: get("hf_token_secret"),
	}
	if err := kc.Install(ctx, crd.ModelServicesYAML, plat.Data()); err != nil {
		return failf(exitUnexpected, "%s", err)
	}
	fmt.Fprintln(d.Stdout, "Installed CRD modelservices.podserve.dev")
	fmt.Fprintf(d.Stdout, "Created namespace %s and service accounts %s, %s\n",
		platform.Namespace, platform.FetchSA, platform.ModelSA)
	fmt.Fprintf(d.Stdout, "Saved platform settings to %s/%s\n", platform.Namespace, platform.ConfigMapName)
	fmt.Fprintln(d.Stdout, "podserve is ready. Next: podserve deploy hf://Qwen/Qwen3-8B")
	return nil
}

// terraformOutput is one entry of `terraform output -json`.
type terraformOutput struct {
	Value string `json:"value"`
}

// readTerraformOutputs parses the file and checks every required output, in order (docs/design/cli.md).
func readTerraformOutputs(configFile string, b []byte) (map[string]terraformOutput, error) {
	var out map[string]terraformOutput
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, failf(exitUsage, "%s: %s", configFile, err)
	}
	for _, name := range initOutputs {
		if _, ok := out[name]; !ok {
			return nil, failf(exitUsage, "%s has no output %q", configFile, name)
		}
	}
	return out, nil
}

// buildFetchImage checks the repository root and runs the two gcloud commands of docs/design/cli.md,
// returning <IMAGES_REPO>/fetch@<DIGEST>.
func buildFetchImage(ctx context.Context, d deps, get func(string) string, version string) (string, error) {
	if _, err := os.Stat(filepath.Join("images", "fetch", "Dockerfile")); err != nil {
		return "", failf(exitUsage, "images/fetch not found; run init from the podserve repository root or pass --fetch-image")
	}
	fmt.Fprintln(d.Stdout, "Building the fetch image with Cloud Build...")
	tag := fmt.Sprintf("%s/fetch:%s", get("images_repo"), imageTag(version))
	if err := d.Exec(ctx, d.Stderr, d.Stderr, "gcloud", "builds", "submit", "images/fetch",
		"--project", get("project_id"),
		"--region", get("region"),
		"--tag", tag,
		"--service-account", "projects/"+get("project_id")+"/serviceAccounts/"+get("build_service_account"),
		"--gcs-source-staging-dir", "gs://"+get("model_bucket")+"/_build/source",
		"--gcs-log-dir", "gs://"+get("model_bucket")+"/_build/logs",
	); err != nil {
		return "", failf(exitUnexpected, "%s", err)
	}
	var digest strings.Builder
	if err := d.Exec(ctx, &digest, d.Stderr, "gcloud", "artifacts", "docker", "images", "describe", tag,
		"--project", get("project_id"),
		"--format", "value(image_summary.digest)",
	); err != nil {
		return "", failf(exitUnexpected, "%s", err)
	}
	ref := strings.TrimSpace(digest.String())
	if ref == "" {
		return "", failf(exitUnexpected, "gcloud reported no digest for %s", tag)
	}
	return fmt.Sprintf("%s/fetch@%s", get("images_repo"), ref), nil
}

// imageTag turns a version into a Docker tag (D-030). Tags allow only [A-Za-z0-9_.-], and Go adds
// "+dirty" to the version of a build from a modified tree.
func imageTag(version string) string {
	return strings.ReplaceAll(version, "+", "-")
}
